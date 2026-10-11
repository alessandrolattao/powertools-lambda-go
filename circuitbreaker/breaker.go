package circuitbreaker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/internal/warn"
)

// The defaults of Powertools Python's CircuitBreakerConfig.
const (
	defaultFailureThreshold = 5
	defaultRecoveryTimeout  = 30 * time.Second
	defaultSuccessThreshold = 3
	defaultLocalCacheMaxAge = 5 * time.Second
)

// ttlBuffer is added to the recovery timeout in the expiration stamped on
// every write, so that a live circuit is never deleted mid-cycle while an
// abandoned one still goes away on its own.
const ttlBuffer = time.Hour

// Option configures a Breaker.
type Option func(*config)

type config struct {
	failureThreshold int
	recoveryTimeout  time.Duration
	successThreshold int
	localCacheMaxAge time.Duration
	handled          []error
	ignored          []error
	filter           func(error) bool
	filters          int
	onTransition     func(ctx context.Context, transition Transition)
}

// WithFailureThreshold sets how many consecutive failures in one execution
// environment trip the circuit from Closed to Open. The default is 5.
func WithFailureThreshold(n int) Option {
	return func(c *config) { c.failureThreshold = n }
}

// WithRecoveryTimeout sets how long the circuit stays Open before a probe
// may test the downstream, and how long a prober holds its lease. It must
// be a whole number of seconds, which is how the store keeps it. The
// default is 30 seconds.
func WithRecoveryTimeout(d time.Duration) Option {
	return func(c *config) { c.recoveryTimeout = d }
}

// WithSuccessThreshold sets how many consecutive successful probes close a
// HalfOpen circuit. The default is 3.
func WithSuccessThreshold(n int) Option {
	return func(c *config) { c.successThreshold = n }
}

// WithLocalCacheMaxAge sets how long the breaker trusts the state it last
// read or wrote before reading the store again. Zero reads it on every
// call. The default is 5 seconds.
func WithLocalCacheMaxAge(d time.Duration) Option {
	return func(c *config) { c.localCacheMaxAge = d }
}

// WithHandledErrors counts as failures only the errors that match one of
// targets (errors.Is); any other error goes back to the caller without
// touching the circuit. It is Powertools Python's handled_exceptions, and
// excludes WithIgnoredErrors and WithFailureFilter.
func WithHandledErrors(targets ...error) Option {
	return func(c *config) {
		c.handled = append([]error{}, targets...)
		c.filters++
	}
}

// WithIgnoredErrors counts as failures every error except those that match
// one of targets (errors.Is). It is Powertools Python's ignored_exceptions,
// and excludes WithHandledErrors and WithFailureFilter.
func WithIgnoredErrors(targets ...error) Option {
	return func(c *config) {
		c.ignored = append([]error{}, targets...)
		c.filters++
	}
}

// WithFailureFilter counts as a failure an error for which counts returns
// true, for the errors that a sentinel cannot match, such as an error type
// (errors.AsType) or a timeout (net.Error). It excludes WithHandledErrors
// and WithIgnoredErrors.
func WithFailureFilter(counts func(err error) bool) Option {
	return func(c *config) {
		c.filter = counts
		c.filters++
	}
}

// WithOnTransition calls fn every time the circuit changes state: Closed to
// Open, Open to HalfOpen (this breaker won the probe), HalfOpen to Closed
// and HalfOpen to Open. It never runs on the path of a healthy call, which
// makes it the place to emit a metric. A panic in fn is recovered and
// ignored, so that observability never breaks the protected call. fn may
// run concurrently with itself.
func WithOnTransition(fn func(ctx context.Context, transition Transition)) Option {
	return func(c *config) { c.onTransition = fn }
}

