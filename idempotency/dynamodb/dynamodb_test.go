package dynamodb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/dynamodb"
)

var now = time.Date(2026, 10, 10, 12, 0, 0, 500_000_000, time.UTC)

func s(v string) *types.AttributeValueMemberS { return &types.AttributeValueMemberS{Value: v} }
func n(v string) *types.AttributeValueMemberN { return &types.AttributeValueMemberN{Value: v} }

// inProgress is the record of an invocation with a 30 seconds deadline.
func inProgress() idempotency.Record {
	return idempotency.Record{
		Key:                 "fn#h",
		Status:              idempotency.StatusInProgress,
		ExpiresAt:           time.Unix(now.Unix()+3600, 0),
		InProgressExpiresAt: now.Add(30 * time.Second),
		PayloadHash:         "abc",
	}
}

func newStore(t *testing.T, client dynamodb.Client, opts ...dynamodb.Option) *dynamodb.Store {
	t.Helper()
	store, err := dynamodb.New(client, "idempotency", opts...)
	require.NoError(t, err)
	return store
}

// TestPutItemIsPythons checks the request against the one Powertools Python
// sends, attribute for attribute.
func TestPutItemIsPythons(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	store := newStore(t, fake)

	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now))

	require.Len(t, fake.puts, 1)
	in := fake.puts[0]
	assert.Equal(t, "idempotency", *in.TableName)
	assert.Equal(t, map[string]types.AttributeValue{
		"id":                     s("fn#h"),
		"expiration":             n("1791637200"),
		"in_progress_expiration": n("1791633630500"),
		"status":                 s("INPROGRESS"),
		"validation":             s("abc"),
	}, in.Item)
	assert.Equal(t, "attribute_not_exists(#id) OR #expiry < :now OR "+
		"(#status = :inprogress AND attribute_exists(#in_progress_expiry) AND #in_progress_expiry < :now_in_millis)",
		*in.ConditionExpression)
	assert.Equal(t, map[string]string{
		"#id": "id", "#expiry": "expiration", "#in_progress_expiry": "in_progress_expiration", "#status": "status",
	}, in.ExpressionAttributeNames)
	assert.Equal(t, map[string]types.AttributeValue{
		":now": n("1791633600"), ":now_in_millis": n("1791633600500"), ":inprogress": s("INPROGRESS"),
	}, in.ExpressionAttributeValues)
	assert.Equal(t, types.ReturnValuesOnConditionCheckFailureAllOld, in.ReturnValuesOnConditionCheckFailure)
}

func TestPutItemLeavesOutUnknownAttributes(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	store := newStore(t, fake)
	record := inProgress()
	record.InProgressExpiresAt, record.PayloadHash = time.Time{}, ""

	require.NoError(t, store.PutRecord(t.Context(), record, now))
	assert.Equal(t, map[string]types.AttributeValue{
		"id": s("fn#h"), "expiration": n("1791637200"), "status": s("INPROGRESS"),
	}, fake.puts[0].Item)
}

func TestUpdateItemIsPythons(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	store := newStore(t, fake)
	record := idempotency.Record{
		Key: "fn#h", Status: idempotency.StatusCompleted, ExpiresAt: time.Unix(1791637200, 0),
		ResponseData: `{"ok":true}`, PayloadHash: "abc",
	}

	require.NoError(t, store.UpdateRecord(t.Context(), record))
	record.PayloadHash = ""
	require.NoError(t, store.UpdateRecord(t.Context(), record))

	withValidation, without := fake.updates[0], fake.updates[1]
	assert.Equal(t, map[string]types.AttributeValue{"id": s("fn#h")}, withValidation.Key)
	assert.Equal(t, "SET #response_data = :response_data, #expiry = :expiry, #status = :status, #validation_key = :validation_key",
		*withValidation.UpdateExpression)
	assert.Equal(t, map[string]string{
		"#expiry": "expiration", "#response_data": "data", "#status": "status", "#validation_key": "validation",
	}, withValidation.ExpressionAttributeNames)
	assert.Equal(t, map[string]types.AttributeValue{
		":expiry": n("1791637200"), ":response_data": s(`{"ok":true}`), ":status": s("COMPLETED"), ":validation_key": s("abc"),
	}, withValidation.ExpressionAttributeValues)
	assert.Equal(t, "SET #response_data = :response_data, #expiry = :expiry, #status = :status", *without.UpdateExpression)
	assert.NotContains(t, without.ExpressionAttributeNames, "#validation_key")
}

