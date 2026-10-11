package integration

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: recomputing Powertools' idempotency keys
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/dynamodb"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/redis"
)

// payment is the payload of the idempotent function under test.
type payment struct {
	UserID    string `json:"user_id"`
	ProductID string `json:"product_id"`
	Amount    int    `json:"amount"`
}

type paymentResult struct {
	PaymentID string `json:"payment_id"`
	Message   string `json:"message"`
}

// charger is an idempotent function's body that counts its runs, and can
// fail or hold its first run until released.
type charger struct {
	runs    atomic.Int32
	fail    atomic.Bool
	started chan struct{}
	release chan struct{}
}

var errDeclined = errors.New("card declined")

func (c *charger) charge(_ context.Context, p payment) (paymentResult, error) {
	run := c.runs.Add(1)
	if c.fail.Load() {
		return paymentResult{}, errDeclined
	}
	if c.started != nil && run == 1 {
		close(c.started)
		<-c.release
	}
	return paymentResult{PaymentID: fmt.Sprintf("%s-%s-%d", p.UserID, p.ProductID, run), Message: "success"}, nil
}

// unique names a resource after the test, so that parallel tests never meet.
func unique(t *testing.T) string {
	t.Helper()
	name := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "-")
	return fmt.Sprintf("%s-%06d", strings.Trim(name, "-"), rand.IntN(1_000_000))
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // G401: recomputing Powertools' idempotency keys
	return hex.EncodeToString(sum[:])
}

// invocationContext has the deadline Lambda gives an invocation.
func invocationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// createTable creates an idempotency table, with a sort key when sortKey is
// not empty, and returns its name.
func createTable(t *testing.T, client *awsdynamodb.Client, sortKey string) string {
	t.Helper()
	name := unique(t)
	attributes := []types.AttributeDefinition{{AttributeName: new("id"), AttributeType: types.ScalarAttributeTypeS}}
	schema := []types.KeySchemaElement{{AttributeName: new("id"), KeyType: types.KeyTypeHash}}
	if sortKey != "" {
		attributes = append(attributes, types.AttributeDefinition{AttributeName: new(sortKey), AttributeType: types.ScalarAttributeTypeS})
		schema = append(schema, types.KeySchemaElement{AttributeName: new(sortKey), KeyType: types.KeyTypeRange})
	}
	_, err := client.CreateTable(t.Context(), &awsdynamodb.CreateTableInput{
		TableName:            new(name),
		AttributeDefinitions: attributes,
		KeySchema:            schema,
		BillingMode:          types.BillingModePayPerRequest,
	})
	require.NoError(t, err)
	require.NoError(t, awsdynamodb.NewTableExistsWaiter(client).Wait(t.Context(),
		&awsdynamodb.DescribeTableInput{TableName: new(name)}, time.Minute))
	return name
}

// backend is a store under test, with a way to read what it wrote.
type backend struct {
	store idempotency.Store
	// raw returns what the store holds for key, as a map of attributes.
	raw func(t *testing.T, key string) map[string]string
}

func dynamoDBBackend(t *testing.T, opts ...dynamodb.Option) backend {
	t.Helper()
	client := awsdynamodb.NewFromConfig(suite.aws)
	table := createTable(t, client, "")
	store, err := dynamodb.New(client, table, opts...)
	require.NoError(t, err)
	return backend{store: store, raw: func(t *testing.T, key string) map[string]string {
		t.Helper()
		out, err := client.GetItem(t.Context(), &awsdynamodb.GetItemInput{
			TableName: new(table), Key: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: key}},
		})
		require.NoError(t, err)
		return itemStrings(out.Item)
	}}
}

func itemStrings(item map[string]types.AttributeValue) map[string]string {
	if item == nil {
		return nil
	}
	out := make(map[string]string, len(item))
	for name, value := range item {
		switch v := value.(type) {
		case *types.AttributeValueMemberS:
			out[name] = "S:" + v.Value
		case *types.AttributeValueMemberN:
			out[name] = "N:" + v.Value
		default:
			out[name] = fmt.Sprintf("%T", v)
		}
	}
	return out
}

