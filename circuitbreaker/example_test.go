package circuitbreaker_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/alessandrolattao/powertools-lambda-go/circuitbreaker"
)

var errUnavailable = errors.New("payment backend unavailable")

func ExampleBreaker_Call() {
	payments, err := circuitbreaker.New("payment-backend", newMapStore(), circuitbreaker.WithFailureThreshold(2))
	if err != nil {
		panic(err)
	}
	charge := func(context.Context) (string, error) { return "", errUnavailable }

	for range 3 {
		_, err := payments.Call(context.Background(), charge)
		if openErr, ok := errors.AsType[*circuitbreaker.OpenError](err); ok {
			fmt.Println("rejected, circuit", openErr.Circuit.State, "after", openErr.Circuit.FailureCount, "failures")
			continue
		}
		fmt.Println(err)
	}
	// Output:
	// payment backend unavailable
	// payment backend unavailable
	// rejected, circuit OPEN after 2 failures
}

func ExampleBreaker_Wrap() {
	type Order struct{ ID string }

	payments, err := circuitbreaker.New("payment-backend", newMapStore(), circuitbreaker.WithFailureThreshold(1))
	if err != nil {
		panic(err)
	}
	charge := func(_ context.Context, order Order) (string, error) {
		return "", fmt.Errorf("charge %s: %w", order.ID, errUnavailable)
	}
	// While the circuit is open, the orders go to an overflow queue.
	buffer := func(_ context.Context, order Order, circuit circuitbreaker.CircuitInfo) (string, error) {
		return "queued " + order.ID + " while " + circuit.Name + " recovers", nil
	}
	guardedCharge := payments.Wrap(charge, circuitbreaker.OnCircuitOpen(buffer))

	for _, id := range []string{"o-1", "o-2"} {
		fmt.Println(guardedCharge(context.Background(), Order{ID: id}))
	}
	// Output:
	// charge o-1: payment backend unavailable
	// queued o-2 while payment-backend recovers <nil>
}

func ExampleWithOnTransition() {
	logTransition := func(_ context.Context, t circuitbreaker.Transition) {
		// Emit a metric here: it runs only when the state changes.
		fmt.Println(t.CircuitName, t.From, "->", t.To)
	}
	payments, err := circuitbreaker.New("payment-backend", newMapStore(),
		circuitbreaker.WithFailureThreshold(1), circuitbreaker.WithOnTransition(logTransition))
	if err != nil {
		panic(err)
	}
	_, err = payments.Call(context.Background(), func(context.Context) (int, error) { return 0, errUnavailable })
	fmt.Println(err)
	// Output:
	// payment-backend CLOSED -> OPEN
	// payment backend unavailable
}

func ExampleWithHandledErrors() {
	errBadRequest := errors.New("400 bad request")
	errTimeout := errors.New("timeout")

	// A 400 is the caller's fault: only timeouts and unavailability count.
	payments, err := circuitbreaker.New("payment-backend", newMapStore(),
		circuitbreaker.WithFailureThreshold(1), circuitbreaker.WithHandledErrors(errTimeout, errUnavailable))
	if err != nil {
		panic(err)
	}
	for _, cause := range []error{errBadRequest, errBadRequest, errTimeout, nil} {
		_, err := payments.Call(context.Background(), func(context.Context) (bool, error) { return cause == nil, cause })
		fmt.Println(err)
	}
	// Output:
	// 400 bad request
	// 400 bad request
	// timeout
	// circuit "payment-backend" is open
}

func ExampleNewDynamoDBStoreFromConfig() {
	// In main: cfg, err := config.LoadDefaultConfig(ctx)
	cfg := aws.Config{Region: "eu-west-1"}

	// A single-table design: circuits share the partition CIRCUIT_BREAKER,
	// so every function guarding the payment backend trips together.
	store, err := circuitbreaker.NewDynamoDBStoreFromConfig(cfg, "CircuitBreakerState",
		circuitbreaker.WithKeyAttr("PK"),
		circuitbreaker.WithSortKeyAttr("SK"),
		circuitbreaker.WithStaticPKValue("CIRCUIT_BREAKER"),
	)
	if err != nil {
		panic(err)
	}
	payments, err := circuitbreaker.New("payment-backend", store)
	fmt.Println(payments != nil, err)
	// Output: true <nil>
}
