package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

// Client is the part of the DynamoDB client the provider calls;
// *dynamodb.Client satisfies it.
type Client interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// The attribute names of Python's table schema.
const (
	DefaultKeyAttribute   = "id"
	DefaultSortAttribute  = "sk"
	DefaultValueAttribute = "value"
)

// Provider reads parameters from a DynamoDB table. It is safe for
// concurrent use: create it once and share it across invocations.
//
// A single value is the item whose partition key is the name. The values
// under a path are the items whose partition key is the path, keyed by
// their sort key. The value is the value attribute: a string is returned as
// is, a binary as its bytes, a number as its text, and any other type
// (map, list, set, boolean, null) as its JSON encoding, which
// parameters.TransformJSON decodes.
type Provider struct {
	*parameters.Provider

	table string
}

// Option configures the table schema of a Provider.
type Option func(*store)

// WithKeyAttribute names the partition key attribute, "id" by default.
func WithKeyAttribute(name string) Option {
	return func(s *store) { s.keyAttr = name }
}

// WithSortAttribute names the sort key attribute, "sk" by default.
func WithSortAttribute(name string) Option {
	return func(s *store) { s.sortAttr = name }
}

// WithValueAttribute names the attribute that holds the value, "value" by
// default.
func WithValueAttribute(name string) Option {
	return func(s *store) { s.valueAttr = name }
}

// New returns a provider that reads table through client.
func New(client Client, table string, opts ...Option) *Provider {
	s := &store{
		client:    client,
		table:     table,
		keyAttr:   DefaultKeyAttribute,
		sortAttr:  DefaultSortAttribute,
		valueAttr: DefaultValueAttribute,
	}
	for _, opt := range opts {
		opt(s)
	}
	return &Provider{Provider: parameters.NewProvider(s), table: table}
}

// FromConfig returns a provider with a DynamoDB client built from cfg. A
// local DynamoDB is reached through cfg's BaseEndpoint, Python's
// endpoint_url.
func FromConfig(cfg aws.Config, table string, opts ...Option) *Provider {
	return New(dynamodb.NewFromConfig(cfg), table, opts...)
}

// Table returns the name of the table the provider reads.
func (p *Provider) Table() string { return p.table }

// readOptions are the DynamoDB options of one read.
type readOptions struct {
	consistentRead bool
}

// WithConsistentRead makes the read strongly consistent (ConsistentRead).
func WithConsistentRead(consistent bool) parameters.Option {
	return parameters.WithStoreOption("", func(o *readOptions) { o.consistentRead = consistent })
}

// store reads the table for the cache and transforms of
// parameters.Provider.
type store struct {
	client    Client
	table     string
	keyAttr   string
	sortAttr  string
	valueAttr string
}

// Get calls GetItem with the name as partition key.
func (s *store) Get(ctx context.Context, name string, opts parameters.StoreOptions) ([]byte, error) {
	var o readOptions
	opts.Apply(&o)
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            map[string]types.AttributeValue{s.keyAttr: &types.AttributeValueMemberS{Value: name}},
		ConsistentRead: aws.Bool(o.consistentRead),
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb: %w", err)
	}
	if len(out.Item) == 0 {
		return nil, parameters.ErrNotFound
	}
	value, ok := out.Item[s.valueAttr]
	if !ok {
		return nil, fmt.Errorf("%w: the item has no attribute %q", parameters.ErrNotFound, s.valueAttr)
	}
	return bytesOf(value)
}

// GetMultiple queries the items whose partition key is path, every page.
func (s *store) GetMultiple(ctx context.Context, path string, opts parameters.StoreOptions) (map[string][]byte, error) {
	var o readOptions
	opts.Apply(&o)
	pages := dynamodb.NewQueryPaginator(s.client, &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		KeyConditionExpression:    aws.String("#key = :key"),
		ExpressionAttributeNames:  map[string]string{"#key": s.keyAttr},
		ExpressionAttributeValues: map[string]types.AttributeValue{":key": &types.AttributeValueMemberS{Value: path}},
		ConsistentRead:            aws.Bool(o.consistentRead),
	})
	values := make(map[string][]byte)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("dynamodb: %w", err)
		}
		for _, item := range page.Items {
			name, value, err := s.entry(item)
			if err != nil {
				return nil, err
			}
			values[name] = value
		}
	}
	return values, nil
}

