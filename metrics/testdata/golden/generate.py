"""Regenerates the golden EMF and Datadog documents from Powertools for AWS Lambda (Python).

The Go tests replay the same calls and compare their output with these files byte
for byte, so that a Go function and a Python function write the same documents.

Usage (from any directory, with the Powertools Python v3.35.0 sources checked out):

    python3 -I generate.py <powertools-lambda-python checkout>

The clock is frozen at 1700000000.5 seconds (2023-11-14T22:13:20.5Z), the time the
Go tests inject. Each scenario writes <name>.jsonl next to this script for EMF and
in ../../datadog/testdata/golden for Datadog: one document per line, in the order
Python prints them, re-serialized compactly (Python prints overflow documents with
spaces after separators; the Go port writes every document compactly).
"""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import types
import warnings
from collections import namedtuple
from pathlib import Path

FROZEN_SECONDS = 1700000000.5
HERE = Path(__file__).resolve().parent
DATADOG_DIR = HERE.parent.parent / "datadog" / "testdata" / "golden"


def load_powertools(checkout: str):
    """Imports the metrics package from a source checkout, stubbing jmespath when it is missing."""
    sys.path.insert(0, checkout)
    try:
        import jmespath  # noqa: F401
    except ImportError:

        class _Any:
            def __init__(self, *args, **kwargs):
                pass

            def __call__(self, *args, **kwargs):
                if len(args) == 1 and callable(args[0]) and not kwargs:
                    return args[0]
                return _Any()

            def __getattr__(self, name):
                return _Any()

            def __mro_entries__(self, bases):
                return (object,)

        for name in ("jmespath", "jmespath.exceptions", "jmespath.functions"):
            module = types.ModuleType(name)
            module.__path__ = []
            module.__getattr__ = lambda attr: _Any()
            sys.modules[name] = module


def freeze_clock():
    import datetime as real

    from aws_lambda_powertools.metrics import base, functions
    from aws_lambda_powertools.metrics.provider.cloudwatch_emf import cloudwatch
    from aws_lambda_powertools.metrics.provider.datadog import datadog

    class FrozenDateTime(real.datetime):
        @classmethod
        def now(cls, tz=None):
            return cls.fromtimestamp(FROZEN_SECONDS, tz)

    frozen_module = types.SimpleNamespace(datetime=FrozenDateTime, timedelta=real.timedelta)
    cloudwatch.datetime = frozen_module
    base.datetime = frozen_module
    functions.datetime = FrozenDateTime
    datadog.time = types.SimpleNamespace(time=lambda: FROZEN_SECONDS)
    return FrozenDateTime


def reset():
    from aws_lambda_powertools.metrics import Metrics
    from aws_lambda_powertools.metrics.provider.cold_start import reset_cold_start_flag
    from aws_lambda_powertools.metrics.provider.datadog import DatadogMetrics

    for state in (Metrics._metrics, Metrics._dimensions, Metrics._metadata, Metrics._default_dimensions):
        state.clear()
    DatadogMetrics._metrics.clear()
    DatadogMetrics._default_tags.clear()
    reset_cold_start_flag()
    for name in list(os.environ):
        if name.startswith("POWERTOOLS_") or name.startswith("DD_"):
            del os.environ[name]


def capture(fn) -> list[str]:
    out = io.StringIO()
    with contextlib.redirect_stdout(out), warnings.catch_warnings():
        warnings.simplefilter("ignore")
        fn()
    lines = []
    for line in out.getvalue().splitlines():
        if line.strip():
            lines.append(json.dumps(json.loads(line), separators=(",", ":")))
    return lines


