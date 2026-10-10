package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// DynamoDBClient is the part of the DynamoDB API the store calls, which
// *dynamodb.Client implements.
type DynamoDBClient interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// DynamoDBStore keeps circuits in a DynamoDB table, one item per circuit,
// in the format Powertools Python's CircuitBreakerDynamoDBPersistence
// writes: a Go and a Python function guard the same circuit when they use
// the same table and circuit name (and, with a composite key, the same
// partition key value).
//
// The table needs a partition key named "id" of type string, and its time
// to live on the attribute "expiration". The function needs
// dynamodb:GetItem, dynamodb:PutItem and dynamodb:UpdateItem on it.
type DynamoDBStore struct {
	client DynamoDBClient
	table  string

	keyAttr              string
	sortKeyAttr          string
	staticPKValue        string
	staticPKSet          bool
	stateAttr            string
	failureCountAttr     string
	openedAtAttr         string
	halfOpenOwnerAttr    string
	probeLeaseExpiryAttr string
	expiryAttr           string
}

// DynamoDBOption configures a DynamoDBStore.
type DynamoDBOption func(*DynamoDBStore)

// WithKeyAttr names the partition key attribute, "id" by default. It holds
// the circuit's name, or with WithSortKeyAttr the static partition value.
func WithKeyAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.keyAttr = name }
}

// WithSortKeyAttr makes the key composite, for a single-table design: the
// sort key attribute name holds the circuit's name, and the partition key
// the value of WithStaticPKValue.
func WithSortKeyAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.sortKeyAttr = name }
}

// WithStaticPKValue sets the partition key value of a composite key. It
// defaults to "circuit_breaker#<function name>", from
// AWS_LAMBDA_FUNCTION_NAME, which keeps the circuits of every function
// apart; a fixed value lets several functions share their circuits. It is
// ignored without WithSortKeyAttr.
func WithStaticPKValue(value string) DynamoDBOption {
	return func(s *DynamoDBStore) {
		s.staticPKValue = value
		s.staticPKSet = true
	}
}

// WithStateAttr names the attribute of the state, "state" by default.
func WithStateAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.stateAttr = name }
}

// WithFailureCountAttr names the attribute of the failure count,
// "failure_count" by default.
func WithFailureCountAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.failureCountAttr = name }
}

// WithOpenedAtAttr names the attribute of the opening time, "opened_at" by
// default.
func WithOpenedAtAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.openedAtAttr = name }
}

// WithHalfOpenOwnerAttr names the attribute of the prober,
// "half_open_owner" by default.
func WithHalfOpenOwnerAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.halfOpenOwnerAttr = name }
}

// WithProbeLeaseExpiryAttr names the attribute of the end of the prober's
// lease, "probe_lease_expiry" by default.
func WithProbeLeaseExpiryAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.probeLeaseExpiryAttr = name }
}

// WithExpiryAttr names the time to live attribute, "expiration" by default.
func WithExpiryAttr(name string) DynamoDBOption {
	return func(s *DynamoDBStore) { s.expiryAttr = name }
}

