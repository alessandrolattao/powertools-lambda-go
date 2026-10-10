package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// The attribute names Powertools Python uses unless told otherwise.
const (
	DefaultKeyAttr              = "id"
	DefaultExpiryAttr           = "expiration"
	DefaultInProgressExpiryAttr = "in_progress_expiration"
	DefaultStatusAttr           = "status"
	DefaultDataAttr             = "data"
	DefaultValidationKeyAttr    = "validation"
)

// putCondition lets an in-progress record replace only a missing, an
// expired or a timed-out in-progress one; the expression is Powertools
// Python's, word for word.
const putCondition = "attribute_not_exists(#id) OR #expiry < :now OR " +
	"(#status = :inprogress AND attribute_exists(#in_progress_expiry) AND #in_progress_expiry < :now_in_millis)"

// Client is the part of the DynamoDB API the store calls; *dynamodb.Client
// from aws-sdk-go-v2 satisfies it.
type Client interface {
	GetItem(ctx context.Context, params *awsdynamodb.GetItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *awsdynamodb.PutItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *awsdynamodb.UpdateItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *awsdynamodb.DeleteItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.DeleteItemOutput, error)
}

// Store keeps idempotency records in a DynamoDB table. It is safe for
// concurrent use, and one Store serves any number of idempotent functions.
type Store struct {
	client               Client
	table                string
	keyAttr              string
	sortKeyAttr          string
	staticPKValue        string
	expiryAttr           string
	inProgressExpiryAttr string
	statusAttr           string
	dataAttr             string
	validationKeyAttr    string
}

var _ idempotency.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithKeyAttr names the partition key attribute, "id" by default.
func WithKeyAttr(name string) Option {
	return func(s *Store) { s.keyAttr = name }
}

// WithSortKeyAttr names the sort key attribute of a table with a composite
// primary key. The idempotency key then goes in the sort key, and the
// partition key holds the static value of WithStaticPKValue.
func WithSortKeyAttr(name string) Option {
	return func(s *Store) { s.sortKeyAttr = name }
}

// WithStaticPKValue sets the partition key of every record when the table
// has a sort key; it defaults to "idempotency#" and the Lambda function's
// name. Without a sort key it is ignored.
func WithStaticPKValue(value string) Option {
	return func(s *Store) { s.staticPKValue = value }
}

// WithExpiryAttr names the attribute of the record's expiry, in epoch
// seconds; it defaults to "expiration", which is the one to make the table's
// TTL attribute.
func WithExpiryAttr(name string) Option {
	return func(s *Store) { s.expiryAttr = name }
}

// WithInProgressExpiryAttr names the attribute of an in-progress record's
// expiry, in epoch milliseconds; it defaults to "in_progress_expiration".
func WithInProgressExpiryAttr(name string) Option {
	return func(s *Store) { s.inProgressExpiryAttr = name }
}

// WithStatusAttr names the status attribute, "status" by default.
func WithStatusAttr(name string) Option {
	return func(s *Store) { s.statusAttr = name }
}

// WithDataAttr names the attribute of the stored response, "data" by
// default.
func WithDataAttr(name string) Option {
	return func(s *Store) { s.dataAttr = name }
}

// WithValidationKeyAttr names the attribute of the payload validation hash,
// "validation" by default.
func WithValidationKeyAttr(name string) Option {
	return func(s *Store) { s.validationKeyAttr = name }
}

// New returns a Store on table, calling DynamoDB through client. It fails
// with idempotency.ErrInvalidConfig when the options cannot work.
func New(client Client, table string, opts ...Option) (*Store, error) {
	s := &Store{
		client:               client,
		table:                table,
		keyAttr:              DefaultKeyAttr,
		staticPKValue:        "idempotency#" + env.Get(env.LambdaFunctionName),
		expiryAttr:           DefaultExpiryAttr,
		inProgressExpiryAttr: DefaultInProgressExpiryAttr,
		statusAttr:           DefaultStatusAttr,
		dataAttr:             DefaultDataAttr,
		validationKeyAttr:    DefaultValidationKeyAttr,
	}
	for _, opt := range opts {
		opt(s)
	}
	switch {
	case client == nil:
		return nil, fmt.Errorf("%w: nil DynamoDB client", idempotency.ErrInvalidConfig)
	case table == "":
		return nil, fmt.Errorf("%w: empty table name", idempotency.ErrInvalidConfig)
	case s.keyAttr == "":
		return nil, fmt.Errorf("%w: empty key attribute name", idempotency.ErrInvalidConfig)
	case s.sortKeyAttr == s.keyAttr:
		return nil, fmt.Errorf("%w: key attribute %q and sort key attribute %q cannot be the same",
			idempotency.ErrInvalidConfig, s.keyAttr, s.sortKeyAttr)
	}
	return s, nil
}

