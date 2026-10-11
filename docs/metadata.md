# Metadata

The Metadata utility reads the
[AWS Lambda Metadata Endpoint](https://docs.aws.amazon.com/lambda/latest/dg/configuration-metadata-endpoint.html):
what Lambda knows about the execution environment a function runs in, such as
the ID of its Availability Zone.

## Key features

* Fetch execution environment metadata from the Lambda Metadata Endpoint
* Automatic caching for the life of the execution environment
* Graceful fallback to empty metadata outside Lambda (local runs, tests)
* One shared request for concurrent first calls (Lambda Managed Instances)
* Forward-compatible result: fields AWS adds later are kept in `Raw`

## Getting started

### Usage

```go
func handle(ctx context.Context, event Event) (Response, error) {
	md, err := metadata.Get(ctx)
	if err != nil {
		return Response{}, err
	}
	return Response{AZID: md.AvailabilityZoneID}, nil // e.g. "use1-az1"
}
```

???+ tip
    The answer cannot change while the execution environment lives, so it is
    cached: only the first call makes a request.

You can also fetch the metadata eagerly, in `main`, so that it is ready before
the first invocation:

```go
func main() {
	md, err := metadata.Get(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	azID = md.AvailabilityZoneID
	lambda.Start(handle)
}
```

### Available metadata

| Field | Type | Description |
|---|---|---|
| `AvailabilityZoneID` | `string` | The Availability Zone ID the function runs in (e.g. `use1-az1`) |
| `Raw` | `jsontext.Value` | The whole response, for the fields AWS adds later |

An Availability Zone ID names the same physical zone in every account, unlike
a zone name (`us-east-1a`): use it to prefer the replica of a cache or a
database in the function's own zone. Converting it to a zone name takes
`ec2:DescribeAvailabilityZones`.

## Advanced

### The request

`Get` sends `GET http://$AWS_LAMBDA_METADATA_API/2026-01-15/metadata/execution-environment`
with `Authorization: Bearer $AWS_LAMBDA_METADATA_TOKEN`, two variables Lambda
sets in every execution environment, with a one second timeout:

```go
md, err := metadata.Get(ctx, metadata.WithTimeout(500*time.Millisecond))
```

`WithHTTPClient` sends the request through your own `http.Client`.

### Errors

A failure is a `*metadata.LambdaMetadataError`, with the HTTP status when the
endpoint answered (`StatusCode`, 0 otherwise). It wraps the cause:

| Cause | When |
|---|---|
| `ErrNoEndpoint` | `AWS_LAMBDA_METADATA_API` is not set |
| `ErrNoToken` | `AWS_LAMBDA_METADATA_TOKEN` is not set |
| `ErrStatus` | the endpoint answered other than 200 (401: bad token) |
| `ErrResponse` | the body is not a JSON object |
| the transport's error | no answer, or the timeout expired |

```go
if mdErr, ok := errors.AsType[*metadata.LambdaMetadataError](err); ok {
	slog.Warn("no metadata", "status", mdErr.StatusCode, "err", err)
}
```

Failures are not cached: the next call asks again.

### Lambda Managed Instances

Several invocations start at once in one process. Concurrent first calls share
one request to the endpoint, and each returns when the request completes, or
as soon as its own context ends.

## Testing your code

The endpoint does not exist outside Lambda. When
`AWS_LAMBDA_INITIALIZATION_TYPE` is not set, or `POWERTOOLS_DEV` is on, `Get`
returns empty metadata and no error without a request, so code that uses it
runs in tests unchanged:

```go
md, err := metadata.Get(context.Background())
fmt.Printf("%q %v\n", md.AvailabilityZoneID, err) // "" <nil>
```

To test with real values, let an `httptest.Server` play the endpoint, point
the variables at it, and clear the cache between cases:

```go
func TestHandlerUsesTheZone(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"AvailabilityZoneID":"use1-az1"}`))
	}))
	t.Cleanup(endpoint.Close)
	t.Setenv("AWS_LAMBDA_INITIALIZATION_TYPE", "on-demand")
	t.Setenv("AWS_LAMBDA_METADATA_API", strings.TrimPrefix(endpoint.URL, "http://"))
	t.Setenv("AWS_LAMBDA_METADATA_TOKEN", "token")
	metadata.ClearCache()
	t.Cleanup(metadata.ClearCache)

	md, err := metadata.Get(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "use1-az1", md.AvailabilityZoneID)
}
```

## Differences from Powertools Python

| Python | Go |
|---|---|
| `get_lambda_metadata(timeout=1.0)` | `metadata.Get(ctx, metadata.WithTimeout(time.Second))` |
| `clear_metadata_cache()` | `metadata.ClearCache()` |
| `LambdaMetadata.availability_zone_id`, `_raw` | `LambdaMetadata.AvailabilityZoneID`, `Raw` |
| `LambdaMetadataError(message, status_code=-1)` | `*LambdaMetadataError{StatusCode: 0, Err: cause}` |
| `POWERTOOLS_DEV` read as `"true"` or `"1"` only | read like every Powertools truth value |
| module-level cache, not safe for threads | safe for concurrent use; concurrent first calls share one request |
| `null` or a non-object body fails with a `TypeError` | `ErrResponse` |
| SnapStart cache invalidation | not needed: SnapStart does not run the `provided` runtimes |
