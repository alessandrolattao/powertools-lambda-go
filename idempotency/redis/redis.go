package redis

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
)

// The attribute names of the JSON record Powertools Python uses unless told
// otherwise.
const (
	DefaultExpiryAttr           = "expiration"
	DefaultInProgressExpiryAttr = "in_progress_expiration"
	DefaultStatusAttr           = "status"
	DefaultDataAttr             = "data"
	DefaultValidationKeyAttr    = "validation"
)

// orphanLockTimeout bounds how long the lock that guards the replacement of
// an orphan record lives; it is never released, so that the record is not
// taken for an orphan again while its new owner runs.
const orphanLockTimeout = 10 * time.Second

// lockSuffix names the orphan lock of a key, as Powertools Python does.
const lockSuffix = ":lock"

// Client is the part of the Redis API the store calls. Every go-redis v9
// client satisfies it: redis.UniversalClient, so a standalone, cluster,
// sentinel (failover) or ring client, with Redis OSS, Valkey, ElastiCache or
// MemoryDB behind it.
type Client interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *goredis.StatusCmd
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *goredis.BoolCmd
	Del(ctx context.Context, keys ...string) *goredis.IntCmd
}

// Store keeps idempotency records in Redis or Valkey: each under its
// idempotency key, as a JSON object that expires with the record. It is
// safe for concurrent use, and one Store serves any number of idempotent
// functions.
type Store struct {
	client               Client
	expiryAttr           string
	inProgressExpiryAttr string
	statusAttr           string
	dataAttr             string
	validationKeyAttr    string
}

var _ idempotency.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithExpiryAttr names the field of the record's expiry, in epoch seconds,
// "expiration" by default. Unlike Python, which writes this name but always
// reads "expiration", the store reads it back too.
func WithExpiryAttr(name string) Option {
	return func(s *Store) { s.expiryAttr = name }
}

// WithInProgressExpiryAttr names the field of an in-progress record's
// expiry, in epoch milliseconds, "in_progress_expiration" by default.
func WithInProgressExpiryAttr(name string) Option {
	return func(s *Store) { s.inProgressExpiryAttr = name }
}

// WithStatusAttr names the status field, "status" by default.
func WithStatusAttr(name string) Option {
	return func(s *Store) { s.statusAttr = name }
}

// WithDataAttr names the field of the stored response, "data" by default.
func WithDataAttr(name string) Option {
	return func(s *Store) { s.dataAttr = name }
}

// WithValidationKeyAttr names the field of the payload validation hash,
// "validation" by default.
func WithValidationKeyAttr(name string) Option {
	return func(s *Store) { s.validationKeyAttr = name }
}

// New returns a Store on client. It fails with idempotency.ErrInvalidConfig
// when the options cannot work.
func New(client Client, opts ...Option) (*Store, error) {
	s := &Store{
		client:               client,
		expiryAttr:           DefaultExpiryAttr,
		inProgressExpiryAttr: DefaultInProgressExpiryAttr,
		statusAttr:           DefaultStatusAttr,
		dataAttr:             DefaultDataAttr,
		validationKeyAttr:    DefaultValidationKeyAttr,
	}
	for _, opt := range opts {
		opt(s)
	}
	if client == nil {
		return nil, fmt.Errorf("%w: nil Redis client", idempotency.ErrInvalidConfig)
	}
	names := map[string]bool{}
	for _, name := range []string{s.expiryAttr, s.inProgressExpiryAttr, s.statusAttr, s.dataAttr, s.validationKeyAttr} {
		if name == "" || names[name] {
			return nil, fmt.Errorf("%w: record field names must be distinct and not empty, %q is not",
				idempotency.ErrInvalidConfig, name)
		}
		names[name] = true
	}
	return s, nil
}

// Mode is how FromConnection talks to the cache.
type Mode string

// The modes of Powertools Python.
const (
	// Standalone talks to one node (or to the primary endpoint of a
	// replication group).
	Standalone Mode = "standalone"
	// Cluster talks to a cluster, Redis Cluster protocol, from any of its
	// nodes; ElastiCache Serverless and MemoryDB are clusters.
	Cluster Mode = "cluster"
)

// DefaultPort is the port of Connection unless it says otherwise.
const DefaultPort = 6379

