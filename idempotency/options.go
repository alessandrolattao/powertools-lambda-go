package idempotency

import (
	"context"
	"hash"
	"log/slog"
	"os"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// DefaultExpiresAfter is how long a record lasts unless WithExpiresAfter
// says otherwise, as in Powertools Python.
const DefaultExpiresAfter = time.Hour

// ResponseHook rewrites the response of a replay: it receives the response
// decoded from the stored record and the record itself, and returns what the
// idempotent function returns. It runs only when a COMPLETED record answers
// the call, never on the first execution or on errors. An error it returns
// is what the call returns.
type ResponseHook[R any] func(ctx context.Context, response R, record Record) (R, error)

// Option configures Idempotent.
type Option func(*config)

type config struct {
	eventKey           string
	payloadValidation  string
	functions          []jmespath.Function
	keyRequired        bool
	expiresAfter       time.Duration
	localCache         bool
	localCacheMaxItems int
	hashName           string
	newHash            func() hash.Hash
	keyPrefix          string
	responseHook       any
	now                func() time.Time
	warnings           *slog.Logger
}

func defaultConfig() config {
	return config{
		expiresAfter:       DefaultExpiresAfter,
		localCacheMaxItems: DefaultLocalCacheMaxItems,
		hashName:           DefaultHashFunction,
		now:                time.Now,
		// Warnings reach standard error whatever the function's own
		// logging, as Python's warnings do.
		warnings: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
}

// WithEventKeyJMESPath selects the part of the payload the idempotency key
// is the hash of, with a JMESPath expression that may use the Powertools
// functions (powertools_json and the others). By default the key is the
// hash of the whole payload.
//
// A payload that carries JSON in a string field (an API Gateway body) should
// go through powertools_json, so that the key does not depend on how the
// JSON is spaced: "powertools_json(body).[user_id, product_id]".
func WithEventKeyJMESPath(expression string) Option {
	return func(c *config) { c.eventKey = expression }
}

// WithPayloadValidationJMESPath turns payload validation on: the part of the
// payload expression selects is hashed into the record, and a later call
// with the same idempotency key but a different such part fails with
// ErrPayloadValidation instead of replaying a response meant for another
// payload.
func WithPayloadValidationJMESPath(expression string) Option {
	return func(c *config) { c.payloadValidation = expression }
}

// WithJMESPathFunctions makes custom functions available to the event key
// and payload validation expressions, next to the built-in and the
// Powertools ones.
func WithJMESPathFunctions(functions ...jmespath.Function) Option {
	return func(c *config) { c.functions = append(c.functions, functions...) }
}

// WithKeyRequired makes a payload without an idempotency key an error
// (ErrNoIdempotencyKey) instead of a call that runs without idempotency.
// Python calls it raise_on_no_idempotency_key.
func WithKeyRequired(required bool) Option {
	return func(c *config) { c.keyRequired = required }
}

// WithExpiresAfter sets how long a record lasts, in whole seconds, after
// which the same idempotency key runs the function again. It defaults to an
// hour.
func WithExpiresAfter(d time.Duration) Option {
	return func(c *config) { c.expiresAfter = d }
}

// WithLocalCache keeps the COMPLETED records in memory too, so that replays
// within one execution environment skip the store. The cache is local to the
// execution environment and to the idempotent function; it is off by
// default, since responses may be large relative to the function's memory.
func WithLocalCache(enabled bool) Option {
	return func(c *config) { c.localCache = enabled }
}

// WithLocalCacheMaxItems sets how many records the local cache holds before
// it evicts the least recently used one. It defaults to 256.
func WithLocalCacheMaxItems(n int) Option {
	return func(c *config) { c.localCacheMaxItems = n }
}

// WithHashFunction picks the hash of idempotency keys and payload
// validation by its name in Python's hashlib: md5 (the default), sha1,
// sha224, sha256, sha384, sha512, sha3_224, sha3_256, sha3_384, sha3_512,
// blake2b, blake2s.
func WithHashFunction(name string) Option {
	return func(c *config) {
		c.hashName = name
		c.newHash = nil
	}
}

// WithHash uses newHash for idempotency keys and payload validation, for a
// hash WithHashFunction does not name. Records are keyed by its hex digest.
func WithHash(newHash func() hash.Hash) Option {
	return func(c *config) {
		c.newHash = newHash
		c.hashName = ""
	}
}

// WithKeyPrefix replaces the first part of the idempotency key, the Lambda
// function's and the Go function's names, so that keys survive renaming the
// function, or match the keys of another function that shares the store.
// The key becomes "<prefix>#<hash>". Changing it makes existing records
// unreachable.
func WithKeyPrefix(prefix string) Option {
	return func(c *config) { c.keyPrefix = prefix }
}

// WithResponseHook runs hook on every replay. R must be the result type of
// the function Idempotent wraps.
func WithResponseHook[R any](hook ResponseHook[R]) Option {
	return func(c *config) { c.responseHook = hook }
}