func (c *config) validate() error {
	var errs []error
	if c.filters > 1 {
		errs = append(errs, fmt.Errorf("%w: WithHandledErrors, WithIgnoredErrors and WithFailureFilter are mutually exclusive; pass only one",
			ErrInvalidConfig))
	}
	if c.failureThreshold <= 0 {
		errs = append(errs, fmt.Errorf("%w: failure threshold must be a positive integer, got %d", ErrInvalidConfig, c.failureThreshold))
	}
	if c.successThreshold <= 0 {
		errs = append(errs, fmt.Errorf("%w: success threshold must be a positive integer, got %d", ErrInvalidConfig, c.successThreshold))
	}
	if c.recoveryTimeout < time.Second || c.recoveryTimeout%time.Second != 0 {
		errs = append(errs, fmt.Errorf("%w: recovery timeout must be a positive whole number of seconds, got %s",
			ErrInvalidConfig, c.recoveryTimeout))
	}
	if c.localCacheMaxAge < 0 {
		errs = append(errs, fmt.Errorf("%w: local cache max age must not be negative, got %s", ErrInvalidConfig, c.localCacheMaxAge))
	}
	if c.handled != nil && !allSet(c.handled) {
		errs = append(errs, fmt.Errorf("%w: WithHandledErrors needs at least one error, and no nil", ErrInvalidConfig))
	}
	if c.ignored != nil && !allSet(c.ignored) {
		errs = append(errs, fmt.Errorf("%w: WithIgnoredErrors needs at least one error, and no nil", ErrInvalidConfig))
	}
	if c.filters == 1 && c.handled == nil && c.ignored == nil && c.filter == nil {
		errs = append(errs, fmt.Errorf("%w: WithFailureFilter needs a function", ErrInvalidConfig))
	}
	return errors.Join(errs...)
}

func allSet(targets []error) bool {
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if t == nil {
			return false
		}
	}
	return true
}

// Breaker is one circuit: a guard around one downstream dependency. Create
// it once, in main or at package level, and share it: it is safe for
// concurrent use, and it must outlive invocations, since it counts the
// consecutive failures of its execution environment in memory.
//
// Every Breaker is its own prober: of all the breakers guarding the same
// circuit name through the same store (one per execution environment,
// typically), one at a time wins the election to probe a recovering
// downstream, and it sends one call at a time.
type Breaker struct {
	name  string
	store Store
	cfg   config
	owner string
	now   func() time.Time

	mu          sync.Mutex
	failures    int
	successes   int
	lastState   State
	cached      Record
	cachedUntil time.Time
	hasCache    bool

	probing atomic.Bool
}

// New creates the breaker of the circuit name, whose state store keeps
// where every execution environment sees it. Breakers with the same name
// and store guard the same circuit. It fails with ErrInvalidConfig when an
// option is out of range.
func New(name string, store Store, opts ...Option) (*Breaker, error) {
	cfg := config{
		failureThreshold: defaultFailureThreshold,
		recoveryTimeout:  defaultRecoveryTimeout,
		successThreshold: defaultSuccessThreshold,
		localCacheMaxAge: defaultLocalCacheMaxAge,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("%w: the circuit needs a name", ErrInvalidConfig)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: the circuit needs a store", ErrInvalidConfig)
	}
	id := uuid.New()
	return &Breaker{
		name:  name,
		store: store,
		cfg:   cfg,
		owner: strconv.Itoa(os.Getpid()) + "#" + hex.EncodeToString(id[:]),
		now:   time.Now,
	}, nil
}

// Call runs fn through the circuit and returns what it returns. While the
// circuit is Open, or HalfOpen and another breaker or call is probing, fn
// does not run and Call returns an *OpenError (errors.Is(err,
// ErrCircuitOpen)). An error of fn that counts as a failure (see
// WithHandledErrors) is counted; any error of fn is returned as it is.
//
// Wrap the call to one downstream, not a whole handler: a bug in the
// handler would trip a circuit named after a healthy dependency.
func (b *Breaker) Call[T any](ctx context.Context, fn func(ctx context.Context) (T, error)) (T, error) {
	return call(ctx, b, fn, nil)
}