func valkeyClient(t *testing.T) *goredis.Client {
	t.Helper()
	options, err := goredis.ParseURL(suite.valkeyURL)
	require.NoError(t, err)
	client := goredis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() }) //nolint:errcheck // closing a test client
	return client
}

func valkeyBackend(t *testing.T) backend {
	t.Helper()
	client := valkeyClient(t)
	store, err := redis.New(client)
	require.NoError(t, err)
	return backend{store: store, raw: func(t *testing.T, key string) map[string]string {
		t.Helper()
		value, err := client.Get(t.Context(), key).Result()
		if errors.Is(err, goredis.Nil) {
			return nil
		}
		require.NoError(t, err)
		return map[string]string{"value": value}
	}}
}

// TestIdempotencyStores runs the same scenarios on DynamoDB (Floci) and on
// Valkey.
func TestIdempotencyStores(t *testing.T) {
	t.Parallel()
	backends := map[string]func(*testing.T) backend{
		"DynamoDB": func(t *testing.T) backend { t.Helper(); return dynamoDBBackend(t) },
		"Valkey":   valkeyBackend,
	}
	for name, newBackend := range backends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			t.Run("FirstCallAndReplay", func(t *testing.T) { t.Parallel(); testFirstCallAndReplay(t, newBackend(t)) })
			t.Run("InProgressConflict", func(t *testing.T) { t.Parallel(); testInProgressConflict(t, newBackend(t)) })
			t.Run("Expiry", func(t *testing.T) { t.Parallel(); testExpiry(t, newBackend(t)) })
			t.Run("PayloadValidation", func(t *testing.T) { t.Parallel(); testPayloadValidation(t, newBackend(t)) })
			t.Run("DeletionOnError", func(t *testing.T) { t.Parallel(); testDeletionOnError(t, newBackend(t)) })
			t.Run("TimedOutInProgress", func(t *testing.T) { t.Parallel(); testTimedOutInProgress(t, newBackend(t)) })
			t.Run("ConcurrentInvocations", func(t *testing.T) { t.Parallel(); testConcurrentInvocations(t, newBackend(t)) })
		})
	}
}

func testFirstCallAndReplay(t *testing.T, b backend) {
	t.Helper()
	var c charger
	prefix := unique(t)
	charge := idempotency.Idempotent(c.charge, b.store,
		idempotency.WithKeyPrefix(prefix), idempotency.WithEventKeyJMESPath("[user_id, product_id]"))
	ctx := invocationContext(t)

	first, err := charge(ctx, payment{UserID: "u-1", ProductID: "p-1", Amount: 10})
	require.NoError(t, err)
	replay, err := charge(ctx, payment{UserID: "u-1", ProductID: "p-1", Amount: 10})
	require.NoError(t, err)
	other, err := charge(ctx, payment{UserID: "u-1", ProductID: "p-2", Amount: 10})
	require.NoError(t, err)

	assert.Equal(t, paymentResult{PaymentID: "u-1-p-1-1", Message: "success"}, first)
	assert.Equal(t, first, replay)
	assert.Equal(t, "u-1-p-2-2", other.PaymentID)
	assert.Equal(t, int32(2), c.runs.Load())

	record, err := b.store.GetRecord(ctx, prefix+"#"+md5Hex(`["u-1", "p-1"]`))
	require.NoError(t, err)
	assert.Equal(t, idempotency.StatusCompleted, record.Status)
	assert.JSONEq(t, `{"payment_id":"u-1-p-1-1","message":"success"}`, record.ResponseData)
}

