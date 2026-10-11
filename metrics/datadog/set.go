package datadog

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/alessandrolattao/powertools-lambda-go/internal/warn"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/metrics/internal/pyjson"
)

// Datadog's limits on names and tags.
const (
	maxNameLength = 200
	maxTagLength  = 200
	// maxTimestampAge is how old a timestamp the extension takes, as
	// datadog-lambda checks it before submitting.
	maxTimestampAge = 4 * time.Hour
)

const (
	namingDocs  = "https://docs.datadoghq.com/metrics/custom_metrics/#naming-custom-metrics"
	taggingDocs = "https://docs.datadoghq.com/getting_started/tagging/#define-tags"
)

// errUnsupported is what the CloudWatch-only operations return.
var errUnsupported = fmt.Errorf("datadog: dimensions, metadata and set timestamps are CloudWatch EMF features, "+
	"tag and timestamp each metric instead: %w", errors.ErrUnsupported)

type record struct {
	name      string
	value     float64
	timestamp time.Time
	explicit  bool // the caller set the timestamp
	tags      []metrics.Dimension
}

// set is the metric set of one invocation: a list of data points, each with
// its own timestamp and tags.
type set struct {
	p        *Provider
	defaults []metrics.Dimension
	records  []record
}

func (s *set) AddMetric(ctx context.Context, m metrics.Metric) error {
	if !validName(m.Name) {
		return fmt.Errorf("%w: %w: %q does not follow the requirements, see %s",
			metrics.ErrSchemaValidation, metrics.ErrMetricName, m.Name, namingDocs)
	}
	validateTags(ctx, m.Tags)
	if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
		return fmt.Errorf("%w: %v is not a valid number", metrics.ErrMetricValue, m.Value)
	}
	r := record{name: m.Name, value: m.Value, timestamp: m.Timestamp, explicit: !m.Timestamp.IsZero(), tags: m.Tags}
	if !r.explicit {
		r.timestamp = s.p.now()
	}
	s.records = append(s.records, r)
	return nil
}

