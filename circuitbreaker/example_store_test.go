package circuitbreaker_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/circuitbreaker"
)

// mapStore keeps circuits in the memory of one process: a Store of your
// own, for a backend other than DynamoDB. It must implement the election
// atomically, as DynamoDB's conditional write does.
type mapStore struct {
	mu       sync.Mutex
	circuits map[string]circuitbreaker.Record
}

func newMapStore() *mapStore {
	return &mapStore{circuits: map[string]circuitbreaker.Record{}}
}

func (s *mapStore) Get(_ context.Context, name string) (circuitbreaker.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.circuits[name]
	return record, ok, nil
}

func (s *mapStore) Put(_ context.Context, record circuitbreaker.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.circuits[record.Name] = record
	return nil
}

func (s *mapStore) AcquireProbe(_ context.Context, record circuitbreaker.Record, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.circuits[record.Name]
	free := current.State == circuitbreaker.Open && current.HalfOpenOwner == "" && current.OpenedAt.Equal(record.OpenedAt)
	expired := current.State == circuitbreaker.HalfOpen && !current.ProbeLeaseExpiry.After(now)
	if !free && !expired {
		return false, nil
	}
	s.circuits[record.Name] = record
	return true, nil
}

func (s *mapStore) Update(_ context.Context, record circuitbreaker.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.circuits[record.Name] = circuitbreaker.Record{
		Name:       record.Name,
		State:      record.State,
		OpenedAt:   record.OpenedAt,
		Expiration: record.Expiration,
	}
	return nil
}

func Example_customStore() {
	payments, err := circuitbreaker.New("payment-backend", newMapStore())
	if err != nil {
		panic(err)
	}
	receipt, err := payments.Call(context.Background(), func(context.Context) (string, error) {
		return "receipt-1", nil
	})
	fmt.Println(receipt, err)
	// Output: receipt-1 <nil>
}