func testInProgressConflict(t *testing.T, b backend) {
	t.Helper()
	c := charger{started: make(chan struct{}), release: make(chan struct{})}
	charge := idempotency.Idempotent(c.charge, b.store,
		idempotency.WithKeyPrefix(unique(t)), idempotency.WithEventKeyJMESPath("user_id"))
	ctx := invocationContext(t)

	var wg sync.WaitGroup
	var first paymentResult
	var firstErr error
	wg.Go(func() { first, firstErr = charge(ctx, payment{UserID: "u-1"}) })
	<-c.started

	_, err := charge(ctx, payment{UserID: "u-1"})
	require.ErrorIs(t, err, idempotency.ErrAlreadyInProgress)

	close(c.release)
	wg.Wait()
	require.NoError(t, firstErr)
	replay, err := charge(ctx, payment{UserID: "u-1"})
	require.NoError(t, err)
	assert.Equal(t, first, replay)
	assert.Equal(t, int32(1), c.runs.Load())
}

func testExpiry(t *testing.T, b backend) {
	t.Helper()
	var c charger
	charge := idempotency.Idempotent(c.charge, b.store,
		idempotency.WithKeyPrefix(unique(t)), idempotency.WithExpiresAfter(time.Second))
	ctx := invocationContext(t)

	_, err := charge(ctx, payment{UserID: "u-1"})
	require.NoError(t, err)
	time.Sleep(2100 * time.Millisecond) // past the second the record expires in
	again, err := charge(ctx, payment{UserID: "u-1"})
	require.NoError(t, err)
	assert.Equal(t, "u-1--2", again.PaymentID, "the expired record does not answer")
}

func testPayloadValidation(t *testing.T, b backend) {
	t.Helper()
	var c charger
	prefix := unique(t)
	options := func(more ...idempotency.Option) []idempotency.Option {
		return append([]idempotency.Option{
			idempotency.WithKeyPrefix(prefix),
			idempotency.WithEventKeyJMESPath("[user_id, product_id]"),
			idempotency.WithPayloadValidationJMESPath("amount"),
		}, more...)
	}
	charge := idempotency.Idempotent(c.charge, b.store, options()...)
	cached := idempotency.Idempotent(c.charge, b.store, options(idempotency.WithLocalCache(true))...)
	ctx := invocationContext(t)

	_, err := charge(ctx, payment{UserID: "u-1", ProductID: "p-1", Amount: 10})
	require.NoError(t, err)
	// The replays read the completed record, validation hash included.
	for _, call := range []func(context.Context, payment) (paymentResult, error){charge, cached, cached} {
		_, err = call(ctx, payment{UserID: "u-1", ProductID: "p-1", Amount: 10})
		require.NoError(t, err, "the same amount replays")
		_, err = call(ctx, payment{UserID: "u-1", ProductID: "p-1", Amount: 11})
		require.ErrorIs(t, err, idempotency.ErrPayloadValidation)
	}
	assert.Equal(t, int32(1), c.runs.Load())
}

func testDeletionOnError(t *testing.T, b backend) {
	t.Helper()
	var c charger
	prefix := unique(t)
	charge := idempotency.Idempotent(c.charge, b.store, idempotency.WithKeyPrefix(prefix), idempotency.WithEventKeyJMESPath("user_id"))
	ctx := invocationContext(t)

	c.fail.Store(true)
	_, err := charge(ctx, payment{UserID: "u-1"})
	require.ErrorIs(t, err, errDeclined)
	_, err = b.store.GetRecord(ctx, prefix+"#"+md5Hex(`"u-1"`))
	require.ErrorIs(t, err, idempotency.ErrItemNotFound, "the in-progress record is gone")

	c.fail.Store(false)
	got, err := charge(ctx, payment{UserID: "u-1"})
	require.NoError(t, err)
	assert.Equal(t, "u-1--2", got.PaymentID, "a retry runs again")
}

