package logging

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/lambdacontext"
)

// A record is built as an ordered set of top-level keys, the way Powertools
// Python builds a dict: each key appears once, a later value replaces an
// earlier one in its place, and keys whose value is nil are left out. The
// sources come in this order, each one able to override the ones before:
//
//  1. the keys of WithRecordOrder, level, location, message, timestamp;
//  2. service, sampling_rate, the Lambda context, correlation_id;
//  3. the keys of AppendKeys, then of WithKeys;
//  4. the logger's attributes (slog With) and the record's own;
//  5. exception, exception_name, stack_trace, xray_trace_id, unless set.
//
// level, location, message and timestamp are Powertools' own: an attribute
// with one of those names is dropped, as Python ignores it, so that Lambda
// always reads the record's real level and time.

// field is one top-level key of a record.
type field struct {
	key   string
	value slog.Value
	// unset marks a place WithRecordOrder keeps for a key not filled yet.
	unset bool
}

type record struct {
	fields    []field
	protected []string
}

func (r *record) find(key string) int {
	return slices.IndexFunc(r.fields, func(f field) bool { return f.key == key })
}

// has reports whether the record holds a value for key.
func (r *record) has(key string) bool {
	i := r.find(key)
	return i >= 0 && !r.fields[i].unset
}

// reserve keeps the next place for key.
func (r *record) reserve(key string) {
	if r.find(key) < 0 {
		r.fields = append(r.fields, field{key: key, unset: true})
	}
}

// fill puts a Powertools key in the place kept for slot; ReplaceAttr may
// have renamed it.
func (r *record) fill(slot string, a slog.Attr) {
	if isEmpty(a) {
		return
	}
	if i := r.find(slot); i >= 0 {
		r.fields[i] = field{key: a.Key, value: a.Value}
		return
	}
	r.put(a.Key, a.Value)
}

// put sets key, in its place when the record already has it.
func (r *record) put(key string, value slog.Value) {
	if i := r.find(key); i >= 0 {
		r.fields[i] = field{key: key, value: value}
		return
	}
	r.fields = append(r.fields, field{key: key, value: value})
}

// isEmpty reports an attribute slog's handlers ignore. slog.Value.Equal
// would panic on a value of an uncomparable type.
func isEmpty(a slog.Attr) bool {
	return a.Key == "" && isNil(a.Value)
}

// build assembles the record r stands for, in the invocation ctx belongs to.
func (h *Handler) build(ctx context.Context, r slog.Record, message any) *record {
	c := h.core
	rec := &record{protected: c.protected}
	for _, key := range c.order {
		rec.reserve(c.name(key))
	}
	for _, key := range c.protected {
		rec.reserve(key)
	}
	c.putStandard(rec, r, message)
	inv := invocationFrom(ctx)
	var (
		keys        []slog.Attr
		correlation slog.Value
		cold        bool
	)
	if inv != nil {
		keys, correlation, cold = inv.snapshot()
	}
	c.putLambda(ctx, rec, inv != nil, cold)
	if !isNil(correlation) {
		c.putKey(rec, KeyCorrelationID, correlation)
	}
	c.putAttrs(rec, keys)
	if s := scopeFrom(ctx); s != nil {
		c.putAttrs(rec, s.attrs)
	}
	c.putAttrs(rec, h.attrs(r))
	c.putException(rec)
	if !c.dropped[KeyXRayTraceID] && !rec.has(c.name(KeyXRayTraceID)) {
		if id := xrayTraceID(ctx); id != "" {
			c.putKey(rec, KeyXRayTraceID, slog.StringValue(id))
		}
	}
	return rec
}

func (c *core) putStandard(rec *record, r slog.Record, message any) {
	c.putKey(rec, KeyLevel, slog.AnyValue(r.Level))
	if r.PC != 0 && !c.dropped[KeyLocation] {
		if loc := location(r.PC); loc != "" {
			c.putKey(rec, KeyLocation, slog.StringValue(loc))
		}
	}
	if message != nil {
		c.putKey(rec, KeyMessage, slog.AnyValue(eventMessage{value: message}))
	} else {
		c.putKey(rec, KeyMessage, slog.StringValue(r.Message))
	}
	t := r.Time
	if c.clock != nil {
		t = c.clock()
	}
	if !t.IsZero() {
		c.putKey(rec, KeyTimestamp, slog.TimeValue(t))
	}
	c.putKey(rec, KeyService, slog.StringValue(c.service))
	if c.sampling {
		c.putKey(rec, KeySamplingRate, slog.Float64Value(c.samplingRate))
	}
}

// putLambda adds what the Lambda context of ctx tells: the cold start, the
// function, the request. They come from the context of the record, not from
// the logger, so that concurrent invocations each log their own. The cold
// start is the one InjectContext decided when it is known, and is decided
// now otherwise.
func (c *core) putLambda(ctx context.Context, rec *record, known, cold bool) {
	lc, ok := lambdacontext.FromContext(ctx)
	if !ok {
		return
	}
	if !known && !c.dropped[KeyColdStart] {
		cold = c.coldStart(ctx)
	}
	c.putKey(rec, KeyColdStart, slog.BoolValue(cold))
	if c.function.name != "" {
		c.putKey(rec, KeyFunctionName, slog.StringValue(c.function.name))
	}
	if c.function.memory > 0 {
		c.putKey(rec, KeyFunctionMemorySize, slog.IntValue(c.function.memory))
	}
	if lc.InvokedFunctionArn != "" {
		c.putKey(rec, KeyFunctionARN, slog.StringValue(lc.InvokedFunctionArn))
	}
	if lc.AwsRequestID != "" {
		c.putKey(rec, KeyFunctionRequestID, slog.StringValue(lc.AwsRequestID))
	}
}

