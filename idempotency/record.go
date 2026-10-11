package idempotency

import "time"

// Status is the state of an idempotency record.
type Status string

// The statuses a record goes through. Stores only ever hold INPROGRESS and
// COMPLETED; EXPIRED is what a record past its expiry counts as.
const (
	StatusInProgress Status = "INPROGRESS"
	StatusCompleted  Status = "COMPLETED"
	StatusExpired    Status = "EXPIRED"
)

// Record is an idempotency record: what a Store keeps for one idempotency
// key. Its fields map one to one onto the attributes Powertools Python
// writes, so that both runtimes can read each other's records.
type Record struct {
	// Key is the idempotency key, "<prefix>#<hash>".
	Key string
	// Status is INPROGRESS while the function runs, COMPLETED once it
	// returned successfully.
	Status Status
	// ExpiresAt is when the record stops counting, in whole seconds. After
	// it, the same key runs the function again.
	ExpiresAt time.Time
	// InProgressExpiresAt is when an INPROGRESS record stops locking its key,
	// in milliseconds: the deadline of the invocation that wrote it, so that
	// a timed-out invocation does not lock the key until ExpiresAt. It is
	// zero when the deadline was unknown.
	InProgressExpiresAt time.Time
	// ResponseData is the JSON the function returned, on COMPLETED records.
	ResponseData string
	// PayloadHash is the hash of the part of the payload that payload
	// validation compares, empty when validation is off.
	PayloadHash string
}

// IsExpired reports whether the record has expired at now. Expiry has a
// granularity of one second, as in Powertools Python: a record expiring at
// second s still counts during s.
func (r Record) IsExpired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && now.Unix() > r.ExpiresAt.Unix()
}

// HoldsKey reports whether the record still holds its key at now, so that a
// new INPROGRESS record must not replace it: it has not expired, and it is not
// an INPROGRESS record whose invocation ran past its deadline. A Store's
// PutRecord fails exactly when the record it holds for the key HoldsKey.
func (r Record) HoldsKey(now time.Time) bool {
	if r.IsExpired(now) {
		return false
	}
	timedOut := r.Status == StatusInProgress && !r.InProgressExpiresAt.IsZero() &&
		r.InProgressExpiresAt.UnixMilli() < now.UnixMilli()
	return !timedOut
}