// WrapOption configures a function made with Wrap.
type WrapOption[In, Out any] func(*wrapConfig[In, Out])

type wrapConfig[In, Out any] struct {
	onOpen func(ctx context.Context, in In, circuit CircuitInfo) (Out, error)
}

// OnCircuitOpen hands the calls the circuit rejects to fn, with the
// argument the call was made with and a snapshot of the circuit: what fn
// returns is the call's result. Buffer the request, return a cached value,
// or answer that the service is degraded. Without it, a rejected call
// returns an *OpenError. fn may run concurrently with itself.
func OnCircuitOpen[In, Out any](fn func(ctx context.Context, in In, circuit CircuitInfo) (Out, error)) WrapOption[In, Out] {
	return func(c *wrapConfig[In, Out]) { c.onOpen = fn }
}

// Wrap returns fn guarded by the circuit, with the same signature, so that
// it replaces fn where it is called: Powertools Python's @circuit_breaker
// decorator. Every call goes through the circuit as with Call.
func (b *Breaker) Wrap[In, Out any](fn func(ctx context.Context, in In) (Out, error), opts ...WrapOption[In, Out]) func(ctx context.Context, in In) (Out, error) {
	var cfg wrapConfig[In, Out]
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return func(ctx context.Context, in In) (Out, error) {
		var onOpen func(context.Context, CircuitInfo) (Out, error)
		if cfg.onOpen != nil {
			onOpen = func(ctx context.Context, circuit CircuitInfo) (Out, error) {
				return cfg.onOpen(ctx, in, circuit)
			}
		}
		return call(ctx, b, func(ctx context.Context) (Out, error) { return fn(ctx, in) }, onOpen)
	}
}

// call routes one call through the state machine.
func call[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error),
	onOpen func(context.Context, CircuitInfo) (T, error),
) (T, error) {
	if env.Bool(env.CircuitBreakerDisabled, false) {
		warnDisabled()
		return fn(ctx)
	}
	record := b.read(ctx)
	switch record.State {
	case Open:
		b.observe(Open)
		return callOpen(ctx, b, &record, fn, onOpen)
	case HalfOpen:
		b.observe(HalfOpen)
		return callHalfOpen(ctx, b, &record, fn, onOpen)
	default:
		b.observe(Closed)
		return callClosed(ctx, b, fn)
	}
}

func callOpen[T any](ctx context.Context, b *Breaker, record *Record, fn func(context.Context) (T, error),
	onOpen func(context.Context, CircuitInfo) (T, error),
) (T, error) {
	now := b.clock()
	// An Open record without an opening time (written by someone else) is
	// treated as opened now: it waits for its time to live.
	openedAt := record.OpenedAt
	if openedAt.IsZero() {
		openedAt = now
	}
	if now.Before(openedAt.Add(b.cfg.recoveryTimeout)) {
		return reject(ctx, record, onOpen)
	}
	won, err := b.elect(ctx, openedAt, now)
	if err != nil {
		var zero T
		return zero, err
	}
	if !won {
		return reject(ctx, record, onOpen)
	}
	b.notify(ctx, Open, HalfOpen, openedAt)
	return probe(ctx, b, fn)
}

func callHalfOpen[T any](ctx context.Context, b *Breaker, record *Record, fn func(context.Context) (T, error),
	onOpen func(context.Context, CircuitInfo) (T, error),
) (T, error) {
	if record.HalfOpenOwner == b.owner {
		if !b.probing.CompareAndSwap(false, true) {
			// Another call of this breaker is probing: one at a time.
			return reject(ctx, record, onOpen)
		}
		return probe(ctx, b, fn)
	}
	now := b.clock()
	if record.ProbeLeaseExpiry.IsZero() || now.Before(record.ProbeLeaseExpiry) {
		return reject(ctx, record, onOpen)
	}
	// The prober's lease ended before it finished (its environment went
	// away): take the probe over, without a transition.
	debug.Logger().DebugContext(ctx, "circuit breaker probe lease expired; attempting takeover", "circuit", b.name)
	openedAt := record.OpenedAt
	if openedAt.IsZero() {
		openedAt = time.Unix(0, 0)
	}
	won, err := b.elect(ctx, openedAt, now)
	if err != nil {
		var zero T
		return zero, err
	}
	if !won {
		return reject(ctx, record, onOpen)
	}
	return probe(ctx, b, fn)
}

