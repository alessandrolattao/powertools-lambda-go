package logging

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// The per-invocation state of the logger lives in the invocation's context,
// never in the Logger: Lambda Managed Instances run several invocations at
// once in one process, and each one must see only its own keys, sampling
// decision and buffer.

// invocationKey and scopeKey key the context values of this package.
type (
	invocationKey struct{}
	scopeKey      struct{}
)

// invocation is the mutable state of one invocation, created by
// InjectContext. Goroutines started by the handler share it, hence the lock.
type invocation struct {
	mu          sync.Mutex
	keys        []slog.Attr
	correlation slog.Value
	coldStart   bool
	loggers     map[*core]*entry
}

// entry is the state one logger keeps for one invocation. The buffer
// pointer is set before the entry is published and never changes; sampled
// and closed change while other goroutines of the invocation log, so they
// are atomic.
type entry struct {
	sampled atomic.Bool
	buffer  *buffer
	closed  atomic.Bool
}

// scope is a set of keys bound to a context by WithKeys.
type scope struct {
	attrs []slog.Attr
}

func invocationFrom(ctx context.Context) *invocation {
	inv, _ := ctx.Value(invocationKey{}).(*invocation) //nolint:errcheck // absent means outside an invocation
	return inv
}

func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope) //nolint:errcheck // absent means no scoped keys
	return s
}

// AppendKeys adds keys to every record of the invocation ctx belongs to,
// from now until it ends, whoever logs them: Powertools Python's
// append_keys, scoped to the invocation. The arguments are key-value pairs
// or slog.Attr values, as slog.Logger.With takes them. A key that is already
// there takes the new value; a nil value hides the key.
//
// The keys live in the state InjectContext puts in the context; outside an
// invocation wrapped by InjectContext there is nowhere to keep them, and
// AppendKeys does nothing. Use slog's With for keys every record of a
// logger carries, and WithKeys for keys bound to a context.
func AppendKeys(ctx context.Context, args ...any) {
	inv := invocationFrom(ctx)
	if inv == nil {
		debug.Logger().DebugContext(ctx, "logging: AppendKeys outside an invocation wrapped by InjectContext")
		return
	}
	attrs := argsToAttrs(args)
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.keys = upsert(inv.keys, attrs)
}

// RemoveKeys removes keys added by AppendKeys from the invocation ctx
// belongs to. Keys bound by WithKeys stay with their context.
func RemoveKeys(ctx context.Context, keys ...string) {
	inv := invocationFrom(ctx)
	if inv == nil {
		return
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.keys = slices.DeleteFunc(inv.keys, func(a slog.Attr) bool { return slices.Contains(keys, a.Key) })
}

// ClearState removes every key added by AppendKeys and the correlation id
// from the invocation ctx belongs to: Powertools Python's clear_state.
func ClearState(ctx context.Context) {
	inv := invocationFrom(ctx)
	if inv == nil {
		return
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.keys = nil
	inv.correlation = slog.Value{}
}

// WithKeys returns a context whose records carry keys, on top of those of
// ctx: Powertools Python's append_context_keys. The keys end with the
// returned context, and the values they shadowed come back: unlike Python,
// leaving the scope never loses a key that existed before it.
func WithKeys(ctx context.Context, args ...any) context.Context {
	var attrs []slog.Attr
	if parent := scopeFrom(ctx); parent != nil {
		attrs = slices.Clone(parent.attrs)
	}
	return context.WithValue(ctx, scopeKey{}, &scope{attrs: upsert(attrs, argsToAttrs(args))})
}

// CurrentKeys returns the keys ctx adds to records: the correlation id, the
// keys of AppendKeys, then those of WithKeys. Powertools Python's
// get_current_keys; the slice is a copy.
func CurrentKeys(ctx context.Context) []slog.Attr {
	var keys []slog.Attr
	if inv := invocationFrom(ctx); inv != nil {
		inv.mu.Lock()
		if !isNil(inv.correlation) {
			keys = append(keys, slog.Attr{Key: KeyCorrelationID, Value: inv.correlation})
		}
		keys = append(keys, inv.keys...)
		inv.mu.Unlock()
	}
	if s := scopeFrom(ctx); s != nil {
		keys = upsert(keys, s.attrs)
	}
	return keys
}

// SetCorrelationID sets the correlation_id key of the invocation ctx belongs
// to; an empty id removes it. Like AppendKeys, it needs InjectContext.
func SetCorrelationID(ctx context.Context, id string) {
	if id == "" {
		setCorrelation(ctx, slog.Value{})
		return
	}
	setCorrelation(ctx, slog.StringValue(id))
}

func setCorrelation(ctx context.Context, value slog.Value) {
	inv := invocationFrom(ctx)
	if inv == nil {
		debug.Logger().DebugContext(ctx, "logging: SetCorrelationID outside an invocation wrapped by InjectContext")
		return
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.correlation = value
}

// CorrelationID returns the correlation id of the invocation ctx belongs to,
// or "" when it has none.
func CorrelationID(ctx context.Context) string {
	inv := invocationFrom(ctx)
	if inv == nil {
		return ""
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if isNil(inv.correlation) {
		return ""
	}
	return inv.correlation.String()
}

// snapshot copies what the invocation adds to a record, so that the record
// is built without holding the lock.
func (inv *invocation) snapshot() (keys []slog.Attr, correlation slog.Value, coldStart bool) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return slices.Clone(inv.keys), inv.correlation, inv.coldStart
}

// argsToAttrs reads key-value pairs and Attrs the way slog.Logger.With does,
// inline groups flattened.
func argsToAttrs(args []any) []slog.Attr {
	return flatten(slog.Group("", args...).Value.Group())
}

// flatten replaces each group with an empty key by its members.
func flatten(attrs []slog.Attr) []slog.Attr {
	if !slices.ContainsFunc(attrs, isInlineGroup) {
		return attrs
	}
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if isInlineGroup(a) {
			out = append(out, flatten(a.Value.Group())...)
			continue
		}
		out = append(out, a)
	}
	return out
}

func isInlineGroup(a slog.Attr) bool {
	return a.Key == "" && a.Value.Kind() == slog.KindGroup
}

// upsert adds attrs to keys: a key already present keeps its place and takes
// the new value, as a Python dict update does.
func upsert(keys, attrs []slog.Attr) []slog.Attr {
	for _, a := range attrs {
		if i := slices.IndexFunc(keys, func(k slog.Attr) bool { return k.Key == a.Key }); i >= 0 {
			keys[i] = a
			continue
		}
		keys = append(keys, a)
	}
	return keys
}
