package metrics

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alessandrolattao/powertools-lambda-go/metrics/internal/pyjson"
	"github.com/alessandrolattao/powertools-lambda-go/metrics/internal/warn"
)

// The window CloudWatch accepts metric timestamps in.
const (
	maxTimestampPast   = 14 * 24 * time.Hour
	maxTimestampFuture = 2 * time.Hour
)

// emfProvider writes CloudWatch Embedded Metric Format documents.
type emfProvider struct {
	namespace string
	service   string
	out       io.Writer
	now       func() time.Time
	disabled  bool
}

func (p *emfProvider) NewSet(ctx context.Context, defaults []Dimension) Set {
	return p.newSet(ctx, defaults)
}

// newSet returns an empty set holding the dimensions of defaults that are
// valid; the others are skipped with a warning, or reported at flush when
// there are too many.
func (p *emfProvider) newSet(ctx context.Context, defaults []Dimension) *emfSet {
	s := &emfSet{p: p, byName: map[string]*emfMetric{}}
	for _, d := range defaults {
		if err := s.AddDimension(ctx, d.Name, d.Value); err != nil {
			s.defaultsErr = err
		}
	}
	s.prefix = slices.Clone(s.dims)
	return s
}

// AddColdStartMetric writes the ColdStart metric in a document of its own,
// with the function_name and service dimensions only, as Powertools Python
// does: the application's dimensions would otherwise multiply it.
func (p *emfProvider) AddColdStartMetric(ctx context.Context, _ Set, functionName string) error {
	s := p.newSet(ctx, nil)
	if err := s.AddMetric(ctx, Metric{Name: coldStartMetric, Unit: Count, Value: 1, Resolution: Standard}); err != nil {
		return err
	}
	if err := s.AddDimension(ctx, "function_name", functionName); err != nil {
		return err
	}
	if p.service != "" {
		if err := s.AddDimension(ctx, "service", p.service); err != nil {
			return err
		}
	}
	return s.Flush(ctx, true)
}

type emfMetric struct {
	name       string
	unit       Unit
	resolution Resolution
	values     []float64
}

type metadataEntry struct {
	key   string
	value jsontext.Value
}

// emfSet is the metric set of one invocation, the content of one EMF
// document.
type emfSet struct {
	p *emfProvider
	// prefix is the default dimensions: the set starts with them, every
	// dimension set too, and Clear goes back to them.
	prefix []Dimension
	// defaultsErr is the failure to apply the defaults, reported at flush.
	defaultsErr error

	metrics   []*emfMetric
	byName    map[string]*emfMetric
	dims      []Dimension
	dimSets   [][]Dimension
	metadata  []metadataEntry
	timestamp time.Time
}

func (s *emfSet) hasDimension(name string) bool {
	return slices.ContainsFunc(s.dims, func(d Dimension) bool { return d.Name == name })
}

func (s *emfSet) AddMetric(ctx context.Context, m Metric) error {
	name := strings.TrimSpace(m.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxMetricNameLength {
		return fmt.Errorf("%w: the metric name should be between 1 and %d characters", ErrMetricName, MaxMetricNameLength)
	}
	if err := checkValue(m.Value); err != nil {
		return err
	}
	unit, unitErr := ParseUnit(string(m.Unit))
	if unitErr != nil {
		return unitErr
	}
	if err := m.Resolution.check(); err != nil {
		return err
	}
	metric, ok := s.byName[name]
	if !ok {
		metric = &emfMetric{name: name}
		s.byName[name] = metric
		s.metrics = append(s.metrics, metric)
	}
	metric.unit, metric.resolution = unit, m.Resolution
	metric.values = append(metric.values, m.Value)
	if len(s.metrics) < MaxMetrics && len(metric.values) < MaxMetrics {
		return nil
	}
	// A full document goes out now; the next metrics start another one
	// with the same dimensions and metadata.
	err := s.write(ctx)
	s.metrics = nil
	clear(s.byName)
	return err
}

func (s *emfSet) AddDimension(ctx context.Context, name, value string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
		warn.Once(ctx, warn.PowertoolsUserWarning, fmt.Sprintf("The dimension %s doesn't meet the requirements and won't be added. "+
			"Ensure the dimension name and value are non-empty strings", name))
		return nil
	}
	if i := slices.IndexFunc(s.dims, func(d Dimension) bool { return d.Name == name }); i >= 0 {
		if s.dims[i].Value != value {
			warn.Once(ctx, warn.PowertoolsUserWarning,
				fmt.Sprintf("Dimension '%s' has already been added. The previous value will be overwritten.", name))
			s.dims[i].Value = value
		}
		return nil
	}
	if len(s.dims) >= MaxDimensions {
		return fmt.Errorf("%w: unable to add dimension %s", ErrTooManyDimensions, name)
	}
	s.dims = append(s.dims, Dimension{Name: name, Value: value})
	return nil
}

