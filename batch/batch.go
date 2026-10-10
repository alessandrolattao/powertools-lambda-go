package batch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	pdebug "github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// RecordHandler processes one record of a batch. It returns nil when the
// record is done and an error when the record failed and must be retried. A
// panic counts as a failure too.
//
// The context is the invocation's: lambdacontext.FromContext reads the
// Lambda context from it, ctx.Deadline tells how much time is left.
type RecordHandler[R any] = func(ctx context.Context, record R) error

// ProcessFunc is the shape of ProcessSQS and its siblings, which Handler
// turns into a Lambda handler.
type ProcessFunc[E, R, Resp any] = func(ctx context.Context, event E, handler RecordHandler[R], opts ...Option) (Resp, error)

// Errors a record fails with when its handler never ran.
var (
	// ErrFIFOShortCircuit fails every record of an SQS FIFO batch that
	// follows a failed one, so that the queue keeps its order: Powertools
	// Python's SQSFifoCircuitBreakerError.
	ErrFIFOShortCircuit = errors.New("a previous record failed processing")
	// ErrFIFOGroupShortCircuit fails every record of an SQS FIFO batch that
	// follows a failed one of the same message group, when
	// WithSkipGroupOnError is on: Powertools Python's
	// SQSFifoMessageGroupCircuitBreakerError.
	ErrFIFOGroupShortCircuit = errors.New("a previous record from this message group failed processing")
	// ErrNotProcessed fails the records left when the invocation's context
	// ends. The error wraps the context's cause too.
	ErrNotProcessed = errors.New("record not processed before the invocation ended")
)

// Errors that fail the whole batch.
var (
	// ErrUnexpectedBatchType reports an event without records, which is what
	// an event of another source decodes to: Powertools Python's
	// UnexpectedBatchTypeError.
	ErrUnexpectedBatchType = errors.New("unexpected batch event type: the event holds no records")
	// ErrHookRecordType reports a hook written for another record type than
	// the batch holds, such as a WithOnFailure taking SQS messages passed to
	// ProcessKinesis.
	ErrHookRecordType = errors.New("hook record type does not match the batch")
)

// ProcessingError reports a batch whose records all failed: returned from
// the Lambda handler, it makes Lambda retry the whole batch and shows in
// the function's error metrics. It is Powertools Python's
// BatchProcessingError.
type ProcessingError struct {
	// Errors holds one error per record, in the order of the batch.
	Errors []error
}

func (e *ProcessingError) Error() string {
	var b strings.Builder
	b.WriteString("All records failed processing. ")
	b.WriteString(strconv.Itoa(len(e.Errors)))
	b.WriteString(" individual errors logged separately below.")
	for _, err := range e.Errors {
		b.WriteString("\n")
		b.WriteString(err.Error())
	}
	return b.String()
}

// Unwrap returns the errors of the records, so that errors.Is and
// errors.AsType look into each of them.
func (e *ProcessingError) Unwrap() []error { return e.Errors }

// PanicError is the failure of a record whose handler panicked.
type PanicError struct {
	// Value is what the handler passed to panic.
	Value any
	// Stack is the stack trace of the panicking goroutine.
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("record handler panicked: %v\n%s", e.Value, e.Stack)
}

// Unwrap returns the panic value when it is an error.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// Option configures how a batch is processed.
type Option func(*config)

type config struct {
	raiseOnEntireBatchFailure bool
	concurrency               int
	skipGroupOnError          bool
	logger                    *slog.Logger
	hooks                     []hook
}

func newConfig(opts []Option) config {
	cfg := config{raiseOnEntireBatchFailure: true, concurrency: 1}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// WithRaiseOnEntireBatchFailure decides what happens when every record of
// the batch fails. On (the default), the Process function returns a
// *ProcessingError next to the response, so that the invocation fails and
// Lambda retries the whole batch. Off, it returns the response alone, with
// every record in it: the invocation succeeds, which keeps Lambda from
// scaling the SQS pollers down when errors are part of the flow.
func WithRaiseOnEntireBatchFailure(raise bool) Option {
	return func(c *config) { c.raiseOnEntireBatchFailure = raise }
}

// WithConcurrency processes up to n records at the same time, each in its
// own goroutine; the default, 1, processes them one after the other. It is
// Powertools Python's AsyncBatchProcessor, with a bound. Use it only when
// the records do not depend on each other's order. ProcessSQSFIFO ignores
// it: a FIFO queue's records are always processed in order.
func WithConcurrency(n int) Option {
	return func(c *config) { c.concurrency = max(n, 1) }
}

// WithSkipGroupOnError makes ProcessSQSFIFO keep going after a failure: it
// fails only the remaining records of the failed record's message group and
// processes the other groups. The other Process functions ignore it.
func WithSkipGroupOnError() Option {
	return func(c *config) { c.skipGroupOnError = true }
}

// WithLogger writes a warning to logger for every record whose handler
// failed, with the error, as Powertools Python does when given a logger.
// Records skipped without running the handler are not logged.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) { c.logger = logger }
}