// elect tries to make this breaker the circuit's prober. When it wins, the
// probing flag stays set for the probe to clear.
func (b *Breaker) elect(ctx context.Context, openedAt, now time.Time) (bool, error) {
	if !b.probing.CompareAndSwap(false, true) {
		return false, nil
	}
	record := Record{
		Name:             b.name,
		State:            HalfOpen,
		OpenedAt:         openedAt,
		HalfOpenOwner:    b.owner,
		ProbeLeaseExpiry: now.Add(b.cfg.recoveryTimeout),
		Expiration:       b.expiration(now),
	}
	won, err := b.store.AcquireProbe(ctx, record, now)
	if err != nil {
		b.probing.Store(false)
		return false, fmt.Errorf("circuit %q: elect the half-open prober: %w", b.name, err)
	}
	if !won {
		b.probing.Store(false)
		return false, nil
	}
	b.cache(record)
	return true, nil
}

func callClosed[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error)) (T, error) {
	result, err := fn(ctx)
	if err == nil {
		b.mu.Lock()
		b.failures = 0
		b.mu.Unlock()
		return result, nil
	}
	if !b.counts(err) {
		return result, err
	}
	// Exactly one call sees the threshold crossed: the counter goes back to
	// zero under the same lock.
	b.mu.Lock()
	b.failures++
	failures := b.failures
	tripped := failures >= b.cfg.failureThreshold
	if tripped {
		b.failures = 0
	}
	b.mu.Unlock()
	if tripped {
		debug.Logger().DebugContext(ctx, "circuit breaker tripping from CLOSED to OPEN", "circuit", b.name, "failures", failures)
		openedAt := b.clock()
		b.persist(ctx, "save_open", b.store.Put, Record{
			Name:         b.name,
			State:        Open,
			FailureCount: failures,
			OpenedAt:     openedAt,
			Expiration:   b.expiration(openedAt),
		})
		b.notify(ctx, Closed, Open, openedAt)
	}
	return result, err
}

// probe runs fn as the circuit's probe, then clears the probing flag.
func probe[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error)) (T, error) {
	defer b.probing.Store(false)
	result, err := fn(ctx)
	if err != nil {
		if !b.counts(err) {
			return result, err
		}
		debug.Logger().DebugContext(ctx, "circuit breaker probe failed; reopening", "circuit", b.name)
		openedAt := b.clock()
		b.persist(ctx, "save_reopen", b.store.Update, Record{
			Name:       b.name,
			State:      Open,
			OpenedAt:   openedAt,
			Expiration: b.expiration(openedAt),
		})
		b.mu.Lock()
		b.successes = 0
		b.mu.Unlock()
		b.notify(ctx, HalfOpen, Open, openedAt)
		return result, err
	}
	b.mu.Lock()
	b.successes++
	successes := b.successes
	closed := successes >= b.cfg.successThreshold
	if closed {
		b.successes = 0
		b.failures = 0
	}
	b.mu.Unlock()
	if closed {
		debug.Logger().DebugContext(ctx, "circuit breaker closing", "circuit", b.name, "probes", successes)
		b.persist(ctx, "save_closed", b.store.Update, Record{
			Name:       b.name,
			State:      Closed,
			Expiration: b.expiration(b.clock()),
		})
		b.notify(ctx, HalfOpen, Closed, time.Time{})
	}
	return result, nil
}

