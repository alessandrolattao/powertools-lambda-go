package idempotency

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// maxRetries is how many times an invocation starts over when the store
// changes under it, as in Powertools Python: three attempts in all.
const maxRetries = 2

// defaultFunctionName stands for the Lambda function's name outside Lambda,
// as in Powertools Python.
const defaultFunctionName = "test-func"

// Idempotent makes fn idempotent: called again with a payload that has the
// same idempotency key while the record of the first call lasts, it returns
// the first call's result instead of running fn again.
//
// fn is a Lambda handler or any other function; event is the payload the
// idempotency key comes from. The result is stored as JSON and decoded back
// into an R on replays, so R must survive a round trip through
// encoding/json/v2 (exported fields, no channels or functions); no output
// serializer is needed.
//
// While fn runs, a concurrent call with the same key fails with an
// AlreadyInProgressError. When fn returns an error, the context's
// cancellation included, or panics, the in-progress record is deleted, so
// that a retry runs fn again; the error is returned and the panic goes on.
// Store failures are PersistenceErrors.
//
// Idempotent checks its options when it is called, before any invocation,
// and panics with an error matching ErrInvalidConfig when they cannot work:
// call it in main or at package level. The returned function is safe for
// concurrent use.
func Idempotent[E, R any](fn func(context.Context, E) (R, error), store Store, opts ...Option) func(context.Context, E) (R, error) {
	h, err := newHandler(fn, store, opts)
	if err != nil {
		panic(err)
	}
	return h.handle
}

// handler is an idempotent function: its configuration, compiled once, and
// the state its invocations share.
type handler[E, R any] struct {
	fn           func(context.Context, E) (R, error)
	store        Store
	prefix       string
	eventKey     *jmespath.Expression
	validation   *jmespath.Expression
	keyRequired  bool
	expiresAfter time.Duration
	newHash      func() hash.Hash
	hook         ResponseHook[R]
	cache        *localCache
	now          func() time.Time
	warnings     *slog.Logger

	warnedDisabled, warnedMissingKey, warnedNoDeadline sync.Once
}

func newHandler[E, R any](fn func(context.Context, E) (R, error), store Store, opts []Option) (*handler[E, R], error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if fn == nil {
		return nil, fmt.Errorf("%w: nil function", ErrInvalidConfig)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalidConfig)
	}
	h := &handler[E, R]{
		fn:           fn,
		store:        store,
		prefix:       cfg.keyPrefix,
		keyRequired:  cfg.keyRequired,
		expiresAfter: cfg.expiresAfter.Truncate(time.Second),
		newHash:      cfg.newHash,
		now:          cfg.now,
		warnings:     cfg.warnings,
	}
	if h.prefix == "" {
		h.prefix = defaultKeyPrefix(fn)
	}
	if h.expiresAfter < time.Second {
		return nil, fmt.Errorf("%w: records must last at least a second, not %v", ErrInvalidConfig, cfg.expiresAfter)
	}
	if h.newHash == nil {
		if h.newHash = hashFunctions[cfg.hashName]; h.newHash == nil {
			return nil, fmt.Errorf("%w: unknown hash function %q", ErrInvalidConfig, cfg.hashName)
		}
	}
	if cfg.localCache {
		if cfg.localCacheMaxItems < 1 {
			return nil, fmt.Errorf("%w: the local cache must hold at least one item, not %d", ErrInvalidConfig, cfg.localCacheMaxItems)
		}
		h.cache = newLocalCache(cfg.localCacheMaxItems)
	}
	if cfg.responseHook != nil {
		hook, ok := cfg.responseHook.(ResponseHook[R])
		if !ok {
			var zero R
			return nil, fmt.Errorf("%w: the response hook takes a %T, not the function's %T",
				ErrInvalidConfig, cfg.responseHook, zero)
		}
		h.hook = hook
	}
	if err := h.compile(&cfg); err != nil {
		return nil, err
	}
	return h, nil
}

// compile compiles the JMESPath expressions, once for every invocation.
func (h *handler[E, R]) compile(cfg *config) error {
	compile := func(expression string) (*jmespath.Expression, error) {
		compiled, err := jmespath.Compile(expression, jmespath.WithFunctions(cfg.functions...))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		return compiled, nil
	}
	var err error
	if cfg.eventKey != "" {
		if h.eventKey, err = compile(cfg.eventKey); err != nil {
			return err
		}
	}
	if cfg.payloadValidation != "" {
		if h.validation, err = compile(cfg.payloadValidation); err != nil {
			return err
		}
	}
	return nil
}

// defaultKeyPrefix names the function the way Powertools Python does, the
// Lambda function's name followed by the function's qualified name, so that
// two functions never share keys by accident:
// "my-function.github.com/me/app/orders.process".
func defaultKeyPrefix(fn any) string {
	lambdaName := env.Get(env.LambdaFunctionName)
	if lambdaName == "" {
		lambdaName = defaultFunctionName
	}
	name := "unknown"
	if f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()); f != nil {
		name = f.Name()
	}
	return lambdaName + "." + name
}

