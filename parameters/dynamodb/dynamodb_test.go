package dynamodb_test

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/dynamodb"
)

type item = map[string]types.AttributeValue

// fakeTable is a table keyed by (partition key, sort key) that serves one
// item per page of a query.
type fakeTable struct {
	mu      sync.Mutex
	key     string
	sort    string
	items   []item
	gets    []awsdynamodb.GetItemInput
	queries []awsdynamodb.QueryInput
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func stringOf(av types.AttributeValue) string {
	if v, ok := av.(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func (f *fakeTable) GetItem(_ context.Context, in *awsdynamodb.GetItemInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, *in)
	want := stringOf(in.Key[f.key])
	for _, it := range f.items {
		if pk, ok := it[f.key].(*types.AttributeValueMemberS); ok && pk.Value == want {
			if _, hasSort := it[f.sort]; !hasSort {
				return &awsdynamodb.GetItemOutput{Item: it}, nil
			}
		}
	}
	return &awsdynamodb.GetItemOutput{}, nil
}

func (f *fakeTable) Query(_ context.Context, in *awsdynamodb.QueryInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.QueryOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, *in)
	want := stringOf(in.ExpressionAttributeValues[":key"])
	var matching []item
	for _, it := range f.items {
		if pk, ok := it[in.ExpressionAttributeNames["#key"]].(*types.AttributeValueMemberS); ok && pk.Value == want {
			if _, hasSort := it[f.sort]; hasSort {
				matching = append(matching, it)
			}
		}
	}
	start := 0
	if lek, ok := in.ExclusiveStartKey["page"].(*types.AttributeValueMemberN); ok {
		start, _ = strconv.Atoi(lek.Value) //nolint:errcheck // the fake wrote this number itself
	}
	out := &awsdynamodb.QueryOutput{}
	if start < len(matching) {
		out.Items = matching[start : start+1]
	}
	if start+1 < len(matching) {
		out.LastEvaluatedKey = item{"page": &types.AttributeValueMemberN{Value: strconv.Itoa(start + 1)}}
	}
	return out, nil
}

func newTable() *fakeTable {
	return &fakeTable{key: "id", sort: "sk", items: []item{
		{"id": s("my-param"), "value": s("my-value")},
		{"id": s("number"), "value": &types.AttributeValueMemberN{Value: "12345678901234567890"}},
		{"id": s("blob"), "value": &types.AttributeValueMemberB{Value: []byte{1, 2}}},
		{"id": s("doc"), "value": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"name":    s("x"),
			"enabled": &types.AttributeValueMemberBOOL{Value: true},
			"nothing": &types.AttributeValueMemberNULL{Value: true},
			"tags":    &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
			"sizes":   &types.AttributeValueMemberNS{Value: []string{"1", "2.5"}},
			"blobs":   &types.AttributeValueMemberBS{Value: [][]byte{{1}}},
			"list":    &types.AttributeValueMemberL{Value: []types.AttributeValue{s("y"), &types.AttributeValueMemberN{Value: "3"}}},
		}}},
		{"id": s("no-value")},
		{"id": s("my-hash-key"), "sk": s("param-a"), "value": s("my-value-a")},
		{"id": s("my-hash-key"), "sk": s("param-b"), "value": s("my-value-b")},
		{"id": s("my-hash-key"), "sk": s("param-c.json"), "value": s(`{"c":true}`)},
		{"id": s("broken"), "sk": s("no-value")},
	}}
}

func TestGet(t *testing.T) {
	t.Parallel()
	table := newTable()
	p := dynamodb.New(table, "parameters")
	ctx := t.Context()

	v, err := p.Get(ctx, "my-param", dynamodb.WithConsistentRead(true))
	require.NoError(t, err)
	assert.Equal(t, "my-value", v)
	assert.Equal(t, "parameters", aws.ToString(table.gets[0].TableName))
	assert.True(t, aws.ToBool(table.gets[0].ConsistentRead))

	n, err := p.Get(ctx, "number")
	require.NoError(t, err)
	assert.Equal(t, "12345678901234567890", n, "numbers keep every digit")

	b, err := parameters.Get[[]byte](ctx, p, "blob")
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2}, b)

	doc, err := parameters.Get[map[string]any](ctx, p, "doc", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"name":    "x",
		"enabled": true,
		"nothing": nil,
		"tags":    []any{"a", "b"},
		"sizes":   []any{float64(1), 2.5},
		"blobs":   []any{"AQ=="},
		"list":    []any{"y", float64(3)},
	}, doc)

	_, err = p.Get(ctx, "missing")
	require.ErrorIs(t, err, parameters.ErrNotFound)
	require.ErrorIs(t, err, parameters.ErrGetParameter)

	_, err = p.Get(ctx, "no-value")
	require.ErrorIs(t, err, parameters.ErrNotFound)
}

func TestGetMultiple(t *testing.T) {
	t.Parallel()
	table := newTable()
	p := dynamodb.New(table, "parameters")
	values, err := parameters.GetMultiple[any](t.Context(), p, "my-hash-key", parameters.WithTransform(parameters.TransformAuto))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"param-a":      "my-value-a",
		"param-b":      "my-value-b",
		"param-c.json": map[string]any{"c": true},
	}, values)
	assert.Len(t, table.queries, 3, "every page is read")
	assert.Equal(t, "#key = :key", aws.ToString(table.queries[0].KeyConditionExpression))

	_, err = p.GetMultiple(t.Context(), "broken")
	require.ErrorIs(t, err, parameters.ErrNotFound, "an item without a value attribute fails the read, as Python's KeyError")
}

func TestCustomAttributes(t *testing.T) {
	t.Parallel()
	table := &fakeTable{key: "pk", sort: "name", items: []item{
		{"pk": s("single"), "val": s("one")},
		{"pk": s("path"), "name": s("x"), "val": s("1")},
		{"pk": s("path"), "name": &types.AttributeValueMemberN{Value: "2"}, "val": s("2")},
	}}
	p := dynamodb.New(table, "custom",
		dynamodb.WithKeyAttribute("pk"),
		dynamodb.WithSortAttribute("name"),
		dynamodb.WithValueAttribute("val"),
	)
	assert.Equal(t, "custom", p.Table())
	v, err := p.Get(t.Context(), "single")
	require.NoError(t, err)
	assert.Equal(t, "one", v)
	values, err := p.GetMultiple(t.Context(), "path")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"x": "1", "2": "2"}, values)
	assert.True(t, slices.ContainsFunc(table.queries, func(q awsdynamodb.QueryInput) bool { return q.ExpressionAttributeNames["#key"] == "pk" }))
}
