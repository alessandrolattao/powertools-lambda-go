package circuitbreaker_test

import (
	"context"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/circuitbreaker"
)

// memStore keeps circuits in memory, with the semantics of DynamoDBStore:
// a custom Store, and the fake the tests share.
type memStore struct {
	mu      sync.Mutex
	records map[string]circuitbreaker.Record

	// Errors the next calls return, and how many calls each operation saw.
	getErr, putErr, acquireErr, updateErr error
	gets, puts, acquires, updates         int
}

func newMemStore() *memStore {
	return &memStore{records: map[string]circuitbreaker.Record{}}
}

func (s *memStore) Get(_ context.Context, name string) (circuitbreaker.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return circuitbreaker.Record{}, false, s.getErr
	}
	record, ok := s.records[name]
	return record, ok, nil
}

func (s *memStore) Put(_ context.Context, record circuitbreaker.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.putErr != nil {
		return s.putErr
	}
	s.records[record.Name] = record
	return nil
}

func (s *memStore) AcquireProbe(_ context.Context, record circuitbreaker.Record, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquires++
	if s.acquireErr != nil {
		return false, s.acquireErr
	}
	current, ok := s.records[record.Name]
	if !ok {
		return false, nil
	}
	free := current.State == circuitbreaker.Open && current.HalfOpenOwner == "" &&
		!current.OpenedAt.IsZero() && current.OpenedAt.Equal(record.OpenedAt)
	expired := current.State == circuitbreaker.HalfOpen && !current.ProbeLeaseExpiry.IsZero() &&
		!current.ProbeLeaseExpiry.After(now)
	if !free && !expired {
		return false, nil
	}
	s.records[record.Name] = record
	return true, nil
}

func (s *memStore) Update(_ context.Context, record circuitbreaker.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates++
	if s.updateErr != nil {
		return s.updateErr
	}
	current := s.records[record.Name]
	current.Name = record.Name
	current.State = record.State
	current.FailureCount = record.FailureCount
	if !record.Expiration.IsZero() {
		current.Expiration = record.Expiration
	}
	current.OpenedAt = record.OpenedAt
	current.HalfOpenOwner = ""
	current.ProbeLeaseExpiry = time.Time{}
	s.records[record.Name] = current
	return nil
}

func (s *memStore) record(name string) circuitbreaker.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[name]
}

func (s *memStore) set(record circuitbreaker.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[record.Name] = record
}

func (s *memStore) fail(get, put, acquire, update error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getErr, s.putErr, s.acquireErr, s.updateErr = get, put, acquire, update
}

func (s *memStore) calls() (gets, puts, acquires, updates int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.puts, s.acquires, s.updates
}

// clock is a time that moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_700_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