func TestGetRecord(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	store := newStore(t, fake)

	_, err := store.GetRecord(t.Context(), "fn#h")
	require.ErrorIs(t, err, idempotency.ErrItemNotFound)

	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now))
	got, err := store.GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.True(t, *fake.gets[0].ConsistentRead)
	assert.Equal(t, "fn#h", got.Key)
	assert.Equal(t, idempotency.StatusInProgress, got.Status)
	assert.Equal(t, now.Unix()+3600, got.ExpiresAt.Unix())
	assert.Equal(t, now.Add(30*time.Second).UnixMilli(), got.InProgressExpiresAt.UnixMilli())
	assert.Equal(t, "abc", got.PayloadHash)
}

func TestConditionFailureCarriesTheRecord(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	store := newStore(t, fake)
	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now))

	err := store.PutRecord(t.Context(), inProgress(), now.Add(time.Second))
	exists, ok := errors.AsType[*idempotency.ItemAlreadyExistsError](err)
	require.True(t, ok, "%v", err)
	require.NotNil(t, exists.Old)
	assert.Equal(t, "fn#h", exists.Old.Key)
	assert.Equal(t, idempotency.StatusInProgress, exists.Old.Status)

	// Once the invocation's deadline passes, the record gives way.
	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now.Add(31*time.Second)))
}

// conditionFailedWithoutItem answers puts as DynamoDB does when the request
// does not ask for the old item.
type conditionFailedWithoutItem struct{ *fakeDynamoDB }

func (c conditionFailedWithoutItem) PutItem(context.Context, *awsdynamodb.PutItemInput, ...func(*awsdynamodb.Options)) (*awsdynamodb.PutItemOutput, error) {
	return nil, &types.ConditionalCheckFailedException{}
}

func TestConditionFailureWithoutTheRecord(t *testing.T) {
	t.Parallel()
	store := newStore(t, conditionFailedWithoutItem{newFakeDynamoDB("id")})
	err := store.PutRecord(t.Context(), inProgress(), now)
	require.ErrorIs(t, err, idempotency.ErrItemAlreadyExists)
	exists, ok := errors.AsType[*idempotency.ItemAlreadyExistsError](err)
	require.True(t, ok)
	assert.Nil(t, exists.Old)
}

func TestCompositeKey(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id", "sort_key")
	store := newStore(t, fake, dynamodb.WithSortKeyAttr("sort_key"), dynamodb.WithStaticPKValue("idempotency#orders"))

	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now))
	assert.Equal(t, s("idempotency#orders"), fake.puts[0].Item["id"])
	assert.Equal(t, s("fn#h"), fake.puts[0].Item["sort_key"])
	assert.Equal(t, "id", fake.puts[0].ExpressionAttributeNames["#id"], "the existence check is on the partition key")

	// Records read back keep their idempotency key, not the static
	// partition key (Python's bug).
	got, err := store.GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, "fn#h", got.Key)
	err = store.PutRecord(t.Context(), inProgress(), now)
	exists, ok := errors.AsType[*idempotency.ItemAlreadyExistsError](err)
	require.True(t, ok)
	assert.Equal(t, "fn#h", exists.Old.Key)

	require.NoError(t, store.DeleteRecord(t.Context(), "fn#h"))
	assert.Equal(t, map[string]types.AttributeValue{"id": s("idempotency#orders"), "sort_key": s("fn#h")}, fake.deletes[0].Key)
	assert.Nil(t, fake.onlyItem())
}

func TestDefaultStaticPKValue(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-fn")
	fake := newFakeDynamoDB("id", "sk")
	store := newStore(t, fake, dynamodb.WithSortKeyAttr("sk"))
	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now))
	assert.Equal(t, s("idempotency#orders-fn"), fake.puts[0].Item["id"])
}

func TestCustomAttributeNames(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("pk")
	store := newStore(t, fake,
		dynamodb.WithKeyAttr("pk"),
		dynamodb.WithExpiryAttr("ttl"),
		dynamodb.WithInProgressExpiryAttr("lock_until"),
		dynamodb.WithStatusAttr("state"),
		dynamodb.WithDataAttr("result"),
		dynamodb.WithValidationKeyAttr("hash"),
	)
	require.NoError(t, store.PutRecord(t.Context(), inProgress(), now))
	record := inProgress()
	record.Status, record.ResponseData = idempotency.StatusCompleted, `"done"`
	require.NoError(t, store.UpdateRecord(t.Context(), record))

	assert.Equal(t, map[string]types.AttributeValue{
		"pk": s("fn#h"), "ttl": n("1791637200"), "lock_until": n("1791633630500"),
		"state": s("COMPLETED"), "result": s(`"done"`), "hash": s("abc"),
	}, fake.onlyItem())
	got, err := store.GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, `"done"`, got.ResponseData)
	assert.Equal(t, idempotency.StatusCompleted, got.Status)
}

