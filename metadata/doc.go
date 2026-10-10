// Package metadata reads the Lambda Metadata Endpoint: what Lambda knows
// about the execution environment a function runs in, such as the ID of its
// Availability Zone.
//
// # Usage
//
//	md, err := metadata.Get(ctx)
//	if err != nil {
//		return err
//	}
//	logger.Info("serving", "az_id", md.AvailabilityZoneID) // "use1-az1"
//
// An Availability Zone ID names the same physical zone in every account, so
// a function can prefer the replica of a cache or a database in its own zone.
// Converting it to a zone name (us-east-1a) takes ec2:DescribeAvailabilityZones.
//
// The first call asks the endpoint, a local HTTP server Lambda runs in every
// execution environment (GET
// http://$AWS_LAMBDA_METADATA_API/2026-01-15/metadata/execution-environment,
// authenticated with the bearer token in AWS_LAMBDA_METADATA_TOKEN), with a
// one second timeout (WithTimeout). The answer cannot change during the
// environment's life, so it is cached until the process ends; calling Get in
// main has it ready before the first invocation. Fields AWS adds to the
// response later are in LambdaMetadata.Raw.
//
// # Outside Lambda
//
// When AWS_LAMBDA_INITIALIZATION_TYPE is not set (tests, local runs) or
// POWERTOOLS_DEV is on, Get returns empty metadata and no error, without a
// request, so that code using it needs no setup to run anywhere. To test
// with real values, point AWS_LAMBDA_METADATA_API at an httptest.Server,
// set AWS_LAMBDA_INITIALIZATION_TYPE and AWS_LAMBDA_METADATA_TOKEN, and call
// ClearCache between cases.
//
// # Errors
//
// A failure is a *LambdaMetadataError, with the HTTP status when the endpoint
// answered, wrapping ErrNoEndpoint, ErrNoToken, ErrStatus, ErrResponse or the
// transport's error. Failures are not cached.
//
// # Environment variables
//
//	AWS_LAMBDA_METADATA_API         the endpoint's address, set by Lambda
//	AWS_LAMBDA_METADATA_TOKEN       the endpoint's token, set by Lambda
//	AWS_LAMBDA_INITIALIZATION_TYPE  set by Lambda: Get runs in Lambda
//	POWERTOOLS_DEV                  on: Get returns empty metadata
//
// # Concurrency
//
// Lambda Managed Instances start several invocations at once in one process.
// Concurrent first calls share one request to the endpoint; each returns
// when it completes, or when its own context ends.
//
// # Differences from Powertools Python
//
//   - get_lambda_metadata is Get, which takes a context; its timeout is an
//     option. clear_metadata_cache is ClearCache.
//   - POWERTOOLS_DEV is read like every other Powertools truth value (true,
//     1, yes, on, ...), where Python's metadata utility accepts only "true"
//     and "1".
//   - The cache is safe for concurrent use, and concurrent first calls share
//     one request.
//   - A response that is not a JSON object (null, an array) is an
//     ErrResponse, where Python fails with an unrelated TypeError.
//   - Python's SnapStart cache invalidation has no Go counterpart: SnapStart
//     does not run the provided runtimes Go functions use.
package metadata
