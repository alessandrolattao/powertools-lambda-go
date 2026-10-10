package tracing

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// The attributes X-Ray reads when it turns a span into a segment.
const (
	// AnnotationsAttribute lists the attributes X-Ray turns into annotations,
	// indexed for filter expressions and trace groups. Every other attribute
	// becomes metadata in the default namespace.
	AnnotationsAttribute = "aws.xray.annotations"
	// MetadataAttributePrefix, followed by a namespace, names an attribute
	// whose value, a JSON object, X-Ray turns into the metadata of that
	// namespace.
	MetadataAttributePrefix = "aws.xray.metadata."
	// DefaultNamespace is X-Ray's metadata namespace for attributes that name
	// none.
	DefaultNamespace = "default"
)

// metadataBudget caps the metadata a span carries, in bytes of JSON. The
// X-Ray endpoint takes a span in one UDP datagram, at most 64 KiB once
// base64-encoded; a span over it is lost whole, annotations included, which
// Powertools Python reports as "message too long". Metadata that would cross
// the budget is dropped instead.
const metadataBudget = 32 << 10

// AnnotationValue is the type of an annotation: X-Ray indexes strings,
// numbers and booleans.
type AnnotationValue interface {
	~string | ~bool |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// PutAnnotation annotates the span in ctx (the invocation, or the function
// captured last) with key and value. X-Ray indexes annotations, so that
// filter expressions and trace groups can select traces by them; it keeps up
// to 50 per trace.
//
// X-Ray accepts letters, digits and underscores in an annotation key: any
// other character becomes an underscore, so "order.id" is annotated as
// "order_id". Annotating a key again replaces its value. Without a span to
// annotate (tracing disabled, or no captured function), PutAnnotation does
// nothing.
func PutAnnotation[V AnnotationValue](ctx context.Context, key string, value V) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		debug.Logger().DebugContext(ctx, "no span to annotate", "key", key)
		return
	}
	kv, ok := annotation(SanitizeAnnotationKey(key), value)
	if !ok {
		debug.Logger().DebugContext(ctx, "annotation dropped: X-Ray cannot index its key or value", "key", key)
		return
	}
	annotate(span, kv)
}

// SanitizeAnnotationKey returns key as X-Ray stores it: letters and digits
// are kept, every other character becomes an underscore.
func SanitizeAnnotationKey(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case '0' <= r && r <= '9', 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z':
			return r
		default:
			return '_'
		}
	}, key)
}