// invocation is what one call computes once from its payload.
type invocation struct {
	key         string
	payloadHash string
	// inProgressExpiry is the invocation's deadline, zero when unknown.
	inProgressExpiry time.Time
}

func (h *handler[E, R]) handle(ctx context.Context, event E) (R, error) {
	var zero R
	if env.Bool(env.IdempotencyDisabled, false) {
		h.warnedDisabled.Do(func() {
			h.warnings.Warn("powertools: idempotency is disabled; this is intended for development environments only and should not be used in production",
				"variable", env.IdempotencyDisabled)
		})
		return h.fn(ctx, event)
	}

	inv, err := h.prepare(event)
	if err != nil {
		return zero, err
	}
	if inv.key == "" {
		// No key: the call is not idempotent, and leaves no trace.
		return h.fn(ctx, event)
	}
	if deadline, ok := ctx.Deadline(); ok {
		inv.inProgressExpiry = time.UnixMilli(deadline.UnixMilli())
	} else {
		h.warnedNoDeadline.Do(func() {
			h.warnings.Warn("powertools: the context has no deadline; an in-progress idempotency record of an invocation that times out locks its key until the record expires",
				"key_prefix", h.prefix)
		})
	}

	for attempt := 0; ; attempt++ {
		result, retry, err := h.process(ctx, event, inv)
		if !retry || attempt == maxRetries {
			return result, err
		}
		debug.Logger().DebugContext(ctx, "idempotency record changed during the invocation, starting over",
			"idempotency_key", inv.key, "attempt", attempt+1, "err", err)
	}
}

// prepare computes the idempotency key and the payload validation hash. An
// empty key means the payload has none.
func (h *handler[E, R]) prepare(event E) (invocation, error) {
	raw, err := json.Marshal(event, json.Deterministic(true))
	if err != nil {
		return invocation{}, fmt.Errorf("marshal the idempotency payload: %w", err)
	}
	var data any
	if h.eventKey != nil {
		data, err = h.eventKey.Search(jsontext.Value(raw))
	} else {
		data, err = decodeTree(raw)
	}
	if err != nil {
		return invocation{}, fmt.Errorf("select the idempotency key: %w", err)
	}
	if isMissingKey(data) {
		if h.keyRequired {
			return invocation{}, ErrNoIdempotencyKey
		}
		h.warnedMissingKey.Do(func() {
			h.warnings.Warn("powertools: no idempotency key value found; skipping persistence layer and validation operations",
				"jmespath", h.expression())
		})
		return invocation{}, nil
	}
	keyHash, err := h.hash(data)
	if err != nil {
		return invocation{}, err
	}
	inv := invocation{key: h.prefix + "#" + keyHash}
	if h.validation != nil {
		selected, err := h.validation.Search(jsontext.Value(raw))
		if err != nil {
			return invocation{}, fmt.Errorf("select the payload to validate: %w", err)
		}
		if inv.payloadHash, err = h.hash(selected); err != nil {
			return invocation{}, err
		}
	}
	return inv, nil
}

func (h *handler[E, R]) hash(data any) (string, error) {
	canonical, err := canonicalJSON(data)
	if err != nil {
		return "", fmt.Errorf("hash the idempotency payload: %w", err)
	}
	return digest(h.newHash, canonical), nil
}

func (h *handler[E, R]) expression() string {
	if h.eventKey == nil {
		return ""
	}
	return h.eventKey.String()
}

// process runs one attempt: it takes the key with an in-progress record and
// runs fn, or answers from the record that holds the key. retry reports a
// record that changed under the attempt, which the next one sorts out.
func (h *handler[E, R]) process(ctx context.Context, event E, inv invocation) (result R, retry bool, err error) {
	existing, found, err := h.saveInProgress(ctx, inv)
	if err != nil {
		return result, errors.Is(err, ErrInconsistentState), err
	}
	if !found {
		result, err = h.call(ctx, event, inv)
		return result, false, err
	}
	return h.answer(ctx, existing)
}

// saveInProgress writes the in-progress record. When a record already holds
// the key, it returns that record, validated against the payload.
func (h *handler[E, R]) saveInProgress(ctx context.Context, inv invocation) (Record, bool, error) {
	now := h.now()
	if cached, ok := h.cache.get(inv.key, now); ok {
		debug.Logger().DebugContext(ctx, "idempotency record found in the local cache", "idempotency_key", inv.key)
		return cached, true, h.validate(cached, inv)
	}
	record := Record{
		Key:                 inv.key,
		Status:              StatusInProgress,
		ExpiresAt:           h.expiry(now),
		InProgressExpiresAt: inv.inProgressExpiry,
		PayloadHash:         inv.payloadHash,
	}
	debug.Logger().DebugContext(ctx, "saving in-progress idempotency record", "idempotency_key", inv.key)
	err := h.store.PutRecord(ctx, record, now)
	if err == nil {
		return Record{}, false, nil
	}
	if !errors.Is(err, ErrItemAlreadyExists) {
		return Record{}, false, &PersistenceError{Op: OpPut, Err: err}
	}
	if exists, ok := errors.AsType[*ItemAlreadyExistsError](err); ok && exists.Old != nil {
		// The store returned the record with the failed write (DynamoDB's
		// ReturnValuesOnConditionCheckFailure): no read needed.
		old := *exists.Old
		if invalid := h.validate(old, inv); invalid != nil {
			return Record{}, false, invalid
		}
		h.cache.put(old)
		return old, true, nil
	}
	old, err := h.store.GetRecord(ctx, inv.key)
	switch {
	case errors.Is(err, ErrItemNotFound):
		// Deleted between the write and the read.
		return Record{}, false, fmt.Errorf("%w: the record was deleted before it could be read", ErrInconsistentState)
	case err != nil:
		return Record{}, false, &PersistenceError{Op: OpGet, Err: err}
	}
	if err := h.validate(*old, inv); err != nil {
		return Record{}, false, err
	}
	h.cache.put(*old)
	return *old, true, nil
}