// entry returns the sort key and the value of one item of a query.
func (s *store) entry(item map[string]types.AttributeValue) (name string, value []byte, err error) {
	sortKey, ok := item[s.sortAttr]
	if !ok {
		return "", nil, fmt.Errorf("%w: an item has no attribute %q", parameters.ErrNotFound, s.sortAttr)
	}
	attr, ok := item[s.valueAttr]
	if !ok {
		return "", nil, fmt.Errorf("%w: an item has no attribute %q", parameters.ErrNotFound, s.valueAttr)
	}
	key, err := bytesOf(sortKey)
	if err != nil {
		return "", nil, err
	}
	if value, err = bytesOf(attr); err != nil {
		return "", nil, err
	}
	return string(key), value, nil
}

// bytesOf returns a string attribute as is, a binary one as its bytes, a
// number as its text, and anything else as its JSON encoding.
func bytesOf(av types.AttributeValue) ([]byte, error) {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		return []byte(v.Value), nil
	case *types.AttributeValueMemberB:
		return v.Value, nil
	case *types.AttributeValueMemberN:
		return []byte(v.Value), nil
	default:
		var enc jsontext.Value
		if err := appendJSON(&enc, av); err != nil {
			return nil, err
		}
		return enc, nil
	}
}

// appendJSON writes av as JSON: numbers keep every digit DynamoDB stored,
// binaries are base64 strings, sets are arrays, map keys are sorted.
func appendJSON(out *jsontext.Value, av types.AttributeValue) error {
	var err error
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		*out, err = jsontext.AppendQuote(*out, v.Value)
	case *types.AttributeValueMemberN:
		*out = append(*out, v.Value...)
	case *types.AttributeValueMemberB:
		*out, err = jsontext.AppendQuote(*out, base64Of(v.Value))
	case *types.AttributeValueMemberBOOL:
		*out = fmt.Append(*out, v.Value)
	case *types.AttributeValueMemberNULL:
		*out = append(*out, "null"...)
	case *types.AttributeValueMemberL:
		err = appendArray(out, len(v.Value), func(i int) error { return appendJSON(out, v.Value[i]) })
	case *types.AttributeValueMemberSS:
		err = appendArray(out, len(v.Value), func(i int) error { return appendJSON(out, &types.AttributeValueMemberS{Value: v.Value[i]}) })
	case *types.AttributeValueMemberNS:
		err = appendArray(out, len(v.Value), func(i int) error { return appendJSON(out, &types.AttributeValueMemberN{Value: v.Value[i]}) })
	case *types.AttributeValueMemberBS:
		err = appendArray(out, len(v.Value), func(i int) error { return appendJSON(out, &types.AttributeValueMemberB{Value: v.Value[i]}) })
	case *types.AttributeValueMemberM:
		err = appendObject(out, v.Value)
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedAttribute, av)
	}
	if err != nil {
		return fmt.Errorf("encode attribute: %w", err)
	}
	return nil
}

func appendObject(out *jsontext.Value, members map[string]types.AttributeValue) error {
	*out = append(*out, '{')
	for i, key := range slices.Sorted(maps.Keys(members)) {
		if i > 0 {
			*out = append(*out, ',')
		}
		quoted, err := jsontext.AppendQuote(*out, key)
		if err != nil {
			return fmt.Errorf("encode attribute name: %w", err)
		}
		*out = quoted
		*out = append(*out, ':')
		if err := appendJSON(out, members[key]); err != nil {
			return err
		}
	}
	*out = append(*out, '}')
	return nil
}

func appendArray(out *jsontext.Value, n int, item func(int) error) error {
	*out = append(*out, '[')
	for i := range n {
		if i > 0 {
			*out = append(*out, ',')
		}
		if err := item(i); err != nil {
			return err
		}
	}
	*out = append(*out, ']')
	return nil
}

// ErrUnsupportedAttribute reports an attribute type this provider does not
// know, one added to DynamoDB after this code was written.
var ErrUnsupportedAttribute = errors.New("unsupported attribute type")

func base64Of(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
