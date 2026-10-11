package idempotency

import (
	"context"
	"sync"
	"time"
)

// Store is where idempotency records persist: the persistence layer.
// DynamoDB (package idempotency/dynamodb) and Redis or Valkey (package
// idempotency/redis) come with Powertools; implement Store to bring another
// one.
//
// A Store only moves records in and out. Keys, hashes, payload validation,
// the local cache and the status machine are the business of Idempotent, so
// a Store has no configuration in common with it and can serve any number of
// idempotent functions at once. Its methods are called concurrently.
type Store interface {
	// PutRecord saves record, an INPROGRESS record, unless the store holds a
	// record with the same key that HoldsKey at now: then it saves nothing
	// and returns ErrItemAlreadyExists, or an ItemAlreadyExistsError with
	// the record when it has it at hand. The check and the write must be one
	// atomic step (a conditional write), as that is what keeps two
	// concurrent invocations from both running the function.
	PutRecord(ctx context.Context, record Record, now time.Time) error
	// GetRecord returns the record with key, or ErrItemNotFound. It must
	// read the latest write (a strongly consistent read).
	GetRecord(ctx context.Context, key string) (*Record, error)
	// UpdateRecord saves record, a COMPLETED record, over the INPROGRESS one
	// with the same key.
	UpdateRecord(ctx context.Context, record Record) error
	// DeleteRecord removes the record with key, if any.
	DeleteRecord(ctx context.Context, key string) error
}

// MemoryStore is a Store that keeps records in memory. It serves tests, and
// functions that only need idempotency within one execution environment;
// records do not survive the environment and are not shared between
// environments, so it does not deduplicate a Lambda function's invocations
// in general. Its zero value is ready to use.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]Record
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

// PutRecord implements Store.
func (s *MemoryStore) PutRecord(_ context.Context, record Record, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.records[record.Key]; ok && old.HoldsKey(now) {
		return &ItemAlreadyExistsError{Old: &old}
	}
	if s.records == nil {
		s.records = make(map[string]Record)
	}
	s.records[record.Key] = record
	return nil
}

// GetRecord implements Store.
func (s *MemoryStore) GetRecord(_ context.Context, key string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return nil, ErrItemNotFound
	}
	return &record, nil
}

// UpdateRecord implements Store.
func (s *MemoryStore) UpdateRecord(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[string]Record)
	}
	s.records[record.Key] = record
	return nil
}

// DeleteRecord implements Store.
func (s *MemoryStore) DeleteRecord(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, key)
	return nil
}
