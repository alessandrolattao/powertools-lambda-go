// Package idempotency makes a Lambda handler, or any function, idempotent:
// called again with the same input while the first call's record lasts, it
// returns the first call's result instead of running again. Retries of an
// event source, a client resending a request after a timeout, two
// invocations racing on the same order: the side effect happens once.
//
// It is the Go side of Powertools for AWS Lambda (Python)'s idempotency
// utility, and writes the same records: a Go and a Python function can share
// a table, and even each other's records when they share a key prefix.
//
// # Getting started
//
// Wrap the handler, give it a Store, and choose the part of the event that
// identifies a request:
//
//	store, err := dynamodb.FromConfig(cfg, "IdempotencyTable")
//	if err != nil { ... }
//	handler := idempotency.Idempotent(createPayment, store,
//		idempotency.WithEventKeyJMESPath("powertools_json(body).[user_id, product_id]"))
//	lambda.Start(handler)
//
// The same call wraps any other function func(context.Context, E) (R, error):
// E is the payload the key comes from (Python's data_keyword_argument), R
// the result. Use one for each record of a batch, with the record as E.
//
// # How it works
//
// Every call computes an idempotency key, "<prefix>#<hash>", and writes an
// INPROGRESS record under it with a conditional write. When the write
// succeeds the function runs, and its result is saved as a COMPLETED record
// that lasts an hour (WithExpiresAfter). When a record already holds the key:
//
//   - a COMPLETED one answers the call with its stored result, decoded into
//     R, after WithResponseHook if set;
//   - an INPROGRESS one fails the call with an AlreadyInProgressError
//     (ErrAlreadyInProgress), which is safe to retry;
//   - an INPROGRESS one whose invocation ran past its deadline, or any
//     expired one, is replaced and the function runs again.
//
// The in-progress record expires with the invocation: its deadline, from the
// context Lambda passes (ctx.Deadline), is stored with it, so that an
// invocation that times out does not lock its key until the record expires.
//
// When the function returns an error, the context's cancellation included,
// the record is deleted and the error returned, so that a retry runs it
// again; a panic deletes the record and goes on. Errors of the store are
// PersistenceErrors (ErrPersistence).
//
// # The idempotency key
//
// The payload is marshalled with encoding/json/v2; WithEventKeyJMESPath
// selects part of it with a JMESPath expression (the Powertools functions,
// such as powertools_json, are available, and WithJMESPathFunctions adds
// more), the whole payload otherwise. The selection is hashed with MD5
// (WithHashFunction picks another of Python's hashlib names, WithHash any
// hash.Hash) after rendering it as Python's json.dumps(sort_keys=True)
// does, so that both runtimes compute the same hash.
//
// The prefix is, as in Python, the Lambda function's name (AWS_LAMBDA_FUNCTION_NAME,
// "test-func" outside Lambda) and the Go function's qualified name from the
// runtime, such as "orders.github.com/me/app/payments.charge". Names of
// closures are positional ("main.main.func1"): give those a stable prefix
// with WithKeyPrefix, which also makes the key independent of renames.
//
// A payload whose selection is empty (null, "", an empty object or array,
// an object or array of nulls) has no key: the function runs without
// idempotency, after a warning, or fails with ErrNoIdempotencyKey under
// WithKeyRequired. 0 and false are keys.
//
// # Options
//
//   - WithEventKeyJMESPath: the part of the payload the key is made of.
//   - WithPayloadValidationJMESPath: the part of the payload that must not
//     change for the same key (ErrPayloadValidation otherwise).
//   - WithJMESPathFunctions: custom JMESPath functions.
//   - WithKeyRequired: a missing key is an error (raise_on_no_idempotency_key).
//   - WithExpiresAfter: how long records last, an hour by default.
//   - WithLocalCache, WithLocalCacheMaxItems: keep COMPLETED records in
//     memory too (256 by default), per function and execution environment.
//   - WithHashFunction, WithHash: the hash, MD5 by default.
//   - WithKeyPrefix: the key's prefix.
//   - WithResponseHook: rewrite replayed responses.
//
// Idempotent panics with ErrInvalidConfig when an option cannot work (an
// expression that does not compile, an unknown hash name, a hook for another
// result type): build idempotent functions in main or at package level.
//
// # Environment variables
//
//   - POWERTOOLS_IDEMPOTENCY_DISABLED: a true value calls the function
//     directly, for local development and tests. It warns once.
//   - AWS_LAMBDA_FUNCTION_NAME: the first part of the default key prefix.
//
// # Stores
//
// Package idempotency/dynamodb stores records in DynamoDB, package
// idempotency/redis in Redis, Valkey or any Redis-compatible cache (ElastiCache,
// MemoryDB), with Python's schemas. MemoryStore keeps them in memory, for
// tests. Implement Store for anything else: it only moves records, while
// Idempotent owns keys, validation, the cache and the status logic, so one
// store serves any number of functions with different options.
//
// # Differences from Python
//
//   - One function, Idempotent, covers both decorators. The result is stored
//     as JSON and decoded into R, so output serializers are not needed; R
//     must round-trip through encoding/json/v2.
//   - The payload is the typed E marshalled to JSON: fields E does not
//     declare do not take part in the key. Take jsontext.Value as E to hash
//     the raw event, byte for byte as Python sees it.
//   - Numbers: JSON integers and floats hash as in Python. JMESPath, though,
//     works on float64 in Go, so a selected number that is integral hashes as
//     an integer even if the event wrote 10.0 (Python hashes 10.0), and an
//     integer beyond 2^53 loses precision. Hash such values as strings.
//   - The remaining time comes from the context's deadline; there is no
//     register_lambda_context.
//   - Options belong to each idempotent function, not to the store (Python
//     keeps the first configuration a store sees).
//   - Fixed: stored responses that are empty or zero ({}, [], 0, "", false)
//     replay as such, not as None; with a sort key, records read back keep
//     their idempotency key, so the local cache finds them.
//   - The local cache is safe for concurrent invocations (Lambda Managed
//     Instances).
//   - Durable function replays and the boto3 user agent tag are not ported.
package idempotency
