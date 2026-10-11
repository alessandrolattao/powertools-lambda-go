package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// State is the mode a circuit is in. Its values are the strings Powertools
// Python stores.
type State string

// The states of a circuit.
const (
	// Closed is normal operation: calls reach the downstream and their
	// failures are counted.
	Closed State = "CLOSED"
	// Open means the downstream is considered unhealthy: calls are rejected
	// without reaching it.
	Open State = "OPEN"
	// HalfOpen means recovery is being tested: one prober's calls reach the
	// downstream, the others are rejected.
	HalfOpen State = "HALF_OPEN"
)

// Valid reports whether s is one of the three states.
func (s State) Valid() bool {
	return s == Closed || s == Open || s == HalfOpen
}

// CircuitInfo is a snapshot of a circuit, handed to the code that deals
// with a rejected call. It exposes nothing of how the state is stored.
type CircuitInfo struct {
	// Name is the circuit's name.
	Name string
	// State is the state the call found the circuit in.
	State State
	// FailureCount is the number of consecutive failures that tripped the
	// circuit, as the execution environment that tripped it counted them.
	// It is not a total across the fleet (failures are counted in memory,
	// so that a healthy circuit never writes), and it is 0 in states
	// reached without a trip, such as HalfOpen or Open after a failed
	// probe. Count failures with a metric for their volume.
	FailureCount int
	// OpenedAt is when the circuit opened, the zero time while it is
	// closed.
	OpenedAt time.Time
}

// Transition describes a change of state, handed to the hook set with
// WithOnTransition.
type Transition struct {
	// CircuitName is the circuit's name.
	CircuitName string
	// From is the state the circuit left.
	From State
	// To is the state the circuit entered.
	To State
	// OpenedAt is when the circuit opened, when it matters to the new state;
	// the zero time otherwise.
	OpenedAt time.Time
}

// Record is the stored state of one circuit: what a Store reads and writes.
// Absent values are zero: the zero time for timestamps, the empty string
// for the owner.
type Record struct {
	// Name is the circuit's name, its key in the store.
	Name string
	// State is the circuit's state.
	State State
	// FailureCount is the number of failures that tripped the circuit.
	FailureCount int
	// OpenedAt is when the circuit opened, which anchors the recovery
	// timeout. It is set while the circuit is open or half-open.
	OpenedAt time.Time
	// HalfOpenOwner identifies the breaker that won the election to probe
	// the half-open circuit.
	HalfOpenOwner string
	// ProbeLeaseExpiry is when the prober's lease ends: another breaker can
	// take the probe over after it.
	ProbeLeaseExpiry time.Time
	// Expiration is when the store may delete the record (its time to
	// live).
	Expiration time.Time
}

func (r *Record) info() CircuitInfo {
	return CircuitInfo{Name: r.Name, State: r.State, FailureCount: r.FailureCount, OpenedAt: r.OpenedAt}
}

// Store keeps the state of circuits where every execution environment sees
// it. DynamoDBStore is the one this package provides; another backend
// implements these four operations, and the Breaker does the rest (local
// caching, failing open, time to live). Every timestamp is stored in whole
// seconds.
type Store interface {
	// Get returns the record of the circuit name, and false when the store
	// has none. An eventually consistent read is enough.
	Get(ctx context.Context, name string) (Record, bool, error)
	// Put writes record unconditionally, replacing the stored one: it
	// records a circuit tripping from Closed to Open.
	Put(ctx context.Context, record Record) error
	// AcquireProbe writes record, a HalfOpen record naming its owner, only
	// if the stored circuit is Open, has no owner and opened at
	// record.OpenedAt, or is HalfOpen with a probe lease that ended by now.
	// It reports whether it wrote: exactly one of the breakers racing for
	// the probe must win.
	AcquireProbe(ctx context.Context, record Record, now time.Time) (bool, error)
	// Update records the end of a probe, unconditionally: it sets the
	// state, the failure count, the expiration and, when it is not zero,
	// OpenedAt, and removes the owner, the lease and, when OpenedAt is
	// zero, the opening time.
	Update(ctx context.Context, record Record) error
}

// ErrCircuitOpen is what every *OpenError matches with errors.Is.
var ErrCircuitOpen = errors.New("circuit is open")

// ErrInvalidConfig reports a breaker or a store built with options that do
// not go together or are out of range: Powertools Python's
// CircuitBreakerConfigError.
var ErrInvalidConfig = errors.New("invalid circuit breaker configuration")

// errInvalidRecord reports a stored record that is not a circuit's: the
// breaker fails open on it.
var errInvalidRecord = errors.New("invalid circuit record")

// OpenError is the error of a call the circuit rejected without running it,
// when no OnCircuitOpen callback handles it: Powertools Python's
// CircuitBreakerOpenError.
type OpenError struct {
	// Circuit is the circuit as the call found it.
	Circuit CircuitInfo
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("circuit %q is open", e.Circuit.Name)
}

// Is makes every OpenError match ErrCircuitOpen.
func (*OpenError) Is(target error) bool { return target == ErrCircuitOpen }