func testTimedOutInProgress(t *testing.T, b backend) {
	t.Helper()
	var c charger
	prefix := unique(t)
	charge := idempotency.Idempotent(c.charge, b.store, idempotency.WithKeyPrefix(prefix), idempotency.WithEventKeyJMESPath("user_id"))
	ctx := invocationContext(t)
	now := time.Now()
	// An invocation that timed out left its record behind.
	require.NoError(t, b.store.PutRecord(ctx, idempotency.Record{
		Key:                 prefix + "#" + md5Hex(`"u-1"`),
		Status:              idempotency.StatusInProgress,
		ExpiresAt:           now.Add(time.Hour),
		InProgressExpiresAt: now.Add(-time.Second),
	}, now.Add(-time.Minute)))

	got, err := charge(ctx, payment{UserID: "u-1"})
	require.NoError(t, err)
	assert.Equal(t, "u-1--1", got.PaymentID)
}

func testConcurrentInvocations(t *testing.T, b backend) {
	t.Helper()
	var c charger
	charge := idempotency.Idempotent(c.charge, b.store, idempotency.WithKeyPrefix(unique(t)), idempotency.WithEventKeyJMESPath("user_id"))
	ctx := invocationContext(t)

	var wg sync.WaitGroup
	var succeeded, inProgress atomic.Int32
	for range 16 {
		wg.Go(func() {
			_, err := charge(ctx, payment{UserID: "u-1"})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, idempotency.ErrAlreadyInProgress):
				inProgress.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), c.runs.Load(), "the function ran once")
	assert.Equal(t, int32(16), succeeded.Load()+inProgress.Load())
}

// TestIdempotencyDynamoDBItemsArePythons checks the items on a real table,
// attribute by attribute.
func TestIdempotencyDynamoDBItemsArePythons(t *testing.T) {
	t.Parallel()
	b := dynamoDBBackend(t)
	prefix := unique(t)
	var c charger
	charge := idempotency.Idempotent(c.charge, b.store, idempotency.WithKeyPrefix(prefix),
		idempotency.WithEventKeyJMESPath("user_id"), idempotency.WithPayloadValidationJMESPath("amount"))
	ctx := invocationContext(t)
	deadline, _ := ctx.Deadline()

	_, err := charge(ctx, payment{UserID: "u-1", Amount: 10})
	require.NoError(t, err)
	key := prefix + "#" + md5Hex(`"u-1"`)
	item := b.raw(t, key)
	assert.Equal(t, "S:"+key, item["id"])
	assert.Equal(t, "S:COMPLETED", item["status"])
	assert.Equal(t, "S:"+md5Hex("10"), item["validation"])
	assert.Equal(t, `S:{"payment_id":"u-1--1","message":"success"}`, item["data"])
	assert.Equal(t, fmt.Sprintf("N:%d", deadline.UnixMilli()), item["in_progress_expiration"], "kept by the update, as in Python")
	assert.Regexp(t, `^N:\d{10}$`, item["expiration"])
	assert.Len(t, item, 6)

	// The failed conditional write hands back the record that holds the key.
	err = b.store.PutRecord(ctx, idempotency.Record{
		Key: key, Status: idempotency.StatusInProgress, ExpiresAt: time.Now().Add(time.Hour),
	}, time.Now())
	exists, ok := errors.AsType[*idempotency.ItemAlreadyExistsError](err)
	require.True(t, ok, "%v", err)
	require.NotNil(t, exists.Old)
	assert.Equal(t, key, exists.Old.Key)
	assert.Equal(t, idempotency.StatusCompleted, exists.Old.Status)
}

