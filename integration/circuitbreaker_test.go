package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/circuitbreaker"
)

var errDownstream = errors.New("downstream unavailable")

func downstreamFails(context.Context) (string, error) { return "", errDownstream }

func downstreamWorks(ctx context.Context) (string, error) { return "ok", ctx.Err() }

// circuitTable creates a table for circuits, with the key Powertools Python
// documents ("id"), or a composite one (PK, SK) when composite is set.
func circuitTable(t *testing.T, composite bool) (client *dynamodb.Client, table string) {
	t.Helper()
	ctx := t.Context()
	client = dynamodb.NewFromConfig(suite.aws)
	name := fmt.Sprintf("circuits-%d", rand.Uint64())
	input := &dynamodb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash}},
	}
	if composite {
		input.AttributeDefinitions = []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS},
		}
		input.KeySchema = []types.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange},
		}
	}
	_, err := client.CreateTable(ctx, input)
	require.NoError(t, err)
	require.NoError(t, dynamodb.NewTableExistsWaiter(client).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: aws.String(name)}, time.Minute))
	t.Cleanup(func() {
		_, err := client.DeleteTable(context.WithoutCancel(ctx), &dynamodb.DeleteTableInput{TableName: aws.String(name)})
		assert.NoError(t, err)
	})
	return client, name
}

// breaker is one execution environment: its own store and breaker on the
// shared table.
func breaker(t *testing.T, client *dynamodb.Client, table string, storeOpts []circuitbreaker.DynamoDBOption,
	opts ...circuitbreaker.Option,
) *circuitbreaker.Breaker {
	t.Helper()
	store, err := circuitbreaker.NewDynamoDBStore(client, table, storeOpts...)
	require.NoError(t, err)
	opts = append([]circuitbreaker.Option{
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithRecoveryTimeout(recoveryTimeout),
		circuitbreaker.WithSuccessThreshold(2),
		circuitbreaker.WithLocalCacheMaxAge(0),
	}, opts...)
	b, err := circuitbreaker.New("payment-backend", store, opts...)
	require.NoError(t, err)
	return b
}

// item reads the circuit's item as it is stored.
func item(t *testing.T, client *dynamodb.Client, table string, key map[string]types.AttributeValue) map[string]types.AttributeValue {
	t.Helper()
	out, err := client.GetItem(t.Context(), &dynamodb.GetItemInput{
		TableName: aws.String(table), Key: key, ConsistentRead: aws.Bool(true),
	})
	require.NoError(t, err)
	return out.Item
}

func str(t *testing.T, item map[string]types.AttributeValue, attr string) string {
	t.Helper()
	v, ok := item[attr].(*types.AttributeValueMemberS)
	require.True(t, ok, "%s is not a string in %v", attr, item)
	return v.Value
}

func num(t *testing.T, item map[string]types.AttributeValue, attr string) int64 {
	t.Helper()
	v, ok := item[attr].(*types.AttributeValueMemberN)
	require.True(t, ok, "%s is not a number in %v", attr, item)
	n, err := strconv.ParseInt(v.Value, 10, 64)
	require.NoError(t, err)
	return n
}

// recoveryTimeout is short, to keep the tests fast, but two seconds: the
// store counts whole seconds, and a check right after a trip must not see
// it elapsed because a second boundary went by.
const recoveryTimeout = 2 * time.Second

// waitRecovery waits for the recovery timeout to elapse since the circuit
// opened.
func waitRecovery() { time.Sleep(recoveryTimeout + 100*time.Millisecond) }

