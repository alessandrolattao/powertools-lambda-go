package circuitbreaker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cb "github.com/alessandrolattao/powertools-lambda-go/circuitbreaker"
)

// fakeDynamoDB records the requests and answers with what the test set.
type fakeDynamoDB struct {
	get    *dynamodb.GetItemInput
	put    *dynamodb.PutItemInput
	update *dynamodb.UpdateItemInput

	item map[string]types.AttributeValue
	err  error
}

func (f *fakeDynamoDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.get = in
	if f.err != nil {
		return nil, f.err
	}
	return &dynamodb.GetItemOutput{Item: f.item}, nil
}

func (f *fakeDynamoDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.put = in
	if f.err != nil {
		return nil, f.err
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamoDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.update = in
	if f.err != nil {
		return nil, f.err
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }

func newStore(t *testing.T, client cb.DynamoDBClient, opts ...cb.DynamoDBOption) *cb.DynamoDBStore {
	t.Helper()
	store, err := cb.NewDynamoDBStore(client, "circuits", opts...)
	require.NoError(t, err)
	return store
}

func TestDynamoDBStoreConfig(t *testing.T) {
	t.Parallel()
	client := &fakeDynamoDB{}
	cases := []struct {
		name   string
		client cb.DynamoDBClient
		table  string
		opts   []cb.DynamoDBOption
	}{
		{name: "no client", table: "t"},
		{name: "no table", client: client},
		{name: "no key", client: client, table: "t", opts: []cb.DynamoDBOption{cb.WithKeyAttr("")}},
		{name: "same keys", client: client, table: "t", opts: []cb.DynamoDBOption{cb.WithSortKeyAttr("id")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := cb.NewDynamoDBStore(tc.client, tc.table, tc.opts...)
			require.ErrorIs(t, err, cb.ErrInvalidConfig)
		})
	}
}

// The items below are what Powertools Python's
// CircuitBreakerDynamoDBPersistence writes and reads.
func TestDynamoDBStoreGet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		item    map[string]types.AttributeValue
		want    cb.Record
		found   bool
		invalid bool
	}{
		{name: "missing"},
		{
			name: "open",
			item: map[string]types.AttributeValue{
				"id": s("payments"), "state": s("OPEN"), "failure_count": n("5"),
				"opened_at": n("1699999999"), "expiration": n("1700003629"),
			},
			want: cb.Record{
				Name: "payments", State: cb.Open, FailureCount: 5,
				OpenedAt: time.Unix(1699999999, 0), Expiration: time.Unix(1700003629, 0),
			},
			found: true,
		},
		{
			name: "half open",
			item: map[string]types.AttributeValue{
				"id": s("payments"), "state": s("HALF_OPEN"), "failure_count": n("0"),
				"opened_at": n("0"), "half_open_owner": s("42#9f3c"), "probe_lease_expiry": n("1700000030.75"),
			},
			want: cb.Record{
				Name: "payments", State: cb.HalfOpen, OpenedAt: time.Unix(0, 0),
				HalfOpenOwner: "42#9f3c", ProbeLeaseExpiry: time.Unix(1700000030, 0),
			},
			found: true,
		},
		{
			name:  "closed without failure count",
			item:  map[string]types.AttributeValue{"id": s("payments"), "state": s("CLOSED")},
			want:  cb.Record{Name: "payments", State: cb.Closed},
			found: true,
		},
		{name: "unknown state", item: map[string]types.AttributeValue{"state": s("AJAR")}, invalid: true},
		{name: "no state", item: map[string]types.AttributeValue{"id": s("payments")}, invalid: true},
		{name: "state not a string", item: map[string]types.AttributeValue{"state": n("1")}, invalid: true},
		{
			name:    "count not a number",
			item:    map[string]types.AttributeValue{"state": s("OPEN"), "failure_count": s("5")},
			invalid: true,
		},
		{
			name:    "garbled time",
			item:    map[string]types.AttributeValue{"state": s("OPEN"), "opened_at": n("NaN")},
			invalid: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeDynamoDB{item: tc.item}
			record, found, err := newStore(t, client).Get(t.Context(), "payments")
			assert.Equal(t, &dynamodb.GetItemInput{
				TableName:      aws.String("circuits"),
				Key:            map[string]types.AttributeValue{"id": s("payments")},
				ConsistentRead: aws.Bool(false),
			}, client.get)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.found, found)
			assert.Equal(t, tc.want, record)
		})
	}
}

func TestDynamoDBStoreWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := &fakeDynamoDB{}
	store := newStore(t, client)

	require.NoError(t, store.Put(ctx, cb.Record{
		Name: "payments", State: cb.Open, FailureCount: 5,
		OpenedAt: time.Unix(1699999999, 0), Expiration: time.Unix(1700003629, 0),
	}))
	assert.Equal(t, &dynamodb.PutItemInput{
		TableName: aws.String("circuits"),
		Item: map[string]types.AttributeValue{
			"id": s("payments"), "state": s("OPEN"), "failure_count": n("5"),
			"opened_at": n("1699999999"), "expiration": n("1700003629"),
		},
	}, client.put)

	won, err := store.AcquireProbe(ctx, cb.Record{
		Name: "payments", State: cb.HalfOpen, OpenedAt: time.Unix(1699999999, 0),
		HalfOpenOwner: "42#9f3c", ProbeLeaseExpiry: time.Unix(1700000060, 0), Expiration: time.Unix(1700003660, 0),
	}, time.Unix(1700000030, 0))
	require.NoError(t, err)
	assert.True(t, won)
	assert.Equal(t, &dynamodb.PutItemInput{
		TableName: aws.String("circuits"),
		Item: map[string]types.AttributeValue{
			"id": s("payments"), "state": s("HALF_OPEN"), "failure_count": n("0"),
			"opened_at": n("1699999999"), "half_open_owner": s("42#9f3c"),
			"probe_lease_expiry": n("1700000060"), "expiration": n("1700003660"),
		},
		ConditionExpression: aws.String("(#state = :open AND attribute_not_exists(#half_open_owner) AND " +
			"#opened_at = :expected_opened_at) OR (#state = :half_open AND #probe_lease_expiry <= :now)"),
		ExpressionAttributeNames: map[string]string{
			"#state": "state", "#half_open_owner": "half_open_owner",
			"#probe_lease_expiry": "probe_lease_expiry", "#opened_at": "opened_at",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":open": s("OPEN"), ":half_open": s("HALF_OPEN"),
			":now": n("1700000030"), ":expected_opened_at": n("1699999999"),
		},
	}, client.put)

	require.NoError(t, store.Update(ctx, cb.Record{
		Name: "payments", State: cb.Closed, Expiration: time.Unix(1700003660, 0),
	}))
	assert.Equal(t, &dynamodb.UpdateItemInput{
		TableName: aws.String("circuits"),
		Key:       map[string]types.AttributeValue{"id": s("payments")},
		UpdateExpression: aws.String("SET #state = :state, #failure_count = :failure_count, #expiration = :expiration " +
			"REMOVE #opened_at, #half_open_owner, #probe_lease_expiry"),
		ExpressionAttributeNames: map[string]string{
			"#state": "state", "#failure_count": "failure_count", "#expiration": "expiration",
			"#opened_at": "opened_at", "#half_open_owner": "half_open_owner", "#probe_lease_expiry": "probe_lease_expiry",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":state": s("CLOSED"), ":failure_count": n("0"), ":expiration": n("1700003660"),
		},
	}, client.update)

	require.NoError(t, store.Update(ctx, cb.Record{
		Name: "payments", State: cb.Open, OpenedAt: time.Unix(1700000045, 0), Expiration: time.Unix(1700003675, 0),
	}))
	assert.Equal(t, "SET #state = :state, #failure_count = :failure_count, #expiration = :expiration, "+
		"#opened_at = :opened_at REMOVE #half_open_owner, #probe_lease_expiry", *client.update.UpdateExpression)
	assert.Equal(t, map[string]types.AttributeValue{
		":state": s("OPEN"), ":failure_count": n("0"), ":expiration": n("1700003675"), ":opened_at": n("1700000045"),
	}, client.update.ExpressionAttributeValues)
}

