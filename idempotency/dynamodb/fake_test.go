package dynamodb_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeDynamoDB is an in-memory table that understands the expressions the
// store sends: enough DynamoDB to run the store without AWS. The integration
// tests run the real thing.
type fakeDynamoDB struct {
	mu       sync.Mutex
	keyAttrs []string
	items    map[string]map[string]types.AttributeValue

	puts    []*awsdynamodb.PutItemInput
	gets    []*awsdynamodb.GetItemInput
	updates []*awsdynamodb.UpdateItemInput
	deletes []*awsdynamodb.DeleteItemInput
	err     error
}

func newFakeDynamoDB(keyAttrs ...string) *fakeDynamoDB {
	return &fakeDynamoDB{keyAttrs: keyAttrs, items: make(map[string]map[string]types.AttributeValue)}
}

var errFake = errors.New("fake failure")

func (f *fakeDynamoDB) primaryKey(key map[string]types.AttributeValue) string {
	parts := make([]string, 0, len(f.keyAttrs))
	for _, name := range f.keyAttrs {
		s, _ := key[name].(*types.AttributeValueMemberS) //nolint:errcheck // a missing key part is an empty one
		if s != nil {
			parts = append(parts, s.Value)
		}
	}
	return strings.Join(parts, "\x00")
}

func (f *fakeDynamoDB) item(key string) map[string]types.AttributeValue {
	return f.items[key]
}

func (f *fakeDynamoDB) GetItem(_ context.Context, in *awsdynamodb.GetItemInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, in)
	if f.err != nil {
		return nil, f.err
	}
	return &awsdynamodb.GetItemOutput{Item: maps.Clone(f.item(f.primaryKey(in.Key)))}, nil
}

func (f *fakeDynamoDB) PutItem(_ context.Context, in *awsdynamodb.PutItemInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, in)
	if f.err != nil {
		return nil, f.err
	}
	key := f.primaryKey(in.Item)
	if old := f.item(key); old != nil && !f.putAllowed(old, in.ExpressionAttributeNames, in.ExpressionAttributeValues) {
		failed := &types.ConditionalCheckFailedException{Message: new("The conditional request failed")}
		if in.ReturnValuesOnConditionCheckFailure == types.ReturnValuesOnConditionCheckFailureAllOld {
			failed.Item = maps.Clone(old)
		}
		return nil, failed
	}
	f.items[key] = maps.Clone(in.Item)
	return &awsdynamodb.PutItemOutput{}, nil
}

// putAllowed evaluates the store's put condition on an existing item:
// "#expiry < :now OR (#status = :inprogress AND attribute_exists(#in_progress_expiry) AND #in_progress_expiry < :now_in_millis)".
func (f *fakeDynamoDB) putAllowed(old map[string]types.AttributeValue, names map[string]string, values map[string]types.AttributeValue) bool {
	num := func(av types.AttributeValue) int64 {
		n, _ := av.(*types.AttributeValueMemberN) //nolint:errcheck // a missing number is no number
		if n == nil {
			return 0
		}
		v, _ := strconv.ParseInt(n.Value, 10, 64) //nolint:errcheck // the store writes integers
		return v
	}
	str := func(av types.AttributeValue) string {
		s, _ := av.(*types.AttributeValueMemberS) //nolint:errcheck // a missing string is no string
		if s == nil {
			return ""
		}
		return s.Value
	}
	if num(old[names["#expiry"]]) < num(values[":now"]) {
		return true
	}
	inProgressExpiry, exists := old[names["#in_progress_expiry"]]
	return str(old[names["#status"]]) == str(values[":inprogress"]) && exists && num(inProgressExpiry) < num(values[":now_in_millis"])
}

func (f *fakeDynamoDB) UpdateItem(_ context.Context, in *awsdynamodb.UpdateItemInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, in)
	if f.err != nil {
		return nil, f.err
	}
	key := f.primaryKey(in.Key)
	item := f.item(key)
	if item == nil {
		item = maps.Clone(in.Key)
		f.items[key] = item
	}
	for assignment := range strings.SplitSeq(strings.TrimPrefix(*in.UpdateExpression, "SET "), ", ") {
		name, value, _ := strings.Cut(assignment, " = ")
		item[in.ExpressionAttributeNames[name]] = in.ExpressionAttributeValues[value]
	}
	return &awsdynamodb.UpdateItemOutput{}, nil
}

func (f *fakeDynamoDB) DeleteItem(_ context.Context, in *awsdynamodb.DeleteItemInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.DeleteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, in)
	if f.err != nil {
		return nil, f.err
	}
	delete(f.items, f.primaryKey(in.Key))
	return &awsdynamodb.DeleteItemOutput{}, nil
}

// onlyItem returns the table's single item.
func (f *fakeDynamoDB) onlyItem() map[string]types.AttributeValue {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) != 1 {
		return nil
	}
	return f.items[slices.Collect(maps.Keys(f.items))[0]]
}