func TestCircuitBreakerTransitions(t *testing.T) {
	t.Parallel()
	client, table := circuitTable(t, false)
	var mu sync.Mutex
	var moves []string
	b := breaker(t, client, table, nil, circuitbreaker.WithOnTransition(func(_ context.Context, tr circuitbreaker.Transition) {
		mu.Lock()
		defer mu.Unlock()
		moves = append(moves, string(tr.From)+">"+string(tr.To))
	}))
	ctx := t.Context()
	key := map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "payment-backend"}}

	// CLOSED: failures are counted in memory, nothing is written.
	_, err := b.Call(ctx, downstreamFails)
	require.ErrorIs(t, err, errDownstream)
	assert.Empty(t, item(t, client, table, key))

	// CLOSED -> OPEN.
	_, err = b.Call(ctx, downstreamFails)
	require.ErrorIs(t, err, errDownstream)
	open := item(t, client, table, key)
	assert.Equal(t, "OPEN", str(t, open, "state"))
	assert.Equal(t, int64(2), num(t, open, "failure_count"))
	openedAt := num(t, open, "opened_at")
	assert.InDelta(t, time.Now().Unix(), openedAt, 2)
	assert.Equal(t, openedAt+2+3600, num(t, open, "expiration"))
	assert.NotContains(t, open, "half_open_owner")

	_, err = b.Call(ctx, downstreamWorks)
	require.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen)

	// OPEN -> HALF_OPEN: this breaker wins the election and probes.
	waitRecovery()
	_, err = b.Call(ctx, downstreamWorks)
	require.NoError(t, err)
	halfOpen := item(t, client, table, key)
	assert.Equal(t, "HALF_OPEN", str(t, halfOpen, "state"))
	assert.Equal(t, openedAt, num(t, halfOpen, "opened_at"))
	assert.Regexp(t, `^\d+#[0-9a-f]{32}$`, str(t, halfOpen, "half_open_owner"))
	assert.Greater(t, num(t, halfOpen, "probe_lease_expiry"), openedAt)
	assert.Equal(t, int64(0), num(t, halfOpen, "failure_count"))

	// HALF_OPEN -> OPEN: a probe fails.
	_, err = b.Call(ctx, downstreamFails)
	require.ErrorIs(t, err, errDownstream)
	reopened := item(t, client, table, key)
	assert.Equal(t, "OPEN", str(t, reopened, "state"))
	assert.GreaterOrEqual(t, num(t, reopened, "opened_at"), openedAt+1)
	assert.NotContains(t, reopened, "half_open_owner")
	assert.NotContains(t, reopened, "probe_lease_expiry")

	// OPEN -> HALF_OPEN -> CLOSED: two probes succeed.
	waitRecovery()
	for range 2 {
		_, err = b.Call(ctx, downstreamWorks)
		require.NoError(t, err)
	}
	closed := item(t, client, table, key)
	assert.Equal(t, "CLOSED", str(t, closed, "state"))
	assert.Equal(t, int64(0), num(t, closed, "failure_count"))
	assert.NotContains(t, closed, "opened_at")
	assert.NotContains(t, closed, "half_open_owner")
	assert.NotContains(t, closed, "probe_lease_expiry")
	assert.Contains(t, closed, "expiration")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{
		"CLOSED>OPEN", "OPEN>HALF_OPEN", "HALF_OPEN>OPEN", "OPEN>HALF_OPEN", "HALF_OPEN>CLOSED",
	}, moves)
}

func TestCircuitBreakerSharedState(t *testing.T) {
	t.Parallel()
	client, table := circuitTable(t, false)
	first := breaker(t, client, table, nil)
	second := breaker(t, client, table, nil)
	ctx := t.Context()

	for range 2 {
		_, err := first.Call(ctx, downstreamFails)
		require.ErrorIs(t, err, errDownstream)
	}
	_, err := second.Call(ctx, downstreamWorks)
	require.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen, "the other environment sees the circuit open")

	waitRecovery()
	_, err = first.Call(ctx, downstreamWorks)
	require.NoError(t, err, "the first environment wins the probe")
	_, err = second.Call(ctx, downstreamWorks)
	openErr, ok := errors.AsType[*circuitbreaker.OpenError](err)
	require.True(t, ok, "the other environment waits for the probe: %v", err)
	assert.Equal(t, circuitbreaker.HalfOpen, openErr.Circuit.State)

	_, err = first.Call(ctx, downstreamWorks)
	require.NoError(t, err)
	got, err := second.Call(ctx, downstreamWorks)
	require.NoError(t, err, "closed for everyone")
	assert.Equal(t, "ok", got)
}