// annotation converts value into the attribute key holds. X-Ray stores
// numbers as doubles, so an unsigned value too large for int64 is kept as a
// float64; NaN and infinities have no JSON form and are refused.
func annotation[V AnnotationValue](key string, value V) (attribute.KeyValue, bool) {
	if key == "" {
		return attribute.KeyValue{}, false
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.String:
		return attribute.String(key, v.String()), true
	case reflect.Bool:
		return attribute.Bool(key, v.Bool()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return attribute.Int64(key, v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if u := v.Uint(); u <= math.MaxInt64 {
			return attribute.Int64(key, int64(u)), true
		}
		return attribute.Float64(key, float64(v.Uint())), true
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return attribute.KeyValue{}, false
		}
		return attribute.Float64(key, f), true
	default:
		return attribute.KeyValue{}, false
	}
}

// spanLocks serialize the read-modify-write of the X-Ray attributes of one
// span, which concurrent goroutines may annotate at once. OpenTelemetry spans
// keep their own lock private, so the tracer stripes its own by span id.
var spanLocks [64]sync.Mutex

func lockSpan(span trace.Span) *sync.Mutex {
	id := span.SpanContext().SpanID()
	mu := &spanLocks[id[len(id)-1]%byte(len(spanLocks))]
	mu.Lock()
	return mu
}

// annotate sets kv on span and adds its key to the list X-Ray turns into
// annotations. The list is read back from the span, which the OpenTelemetry
// SDK allows; a span of another implementation cannot be read, and only its
// last annotation is listed.
func annotate(span trace.Span, kv attribute.KeyValue) {
	defer lockSpan(span).Unlock()

	var keys []string
	if ro, ok := span.(sdktrace.ReadOnlySpan); ok {
		for _, existing := range ro.Attributes() {
			if existing.Key == AnnotationsAttribute {
				keys = existing.Value.AsStringSlice()
				break
			}
		}
	}
	if !slices.Contains(keys, string(kv.Key)) {
		keys = append(keys, string(kv.Key))
	}
	span.SetAttributes(kv, attribute.StringSlice(AnnotationsAttribute, keys))
}

// PutMetadata adds key and value to the metadata of the span in ctx, under
// namespace. Metadata is not indexed: it carries detail, such as a payload,
// that traces are not searched by. value is stored as JSON, encoded with
// encoding/json/v2; a jsontext.Value or a []byte that holds a JSON document is
// stored as that document, an error as its message. An io.Reader is skipped,
// since encoding it would consume it. Metadata that would take a span past
// 32 KiB of JSON is dropped: a span must fit the X-Ray endpoint's datagram.
//
// An empty namespace is the service of the tracer that captured the current
// function, else POWERTOOLS_SERVICE_NAME, else X-Ray's "default" namespace.
// Namespaces starting with "AWS." are reserved by X-Ray and refused.
//
// The namespace is kept the way the X-Ray exporter of OpenTelemetry reads
// it: an attribute named "aws.xray.metadata.<namespace>" whose value is the
// JSON object of the namespace's entries. Other OpenTelemetry backends show
// that attribute as a JSON string.
func PutMetadata(ctx context.Context, key string, value any, namespace string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		debug.Logger().DebugContext(ctx, "no span to add metadata to", "key", key)
		return
	}
	if namespace == "" {
		namespace = serviceOf(ctx)
	}
	if strings.HasPrefix(namespace, "AWS.") {
		debug.Logger().DebugContext(ctx, "metadata dropped: X-Ray reserves the AWS. namespaces", "namespace", namespace)
		return
	}
	encoded, ok := encodeMetadata(value)
	if !ok {
		debug.Logger().DebugContext(ctx, "metadata dropped: the value has no JSON form", "key", key)
		return
	}
	if !putMetadata(span, namespace, key, encoded) {
		debug.Logger().DebugContext(ctx, "metadata dropped: the span would exceed its size budget",
			"key", key, "bytes", len(encoded))
	}
}

// encodeMetadata returns the JSON form of value.
func encodeMetadata(value any) (jsontext.Value, bool) {
	switch v := value.(type) {
	case nil:
		return jsontext.Value("null"), true
	case io.Reader:
		return nil, false
	case jsontext.Value:
		if v.IsValid() {
			return v, true
		}
	case []byte:
		if jsontext.Value(v).IsValid() {
			return jsontext.Value(v), true
		}
	case error:
		value = v.Error()
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// putMetadata merges key and value into the namespace's attribute of span. It
// reports false when the metadata of the span would cross metadataBudget.
func putMetadata(span trace.Span, namespace, key string, value jsontext.Value) bool {
	defer lockSpan(span).Unlock()

	name := MetadataAttributePrefix + namespace
	entries := map[string]jsontext.Value{}
	others := 0
	if ro, ok := span.(sdktrace.ReadOnlySpan); ok {
		for _, kv := range ro.Attributes() {
			switch k := string(kv.Key); {
			case k == name:
				if err := json.Unmarshal([]byte(kv.Value.AsString()), &entries); err != nil {
					entries = map[string]jsontext.Value{}
				}
			case strings.HasPrefix(k, MetadataAttributePrefix):
				others += len(kv.Value.AsString())
			}
		}
	}
	entries[key] = value
	document, err := json.Marshal(entries, json.Deterministic(true))
	if err != nil || others+len(document) > metadataBudget {
		return false
	}
	span.SetAttributes(attribute.String(name, string(document)))
	return true
}