// FromConfig returns a Store on table with a DynamoDB client built from cfg.
func FromConfig(cfg aws.Config, table string, opts ...Option) (*Store, error) {
	return New(awsdynamodb.NewFromConfig(cfg), table, opts...)
}

// key is the primary key of the record with idempotency key idemKey.
func (s *Store) key(idemKey string) map[string]types.AttributeValue {
	if s.sortKeyAttr != "" {
		return map[string]types.AttributeValue{
			s.keyAttr:     &types.AttributeValueMemberS{Value: s.staticPKValue},
			s.sortKeyAttr: &types.AttributeValueMemberS{Value: idemKey},
		}
	}
	return map[string]types.AttributeValue{s.keyAttr: &types.AttributeValueMemberS{Value: idemKey}}
}

// PutRecord implements idempotency.Store with a conditional PutItem. When
// the condition fails, DynamoDB returns the record that holds the key with
// the error, which saves a read.
func (s *Store) PutRecord(ctx context.Context, record idempotency.Record, now time.Time) error {
	item := s.key(record.Key)
	item[s.expiryAttr] = number(record.ExpiresAt.Unix())
	item[s.statusAttr] = &types.AttributeValueMemberS{Value: string(record.Status)}
	if !record.InProgressExpiresAt.IsZero() {
		item[s.inProgressExpiryAttr] = number(record.InProgressExpiresAt.UnixMilli())
	}
	if record.PayloadHash != "" {
		item[s.validationKeyAttr] = &types.AttributeValueMemberS{Value: record.PayloadHash}
	}
	_, err := s.client.PutItem(ctx, &awsdynamodb.PutItemInput{
		TableName:           new(s.table),
		Item:                item,
		ConditionExpression: new(putCondition),
		ExpressionAttributeNames: map[string]string{
			"#id":                 s.keyAttr,
			"#expiry":             s.expiryAttr,
			"#in_progress_expiry": s.inProgressExpiryAttr,
			"#status":             s.statusAttr,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now":           number(now.Unix()),
			":now_in_millis": number(now.UnixMilli()),
			":inprogress":    &types.AttributeValueMemberS{Value: string(idempotency.StatusInProgress)},
		},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if failed, ok := errors.AsType[*types.ConditionalCheckFailedException](err); ok {
		if len(failed.Item) == 0 {
			return &idempotency.ItemAlreadyExistsError{}
		}
		old, readErr := s.record(failed.Item)
		if readErr != nil {
			return readErr
		}
		return &idempotency.ItemAlreadyExistsError{Old: old}
	}
	if err != nil {
		return fmt.Errorf("put item: %w", err)
	}
	return nil
}

// GetRecord implements idempotency.Store with a strongly consistent
// GetItem.
func (s *Store) GetRecord(ctx context.Context, key string) (*idempotency.Record, error) {
	out, err := s.client.GetItem(ctx, &awsdynamodb.GetItemInput{
		TableName:      new(s.table),
		Key:            s.key(key),
		ConsistentRead: new(true),
	})
	if err != nil {
		return nil, fmt.Errorf("get item: %w", err)
	}
	if len(out.Item) == 0 {
		return nil, idempotency.ErrItemNotFound
	}
	return s.record(out.Item)
}

// UpdateRecord implements idempotency.Store with an UpdateItem that sets the
// response, the expiry, the status and the validation hash.
func (s *Store) UpdateRecord(ctx context.Context, record idempotency.Record) error {
	expression := "SET #response_data = :response_data, #expiry = :expiry, #status = :status"
	names := map[string]string{
		"#expiry":        s.expiryAttr,
		"#response_data": s.dataAttr,
		"#status":        s.statusAttr,
	}
	values := map[string]types.AttributeValue{
		":expiry":        number(record.ExpiresAt.Unix()),
		":response_data": &types.AttributeValueMemberS{Value: record.ResponseData},
		":status":        &types.AttributeValueMemberS{Value: string(record.Status)},
	}
	if record.PayloadHash != "" {
		expression += ", #validation_key = :validation_key"
		names["#validation_key"] = s.validationKeyAttr
		values[":validation_key"] = &types.AttributeValueMemberS{Value: record.PayloadHash}
	}
	_, err := s.client.UpdateItem(ctx, &awsdynamodb.UpdateItemInput{
		TableName:                 new(s.table),
		Key:                       s.key(record.Key),
		UpdateExpression:          new(expression),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		return fmt.Errorf("update item: %w", err)
	}
	return nil
}

// DeleteRecord implements idempotency.Store with a DeleteItem.
func (s *Store) DeleteRecord(ctx context.Context, key string) error {
	_, err := s.client.DeleteItem(ctx, &awsdynamodb.DeleteItemInput{
		TableName: new(s.table),
		Key:       s.key(key),
	})
	if err != nil {
		return fmt.Errorf("delete item: %w", err)
	}
	return nil
}

// record reads an item. The key, the status and the expiry are required, as
// in Python; the idempotency key comes from the sort key when there is one.
func (s *Store) record(item map[string]types.AttributeValue) (*idempotency.Record, error) {
	keyAttr := s.keyAttr
	if s.sortKeyAttr != "" {
		keyAttr = s.sortKeyAttr
	}
	key, err := stringAttr(item, keyAttr, true)
	if err != nil {
		return nil, err
	}
	status, err := stringAttr(item, s.statusAttr, true)
	if err != nil {
		return nil, err
	}
	expiry, err := numberAttr(item, s.expiryAttr, true)
	if err != nil {
		return nil, err
	}
	inProgressExpiry, err := numberAttr(item, s.inProgressExpiryAttr, false)
	if err != nil {
		return nil, err
	}
	data, err := stringAttr(item, s.dataAttr, false)
	if err != nil {
		return nil, err
	}
	validation, err := stringAttr(item, s.validationKeyAttr, false)
	if err != nil {
		return nil, err
	}
	record := &idempotency.Record{
		Key:          key,
		Status:       idempotency.Status(status),
		ExpiresAt:    time.Unix(expiry, 0),
		ResponseData: data,
		PayloadHash:  validation,
	}
	if _, ok := item[s.inProgressExpiryAttr]; ok {
		record.InProgressExpiresAt = time.UnixMilli(inProgressExpiry)
	}
	return record, nil
}

func number(n int64) *types.AttributeValueMemberN {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}

// stringAttr reads a string attribute; a missing optional one is "".
func stringAttr(item map[string]types.AttributeValue, name string, required bool) (string, error) {
	switch v := item[name].(type) {
	case *types.AttributeValueMemberS:
		return v.Value, nil
	case nil:
		if required {
			return "", fmt.Errorf("%w: attribute %q is missing", idempotency.ErrMalformedRecord, name)
		}
		return "", nil
	default:
		return "", fmt.Errorf("%w: attribute %q is a %T, not a string", idempotency.ErrMalformedRecord, name, v)
	}
}

// numberAttr reads a number attribute; a missing optional one is 0. A
// fraction, which Powertools never writes, is dropped.
func numberAttr(item map[string]types.AttributeValue, name string, required bool) (int64, error) {
	switch v := item[name].(type) {
	case *types.AttributeValueMemberN:
		if n, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
			return n, nil
		}
		f, err := strconv.ParseFloat(v.Value, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: attribute %q: %w", idempotency.ErrMalformedRecord, name, err)
		}
		return int64(f), nil
	case nil:
		if required {
			return 0, fmt.Errorf("%w: attribute %q is missing", idempotency.ErrMalformedRecord, name)
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("%w: attribute %q is a %T, not a number", idempotency.ErrMalformedRecord, name, v)
	}
}
