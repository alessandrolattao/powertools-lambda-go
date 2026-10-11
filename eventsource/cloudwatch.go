package eventsource

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// CloudWatchAlarmEvent is what a CloudWatch alarm sends when it invokes a
// Lambda function directly (alarm action). Alarms delivered through SNS use
// events.CloudWatchAlarmSNSPayload instead.
type CloudWatchAlarmEvent struct {
	Source    string `json:"source"`
	AlarmARN  string `json:"alarmArn"`
	Region    string `json:"region"`
	AccountID string `json:"accountId"`
	// Time is when the alarm changed state, in the CloudWatch format
	// (2024-02-17T11:53:08.431+0000); ParseCloudWatchTime reads it.
	Time      string              `json:"time"`
	AlarmData CloudWatchAlarmData `json:"alarmData"`
}

// CloudWatchAlarmData describes the alarm and its state change.
type CloudWatchAlarmData struct {
	AlarmName     string                       `json:"alarmName"`
	State         CloudWatchAlarmState         `json:"state"`
	PreviousState CloudWatchAlarmState         `json:"previousState"`
	Configuration CloudWatchAlarmConfiguration `json:"configuration"`
}

// The values of an alarm state.
const (
	CloudWatchAlarmStateOK               = "OK"
	CloudWatchAlarmStateAlarm            = "ALARM"
	CloudWatchAlarmStateInsufficientData = "INSUFFICIENT_DATA"
)

// CloudWatchAlarmState is a state of the alarm.
type CloudWatchAlarmState struct {
	// Value is OK, ALARM or INSUFFICIENT_DATA.
	Value  string `json:"value"`
	Reason string `json:"reason"`
	// ReasonData is a JSON document about the evaluation;
	// DecodeReasonData decodes it.
	ReasonData string `json:"reasonData,omitempty"`
	// Timestamp is in the CloudWatch format; ParseCloudWatchTime reads it.
	Timestamp string `json:"timestamp"`
	// ActionsSuppressedBy is Alarm, ExtensionPeriod or WaitPeriod when a
	// composite alarm's actions were suppressed.
	ActionsSuppressedBy     string `json:"actionsSuppressedBy,omitempty"`
	ActionsSuppressedReason string `json:"actionsSuppressedReason,omitempty"`
}

// DecodeReasonData decodes the reason data into a T. Empty reason data
// gives the zero T, as Powertools Python's reason_data_decoded gives None.
func (s CloudWatchAlarmState) DecodeReasonData[T any]() (T, error) {
	var out T
	if s.ReasonData == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(s.ReasonData), &out); err != nil {
		return out, fmt.Errorf("decode the alarm reason data into %T: %w", out, err)
	}
	return out, nil
}

// CloudWatchAlarmConfiguration is the configuration of the alarm: metrics
// for a metric alarm, a rule for a composite alarm.
type CloudWatchAlarmConfiguration struct {
	Description string                  `json:"description,omitempty"`
	AlarmRule   string                  `json:"alarmRule,omitempty"`
	Metrics     []CloudWatchAlarmMetric `json:"metrics,omitempty"`
	// ActionsSuppressor and its periods (seconds) configure the action
	// suppression of a composite alarm.
	ActionsSuppressor                string `json:"actionsSuppressor,omitempty"`
	ActionsSuppressorWaitPeriod      int64  `json:"actionsSuppressorWaitPeriod,omitempty"`
	ActionsSuppressorExtensionPeriod int64  `json:"actionsSuppressorExtensionPeriod,omitempty"`
}

// CloudWatchAlarmMetric is a metric or a metric math expression of the alarm.
type CloudWatchAlarmMetric struct {
	ID         string                     `json:"id"`
	Expression string                     `json:"expression,omitempty"`
	Label      string                     `json:"label,omitempty"`
	ReturnData bool                       `json:"returnData"`
	MetricStat *CloudWatchAlarmMetricStat `json:"metricStat,omitempty"`
}

// CloudWatchAlarmMetricStat is the metric and statistic a metric of the
// alarm evaluates.
type CloudWatchAlarmMetricStat struct {
	Metric CloudWatchMetric `json:"metric"`
	// Period is in seconds.
	Period int64  `json:"period,omitempty"`
	Stat   string `json:"stat,omitempty"`
	Unit   string `json:"unit,omitempty"`
}

// CloudWatchMetric identifies a metric.
type CloudWatchMetric struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Dimensions map[string]string `json:"dimensions"`
}

// cloudWatchTimeLayout is the format of CloudWatch alarm times, with a zone
// offset without colon that time.RFC3339 does not accept.
const cloudWatchTimeLayout = "2006-01-02T15:04:05.000-0700"

