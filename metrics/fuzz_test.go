package metrics_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/metrics"
)

// FuzzAddMetric feeds arbitrary names, units, values, dimensions and
// metadata keys to a metric set: each call is accepted or rejected with one
// of the package's errors, never panics, and what is flushed is a JSON
// document holding the metric under its trimmed name.
func FuzzAddMetric(f *testing.F) {
	f.Add("SuccessfulBooking", "Count", 1.0, "service", "booking", "booking_id")
	f.Add("  spaced  ", "BytesPerSecond", 0.5, "", " ", "")
	f.Add("Ünïcode \u2028", "None", -1e300, "_aws", "x", "_aws")
	f.Add("bad\xffutf8", "Count/Second", 1e-7, "d\xff", "v\xfe", "k\xff")
	f.Add("", "Furlongs", 0.0, "a", "b", "c")
	f.Fuzz(func(t *testing.T, name, unit string, value float64, dimName, dimValue, metaKey string) {
		m, out := newGolden(metrics.WithNamespace("ns"), metrics.WithService("svc"))
		ctx := m.NewContext(t.Context())
		err := metrics.AddMetric(ctx, name, metrics.Unit(unit), value)
		if err != nil {
			require.True(t, errors.Is(err, metrics.ErrMetricName) || errors.Is(err, metrics.ErrMetricUnit) ||
				errors.Is(err, metrics.ErrMetricValue), err)
			return
		}
		require.NoError(t, metrics.AddDimension(ctx, dimName, dimValue))
		require.NoError(t, metrics.AddMetadata(ctx, metaKey, dimValue))
		if flushErr := metrics.Flush(ctx); flushErr != nil {
			// A key may collide with the _aws member.
			require.ErrorContains(t, flushErr, "duplicate")
			return
		}
		var doc map[string]any
		require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
		// Invalid UTF-8 reads back as replacement characters, as written.
		quoted, err := json.Marshal(strings.TrimSpace(name), jsontext.AllowInvalidUTF8(true))
		require.NoError(t, err)
		var key string
		require.NoError(t, json.Unmarshal(quoted, &key))
		require.Contains(t, doc, key)
		for _, c := range out.Bytes()[:out.Len()-1] {
			require.True(t, c >= 0x20 && c < 0x7f, "the document is printable ASCII")
		}
	})
}

// FuzzParseUnit checks that a unit parses only into one of the units
// CloudWatch knows.
func FuzzParseUnit(f *testing.F) {
	for _, seed := range []string{"Count", "Count/Second", "CountPerSecond", "None", "NoUnit", "count", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := metrics.ParseUnit(s)
		if err != nil {
			require.ErrorIs(t, err, metrics.ErrMetricUnit)
			return
		}
		again, err := metrics.ParseUnit(string(u))
		require.NoError(t, err)
		require.Equal(t, u, again)
	})
}