func (s *emfSet) AddDimensions(ctx context.Context, dims []Dimension) error {
	if len(dims) == 0 {
		warn.Once(ctx, warn.PowertoolsUserWarning, "Empty dimensions dictionary provided")
		return nil
	}
	var sanitized []Dimension
	for _, d := range dims {
		if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Value) == "" {
			warn.Once(ctx, warn.PowertoolsUserWarning, fmt.Sprintf("Dimension %s has empty name or value", d.Name))
			continue
		}
		sanitized = mergeDimensions(sanitized, []Dimension{d})
	}
	if len(sanitized) == 0 {
		return nil
	}
	keys := map[string]struct{}{}
	for _, set := range append(slices.Clone(s.dimSets), s.dims, sanitized) {
		for _, d := range set {
			keys[d.Name] = struct{}{}
		}
	}
	if len(keys) > MaxDimensions {
		return ErrTooManyDimensions
	}
	s.dimSets = append(s.dimSets, mergeDimensions(s.prefix, sanitized))
	return nil
}

func (s *emfSet) AddMetadata(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value, pyjson.Options())
	if err != nil {
		return fmt.Errorf("%w %q: %w", ErrMetadata, key, err)
	}
	if i := slices.IndexFunc(s.metadata, func(e metadataEntry) bool { return e.key == key }); i >= 0 {
		s.metadata[i].value = raw
		return nil
	}
	s.metadata = append(s.metadata, metadataEntry{key: key, value: raw})
	return nil
}

func (s *emfSet) SetTimestamp(ctx context.Context, t time.Time) error {
	ms := millis(t)
	now := s.p.now()
	if ms < now.Add(-maxTimestampPast).UnixMilli() || ms > now.Add(maxTimestampFuture).UnixMilli() {
		warn.Once(ctx, warn.UserWarning, "This metric doesn't meet the requirements and will be skipped by Amazon CloudWatch. "+
			"Ensure the timestamp is within 14 days past or 2 hours future.")
	}
	s.timestamp = t
	return nil
}

// millis converts t to epoch milliseconds, rounding as Python's
// convert_timestamp_to_emf_format does.
func millis(t time.Time) int64 {
	return t.Round(time.Millisecond).UnixMilli()
}

func (s *emfSet) Flush(ctx context.Context, raiseOnEmpty bool) error {
	defer s.Clear()
	if len(s.metrics) == 0 && !raiseOnEmpty {
		warn.Once(ctx, warn.UserWarning, warn.NoMetrics)
		return nil
	}
	if s.p.disabled {
		return nil
	}
	return errors.Join(s.defaultsErr, s.write(ctx))
}

func (s *emfSet) Clear() {
	s.metrics = nil
	clear(s.byName)
	s.dims, s.dimSets, s.metadata = slices.Clone(s.prefix), nil, nil
	s.timestamp = time.Time{}
}

func (s *emfSet) Len() int {
	return len(s.metrics)
}

// write writes the document of the set, unless metrics are off.
func (s *emfSet) write(ctx context.Context) error {
	if s.p.disabled {
		return nil
	}
	doc, err := s.document(ctx)
	if err != nil {
		return err
	}
	if _, err := s.p.out.Write(doc); err != nil {
		return fmt.Errorf("metrics: write the EMF document: %w", err)
	}
	return nil
}

// rootEntry is a member of the document's root object after _aws: a
// dimension value, a metadata value or the values of a metric.
type rootEntry struct {
	key    string
	str    string
	raw    jsontext.Value
	values []float64
}