func TestDynamoDBStoreLostElection(t *testing.T) {
	t.Parallel()
	client := &fakeDynamoDB{err: &types.ConditionalCheckFailedException{Message: aws.String("The conditional request failed")}}
	won, err := newStore(t, client).AcquireProbe(t.Context(), cb.Record{Name: "c", State: cb.HalfOpen}, time.Now())
	require.NoError(t, err)
	assert.False(t, won)

	errThrottled := errors.New("throttled")
	client.err = errThrottled
	_, err = newStore(t, client).AcquireProbe(t.Context(), cb.Record{Name: "c", State: cb.HalfOpen}, time.Now())
	require.ErrorIs(t, err, errThrottled)
	require.ErrorIs(t, newStore(t, client).Put(t.Context(), cb.Record{Name: "c"}), errThrottled)
	require.ErrorIs(t, newStore(t, client).Update(t.Context(), cb.Record{Name: "c"}), errThrottled)
	_, _, err = newStore(t, client).Get(t.Context(), "c")
	require.ErrorIs(t, err, errThrottled)
}

func TestDynamoDBStoreCustomAttributes(t *testing.T) {
	t.Parallel()
	client := &fakeDynamoDB{}
	store := newStore(t, client,
		cb.WithKeyAttr("PK"), cb.WithSortKeyAttr("SK"), cb.WithStaticPKValue("CIRCUIT_BREAKER"),
		cb.WithStateAttr("st"), cb.WithFailureCountAttr("fc"), cb.WithOpenedAtAttr("oa"),
		cb.WithHalfOpenOwnerAttr("ow"), cb.WithProbeLeaseExpiryAttr("le"), cb.WithExpiryAttr("ttl"))
	_, err := store.AcquireProbe(t.Context(), cb.Record{
		Name: "payments", State: cb.HalfOpen, OpenedAt: time.Unix(1, 0),
		HalfOpenOwner: "me", ProbeLeaseExpiry: time.Unix(2, 0), Expiration: time.Unix(3, 0),
	}, time.Unix(1, 0))
	require.NoError(t, err)
	assert.Equal(t, map[string]types.AttributeValue{
		"PK": s("CIRCUIT_BREAKER"), "SK": s("payments"), "st": s("HALF_OPEN"), "fc": n("0"),
		"oa": n("1"), "ow": s("me"), "le": n("2"), "ttl": n("3"),
	}, client.put.Item)
	assert.Equal(t, map[string]string{
		"#state": "st", "#half_open_owner": "ow", "#probe_lease_expiry": "le", "#opened_at": "oa",
	}, client.put.ExpressionAttributeNames)

	client.item = map[string]types.AttributeValue{"PK": s("CIRCUIT_BREAKER"), "SK": s("payments"), "st": s("OPEN"), "fc": n("3")}
	record, found, err := store.Get(t.Context(), "payments")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, cb.Record{Name: "payments", State: cb.Open, FailureCount: 3}, record)
	assert.Equal(t, map[string]types.AttributeValue{"PK": s("CIRCUIT_BREAKER"), "SK": s("payments")}, client.get.Key)
}

func TestDynamoDBStoreDefaultPartition(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "checkout")
	client := &fakeDynamoDB{}
	_, _, err := newStore(t, client, cb.WithSortKeyAttr("sk")).Get(t.Context(), "payments")
	require.NoError(t, err)
	assert.Equal(t, map[string]types.AttributeValue{"id": s("circuit_breaker#checkout"), "sk": s("payments")}, client.get.Key)

	_, _, err = newStore(t, client, cb.WithStaticPKValue("shared")).Get(t.Context(), "payments")
	require.NoError(t, err)
	assert.Equal(t, map[string]types.AttributeValue{"id": s("payments")}, client.get.Key,
		"a static partition value without a sort key is ignored")
}

func TestDynamoDBStoreFromConfig(t *testing.T) {
	t.Parallel()
	store, err := cb.NewDynamoDBStoreFromConfig(aws.Config{Region: "eu-south-1"}, "circuits")
	require.NoError(t, err)
	assert.NotNil(t, store)
}