// putKey adds one of the Powertools keys, under its configured name, through
// ReplaceAttr.
func (c *core) putKey(rec *record, key string, value slog.Value) {
	name := c.name(key)
	if c.dropped[key] || c.dropped[name] {
		return
	}
	a := slog.Attr{Key: name, Value: value}
	if c.replaceAttr != nil {
		a = c.replaceAttr(nil, a)
		a.Value = a.Value.Resolve()
	}
	a.Value = c.format(key, a.Value)
	rec.fill(name, a)
}

// format writes the level and the timestamp as configured, unless
// ReplaceAttr already turned them into something else.
func (c *core) format(key string, v slog.Value) slog.Value {
	switch {
	case key == KeyLevel && v.Kind() == slog.KindAny:
		if level, ok := v.Any().(slog.Level); ok {
			return slog.StringValue(c.levelName(level))
		}
	case key == KeyTimestamp && v.Kind() == slog.KindTime:
		t := v.Time()
		if c.utc {
			t = t.UTC()
		} else {
			t = t.Local()
		}
		return slog.StringValue(t.Format(c.timeLayout))
	}
	return v
}

// name is the output name of a Powertools key.
func (c *core) name(key string) string {
	if name, ok := c.names[key]; ok {
		return name
	}
	return key
}

// attrs returns the logger's attributes and the record's, nested in the
// groups WithGroup opened.
func (h *Handler) attrs(r slog.Record) []slog.Attr {
	nested := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		nested = append(nested, a)
		return true
	})
	for i := len(h.chain) - 1; i >= 1; i-- {
		members := slices.Concat(h.chain[i].attrs, nested)
		nested = []slog.Attr{{Key: h.chain[i].group, Value: slog.GroupValue(members...)}}
	}
	return slices.Concat(h.chain[0].attrs, nested)
}

// putAttrs adds attributes at the top level of the record.
func (c *core) putAttrs(rec *record, attrs []slog.Attr) {
	for _, attr := range attrs {
		a, ok := c.prepare(nil, attr)
		if !ok {
			continue
		}
		if a.Key == "" && a.Value.Kind() == slog.KindGroup {
			for _, member := range a.Value.Group() {
				c.putAttr(rec, member)
			}
			continue
		}
		c.putAttr(rec, a)
	}
}

func (c *core) putAttr(rec *record, a slog.Attr) {
	if slices.Contains(rec.protected, a.Key) || c.dropped[a.Key] {
		return
	}
	rec.put(a.Key, a.Value)
}

// prepare resolves an attribute and passes it through ReplaceAttr, as slog's
// own handlers do: groups are prepared member by member, and a group left
// without members disappears.
func (c *core) prepare(groups []string, a slog.Attr) (slog.Attr, bool) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() != slog.KindGroup && c.replaceAttr != nil {
		a = c.replaceAttr(groups, a)
		a.Value = a.Value.Resolve()
	}
	if a.Value.Kind() != slog.KindGroup {
		return a, !isEmpty(a)
	}
	inner := groups
	if a.Key != "" {
		inner = append(slices.Clip(groups), a.Key)
	}
	var members []slog.Attr
	for _, attr := range a.Value.Group() {
		member, ok := c.prepare(inner, attr)
		if !ok {
			continue
		}
		if member.Key == "" && member.Value.Kind() == slog.KindGroup {
			members = append(members, member.Value.Group()...)
			continue
		}
		members = append(members, member)
	}
	if len(members) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: a.Key, Value: slog.GroupValue(members...)}, true
}

// putException turns the error under ErrorKey into the exception keys,
// unless the record sets them itself.
func (c *core) putException(rec *record) {
	i := rec.find(ErrorKey)
	if i < 0 || rec.fields[i].unset || rec.fields[i].value.Kind() != slog.KindAny {
		return
	}
	err, ok := rec.fields[i].value.Any().(error)
	if !ok {
		return
	}
	rec.fields = slices.Delete(rec.fields, i, i+1)
	ex := describe(err)
	if !rec.has(c.name(KeyException)) {
		c.putKey(rec, KeyException, slog.StringValue(ex.message))
	}
	if !rec.has(c.name(KeyExceptionName)) {
		c.putKey(rec, KeyExceptionName, slog.StringValue(ex.name))
	}
	if c.stackTrace && !rec.has(c.name(KeyStackTrace)) {
		if st := ex.trace(); st != nil {
			c.putKey(rec, KeyStackTrace, slog.AnyValue(st))
		}
	}
}

// location writes where a record was logged as Powertools Python does,
// function:line, the function qualified by its package name.
func location(pc uintptr) string {
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	fn := frame.Function
	if fn == "" {
		return ""
	}
	if i := strings.LastIndexByte(fn, '/'); i >= 0 {
		fn = fn[i+1:]
	}
	return fn + ":" + strconv.Itoa(frame.Line)
}

// eventMessage is the message of the record InjectContext logs the event
// with: a JSON value, as Powertools Python logs the event dict.
type eventMessage struct {
	value any
}
