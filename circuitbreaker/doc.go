// Package circuitbreaker stops calling an unhealthy downstream dependency,
// giving it room to recover while you decide what happens to the rejected
// calls. It is the Go side of Powertools for AWS Lambda (Python)'s circuit
// breaker utility, and shares its state with it.
//
// # How it works
//
// A circuit is a named guard around one downstream. It is Closed while the
// downstream is healthy: calls go through and their failures are counted,
// in memory, per execution environment. After WithFailureThreshold
// consecutive failures (5 by default) the circuit trips to Open and the
// state is written to the store, where every execution environment sees
// it: calls are rejected without reaching the downstream. After
// WithRecoveryTimeout (30 seconds by default), one breaker, elected through
// a conditional write, moves the circuit to HalfOpen and sends probes: after
// WithSuccessThreshold successful probes in a row (3 by default) the
// circuit closes, at the first failed one it opens again.
//
//	CLOSED to OPEN: N consecutive failures
//	OPEN to HALF_OPEN: the recovery timeout elapsed, one breaker elected
//	HALF_OPEN to CLOSED: the probes succeeded
//	HALF_OPEN to OPEN: a probe failed
//
// A healthy circuit never writes: the store is read at most once per
// WithLocalCacheMaxAge (5 seconds by default) and written only on a
// transition.
//
// # Getting started
//
// The state lives in a DynamoDB table with a string partition key "id" and
// time to live on "expiration"; the function needs dynamodb:GetItem,
// dynamodb:PutItem and dynamodb:UpdateItem on it. Create the breaker once,
// in main, and guard the function that calls the downstream:
//
//	store, err := circuitbreaker.NewDynamoDBStoreFromConfig(cfg, os.Getenv("CIRCUIT_BREAKER_TABLE"))
//	payments, err := circuitbreaker.New("payment-backend", store)
//	charge := payments.Wrap(paymentAPI.Charge)
//
// Wrap returns a function with the signature it was given; Call runs a
// single call through the circuit:
//
//	receipt, err := payments.Call(ctx, func(ctx context.Context) (Receipt, error) {
//		return paymentAPI.Charge(ctx, order)
//	})
//
// Guard the call to one downstream, not the whole handler: a bug in the
// handler's parsing would trip a circuit named after a healthy dependency.
//
// # Rejected calls
//
// While the circuit rejects calls, they return an *OpenError, which
// matches ErrCircuitOpen and carries a CircuitInfo snapshot of the circuit.
// With Wrap, OnCircuitOpen hands them to a callback instead, with the
// call's argument: what it returns is the result of the call. Buffer the
// request in a queue, return a cached value, answer that the service is
// degraded.
//
// # Which errors count
//
// Every error counts as a failure by default. WithHandledErrors counts only
// the errors that match one of its targets, WithIgnoredErrors every error
// but those, WithFailureFilter those its function picks (for error types,
// or timeouts). The three are mutually exclusive. An error that does not
// count is returned as it is and leaves the circuit alone. A context error
// counts like any other, so that timeouts of the downstream do; ignore
// context.Canceled if the caller's cancellations should not.
//
// A panic of the guarded function is not recovered: it goes through the
// breaker untouched, as Python lets a BaseException through.
//
// # Observability
//
// WithOnTransition calls a hook on every change of state, never on the
// path of a healthy call: the place to emit a metric. A panic in the hook is
// recovered and ignored.
//
// # Concurrency
//
// A Breaker is safe for concurrent use and meant to be shared by the
// invocations of the process (Lambda Managed Instances run several at once):
// it counts failures under a lock, so that exactly one call sees the
// threshold crossed, and it probes one call at a time. Callbacks and hooks
// may run concurrently with themselves.
//
// # The store
//
// DynamoDBStore reads and writes the items of Powertools Python's
// CircuitBreakerDynamoDBPersistence, with the same attribute names, state
// strings, whole-second timestamps and condition expressions: a Go and a
// Python function guard the same circuit when they share the table and the
// circuit name. WithSortKeyAttr supports a composite key, as in a
// single-table design, where the partition key defaults to
// "circuit_breaker#<function name>" and WithStaticPKValue lets several
// functions share their circuits. Every attribute can be renamed.
//
// The breaker fails open: when the store cannot be read, the circuit is
// considered Closed, and the call goes through. A failed transition write
// never hides the downstream's result; the transition happens later.
// Another backend implements the Store interface; the breaker keeps the
// local cache, the fail-open policy and the time to live.
//
// # Environment variables
//
// POWERTOOLS_CIRCUIT_BREAKER_DISABLED, when true, bypasses every circuit:
// the guarded function always runs and the store is never touched. It is
// meant for development and tests, and logs a warning to standard error
// once. AWS_LAMBDA_FUNCTION_NAME names the default partition of a composite
// key. Internal diagnostics, such as failed store reads and writes, go to
// standard error when POWERTOOLS_DEBUG is on.
//
// # Differences from Powertools Python
//
//   - The decorator becomes the Breaker value, created once, with Call and
//     Wrap as generic methods; CircuitBreakerConfig becomes the options of
//     New. The on_circuit_open callback is OnCircuitOpen, an option of Wrap:
//     it receives the call's argument instead of Python's positional and
//     keyword arguments. With Call, a rejected call is the *OpenError.
//   - Error classes become errors.Is targets (WithHandledErrors,
//     WithIgnoredErrors), plus WithFailureFilter for what a sentinel cannot
//     match.
//   - The prober is the Breaker, not the thread: Go has no goroutine
//     identity, so a breaker owns the probe and lets one of its calls
//     through at a time.
//   - The breaker keeps the local cache, its circuit's settings and its
//     counters itself, so breakers on the same circuit in one process
//     behave like separate execution environments. Python keeps the
//     counters per process and the cache per persistence layer.
//   - Transition writes do not inherit the cancellation of the call's
//     context, which may be why the call failed.
//   - The on_transition hook receives the context too.
//   - CircuitBreakerPersistenceError is not ported: a custom Store returns
//     its own errors. Boto3 user agent feature tracking is not ported.
package circuitbreaker
