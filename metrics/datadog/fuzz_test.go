package datadog

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/metrics"
)

// FuzzAddMetric feeds arbitrary names, tags and values to a Datadog set:
// a metric is accepted or rejected with the package's errors, never panics,
// its forwarder line is one JSON object on one line, and its DogStatsD
// datagram has exactly the separators of one metric.
func FuzzAddMetric(f *testing.F) {
	f.Add("item_sold", "product", "latte", 12.45)
	f.Add("1_item", "", "", 1.0)
	f.Add("a.b_c", "city", "Zurich|x,y #z\nnext", -3.0)
	f.Add("ok", "k\xff", "v\xfe", 1e300)
	f.Fuzz(func(t *testing.T, name, key, value string, v float64) {
		var out bytes.Buffer
		p := NewProvider(WithFlushToLog(true), WithWriter(&out), WithNamespace("ns"))
		s, ok := p.NewSet(t.Context(), nil).(*set)
		require.True(t, ok)
		err := s.AddMetric(t.Context(), metrics.Metric{Name: name, Value: v, Tags: []metrics.Dimension{{Name: key, Value: value}}})
		if err != nil {
			require.True(t, errors.Is(err, metrics.ErrMetricName) || errors.Is(err, metrics.ErrMetricValue), err)
			return
		}
		datagram := string(s.datagram(s.records[0]))
		require.NotContains(t, datagram, "\n")
		require.Equal(t, 1, strings.Count(datagram, "|#"))
		require.Equal(t, 1, strings.Count(datagram, "|d"))
		require.NoError(t, s.Flush(t.Context(), true))
		line := out.Bytes()
		require.Equal(t, 1, bytes.Count(line, []byte("\n")))
		var got struct {
			M string   `json:"m"`
			T []string `json:"t"`
		}
		require.NoError(t, json.Unmarshal(line, &got))
		require.Equal(t, "ns."+name, got.M)
		require.Len(t, got.T, 1)
	})
}