// hook is a record hook, typed with the record it takes. The record type is
// checked against the batch's when processing starts.
type hook interface {
	recordType() reflect.Type
}

type (
	beforeHook[R any]  func(ctx context.Context, record R)
	afterHook[R any]   func(ctx context.Context, record R, err error)
	failureHook[R any] func(ctx context.Context, record R, err error)
)

func (beforeHook[R]) recordType() reflect.Type  { return reflect.TypeFor[R]() }
func (afterHook[R]) recordType() reflect.Type   { return reflect.TypeFor[R]() }
func (failureHook[R]) recordType() reflect.Type { return reflect.TypeFor[R]() }

// WithBeforeRecord runs fn right before the handler receives each record.
// Records that are skipped (after a FIFO failure, or when the invocation
// ends) never reach it.
//
// The record type R must be the batch's: events.SQSMessage for ProcessSQS,
// events.KinesisEventRecord for ProcessKinesis, and so on. With
// WithConcurrency, hooks run concurrently and must be safe for it.
func WithBeforeRecord[R any](fn func(ctx context.Context, record R)) Option {
	return func(c *config) { c.hooks = append(c.hooks, beforeHook[R](fn)) }
}

// WithAfterRecord runs fn once the outcome of each record is known, err
// being nil when the record succeeded: Powertools Python's success_handler
// and failure_handler together. It sees every record, skipped ones
// included, after the WithOnFailure hooks. Record types and concurrency are
// as for WithBeforeRecord.
func WithAfterRecord[R any](fn func(ctx context.Context, record R, err error)) Option {
	return func(c *config) { c.hooks = append(c.hooks, afterHook[R](fn)) }
}

// WithOnFailure runs fn for every record that failed, skipped ones
// included: Powertools Python's failure_handler, the place to send errors
// to a tracking service or to count failures in a metric. Record types and
// concurrency are as for WithBeforeRecord.
func WithOnFailure[R any](fn func(ctx context.Context, record R, err error)) Option {
	return func(c *config) { c.hooks = append(c.hooks, failureHook[R](fn)) }
}

// hooks are the record hooks of a batch of R.
type hooks[R any] struct {
	before  []beforeHook[R]
	after   []afterHook[R]
	failure []failureHook[R]
}

func typedHooks[R any](all []hook) (hooks[R], error) {
	var h hooks[R]
	for _, x := range all {
		switch fn := x.(type) {
		case beforeHook[R]:
			h.before = append(h.before, fn)
		case afterHook[R]:
			h.after = append(h.after, fn)
		case failureHook[R]:
			h.failure = append(h.failure, fn)
		default:
			return h, fmt.Errorf("%w: a hook takes %s records, the batch holds %s",
				ErrHookRecordType, x.recordType(), reflect.TypeFor[R]())
		}
	}
	return h, nil
}

// engine processes the records of one batch.
type engine[R any] struct {
	cfg     *config
	hooks   hooks[R]
	handler RecordHandler[R]
}

// process runs handler over records and returns one error per record, nil
// for the records that succeeded. A non-nil group makes the batch an SQS
// FIFO one: records run in order and group tells each record's message
// group.
func process[R any](ctx context.Context, records []R, handler RecordHandler[R], cfg *config,
	group func(R) string,
) ([]error, error) {
	if len(records) == 0 {
		return nil, ErrUnexpectedBatchType
	}
	h, err := typedHooks[R](cfg.hooks)
	if err != nil {
		return nil, err
	}
	e := &engine[R]{cfg: cfg, hooks: h, handler: handler}
	errs := make([]error, len(records))
	switch {
	case group != nil:
		if cfg.concurrency > 1 {
			pdebug.Logger().DebugContext(ctx, "SQS FIFO batches are processed in order; ignoring the concurrency")
		}
		e.fifo(ctx, records, errs, group)
	case cfg.concurrency > 1:
		e.concurrent(ctx, records, errs)
	default:
		e.sequential(ctx, records, errs)
	}
	return errs, nil
}