// NewDynamoDBStore returns a store on the DynamoDB table, through client. It
// fails with ErrInvalidConfig when the table has no name or the partition
// and sort keys have the same.
func NewDynamoDBStore(client DynamoDBClient, table string, opts ...DynamoDBOption) (*DynamoDBStore, error) {
	s := &DynamoDBStore{
		client:               client,
		table:                table,
		keyAttr:              "id",
		stateAttr:            "state",
		failureCountAttr:     "failure_count",
		openedAtAttr:         "opened_at",
		halfOpenOwnerAttr:    "half_open_owner",
		probeLeaseExpiryAttr: "probe_lease_expiry",
		expiryAttr:           "expiration",
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	switch {
	case client == nil:
		return nil, fmt.Errorf("%w: the store needs a DynamoDB client", ErrInvalidConfig)
	case table == "":
		return nil, fmt.Errorf("%w: the store needs a table name", ErrInvalidConfig)
	case s.keyAttr == "":
		return nil, fmt.Errorf("%w: the partition key attribute needs a name", ErrInvalidConfig)
	case s.sortKeyAttr == s.keyAttr:
		return nil, fmt.Errorf("%w: key attribute [%s] and sort key attribute [%s] cannot be the same",
			ErrInvalidConfig, s.keyAttr, s.sortKeyAttr)
	}
	if s.staticPKSet && s.sortKeyAttr == "" {
		stderr().Warn("static_pk_value is ignored unless sort_key_attr is also set.")
	}
	if !s.staticPKSet {
		s.staticPKValue = "circuit_breaker#" + env.Get(env.LambdaFunctionName)
	}
	return s, nil
}

// NewDynamoDBStoreFromConfig returns a store on the DynamoDB table, with a
// client built from cfg.
func NewDynamoDBStoreFromConfig(cfg aws.Config, table string, opts ...DynamoDBOption) (*DynamoDBStore, error) {
	return NewDynamoDBStore(dynamodb.NewFromConfig(cfg), table, opts...)
}

// electionCondition is the condition of AcquireProbe's write, as Powertools
// Python writes it: the circuit is Open, nobody owns the probe and the
// circuit opened when the caller saw it open (a stale read after a reopen
// cannot win), or the prober's lease has ended.
const electionCondition = "(#state = :open AND attribute_not_exists(#half_open_owner) AND #opened_at = :expected_opened_at)" +
	" OR (#state = :half_open AND #probe_lease_expiry <= :now)"

// Get reads the circuit's item, with an eventually consistent read: the
// breaker tolerates a state a few seconds old anyway, and it costs half.
func (s *DynamoDBStore) Get(ctx context.Context, name string) (Record, bool, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            s.key(name),
		ConsistentRead: aws.Bool(false),
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("get circuit %q: %w", name, err)
	}
	if out.Item == nil {
		return Record{}, false, nil
	}
	record, err := s.record(name, out.Item)
	if err != nil {
		return Record{}, false, fmt.Errorf("get circuit %q: %w", name, err)
	}
	return record, true, nil
}

// Put writes the circuit's item unconditionally.
func (s *DynamoDBStore) Put(ctx context.Context, record Record) error {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      s.item(&record),
	})
	if err != nil {
		return fmt.Errorf("put circuit %q: %w", record.Name, err)
	}
	return nil
}

// AcquireProbe writes the circuit's item if the conditions of the election
// hold, in a single conditional PutItem; a failed condition means another
// breaker won.
func (s *DynamoDBStore) AcquireProbe(ctx context.Context, record Record, now time.Time) (bool, error) {
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                s.item(&record),
		ConditionExpression: aws.String(electionCondition),
		ExpressionAttributeNames: map[string]string{
			"#state":              s.stateAttr,
			"#half_open_owner":    s.halfOpenOwnerAttr,
			"#probe_lease_expiry": s.probeLeaseExpiryAttr,
			"#opened_at":          s.openedAtAttr,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":open":               stringValue(string(Open)),
			":half_open":          stringValue(string(HalfOpen)),
			":now":                numberValue(now.Unix()),
			":expected_opened_at": numberValue(seconds(record.OpenedAt)),
		},
	})
	switch {
	case err == nil:
		return true, nil
	case lostElection(err):
		return false, nil
	default:
		return false, fmt.Errorf("acquire the probe of circuit %q: %w", record.Name, err)
	}
}

// lostElection tells the failed condition of a conditional write, which
// means that another breaker won, from a failed request.
func lostElection(err error) bool {
	lost, ok := errors.AsType[*types.ConditionalCheckFailedException](err)
	return ok && lost != nil
}

// Update records the end of a probe with an UpdateItem, clearing the
// prober and its lease.
func (s *DynamoDBStore) Update(ctx context.Context, record Record) error {
	set := []string{"#state = :state", "#failure_count = :failure_count"}
	names := map[string]string{
		"#state":              s.stateAttr,
		"#failure_count":      s.failureCountAttr,
		"#opened_at":          s.openedAtAttr,
		"#half_open_owner":    s.halfOpenOwnerAttr,
		"#probe_lease_expiry": s.probeLeaseExpiryAttr,
	}
	values := map[string]types.AttributeValue{
		":state":         stringValue(string(record.State)),
		":failure_count": numberValue(int64(record.FailureCount)),
	}
	if !record.Expiration.IsZero() {
		set = append(set, "#expiration = :expiration")
		names["#expiration"] = s.expiryAttr
		values[":expiration"] = numberValue(record.Expiration.Unix())
	}
	remove := "#opened_at, #half_open_owner, #probe_lease_expiry"
	if !record.OpenedAt.IsZero() {
		set = append(set, "#opened_at = :opened_at")
		values[":opened_at"] = numberValue(record.OpenedAt.Unix())
		remove = "#half_open_owner, #probe_lease_expiry"
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       s.key(record.Name),
		UpdateExpression:          aws.String("SET " + strings.Join(set, ", ") + " REMOVE " + remove),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		return fmt.Errorf("update circuit %q: %w", record.Name, err)
	}
	return nil
}