// validName applies Datadog's naming rules: a letter first, then ASCII
// letters, digits, underscores and periods, 200 characters at most.
func validName(name string) bool {
	if name == "" || len(name) > maxNameLength {
		return false
	}
	first, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsLetter(first) {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// validateTags warns about the tags Datadog may drop: not starting with a
// letter, or longer than 200 characters. They are kept, as Python keeps them.
func validateTags(ctx context.Context, tags []metrics.Dimension) {
	for _, t := range tags {
		tag := t.Name + ":" + t.Value
		first, _ := utf8.DecodeRuneInString(tag)
		if unicode.IsLetter(first) && utf8.RuneCountInString(tag) <= maxTagLength {
			continue
		}
		warn.Once(ctx, warn.DatadogDataValidationWarning,
			fmt.Sprintf("Invalid tag value. Please ensure the specific tag %s follows the requirements. \n"+
				"May incur data loss for metrics. \nSee Datadog documentation here: \n %s", tag, taggingDocs))
	}
}

func (*set) AddDimension(context.Context, string, string) error { return errUnsupported }

func (*set) AddDimensions(context.Context, []metrics.Dimension) error { return errUnsupported }

func (*set) AddMetadata(context.Context, string, any) error { return errUnsupported }

func (*set) SetTimestamp(context.Context, time.Time) error { return errUnsupported }

func (s *set) Clear() { s.records = nil }

func (s *set) Len() int { return len(s.records) }

// Flush sends the metrics to the extension, or writes them to standard
// output, one JSON line per metric, and empties the set.
func (s *set) Flush(ctx context.Context, raiseOnEmpty bool) error {
	defer s.Clear()
	if len(s.records) == 0 {
		if !raiseOnEmpty {
			warn.Once(ctx, warn.UserWarning, warn.NoMetrics)
			return nil
		}
		return metrics.ErrNoMetrics
	}
	if s.p.disabled {
		return nil
	}
	if s.p.statsd != "" && !s.p.flushToLog {
		s.send(ctx)
		return nil
	}
	for _, r := range s.records {
		line, err := s.line(r)
		if err != nil {
			return err
		}
		if _, err := s.p.out.Write(line); err != nil {
			return fmt.Errorf("datadog: write the metric: %w", err)
		}
	}
	return nil
}

// name returns the name Datadog receives: prefixed with the namespace,
// unless it is the default one.
func (s *set) name(r record) string {
	if s.p.namespace == DefaultNamespace {
		return r.name
	}
	return s.p.namespace + "." + r.name
}

// tags returns the key:value tags of r, the default ones first, the metric's
// winning on the same key.
func (s *set) tags(r record) []string {
	merged := mergeTags(s.defaults, r.tags)
	out := make([]string, len(merged))
	for i, t := range merged {
		out[i] = t.Name + ":" + t.Value
	}
	return out
}

// line serializes r the way Powertools Python prints it for the Datadog
// Forwarder: {"m":name,"v":value,"e":epoch seconds,"t":[tags]}.
func (s *set) line(r record) ([]byte, error) {
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf, jsontext.AllowInvalidUTF8(true))
	tags := s.tags(r)
	tokens := make([]jsontext.Token, 0, 11+len(tags))
	tokens = append(tokens,
		jsontext.BeginObject,
		jsontext.String("m"), jsontext.String(s.name(r)),
		jsontext.String("v"), jsontext.Float(r.value),
		jsontext.String("e"), jsontext.Int(r.timestamp.Unix()),
		jsontext.String("t"), jsontext.BeginArray,
	)
	for _, tag := range tags {
		tokens = append(tokens, jsontext.String(tag))
	}
	tokens = append(tokens, jsontext.EndArray, jsontext.EndObject)
	for _, t := range tokens {
		if err := enc.WriteToken(t); err != nil {
			return nil, fmt.Errorf("datadog: serialize the metric: %w", err)
		}
	}
	return pyjson.EscapeNonASCII(buf.Bytes()), nil
}

// send submits the metrics to the Datadog Lambda Extension as DogStatsD
// distributions, as datadog-lambda's lambda_metric does. Like it, a metric
// that cannot be delivered is reported, not returned: telemetry must not
// fail the invocation.
func (s *set) send(ctx context.Context) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", s.p.statsd)
	if err != nil {
		warn.Once(ctx, warn.UserWarning, "Could not reach the Datadog extension", "err", err)
		return
	}
	defer conn.Close() //nolint:errcheck // closing a UDP socket reports nothing worth acting on
	floor := s.p.now().Add(-maxTimestampAge)
	for _, r := range s.records {
		if r.explicit && r.timestamp.Before(floor) {
			warn.Once(ctx, warn.UserWarning, fmt.Sprintf("Timestamp %d is older than 4 hours, not submitting metric %s",
				r.timestamp.Unix(), s.name(r)))
			continue
		}
		if _, err := conn.Write(s.datagram(r)); err != nil {
			warn.Once(ctx, warn.UserWarning, "Could not send a metric to the Datadog extension", "err", err)
			return
		}
	}
}

// datagram formats r as a DogStatsD distribution:
// name:value|d|#tag1,tag2|Ttimestamp. The timestamp goes only with the
// metrics whose caller set it; the extension stamps the others on arrival.
func (s *set) datagram(r record) []byte {
	b := []byte(s.name(r))
	b = append(b, ':')
	b = strconv.AppendFloat(b, r.value, 'f', -1, 64)
	b = append(b, "|d"...)
	if tags := s.tags(r); len(tags) > 0 {
		b = append(b, "|#"...)
		for i, tag := range tags {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, normalizeTag(tag)...)
		}
	}
	if r.explicit {
		b = append(b, "|T"...)
		b = strconv.AppendInt(b, r.timestamp.Unix(), 10)
	}
	return b
}

// normalizeTag replaces what a DogStatsD tag cannot hold with underscores,
// as datadog-lambda's normalize_tags does: everything but letters, digits
// and _-:/. characters.
func normalizeTag(tag string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("_-:/.", r) {
			return r
		}
		return '_'
	}, tag)
}