def emf_scenarios(frozen):
    from aws_lambda_powertools.metrics import Metrics, MetricResolution, MetricUnit, single_metric
    from aws_lambda_powertools.metrics.provider.cloudwatch_emf.cloudwatch import AmazonCloudWatchEMFProvider

    context = namedtuple("LambdaContext", "function_name")("test")

    def log_metrics_basic():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)

        handler({}, context)

    def multi_value():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="environment", value="dev")
            metrics.add_metric(name="TurbineReads", unit=MetricUnit.Count, value=1)
            metrics.add_metric(name="TurbineReads", unit=MetricUnit.Count, value=8)

        handler({}, context)

    def high_resolution():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1, resolution=MetricResolution.High)
            metrics.add_metric(name="Latency", unit=MetricUnit.Milliseconds, value=12.5)

        handler({}, context)

    def two_dimensions():
        metrics = Metrics(namespace="test_namespace")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="metric_one", unit=MetricUnit.Count, value=1)
            metrics.add_metric(name="metric_two", unit=MetricUnit.Count, value=1)
            metrics.add_dimension(name="test_dimension", value="test")
            metrics.add_dimension(name="test_dimension_2", value="test")

        handler({}, context)

    def metadata():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)
            metrics.add_metadata(key="booking_id", value="00347014-341d-4b8e-8421-a89d3d588ab3")
            metrics.add_metadata(
                key="details",
                value={"amount": 10.5, "count": 3, "nested": {"a": [1, 2.5, None, True], "b": 1.0}, "text": "café"},
            )

        handler({}, context)

    def dimension_sets():
        metrics = AmazonCloudWatchEMFProvider(namespace="TestApp")
        metrics.add_dimension(name="service", value="booking")
        metrics.add_dimensions(environment="prod", region="us-east-1")
        metrics.add_dimensions(environment="prod")
        metrics.add_dimensions(region="us-east-1")
        metrics.add_metric(name="SuccessfulRequests", unit=MetricUnit.Count, value=10)
        metrics.flush_metrics()

    def dimension_sets_with_defaults():
        metrics = AmazonCloudWatchEMFProvider(namespace="TestApp")
        metrics.set_default_dimensions(tenant_id="123", application="api")
        metrics.add_dimensions(environment="prod")
        metrics.add_dimensions(region="eu-west-1")
        metrics.add_metric(name="ApiCalls", unit=MetricUnit.Count, value=5)
        metrics.flush_metrics()

    def dimension_sets_last_value_wins():
        metrics = AmazonCloudWatchEMFProvider(namespace="TestApp")
        metrics.add_dimensions(environment="dev", region="us-east-1")
        metrics.add_dimensions(environment="staging", region="us-west-2")
        metrics.add_dimensions(environment="prod")
        metrics.add_metric(name="TestMetric", unit=MetricUnit.Count, value=1)
        metrics.flush_metrics()

    def default_dimensions():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")
        metrics.set_default_dimensions(environment="test", log_group="/lambda/test")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="metric_one", unit=MetricUnit.Count, value=1)

        handler({}, context)
        handler({}, context)

    def cold_start():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")

        @metrics.log_metrics(capture_cold_start_metric=True)
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)
            metrics.add_dimension(name="test_dimension", value="test")

        handler({}, context)
        handler({}, context)

    def cold_start_no_service():
        metrics = Metrics(namespace="ServerlessAirline")

        @metrics.log_metrics(capture_cold_start_metric=True)
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)

        handler({}, context)

    def cold_start_function_name():
        metrics = Metrics(namespace="ServerlessAirline", service="booking", function_name="example_fn_constructor")

        @metrics.log_metrics(capture_cold_start_metric=True)
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)

        handler({}, context)

    def single():
        os.environ["POWERTOOLS_SERVICE_NAME"] = "booking"
        with single_metric(name="MySingleMetric", unit=MetricUnit.Count, value=1, namespace="ServerlessAirline") as m:
            m.add_dimension(name="environment", value="dev")

    def single_default_dimensions():
        with single_metric(
            name="RecordsCount",
            unit=MetricUnit.Count,
            value=10,
            namespace="ServerlessAirline",
            default_dimensions={"environment": "dev"},
        ) as m:
            m.add_dimension(name="TableName", value="Users")

    def single_timestamp():
        with single_metric(name="Orders", unit=MetricUnit.Count, value=10, namespace="Powertools") as m:
            m.set_timestamp(frozen.fromtimestamp(FROZEN_SECONDS - 3600))
            m.add_metadata(key="record_id", value="6ba7b810-9dad-11d1-80b4-00c04fd430c8")

    def custom_timestamp():
        metrics = Metrics(namespace="ServerlessAirline", service="booking")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)
            metrics.set_timestamp(int((FROZEN_SECONDS - 2 * 86400) * 1000))

        handler({}, context)

    def overflow_metrics():
        metrics = Metrics(namespace="test_namespace")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="test_dimension", value="test")
            for i in range(101):
                metrics.add_metric(name=f"metric_{i}", unit="Count", value=1)

        handler({}, context)

    def overflow_values():
        metrics = Metrics(namespace="test_namespace", service="test_service")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="test_dimension", value="test")
            for i in range(101):
                metrics.add_metric(name="metric", unit="Count", value=i)

        handler({}, context)

    def renamed_service():
        metrics = Metrics(namespace="test_application", service="test_service")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="service", value="another_test_service")
            metrics.add_metric(name="single_metric", unit=MetricUnit.Count, value=1)

        handler({}, context)

    def units_by_name():
        metrics = Metrics(namespace="ServerlessAirline")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="Throughput", unit="BytesPerSecond", value=2048)
            metrics.add_metric(name="Ratio", unit="NoUnit", value=0.5)
            metrics.add_metric(name="Rate", unit="Count/Second", value=3)

        handler({}, context)

    def float_formats():
        metrics = Metrics(namespace="ServerlessAirline")

        @metrics.log_metrics
        def handler(event, ctx):
            for value in (0.1, 1e16, 1e15, 1e-05, 0.0001, 123456789.123, -0.0, 1.5e300, 100, -2.5, 1 / 3):
                metrics.add_metric(name="Values", unit=MetricUnit.Count, value=value)

        handler({}, context)

    def non_ascii():
        metrics = Metrics(namespace="Café")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="city", value="Zürich ☕ \U0001f600")
            metrics.add_metric(name="Ünits", unit=MetricUnit.Count, value=1)
            metrics.add_metadata(key="note", value='tab\there "quoted" \\ back\x7fdel  ')

        handler({}, context)

    def metadata_overrides_dimension():
        metrics = Metrics(namespace="ServerlessAirline")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="environment", value="prod")
            metrics.add_metadata(key="environment", value="from-metadata")
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)

        handler({}, context)

    def empty_dimension_skipped():
        metrics = Metrics(namespace="ServerlessAirline")

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_dimension(name="my_empty_dimension", value=" ")
            metrics.add_dimension(name="environment", value="prod")
            metrics.add_metric(name="SuccessfulBooking", unit=MetricUnit.Count, value=1)

        handler({}, context)

    return [
        log_metrics_basic,
        multi_value,
        high_resolution,
        two_dimensions,
        metadata,
        dimension_sets,
        dimension_sets_with_defaults,
        dimension_sets_last_value_wins,
        default_dimensions,
        cold_start,
        cold_start_no_service,
        cold_start_function_name,
        single,
        single_default_dimensions,
        single_timestamp,
        custom_timestamp,
        overflow_metrics,
        overflow_values,
        renamed_service,
        units_by_name,
        float_formats,
        non_ascii,
        metadata_overrides_dimension,
        empty_dimension_skipped,
    ]