func TestCircuitBreakerSingleProbe(t *testing.T) {
	t.Parallel()
	client, table := circuitTable(t, false)
	environments := []*circuitbreaker.Breaker{
		breaker(t, client, table, nil, circuitbreaker.WithSuccessThreshold(1)),
		breaker(t, client, table, nil, circuitbreaker.WithSuccessThreshold(1)),
	}
	ctx := t.Context()
	for range 2 {
		_, err := environments[0].Call(ctx, downstreamFails)
		require.ErrorIs(t, err, errDownstream)
	}
	waitRecovery()

	release := make(chan struct{})
	var running, peak, ran, rejected atomic.Int32
	probe := func(ctx context.Context) (string, error) {
		ran.Add(1)
		n := running.Add(1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return "ok", ctx.Err()
	}
	const callers = 40
	var wg sync.WaitGroup
	for i := range callers {
		b := environments[i%len(environments)]
		wg.Go(func() {
			_, err := b.Call(ctx, probe)
			if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
				rejected.Add(1)
			}
		})
	}
	require.Eventually(t, func() bool { return rejected.Load() == callers-1 }, 30*time.Second, 10*time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), ran.Load(), "exactly one call probed")
	assert.Equal(t, int32(1), peak.Load())

	got, err := environments[1].Call(ctx, downstreamWorks)
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
}

func TestCircuitBreakerCompositeKey(t *testing.T) {
	t.Parallel()
	client, table := circuitTable(t, true)
	storeOpts := []circuitbreaker.DynamoDBOption{
		circuitbreaker.WithKeyAttr("PK"),
		circuitbreaker.WithSortKeyAttr("SK"),
		circuitbreaker.WithStaticPKValue("CIRCUIT_BREAKER"),
	}
	checkout := breaker(t, client, table, storeOpts)
	refunds := breaker(t, client, table, storeOpts)
	ctx := t.Context()
	for range 2 {
		_, err := checkout.Call(ctx, downstreamFails)
		require.ErrorIs(t, err, errDownstream)
	}
	stored := item(t, client, table, map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "CIRCUIT_BREAKER"},
		"SK": &types.AttributeValueMemberS{Value: "payment-backend"},
	})
	assert.Equal(t, "OPEN", str(t, stored, "state"))
	_, err := refunds.Call(ctx, downstreamWorks)
	require.ErrorIs(t, err, circuitbreaker.ErrCircuitOpen, "functions sharing the partition share the circuit")
}

// TestCircuitBreakerReadsPythonItems seeds the table with the item
// Powertools Python's CircuitBreakerDynamoDBPersistence writes when it trips
// a circuit, and checks that a Go breaker takes the circuit over from it.
func TestCircuitBreakerReadsPythonItems(t *testing.T) {
	t.Parallel()
	client, table := circuitTable(t, false)
	ctx := t.Context()
	openedAt := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	_, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(table),
		Item: map[string]types.AttributeValue{
			"id":            &types.AttributeValueMemberS{Value: "payment-backend"},
			"state":         &types.AttributeValueMemberS{Value: "OPEN"},
			"failure_count": &types.AttributeValueMemberN{Value: "5"},
			"opened_at":     &types.AttributeValueMemberN{Value: openedAt},
			"expiration":    &types.AttributeValueMemberN{Value: "4102444800"},
		},
	})
	require.NoError(t, err)

	b := breaker(t, client, table, nil)
	_, err = b.Call(ctx, downstreamWorks)
	require.NoError(t, err, "the recovery timeout elapsed: the Go breaker probes")
	halfOpen := item(t, client, table, map[string]types.AttributeValue{
		"id": &types.AttributeValueMemberS{Value: "payment-backend"},
	})
	assert.Equal(t, "HALF_OPEN", str(t, halfOpen, "state"))
	assert.Equal(t, openedAt, strconv.FormatInt(num(t, halfOpen, "opened_at"), 10))
}