// TestReadsPythonItems reads an item as Powertools Python writes it.
func TestReadsPythonItems(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	fake.items["fn#h"] = map[string]types.AttributeValue{
		"id": s("fn#h"), "expiration": n("1636549553"), "status": s("COMPLETED"),
		"data": s(`{"message": "success", "user_id": 12391}`), "in_progress_expiration": n("1636545953123"),
	}
	got, err := newStore(t, fake).GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, idempotency.Record{
		Key: "fn#h", Status: idempotency.StatusCompleted, ExpiresAt: time.Unix(1636549553, 0),
		InProgressExpiresAt: time.UnixMilli(1636545953123), ResponseData: `{"message": "success", "user_id": 12391}`,
	}, *got)
}

func TestMalformedItems(t *testing.T) {
	t.Parallel()
	valid := func() map[string]types.AttributeValue {
		return map[string]types.AttributeValue{"id": s("fn#h"), "expiration": n("1"), "status": s("COMPLETED")}
	}
	cases := map[string]func(map[string]types.AttributeValue){
		"no status":            func(i map[string]types.AttributeValue) { delete(i, "status") },
		"no expiry":            func(i map[string]types.AttributeValue) { delete(i, "expiration") },
		"status not a string":  func(i map[string]types.AttributeValue) { i["status"] = n("1") },
		"expiry not a number":  func(i map[string]types.AttributeValue) { i["expiration"] = s("1") },
		"expiry not numeric":   func(i map[string]types.AttributeValue) { i["expiration"] = n("soon") },
		"data not a string":    func(i map[string]types.AttributeValue) { i["data"] = n("1") },
		"in progress expiry S": func(i map[string]types.AttributeValue) { i["in_progress_expiration"] = s("1") },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeDynamoDB("id")
			item := valid()
			corrupt(item)
			fake.items["fn#h"] = item
			_, err := newStore(t, fake).GetRecord(t.Context(), "fn#h")
			require.ErrorIs(t, err, idempotency.ErrMalformedRecord)
		})
	}
	fake := newFakeDynamoDB("id")
	item := valid()
	item["expiration"] = n("1.5e3")
	fake.items["fn#h"] = item
	got, err := newStore(t, fake).GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, int64(1500), got.ExpiresAt.Unix())
}

func TestClientErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	fake.err = errFake
	store := newStore(t, fake)
	require.ErrorIs(t, store.PutRecord(t.Context(), inProgress(), now), errFake)
	_, err := store.GetRecord(t.Context(), "fn#h")
	require.ErrorIs(t, err, errFake)
	require.ErrorIs(t, store.UpdateRecord(t.Context(), inProgress()), errFake)
	require.ErrorIs(t, store.DeleteRecord(t.Context(), "fn#h"), errFake)
}

func TestNewRejectsInvalidSettings(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	_, err := dynamodb.New(nil, "t")
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)
	_, err = dynamodb.New(fake, "")
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)
	_, err = dynamodb.New(fake, "t", dynamodb.WithKeyAttr(""))
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)
	_, err = dynamodb.New(fake, "t", dynamodb.WithSortKeyAttr("id"))
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)

	store, err := dynamodb.FromConfig(aws.Config{Region: "eu-west-1"}, "t")
	require.NoError(t, err)
	assert.NotNil(t, store)
}

// TestIdempotentOnDynamoDB runs an idempotent function on the store.
func TestIdempotentOnDynamoDB(t *testing.T) {
	t.Parallel()
	fake := newFakeDynamoDB("id")
	runs := 0
	process := idempotency.Idempotent(func(_ context.Context, id string) (string, error) {
		runs++
		return "processed " + id, nil
	}, newStore(t, fake), idempotency.WithKeyPrefix("p"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	for range 2 {
		got, err := process(ctx, "o-1")
		require.NoError(t, err)
		assert.Equal(t, "processed o-1", got)
	}
	assert.Equal(t, 1, runs)
	assert.Len(t, fake.puts, 2)
	assert.Empty(t, fake.gets, "the failed write returned the record")
}