// notProcessed returns the failure of the records left when ctx has ended,
// nil while it is alive.
func notProcessed(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrNotProcessed, context.Cause(ctx))
}

func (e *engine[R]) sequential(ctx context.Context, records []R, errs []error) {
	for i, rec := range records {
		if cause := notProcessed(ctx); cause != nil {
			errs[i] = e.skip(ctx, rec, cause)
			continue
		}
		errs[i] = e.handle(ctx, rec)
	}
}

func (e *engine[R]) concurrent(ctx context.Context, records []R, errs []error) {
	slots := make(chan struct{}, e.cfg.concurrency)
	var wg sync.WaitGroup
	for i, rec := range records {
		if !acquire(ctx, slots) {
			errs[i] = e.skip(ctx, rec, notProcessed(ctx))
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = e.handle(ctx, rec)
		})
	}
	wg.Wait()
}

// acquire takes one of the slots, waiting for a running record to free one.
// It gives up when ctx ends.
func acquire(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	if ctx.Err() != nil {
		<-slots
		return false
	}
	return true
}

func (e *engine[R]) fifo(ctx context.Context, records []R, errs []error, groupOf func(R) string) {
	failedGroups := map[string]bool{}
	failed := false
	for i, rec := range records {
		group := groupOf(rec)
		switch {
		case !e.cfg.skipGroupOnError && failed:
			errs[i] = e.skip(ctx, rec, ErrFIFOShortCircuit)
		case e.cfg.skipGroupOnError && failedGroups[group]:
			errs[i] = e.skip(ctx, rec, ErrFIFOGroupShortCircuit)
		default:
			if cause := notProcessed(ctx); cause != nil {
				errs[i] = e.skip(ctx, rec, cause)
			} else {
				errs[i] = e.handle(ctx, rec)
			}
		}
		if errs[i] != nil {
			failed = true
			// A record without a message group never fails the others.
			if group != "" {
				failedGroups[group] = true
			}
		}
	}
}

// handle runs the handler on rec, between the record hooks.
func (e *engine[R]) handle(ctx context.Context, rec R) error {
	for _, fn := range e.hooks.before {
		fn(ctx, rec)
	}
	err := call(ctx, e.handler, rec)
	if err != nil && e.cfg.logger != nil {
		e.cfg.logger.WarnContext(ctx, "Record processing exception; skipping this record", "err", err)
	}
	e.finish(ctx, rec, err)
	return err
}

// skip fails rec without running the handler.
func (e *engine[R]) skip(ctx context.Context, rec R, err error) error {
	e.finish(ctx, rec, err)
	return err
}

func (e *engine[R]) finish(ctx context.Context, rec R, err error) {
	if err != nil {
		pdebug.Logger().DebugContext(ctx, "record processing failed", "err", err)
		for _, fn := range e.hooks.failure {
			fn(ctx, rec, err)
		}
	}
	for _, fn := range e.hooks.after {
		fn(ctx, rec, err)
	}
}

// call runs handler, turning a panic into the record's failure.
func call[R any](ctx context.Context, handler RecordHandler[R], rec R) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	return handler(ctx, rec)
}

// respond collects the failure of each failed record, in the order of the
// batch, and decides whether the batch as a whole failed.
func respond[R, F any](ctx context.Context, records []R, errs []error, cfg *config, failure func(R) F) ([]F, error) {
	// Never nil: Lambda reads a null list as success too, but Powertools
	// Python answers with an empty one.
	failures := make([]F, 0)
	failed := make([]error, 0, len(errs))
	for i, err := range errs {
		if err != nil {
			failures = append(failures, failure(records[i]))
			failed = append(failed, err)
		}
	}
	if len(failed) == 0 {
		pdebug.Logger().DebugContext(ctx, "all records successfully processed", "records", len(records))
		return failures, nil
	}
	if len(failed) == len(records) && cfg.raiseOnEntireBatchFailure {
		return failures, &ProcessingError{Errors: failed}
	}
	return failures, nil
}