// Connection describes a cache to connect to, as the connection arguments
// of Powertools Python's CachePersistenceLayer do. For anything they do not
// cover (timeouts, pools, certificates, sentinel), build a go-redis client
// and pass it to New.
type Connection struct {
	// URL is a redis:// or rediss:// (TLS) URL; when set, it replaces Host,
	// Port, Username, Password, DB and DisableTLS.
	URL string
	// Host and Port locate the cache; Port defaults to 6379.
	Host string
	Port int
	// Username and Password authenticate, with an ACL user or AUTH.
	Username string
	Password string
	// DB selects a database; clusters only have database 0.
	DB int
	// Mode is Standalone, the default, or Cluster.
	Mode Mode
	// DisableTLS connects in plain text. TLS is on by default, as in
	// Powertools Python.
	DisableTLS bool
}

// FromConnection connects to the cache conn describes and returns a Store
// on it. The connection opens on first use.
func FromConnection(conn Connection, opts ...Option) (*Store, error) {
	client, err := conn.client()
	if err != nil {
		return nil, err
	}
	return New(client, opts...)
}

func (c Connection) client() (goredis.UniversalClient, error) {
	mode := c.Mode
	if mode == "" {
		mode = Standalone
	}
	if mode != Standalone && mode != Cluster {
		return nil, fmt.Errorf("%w: mode %q not supported", idempotency.ErrInvalidConfig, string(mode))
	}
	if c.URL != "" {
		return clientFromURL(c.URL, mode)
	}
	port := c.Port
	if port == 0 {
		port = DefaultPort
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(port))
	var tlsConfig *tls.Config
	if !c.DisableTLS {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.Host}
	}
	if mode == Cluster {
		return goredis.NewClusterClient(&goredis.ClusterOptions{
			Addrs: []string{addr}, Username: c.Username, Password: c.Password, TLSConfig: tlsConfig,
		}), nil
	}
	return goredis.NewClient(&goredis.Options{
		Addr: addr, Username: c.Username, Password: c.Password, DB: c.DB, TLSConfig: tlsConfig,
	}), nil
}

func clientFromURL(url string, mode Mode) (goredis.UniversalClient, error) {
	if mode == Cluster {
		options, err := goredis.ParseClusterURL(url)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", idempotency.ErrInvalidConfig, err)
		}
		return goredis.NewClusterClient(options), nil
	}
	options, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", idempotency.ErrInvalidConfig, err)
	}
	return goredis.NewClient(options), nil
}

// PutRecord implements idempotency.Store. SET NX writes the record when the
// key is free. Otherwise the record that holds it is read: when it no
// longer holds the key (expired, or in progress past its deadline, but not
// yet evicted), it is an orphan, replaced by whoever takes the orphan lock
// first.
func (s *Store) PutRecord(ctx context.Context, record idempotency.Record, now time.Time) error {
	value, err := s.encode(record, false)
	if err != nil {
		return err
	}
	ttl := timeToLive(record.ExpiresAt, now)
	written, err := s.client.SetNX(ctx, record.Key, value, ttl).Result()
	if err != nil {
		return fmt.Errorf("set the record: %w", err)
	}
	if written {
		return nil
	}

	raw, err := s.client.Get(ctx, record.Key).Result()
	if errors.Is(err, goredis.Nil) {
		// Evicted since the write: the next attempt finds the key free.
		return &idempotency.ItemAlreadyExistsError{}
	}
	if err != nil {
		return fmt.Errorf("get the record: %w", err)
	}
	if old, decodeErr := s.decode(record.Key, raw); decodeErr == nil && old.HoldsKey(now) {
		return &idempotency.ItemAlreadyExistsError{Old: old}
	}

	// An orphan, or a value that is not a record at all.
	locked, err := s.client.SetNX(ctx, record.Key+lockSuffix, "True", min(orphanLockTimeout, ttl)).Result()
	if err != nil {
		return fmt.Errorf("lock the orphan record: %w", err)
	}
	if !locked {
		// Another invocation is replacing it: the next read tells what it
		// wrote.
		return &idempotency.ItemAlreadyExistsError{}
	}
	if err := s.client.Set(ctx, record.Key, value, ttl).Err(); err != nil {
		return fmt.Errorf("replace the orphan record: %w", err)
	}
	return nil
}

