package datadog

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/metrics"
)

var frozen = time.Unix(1700000000, 500_000_000)

// listen opens a DogStatsD listener standing in for the extension.
func listen(t *testing.T) net.PacketConn {
	t.Helper()
	var lc net.ListenConfig
	conn, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // the test is over
	return conn
}

// receive reads n datagrams.
func receive(t *testing.T, conn net.PacketConn, n int) []string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	got := make([]string, 0, n)
	buf := make([]byte, 65536)
	for range n {
		size, _, err := conn.ReadFrom(buf)
		require.NoError(t, err)
		got = append(got, string(buf[:size]))
	}
	return got
}

// extensionProvider returns a provider that believes the extension listens
// on conn.
func extensionProvider(conn net.PacketConn, opts ...Option) (*Provider, *bytes.Buffer) {
	var out bytes.Buffer
	opts = append([]Option{
		WithFlushToLog(false),
		WithWriter(&out),
		WithClock(func() time.Time { return frozen }),
	}, opts...)
	p := NewProvider(opts...)
	p.statsd = conn.LocalAddr().String()
	p.disabled = false
	return p, &out
}

func flush(t *testing.T, p *Provider, add func(ctx context.Context) error) {
	t.Helper()
	m := metrics.New(metrics.WithProvider(p))
	ctx := m.NewContext(t.Context())
	require.NoError(t, add(ctx))
	require.NoError(t, metrics.Flush(ctx))
}

func TestExtension(t *testing.T) {
	t.Parallel()
	conn := listen(t)
	p, out := extensionProvider(conn, WithNamespace("ServerlessAirline"), WithDefaultTag("env", "prod"))
	flush(t, p, func(ctx context.Context) error {
		if err := AddMetric(ctx, "item_sold", 12.45, WithTag("product", "latte"), WithTag("order", "online")); err != nil {
			return err
		}
		if err := AddMetric(ctx, "late", 1, WithTimestamp(frozen.Add(-time.Hour))); err != nil {
			return err
		}
		return AddMetric(ctx, "odd_tags", 2, WithTag("city", "Zürich|x,y #z"))
	})
	assert.Equal(t, []string{
		"ServerlessAirline.item_sold:12.45|d|#env:prod,product:latte,order:online",
		"ServerlessAirline.late:1|d|#env:prod|T1699996400",
		"ServerlessAirline.odd_tags:2|d|#env:prod,city:Zürich_x_y__z",
	}, receive(t, conn, 3))
	assert.Empty(t, out.String(), "nothing goes to standard output with the extension")
}

func TestExtensionDropsOldMetrics(t *testing.T) {
	t.Parallel()
	conn := listen(t)
	p, _ := extensionProvider(conn)
	flush(t, p, func(ctx context.Context) error {
		if err := AddMetric(ctx, "too_old", 1, WithTimestamp(frozen.Add(-5*time.Hour))); err != nil {
			return err
		}
		return AddMetric(ctx, "fresh", 1)
	})
	assert.Equal(t, []string{"fresh:1|d"}, receive(t, conn, 1))
}

func TestExtensionFlushToLog(t *testing.T) {
	t.Parallel()
	conn := listen(t)
	p, out := extensionProvider(conn, WithFlushToLog(true))
	flush(t, p, func(ctx context.Context) error { return AddMetric(ctx, "m", 1) })
	assert.JSONEq(t, `{"m":"m","v":1,"e":1700000000,"t":[]}`, out.String())
}

func TestExtensionDisabled(t *testing.T) {
	t.Parallel()
	conn := listen(t)
	p, _ := extensionProvider(conn)
	p.disabled = true
	flush(t, p, func(ctx context.Context) error { return AddMetric(ctx, "m", 1) })
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, _, err := conn.ReadFrom(make([]byte, 1024))
	require.Error(t, err, "a disabled provider sends nothing")
}

func TestExtensionUnreachable(t *testing.T) {
	t.Parallel()
	conn := listen(t)
	p, _ := extensionProvider(conn)
	p.statsd = "invalid address"
	// Telemetry that cannot be delivered does not fail the invocation.
	flush(t, p, func(ctx context.Context) error { return AddMetric(ctx, "m", 1) })
}