// ParseCloudWatchTime parses a CloudWatch alarm time such as
// 2024-02-17T11:53:08.431+0000. RFC 3339 times are accepted too.
func ParseCloudWatchTime(value string) (time.Time, error) {
	t, err := time.Parse(cloudWatchTimeLayout, value)
	if err == nil {
		return t, nil
	}
	if t, rfcErr := time.Parse(time.RFC3339Nano, value); rfcErr == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("parse the CloudWatch time %q: %w", value, err)
}

// CloudWatchDashboardCustomWidgetEvent is what a CloudWatch dashboard custom
// widget sends to its Lambda function.
type CloudWatchDashboardCustomWidgetEvent struct {
	// Describe asks for the widget's documentation instead of its content.
	Describe      bool                     `json:"describe,omitempty"`
	WidgetContext *CloudWatchWidgetContext `json:"widgetContext,omitempty"`
	// Params holds the other members of the event: the parameters the
	// widget definition passes to the function. It is filled when the event
	// is decoded with encoding/json/v2 (the parser package does).
	Params jsontext.Value `json:",embed"` //nolint:revive // "embed" is an encoding/json/v2 option revive does not know
}

// CloudWatchWidgetContext describes the dashboard and the widget.
type CloudWatchWidgetContext struct {
	DashboardName string              `json:"dashboardName"`
	WidgetID      string              `json:"widgetId"`
	Domain        string              `json:"domain"`
	AccountID     string              `json:"accountId"`
	Locale        string              `json:"locale"`
	Timezone      CloudWatchTimezone  `json:"timezone"`
	Period        int64               `json:"period"`
	IsAutoPeriod  bool                `json:"isAutoPeriod"`
	TimeRange     CloudWatchTimeRange `json:"timeRange"`
	Theme         string              `json:"theme"`
	LinkCharts    bool                `json:"linkCharts"`
	Title         string              `json:"title"`
	Forms         CloudWatchForms     `json:"forms"`
	Params        map[string]any      `json:"params"`
	Width         int64               `json:"width"`
	Height        int64               `json:"height"`
}

// CloudWatchTimezone is the time zone the dashboard displays.
type CloudWatchTimezone struct {
	Label           string `json:"label"`
	OffsetISO       string `json:"offsetISO"`
	OffsetInMinutes int64  `json:"offsetInMinutes"`
}

// CloudWatchTimeRange is the time range the dashboard displays, in
// milliseconds since the Unix epoch.
type CloudWatchTimeRange struct {
	Mode          string              `json:"mode"`
	Start         int64               `json:"start"`
	End           int64               `json:"end"`
	RelativeStart int64               `json:"relativeStart,omitempty"`
	Zoom          *CloudWatchTimeZoom `json:"zoom,omitempty"`
}

// CloudWatchTimeZoom is the zoomed part of the time range.
type CloudWatchTimeZoom struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// CloudWatchForms holds the values of the forms in the widget.
type CloudWatchForms struct {
	All map[string]any `json:"all"`
}

// CloudWatchLogsData is events.CloudwatchLogsData with the members it lacks:
// the policy level of account-level subscriptions and the extracted fields
// of each log event.
type CloudWatchLogsData struct {
	events.CloudwatchLogsData

	PolicyLevel string                   `json:"policyLevel,omitempty"`
	LogEvents   []CloudWatchLogsLogEvent `json:"logEvents"`
}

// CloudWatchLogsLogEvent is events.CloudwatchLogsLogEvent with the fields a
// subscription filter pattern extracted.
type CloudWatchLogsLogEvent struct {
	events.CloudwatchLogsLogEvent

	ExtractedFields map[string]string `json:"extractedFields,omitempty"`
}

// ParseCloudWatchLogs decodes the payload of a CloudWatch Logs subscription
// event: base64, then gzip or zlib, then JSON. Unlike
// events.CloudwatchLogsRawData.Parse it accepts zlib streams and keeps the
// policy level and extracted fields.
func ParseCloudWatchLogs(raw events.CloudwatchLogsRawData) (CloudWatchLogsData, error) {
	compressed, err := base64.StdEncoding.DecodeString(raw.Data)
	if err != nil {
		return CloudWatchLogsData{}, fmt.Errorf("decode the base64 logs data: %w", err)
	}
	return decodeCloudWatchLogs(compressed)
}

func decodeCloudWatchLogs(compressed []byte) (CloudWatchLogsData, error) {
	var data CloudWatchLogsData
	decompressed, err := Decompress(compressed)
	if err != nil {
		return data, err
	}
	if err := json.Unmarshal(decompressed, &data); err != nil {
		return data, fmt.Errorf("decode the logs data: %w", err)
	}
	return data, nil
}
