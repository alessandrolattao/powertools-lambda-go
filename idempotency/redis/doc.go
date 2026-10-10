// Package redis keeps idempotency records in Valkey, Redis OSS or any
// Redis-compatible cache (Amazon ElastiCache, Amazon MemoryDB): the
// persistence layer Powertools Python calls CachePersistenceLayer, with the
// same record format, so that Go and Python functions can share a cache.
//
// # Usage
//
// Hand New a go-redis v9 client, any of them (redis.UniversalClient:
// standalone, cluster, sentinel), configured as the cache needs (TLS
// certificates, timeouts, credentials from Secrets Manager):
//
//	client := goredis.NewClient(&goredis.Options{
//		Addr:      os.Getenv("CACHE_ENDPOINT"),
//		Password:  password,
//		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
//	})
//	store, err := redis.New(client)
//	if err != nil { ... }
//	lambda.Start(idempotency.Idempotent(handle, store))
//
// FromConnection connects from the arguments Python takes instead (URL,
// host, port, username, password, database, standalone or cluster mode),
// with TLS on unless DisableTLS says otherwise.
//
// The function reaches the cache from inside its VPC; the data path needs no
// IAM permission.
//
// # Records
//
// A record is a JSON object under the idempotency key itself, which expires
// with the record:
//
//	{"status": "INPROGRESS", "expiration": 1636549553, "in_progress_expiration": 1636545953123, "validation": "..."}
//	{"data": "{\"payment_id\": 12345}", "status": "COMPLETED", "expiration": 1636549553, "validation": "..."}
//
// expiration is in epoch seconds, in_progress_expiration in epoch
// milliseconds, validation only there with payload validation. Every field
// name can be changed: WithExpiryAttr, WithInProgressExpiryAttr,
// WithStatusAttr, WithDataAttr, WithValidationKeyAttr.
//
// # Commands
//
// Every command touches one key, so clusters and serverless caches work:
// SET NX EX takes a free key; when the key is held, GET reads the record. A
// record that no longer holds its key (expired, or in progress past its
// invocation's deadline) but is still there is an orphan: the invocation
// that takes "<key>:lock" (SET NX EX 10) replaces it, and leaves the lock to
// expire, so that nobody takes the new record for an orphan meanwhile.
// Completing is a SET EX, a failure a DEL.
//
// # Differences from Python
//
// Fixed here: completed records keep the validation hash (Python drops it,
// so every replay of a validated payload failed); a custom expiry field name
// is read back (Python always reads "expiration"; a Python function reading
// such records sees no expiry); a key evicted between the SET NX and the GET
// is retried rather than reported as a store failure; records never get an
// EX of 0. A record read on a failed SET NX is handed over, which saves
// Python's second GET.
package redis
