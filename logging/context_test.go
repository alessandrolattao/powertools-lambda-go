package logging

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppendKeys(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	f.invoke(t.Context(), func(ctx context.Context) {
		AppendKeys(ctx, "order_id", "o-1", slog.Int("attempt", 1), slog.Group("", "inline", true))
		f.logger.InfoContext(ctx, "hello")
		AppendKeys(ctx, "order_id", "o-2", "hidden", "x")
		AppendKeys(ctx, "hidden", nil)
		f.logger.InfoContext(ctx, "hello")
		RemoveKeys(ctx, "attempt", "missing")
		f.logger.InfoContext(ctx, "hello")
	})
	assert.Equal(t, []string{
		head + `,"order_id":"o-1","attempt":1,"inline":true}`,
		head + `,"order_id":"o-2","attempt":1,"inline":true}`,
		head + `,"order_id":"o-2","inline":true}`,
	}, f.out.lines())
}

func TestKeysEndWithTheInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	f.invoke(t.Context(), func(ctx context.Context) {
		AppendKeys(ctx, "order_id", "o-1")
		SetCorrelationID(ctx, "c-1")
	})
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.InfoContext(ctx, "hello")
	})
	assert.Equal(t, []string{head + `}`}, f.out.lines())
}

func TestKeysOutsideAnInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	ctx := t.Context()
	AppendKeys(ctx, "k", 1)
	SetCorrelationID(ctx, "c-1")
	RemoveKeys(ctx, "k")
	ClearState(ctx)
	f.logger.InfoContext(ctx, "hello")
	assert.Equal(t, []string{head + `}`}, f.out.lines())
	assert.Empty(t, CorrelationID(ctx))
	assert.Empty(t, CurrentKeys(ctx))
}

func TestWithKeysRestoresWhatItShadowed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	f.invoke(t.Context(), func(ctx context.Context) {
		AppendKeys(ctx, "order_id", "o-1")
		scoped := WithKeys(ctx, "order_id", "o-child", "step", "charge")
		f.logger.InfoContext(WithKeys(scoped, "step", "refund"), "hello")
		f.logger.InfoContext(scoped, "hello")
		f.logger.InfoContext(ctx, "hello")
	})
	assert.Equal(t, []string{
		head + `,"order_id":"o-child","step":"refund"}`,
		head + `,"order_id":"o-child","step":"charge"}`,
		head + `,"order_id":"o-1"}`,
	}, f.out.lines())
}

func TestWithKeysWithoutAnInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	f.logger.InfoContext(WithKeys(t.Context(), "k", 1), "hello")
	assert.Equal(t, []string{head + `,"k":1}`}, f.out.lines())
}

func TestCurrentKeysAndClearState(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	f.invoke(t.Context(), func(ctx context.Context) {
		SetCorrelationID(ctx, "c-1")
		AppendKeys(ctx, "a", 1, "b", 2)
		ctx = WithKeys(ctx, "b", 3, "c", 4)
		assert.Equal(t, []slog.Attr{
			slog.String(KeyCorrelationID, "c-1"), slog.Int("a", 1), slog.Int("b", 3), slog.Int("c", 4),
		}, CurrentKeys(ctx))
		assert.Equal(t, "c-1", CorrelationID(ctx))

		ClearState(ctx)
		assert.Equal(t, []slog.Attr{slog.Int("b", 3), slog.Int("c", 4)}, CurrentKeys(ctx), "scoped keys stay with their context")
		assert.Empty(t, CorrelationID(ctx))
		f.logger.InfoContext(ctx, "hello")
	})
	assert.Equal(t, []string{head + `,"b":3,"c":4}`}, f.out.lines())
}

func TestCorrelationID(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	f.invoke(t.Context(), func(ctx context.Context) {
		SetCorrelationID(ctx, "c-1")
		f.logger.InfoContext(ctx, "hello", "k", 1)
		SetCorrelationID(ctx, "")
		f.logger.InfoContext(ctx, "hello")
	})
	assert.Equal(t, []string{
		head + `,"correlation_id":"c-1","k":1}`,
		head + `}`,
	}, f.out.lines())
}

func TestInvocationKeysAreSharedByLoggers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	other := newFixture(t, WithoutKeys(KeyLocation), WithService("other"))
	f.invoke(t.Context(), func(ctx context.Context) {
		AppendKeys(ctx, "order_id", "o-1")
		other.logger.InfoContext(ctx, "hello")
	})
	assert.Equal(t, []string{
		`{"level":"INFO","message":"hello","timestamp":"` + testTimestamp + `","service":"other","order_id":"o-1"}`,
	}, other.out.lines())
}