// TestIdempotencyDynamoDBReplaysPythonRecords writes a record as Powertools
// Python does and replays it from Go.
func TestIdempotencyDynamoDBReplaysPythonRecords(t *testing.T) {
	t.Parallel()
	client := awsdynamodb.NewFromConfig(suite.aws)
	table := createTable(t, client, "")
	store, err := dynamodb.New(client, table)
	require.NoError(t, err)
	ctx := invocationContext(t)
	// A Python function with key_prefix="payments" and
	// event_key_jmespath="user_id" handled {"user_id": 12391}.
	_, err = client.PutItem(ctx, &awsdynamodb.PutItemInput{TableName: new(table), Item: map[string]types.AttributeValue{
		"id":         &types.AttributeValueMemberS{Value: "payments#" + md5Hex("12391")},
		"expiration": &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		"status":     &types.AttributeValueMemberS{Value: "COMPLETED"},
		"data":       &types.AttributeValueMemberS{Value: `{"message": "success", "payment_id": "from-python"}`},
	}})
	require.NoError(t, err)

	type subscription struct {
		UserID int `json:"user_id"`
	}
	var runs atomic.Int32
	subscribe := idempotency.Idempotent(func(context.Context, subscription) (paymentResult, error) {
		runs.Add(1)
		return paymentResult{PaymentID: "from-go"}, nil
	}, store, idempotency.WithKeyPrefix("payments"), idempotency.WithEventKeyJMESPath("user_id"))

	got, err := subscribe(ctx, subscription{UserID: 12391})
	require.NoError(t, err)
	assert.Equal(t, paymentResult{PaymentID: "from-python", Message: "success"}, got)
	assert.Zero(t, runs.Load())
}

