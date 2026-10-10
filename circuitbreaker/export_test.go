package circuitbreaker

import "time"

// SetClock makes b read the time from now, so that tests need not wait for
// the recovery timeout.
func SetClock(b *Breaker, now func() time.Time) { b.now = now }

// Owner returns the identity b claims the probe with.
func Owner(b *Breaker) string { return b.owner }
