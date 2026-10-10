// Package dynamodb keeps idempotency records in Amazon DynamoDB: the
// persistence layer Powertools Python calls DynamoDBPersistenceLayer, with
// the same item schema and the same conditional writes, so that Go and
// Python functions can share a table.
//
// # Table
//
// One table can serve every idempotent function. Unless the attributes are
// renamed, it needs:
//
//   - partition key "id", a string;
//   - time to live on "expiration", so that DynamoDB removes expired records
//     (it may take up to 48 hours: records carry their own expiry, which is
//     what counts).
//
// The function needs dynamodb:GetItem, dynamodb:PutItem, dynamodb:UpdateItem
// and dynamodb:DeleteItem on it.
//
// # Usage
//
//	cfg, err := config.LoadDefaultConfig(ctx)
//	if err != nil { ... }
//	store, err := dynamodb.FromConfig(cfg, os.Getenv("IDEMPOTENCY_TABLE"))
//	if err != nil { ... }
//	lambda.Start(idempotency.Idempotent(handle, store))
//
// New takes any Client, the four methods of *dynamodb.Client the store
// calls: a client with a custom endpoint (DynamoDB Local), retryer or
// middleware, or a fake in tests.
//
// # Items
//
//	Attribute               Type  Content
//	id                      S     the idempotency key, or the static partition key value
//	(sort key)              S     the idempotency key, on tables with a composite key
//	expiration              N     when the record expires, epoch seconds
//	in_progress_expiration  N     when an in-progress record times out, epoch milliseconds
//	status                  S     INPROGRESS or COMPLETED
//	data                    S     the function's result as JSON, on COMPLETED records
//	validation              S     the payload validation hash, when validation is on
//
// Every name can be changed: WithKeyAttr, WithExpiryAttr,
// WithInProgressExpiryAttr, WithStatusAttr, WithDataAttr,
// WithValidationKeyAttr.
//
// A table with a composite primary key (WithSortKeyAttr) stores the
// idempotency key in the sort key, under a partition key of
// "idempotency#<function name>" (AWS_LAMBDA_FUNCTION_NAME), or the value of
// WithStaticPKValue.
//
// # Requests
//
// A first call costs a conditional PutItem and an UpdateItem (2 WCU); a
// failed function a DeleteItem. The PutItem succeeds when the key is free,
// expired, or held by an in-progress record whose invocation timed out:
//
//	attribute_not_exists(#id) OR #expiry < :now OR
//	(#status = :inprogress AND attribute_exists(#in_progress_expiry) AND #in_progress_expiry < :now_in_millis)
//
// When it fails, DynamoDB returns the item that holds the key
// (ReturnValuesOnConditionCheckFailure ALL_OLD), so a replay costs no read.
// Records must fit the 400 KB item limit, response included.
//
// # Differences from Python
//
// Records read back from a table with a composite key keep their idempotency
// key; Python takes the static partition key for it, which keeps them out of
// its local cache. Boto3 sessions and configs are an aws.Config here, and
// the user agent tag is not added.
package dynamodb