// document serializes the set the way Powertools Python does, key order
// included: _aws, then the dimension values, the metadata and the metric
// values. Python builds the root as a dictionary, so a key given twice keeps
// its first place and its last value; so does this.
func (s *emfSet) document(ctx context.Context) ([]byte, error) {
	if s.p.service != "" && !s.hasDimension("service") {
		if err := s.AddDimension(ctx, "service", s.p.service); err != nil {
			return nil, err
		}
	}
	if len(s.metrics) == 0 {
		return nil, ErrNoMetrics
	}
	if s.p.namespace == "" {
		return nil, ErrNoNamespace
	}
	timestamp := s.p.now().UnixMilli()
	if !s.timestamp.IsZero() {
		timestamp = millis(s.timestamp)
	}

	var buf bytes.Buffer
	w := docWriter{enc: jsontext.NewEncoder(&buf, jsontext.AllowInvalidUTF8(true))}
	w.token(jsontext.BeginObject)
	s.writeAWS(&w, timestamp)
	s.writeRoot(&w)
	w.token(jsontext.EndObject)
	if w.err != nil {
		return nil, fmt.Errorf("metrics: serialize the EMF document: %w", w.err)
	}
	return pyjson.EscapeNonASCII(buf.Bytes()), nil
}

// writeAWS writes the _aws member: the metric directive.
func (s *emfSet) writeAWS(w *docWriter, timestamp int64) {
	w.str("_aws")
	w.token(jsontext.BeginObject)
	w.str("Timestamp")
	w.token(jsontext.Int(timestamp))
	w.str("CloudWatchMetrics")
	w.token(jsontext.BeginArray)
	w.token(jsontext.BeginObject)
	w.str("Namespace")
	w.str(s.p.namespace)
	w.str("Dimensions")
	w.token(jsontext.BeginArray)
	for _, set := range append([][]Dimension{s.dims}, s.dimSets...) {
		w.token(jsontext.BeginArray)
		for _, d := range set {
			w.str(d.Name)
		}
		w.token(jsontext.EndArray)
	}
	w.token(jsontext.EndArray)
	w.str("Metrics")
	w.token(jsontext.BeginArray)
	for _, m := range s.metrics {
		w.token(jsontext.BeginObject)
		w.str("Name")
		w.str(m.name)
		w.str("Unit")
		w.str(string(m.unit))
		// Only high resolution metrics carry StorageResolution: standard
		// ones cost less to ingest without it.
		if m.resolution == High {
			w.str("StorageResolution")
			w.token(jsontext.Int(int64(High)))
		}
		w.token(jsontext.EndObject)
	}
	w.token(jsontext.EndArray)
	w.token(jsontext.EndObject)
	w.token(jsontext.EndArray)
	w.token(jsontext.EndObject)
}

// writeRoot writes the members of the root object after _aws.
func (s *emfSet) writeRoot(w *docWriter) {
	for _, e := range s.root() {
		w.str(e.key)
		switch {
		case e.raw != nil:
			w.value(e.raw)
		case e.values != nil:
			w.floats(e.values)
		default:
			w.str(e.str)
		}
	}
}

// root lists the members of the root object after _aws.
func (s *emfSet) root() []rootEntry {
	var entries []rootEntry
	index := map[string]int{}
	put := func(e rootEntry) {
		if i, ok := index[e.key]; ok {
			entries[i] = e
			return
		}
		index[e.key] = len(entries)
		entries = append(entries, e)
	}
	for _, set := range append([][]Dimension{s.dims}, s.dimSets...) {
		for _, d := range set {
			put(rootEntry{key: d.Name, str: d.Value})
		}
	}
	for _, md := range s.metadata {
		put(rootEntry{key: md.key, raw: md.value})
	}
	for _, m := range s.metrics {
		put(rootEntry{key: m.name, values: m.values})
	}
	return entries
}

// docWriter writes tokens until the first error, which it keeps.
type docWriter struct {
	enc     *jsontext.Encoder
	err     error
	scratch []byte
}

func (w *docWriter) token(t jsontext.Token) {
	if w.err == nil {
		w.err = w.enc.WriteToken(t)
	}
}

func (w *docWriter) str(s string) {
	w.token(jsontext.String(s))
}

func (w *docWriter) value(v jsontext.Value) {
	if w.err == nil {
		w.err = w.enc.WriteValue(v)
	}
}

// floats writes values as an array of numbers in Python's repr: 1.0, not 1.
func (w *docWriter) floats(values []float64) {
	w.token(jsontext.BeginArray)
	for _, v := range values {
		w.scratch = pyjson.AppendFloat(w.scratch[:0], v, 64)
		w.value(w.scratch)
	}
	w.token(jsontext.EndArray)
}