// reject answers a call the circuit does not let through.
func reject[T any](ctx context.Context, record *Record, onOpen func(context.Context, CircuitInfo) (T, error)) (T, error) {
	if onOpen != nil {
		return onOpen(ctx, record.info())
	}
	var zero T
	return zero, &OpenError{Circuit: record.info()}
}

// clock returns the current time in whole seconds, the store's resolution.
func (b *Breaker) clock() time.Time {
	return time.Unix(b.now().Unix(), 0)
}

func (b *Breaker) expiration(now time.Time) time.Time {
	return now.Add(b.cfg.recoveryTimeout + ttlBuffer)
}

// counts tells whether err counts as a failure of the downstream.
func (b *Breaker) counts(err error) bool {
	switch {
	case b.cfg.filter != nil:
		return b.cfg.filter(err)
	case b.cfg.handled != nil:
		return matchesAny(err, b.cfg.handled)
	case b.cfg.ignored != nil:
		return !matchesAny(err, b.cfg.ignored)
	default:
		return true
	}
}

func matchesAny(err error, targets []error) bool {
	for _, target := range targets {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// read returns the circuit's state: from the local cache while it is
// fresh, from the store otherwise. It fails open: when the store cannot be
// read, the circuit is Closed, and that guess is not cached, so that the
// next call asks the store again.
func (b *Breaker) read(ctx context.Context) Record {
	b.mu.Lock()
	if b.hasCache && b.now().Before(b.cachedUntil) {
		record := b.cached
		b.mu.Unlock()
		return record
	}
	b.mu.Unlock()

	record, found, err := b.store.Get(ctx, b.name)
	if err == nil && found && !record.State.Valid() {
		err = fmt.Errorf("%w: unknown state %q", errInvalidRecord, record.State)
	}
	if err != nil {
		debug.Logger().WarnContext(ctx, "failed to read circuit state; failing open (treating as CLOSED)",
			"circuit", b.name, "err", err)
		return Record{Name: b.name, State: Closed}
	}
	if !found {
		record = Record{Name: b.name, State: Closed}
	}
	b.cache(record)
	return record
}

func (b *Breaker) cache(record Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cached = record
	b.cachedUntil = b.now().Add(b.cfg.localCacheMaxAge)
	b.hasCache = true
}

// observe remembers the state the breaker last saw. A circuit found Closed
// after this breaker saw it otherwise was recovered by another execution
// environment: the failures counted before it opened are stale.
func (b *Breaker) observe(state State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state == Closed && b.lastState != "" && b.lastState != Closed {
		b.failures = 0
	}
	b.lastState = state
}

// persist writes a transition. A failed write never hides the result of the
// call that caused it: it is logged, and the transition happens later. The
// write outlives the call's context, which may be what made the call fail.
func (b *Breaker) persist(ctx context.Context, operation string, write func(context.Context, Record) error, record Record) {
	if err := write(context.WithoutCancel(ctx), record); err != nil {
		debug.Logger().WarnContext(ctx,
			"circuit breaker persistence write failed; the transition may be delayed but the downstream result is preserved",
			"circuit", b.name, "operation", operation, "err", err)
		return
	}
	b.cache(record)
}

// notify calls the transition hook.
func (b *Breaker) notify(ctx context.Context, from, to State, openedAt time.Time) {
	if b.cfg.onTransition == nil {
		return
	}
	defer func() {
		if v := recover(); v != nil {
			debug.Logger().WarnContext(ctx, "on_transition hook panicked; ignoring", "circuit", b.name, "panic", v)
		}
	}()
	b.cfg.onTransition(ctx, Transition{CircuitName: b.name, From: from, To: to, OpenedAt: openedAt})
}

// warnDisabled warns, once per process, that the breaker is switched off.
func warnDisabled() {
	warn.Once(context.Background(), warn.UserWarning, "Disabling the circuit breaker is intended for development environments only "+
		"and should not be used in production.")
}