// GetRecord implements idempotency.Store.
func (s *Store) GetRecord(ctx context.Context, key string) (*idempotency.Record, error) {
	raw, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) || (err == nil && raw == "") {
		return nil, idempotency.ErrItemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get the record: %w", err)
	}
	return s.decode(key, raw)
}

// UpdateRecord implements idempotency.Store: it writes the COMPLETED record
// over the in-progress one, with the validation hash Python forgets.
func (s *Store) UpdateRecord(ctx context.Context, record idempotency.Record) error {
	value, err := s.encode(record, true)
	if err != nil {
		return err
	}
	if err := s.client.Set(ctx, record.Key, value, timeToLive(record.ExpiresAt, time.Now())).Err(); err != nil {
		return fmt.Errorf("set the record: %w", err)
	}
	return nil
}

// DeleteRecord implements idempotency.Store.
func (s *Store) DeleteRecord(ctx context.Context, key string) error {
	if err := s.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("delete the record: %w", err)
	}
	return nil
}

// timeToLive is how long Redis keeps a record that expires at expiresAt,
// whole seconds and at least one: Redis rejects EX 0.
func timeToLive(expiresAt, now time.Time) time.Duration {
	if expiresAt.IsZero() {
		return idempotency.DefaultExpiresAfter
	}
	return max(time.Duration(expiresAt.Unix()-now.Unix())*time.Second, time.Second)
}

// encode renders record as the JSON object Python writes, fields in Python's
// order and with its separators: status, expiry, in-progress expiry and
// validation for an in-progress record; data, status, expiry and validation
// for a completed one.
func (s *Store) encode(record idempotency.Record, completed bool) (string, error) {
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf, jsontext.SpaceAfterColon(true), jsontext.SpaceAfterComma(true))
	tokens := []jsontext.Token{jsontext.BeginObject}
	if completed {
		tokens = append(tokens, jsontext.String(s.dataAttr), jsontext.String(record.ResponseData))
	}
	tokens = append(tokens,
		jsontext.String(s.statusAttr), jsontext.String(string(record.Status)),
		jsontext.String(s.expiryAttr), jsontext.Int(record.ExpiresAt.Unix()),
	)
	if !completed && !record.InProgressExpiresAt.IsZero() {
		tokens = append(tokens, jsontext.String(s.inProgressExpiryAttr), jsontext.Int(record.InProgressExpiresAt.UnixMilli()))
	}
	if record.PayloadHash != "" {
		tokens = append(tokens, jsontext.String(s.validationKeyAttr), jsontext.String(record.PayloadHash))
	}
	tokens = append(tokens, jsontext.EndObject)
	for _, token := range tokens {
		if err := enc.WriteToken(token); err != nil {
			return "", fmt.Errorf("encode the record: %w", err)
		}
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// decode reads the JSON object of the record with key. The status is
// required, as in Python; the rest is optional.
func (s *Store) decode(key, raw string) (*idempotency.Record, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, fmt.Errorf("%w: %w", idempotency.ErrMalformedRecord, err)
	}
	record := &idempotency.Record{Key: key}
	var status string
	var expiry, inProgressExpiry float64
	found := make(map[string]bool, 5)
	for name, target := range map[string]any{
		s.statusAttr:           &status,
		s.expiryAttr:           &expiry,
		s.inProgressExpiryAttr: &inProgressExpiry,
		s.dataAttr:             &record.ResponseData,
		s.validationKeyAttr:    &record.PayloadHash,
	} {
		value, ok := fields[name]
		if !ok || value.Kind() == jsontext.KindNull {
			continue
		}
		if err := json.Unmarshal(value, target); err != nil {
			return nil, fmt.Errorf("%w: field %q: %w", idempotency.ErrMalformedRecord, name, err)
		}
		found[name] = true
	}
	if !found[s.statusAttr] {
		return nil, fmt.Errorf("%w: field %q is missing", idempotency.ErrMalformedRecord, s.statusAttr)
	}
	record.Status = idempotency.Status(status)
	if found[s.expiryAttr] {
		record.ExpiresAt = time.Unix(int64(expiry), 0)
	}
	if found[s.inProgressExpiryAttr] {
		record.InProgressExpiresAt = time.UnixMilli(int64(inProgressExpiry))
	}
	return record, nil
}