// key returns the primary key of the circuit name.
func (s *DynamoDBStore) key(name string) map[string]types.AttributeValue {
	if s.sortKeyAttr != "" {
		return map[string]types.AttributeValue{
			s.keyAttr:     stringValue(s.staticPKValue),
			s.sortKeyAttr: stringValue(name),
		}
	}
	return map[string]types.AttributeValue{s.keyAttr: stringValue(name)}
}

// item returns the item of record; absent values have no attribute.
func (s *DynamoDBStore) item(record *Record) map[string]types.AttributeValue {
	item := s.key(record.Name)
	item[s.stateAttr] = stringValue(string(record.State))
	item[s.failureCountAttr] = numberValue(int64(record.FailureCount))
	if !record.OpenedAt.IsZero() {
		item[s.openedAtAttr] = numberValue(record.OpenedAt.Unix())
	}
	if record.HalfOpenOwner != "" {
		item[s.halfOpenOwnerAttr] = stringValue(record.HalfOpenOwner)
	}
	if !record.ProbeLeaseExpiry.IsZero() {
		item[s.probeLeaseExpiryAttr] = numberValue(record.ProbeLeaseExpiry.Unix())
	}
	if !record.Expiration.IsZero() {
		item[s.expiryAttr] = numberValue(record.Expiration.Unix())
	}
	return item
}

// record reads the circuit name's item.
func (s *DynamoDBStore) record(name string, item map[string]types.AttributeValue) (Record, error) {
	state, ok := item[s.stateAttr].(*types.AttributeValueMemberS)
	if !ok {
		return Record{}, fmt.Errorf("%w: no string attribute %q", errInvalidRecord, s.stateAttr)
	}
	record := Record{Name: name, State: State(state.Value)}
	if !record.State.Valid() {
		return Record{}, fmt.Errorf("%w: unknown state %q", errInvalidRecord, state.Value)
	}
	if owner, ok := item[s.halfOpenOwnerAttr].(*types.AttributeValueMemberS); ok {
		record.HalfOpenOwner = owner.Value
	}
	failures, err := s.number(item, s.failureCountAttr)
	if err != nil {
		return Record{}, err
	}
	record.FailureCount = int(failures.value)
	for _, field := range []struct {
		attr string
		into *time.Time
	}{
		{s.openedAtAttr, &record.OpenedAt},
		{s.probeLeaseExpiryAttr, &record.ProbeLeaseExpiry},
		{s.expiryAttr, &record.Expiration},
	} {
		n, err := s.number(item, field.attr)
		if err != nil {
			return Record{}, err
		}
		if n.present {
			*field.into = time.Unix(n.value, 0)
		}
	}
	return record, nil
}

type optionalNumber struct {
	value   int64
	present bool
}

// number reads a numeric attribute as whole seconds, dropping a fraction as
// Python's int() does.
func (s *DynamoDBStore) number(item map[string]types.AttributeValue, attr string) (optionalNumber, error) {
	raw, ok := item[attr]
	if !ok {
		return optionalNumber{}, nil
	}
	n, ok := raw.(*types.AttributeValueMemberN)
	if !ok {
		return optionalNumber{}, fmt.Errorf("%w: attribute %q is not a number", errInvalidRecord, attr)
	}
	if v, err := strconv.ParseInt(n.Value, 10, 64); err == nil {
		return optionalNumber{value: v, present: true}, nil
	}
	f, err := strconv.ParseFloat(n.Value, 64)
	if err != nil || math.IsNaN(f) || math.Abs(f) > maxSeconds {
		return optionalNumber{}, fmt.Errorf("%w: attribute %q is %q", errInvalidRecord, attr, n.Value)
	}
	return optionalNumber{value: int64(f), present: true}, nil
}

// maxSeconds bounds the timestamps read from the store to what int64
// holds.
const maxSeconds = 1 << 62

// seconds is the Unix time of t, 0 for the zero time.
func seconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func stringValue(s string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: s}
}

func numberValue(n int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}