func TestIdempotencyDynamoDBCompositeKey(t *testing.T) {
	t.Parallel()
	client := awsdynamodb.NewFromConfig(suite.aws)
	table := createTable(t, client, "sort_key")
	store, err := dynamodb.New(client, table,
		dynamodb.WithSortKeyAttr("sort_key"), dynamodb.WithStaticPKValue("idempotency#orders"))
	require.NoError(t, err)
	var c charger
	prefix := unique(t)
	charge := idempotency.Idempotent(c.charge, store, idempotency.WithKeyPrefix(prefix),
		idempotency.WithEventKeyJMESPath("user_id"), idempotency.WithLocalCache(true))
	ctx := invocationContext(t)

	for _, user := range []string{"u-1", "u-2", "u-1"} {
		_, err = charge(ctx, payment{UserID: user})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(2), c.runs.Load())

	out, err := client.Query(ctx, &awsdynamodb.QueryInput{
		TableName:                 new(table),
		KeyConditionExpression:    new("id = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: "idempotency#orders"}},
	})
	require.NoError(t, err)
	require.Len(t, out.Items, 2, "two records under the static partition key")
	sortKeys := []string{itemStrings(out.Items[0])["sort_key"], itemStrings(out.Items[1])["sort_key"]}
	assert.ElementsMatch(t, []string{"S:" + prefix + "#" + md5Hex(`"u-1"`), "S:" + prefix + "#" + md5Hex(`"u-2"`)}, sortKeys)

	record, err := store.GetRecord(ctx, prefix+"#"+md5Hex(`"u-2"`))
	require.NoError(t, err)
	assert.Equal(t, prefix+"#"+md5Hex(`"u-2"`), record.Key, "the idempotency key, not the partition key")

	// Errors delete the right item too.
	c.fail.Store(true)
	_, err = charge(ctx, payment{UserID: "u-3"})
	require.ErrorIs(t, err, errDeclined)
	_, err = store.GetRecord(ctx, prefix+"#"+md5Hex(`"u-3"`))
	require.ErrorIs(t, err, idempotency.ErrItemNotFound)
}

// TestIdempotencyValkeyValuesArePythons checks the values on a real cache.
func TestIdempotencyValkeyValuesArePythons(t *testing.T) {
	t.Parallel()
	b := valkeyBackend(t)
	prefix := unique(t)
	var c charger
	charge := idempotency.Idempotent(c.charge, b.store, idempotency.WithKeyPrefix(prefix),
		idempotency.WithEventKeyJMESPath("user_id"), idempotency.WithPayloadValidationJMESPath("amount"))
	ctx := invocationContext(t)

	_, err := charge(ctx, payment{UserID: "u-1", Amount: 10})
	require.NoError(t, err)
	key := prefix + "#" + md5Hex(`"u-1"`)
	value := b.raw(t, key)["value"]
	assert.Regexp(t, `^\{"data": "\{\\"payment_id\\":\\"u-1--1\\",\\"message\\":\\"success\\"\}", "status": "COMPLETED", "expiration": \d{10}, "validation": "`+md5Hex("10")+`"\}$`, value)

	ttl, err := valkeyClient(t).TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.InDelta(t, time.Hour.Seconds(), ttl.Seconds(), 5)
}

// TestIdempotencyValkeyReplaysPythonRecords writes a record as Powertools
// Python does and replays it from Go, payload validation included.
func TestIdempotencyValkeyReplaysPythonRecords(t *testing.T) {
	t.Parallel()
	client := valkeyClient(t)
	store, err := redis.New(client)
	require.NoError(t, err)
	ctx := invocationContext(t)
	prefix := unique(t)
	value := fmt.Sprintf(`{"data": "{\"message\": \"success\", \"payment_id\": \"from-python\"}", "status": "COMPLETED", "expiration": %d, "validation": %q}`,
		time.Now().Add(time.Hour).Unix(), md5Hex("10"))
	require.NoError(t, client.Set(ctx, prefix+"#"+md5Hex(`"u-1"`), value, time.Hour).Err())

	var runs atomic.Int32
	charge := idempotency.Idempotent(func(context.Context, payment) (paymentResult, error) {
		runs.Add(1)
		return paymentResult{PaymentID: "from-go"}, nil
	}, store, idempotency.WithKeyPrefix(prefix), idempotency.WithEventKeyJMESPath("user_id"),
		idempotency.WithPayloadValidationJMESPath("amount"))

	got, err := charge(ctx, payment{UserID: "u-1", Amount: 10})
	require.NoError(t, err)
	assert.Equal(t, paymentResult{PaymentID: "from-python", Message: "success"}, got)
	_, err = charge(ctx, payment{UserID: "u-1", Amount: 11})
	require.ErrorIs(t, err, idempotency.ErrPayloadValidation)
	assert.Zero(t, runs.Load())
}

// TestIdempotencyValkeyOrphanRecord leaves an expired record in the cache,
// as one whose TTL Redis has not enforced yet, and checks that the next call
// replaces it.
func TestIdempotencyValkeyOrphanRecord(t *testing.T) {
	t.Parallel()
	client := valkeyClient(t)
	store, err := redis.New(client)
	require.NoError(t, err)
	ctx := invocationContext(t)
	prefix := unique(t)
	key := prefix + "#" + md5Hex(`"u-1"`)
	value := fmt.Sprintf(`{"status": "INPROGRESS", "expiration": %d}`, time.Now().Add(-time.Minute).Unix())
	require.NoError(t, client.Set(ctx, key, value, time.Hour).Err())

	var c charger
	charge := idempotency.Idempotent(c.charge, store, idempotency.WithKeyPrefix(prefix), idempotency.WithEventKeyJMESPath("user_id"))
	_, err = charge(ctx, payment{UserID: "u-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), c.runs.Load())
	exists, err := client.Exists(ctx, key+":lock").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "the orphan lock expires by itself")
}

func TestIdempotencyValkeyFromConnection(t *testing.T) {
	t.Parallel()
	store, err := redis.FromConnection(redis.Connection{URL: suite.valkeyURL})
	require.NoError(t, err)
	var c charger
	charge := idempotency.Idempotent(c.charge, store, idempotency.WithKeyPrefix(unique(t)))
	ctx := invocationContext(t)
	for range 2 {
		_, err := charge(ctx, payment{UserID: "u-1"})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), c.runs.Load())
}