def datadog_scenarios():
    from aws_lambda_powertools.metrics.provider.datadog import DatadogMetrics, DatadogProvider

    context = namedtuple("LambdaContext", "function_name")("HelloWorldFunction")

    def tags():
        metrics = DatadogMetrics(flush_to_log=True)

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", value=1, tag1="powertools", tag2="python")

        handler({}, context)

    def namespace():
        metrics = DatadogMetrics(namespace="ServerlessAirline", flush_to_log=True)

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="item_sold", value=12.45, product="latte", order="online")
            metrics.add_metric(name="item_sold", value=3)

        handler({}, context)

    def default_tags():
        metrics = DatadogMetrics(flush_to_log=True)

        @metrics.log_metrics(default_tags={"environment": "test", "log_group": "/lambda/test"})
        def handler(event, ctx):
            metrics.add_metric(name="item_sold", value=1, environment="metric_precedence", product="powertools")

        handler({}, context)

    def cold_start():
        metrics = DatadogMetrics(provider=DatadogProvider(flush_to_log=True))

        @metrics.log_metrics(capture_cold_start_metric=True)
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", value=1, tag1="powertools")

        handler({}, context)

    def timestamp():
        metrics = DatadogMetrics(flush_to_log=True)

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="SuccessfulBooking", value=1, timestamp=1691768022)

        handler({}, context)

    def non_ascii():
        metrics = DatadogMetrics(flush_to_log=True)

        @metrics.log_metrics
        def handler(event, ctx):
            metrics.add_metric(name="orders", value=2, city="Zürich")

        handler({}, context)

    return [tags, namespace, default_tags, cold_start, timestamp, non_ascii]


def main():
    load_powertools(sys.argv[1])
    frozen = freeze_clock()
    for directory, scenarios in ((HERE, emf_scenarios(frozen)), (DATADOG_DIR, datadog_scenarios())):
        directory.mkdir(parents=True, exist_ok=True)
        for scenario in scenarios:
            reset()
            lines = capture(scenario)
            (directory / f"{scenario.__name__}.jsonl").write_text("".join(line + "\n" for line in lines))


if __name__ == "__main__":
    main()
