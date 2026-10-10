// Package coldstart tells the one invocation that paid for the execution
// environment's initialization apart from all the others.
//
// The cold invocation is the first one any Powertools utility sees. Every
// handler decorator asks on entry, so the first invocation that runs through
// one of them claims it, and every utility that asks later during the same
// invocation gets the same answer. Concurrent invocations (Lambda Managed
// Instances) race for the claim; exactly one wins.
package coldstart

import (
	"context"
	"sync/atomic"

	"github.com/aws/aws-lambda-go/lambdacontext"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// claim identifies the cold invocation: the request id it carries, or for an
// invocation without one, a value nobody else holds.
var claim atomic.Pointer[string]

// Is reports whether the invocation ctx belongs to is the cold one. An
// environment initialized by provisioned concurrency has none.
func Is(ctx context.Context) bool {
	if env.IsProvisioned() {
		return false
	}
	mine := new(string)
	if lc, ok := lambdacontext.FromContext(ctx); ok {
		*mine = lc.AwsRequestID
	}
	if claim.CompareAndSwap(nil, mine) {
		return true
	}
	first := claim.Load()
	return *mine != "" && *first == *mine
}

// Reset forgets the cold invocation, as a fresh process would. Tests use it.
func Reset() {
	claim.Store(nil)
}