// validate checks that record was written for the same payload, when payload
// validation is on.
func (h *handler[E, R]) validate(record Record, inv invocation) error {
	if h.validation != nil && record.PayloadHash != inv.payloadHash {
		return ErrPayloadValidation
	}
	return nil
}

// answer turns the record that holds the key into the call's outcome.
func (h *handler[E, R]) answer(ctx context.Context, record Record) (result R, retry bool, err error) {
	now := h.now()
	if record.IsExpired(now) {
		return result, true, fmt.Errorf("%w: the record expired after it was read", ErrInconsistentState)
	}
	switch record.Status {
	case StatusInProgress:
		if !record.InProgressExpiresAt.IsZero() && record.InProgressExpiresAt.UnixMilli() < now.UnixMilli() {
			return result, true, fmt.Errorf("%w: the in-progress record timed out after it was read", ErrInconsistentState)
		}
		return result, false, &AlreadyInProgressError{Key: record.Key, KeyExpression: h.expression()}
	case StatusCompleted:
		result, err = h.replay(ctx, record)
		return result, false, err
	default:
		return result, false, &InvalidStatusError{Status: record.Status}
	}
}

// replay returns the stored response of a COMPLETED record.
func (h *handler[E, R]) replay(ctx context.Context, record Record) (R, error) {
	var result R
	if record.ResponseData != "" {
		if err := json.Unmarshal([]byte(record.ResponseData), &result); err != nil {
			return result, fmt.Errorf("decode the stored response into %T: %w", result, err)
		}
	}
	if h.hook != nil {
		debug.Logger().DebugContext(ctx, "response hook configured, invoking it", "idempotency_key", record.Key)
		return h.hook(ctx, result, record)
	}
	return result, nil
}

// call runs fn while the in-progress record holds the key, then completes or
// deletes the record.
func (h *handler[E, R]) call(ctx context.Context, event E, inv invocation) (R, error) {
	// The record must outlive a cancelled invocation, so that a cancellation
	// still deletes it and a late success still completes it.
	storeCtx := context.WithoutCancel(ctx)
	returned := false
	defer func() {
		if returned {
			return
		}
		// fn panicked (or ended its goroutine): delete the record and let the
		// panic go on, its stack intact.
		if err := h.delete(storeCtx, inv); err != nil {
			h.warnings.Warn("powertools: failed to delete the in-progress idempotency record after a panic",
				"idempotency_key", inv.key, "err", err)
		}
	}()
	result, err := h.fn(ctx, event)
	returned = true
	if err != nil {
		if deleteErr := h.delete(storeCtx, inv); deleteErr != nil {
			return result, errors.Join(err, &PersistenceError{Op: OpDelete, Err: deleteErr})
		}
		return result, err
	}
	// The function did run: its result goes back along with a failure to
	// record it.
	return result, h.saveSuccess(storeCtx, inv, result)
}

func (h *handler[E, R]) delete(ctx context.Context, inv invocation) error {
	debug.Logger().DebugContext(ctx, "function failed, deleting the in-progress idempotency record", "idempotency_key", inv.key)
	h.cache.remove(inv.key)
	return h.store.DeleteRecord(ctx, inv.key)
}

func (h *handler[E, R]) saveSuccess(ctx context.Context, inv invocation, result R) error {
	data, err := json.Marshal(result, json.Deterministic(true))
	if err != nil {
		return &PersistenceError{Op: OpUpdate, Err: fmt.Errorf("marshal the response: %w", err)}
	}
	record := Record{
		Key:          inv.key,
		Status:       StatusCompleted,
		ExpiresAt:    h.expiry(h.now()),
		ResponseData: string(data),
		PayloadHash:  inv.payloadHash,
	}
	debug.Logger().DebugContext(ctx, "function succeeded, completing the idempotency record", "idempotency_key", inv.key)
	if err := h.store.UpdateRecord(ctx, record); err != nil {
		return &PersistenceError{Op: OpUpdate, Err: err}
	}
	h.cache.put(record)
	return nil
}

// expiry is when a record written at now expires, in whole seconds.
func (h *handler[E, R]) expiry(now time.Time) time.Time {
	return time.Unix(now.Add(h.expiresAfter).Unix(), 0)
}
