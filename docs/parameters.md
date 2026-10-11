# Parameters

The parameters utility retrieves one or many values from AWS Systems Manager
Parameter Store, AWS Secrets Manager, AWS AppConfig, Amazon DynamoDB, or a
store of your own, with a cache in front and transforms that decode the
values into the Go types you name.

Packages: `parameters` (the provider, the cache, the transforms, the errors)
and one package per store: `parameters/ssm`, `parameters/secrets`,
`parameters/appconfig`, `parameters/dynamodb`.

## Key features

* Retrieve one or multiple parameters from the underlying store
* Cache values for a given amount of time (five minutes by default), safely
  across concurrent invocations, with concurrent misses sharing one call
* Decode JSON and base64 values into the Go type you ask for
* Write SSM parameters and Secrets Manager secrets
* Bring your own parameter store

## Getting started

### IAM permissions

| Store | Function or method | IAM permission |
| --- | --- | --- |
| SSM | `ssm.GetParameter`, `Provider.Get` | `ssm:GetParameter` |
| SSM | `ssm.GetParameters`, `Provider.GetMultiple` | `ssm:GetParametersByPath` |
| SSM | `ssm.GetParametersByName`, `Provider.GetByName` | `ssm:GetParameter` and `ssm:GetParameters` |
| SSM | `ssm.SetParameter`, `Provider.Set` | `ssm:PutParameter` |
| SSM | decrypting a SecureString | `kms:Decrypt` as well |
| Secrets | `secrets.GetSecret`, `Provider.Get` | `secretsmanager:GetSecretValue` |
| Secrets | `secrets.GetSecretsByName`, `Provider.GetByName` | `secretsmanager:BatchGetSecretValue` and `secretsmanager:GetSecretValue` (plus `secretsmanager:ListSecrets` with `secrets.WithFilters`) |
| Secrets | `secrets.SetSecret`, `Provider.Set` | `secretsmanager:PutSecretValue` and `secretsmanager:CreateSecret` |
| DynamoDB | `Provider.Get` | `dynamodb:GetItem` |
| DynamoDB | `Provider.GetMultiple` | `dynamodb:Query` |
| AppConfig | `appconfig.GetAppConfig`, `Provider.Get` | `appconfig:StartConfigurationSession` and `appconfig:GetLatestConfiguration` |

### Fetching parameters

The shortcuts build a provider from the default AWS configuration on first
use, as Python's high-level functions do. They are generic: you name the Go
type of the value.

```go
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

func handler(ctx context.Context, _ map[string]any) (string, error) {
	// Retrieve a single parameter.
	endpoint, err := ssm.GetParameter[string](ctx, "/lambda-powertools/endpoint_comments")
	if err != nil {
		return "", err
	}
	return endpoint, nil
}

func main() {
	lambda.Start(handler)
}
```

For multiple parameters, use either:

* `ssm.GetParameters` to fetch every parameter under a path, recursively.
  The names come back without the path: `/dev/config` under `/dev` is
  `config`, `/dev/webhook/config` is `webhook/config`.
* `ssm.GetParametersByName` to fetch distinct parameters by their full name,
  each with its own transform, decryption and max age.

```go
package main

import (
	"context"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

func readAll(ctx context.Context) error {
	// Every parameter under /dev, recursively.
	all, err := ssm.GetParameters[string](ctx, "/dev")
	if err != nil {
		return err
	}
	for name, value := range all {
		fmt.Println(name, value)
	}

	// Distinct parameters, each with its own options.
	values, _, err := ssm.GetParametersByName[any](ctx, map[string][]parameters.Option{
		"/develop/service/commons/telemetry/config": {parameters.WithTransform(parameters.TransformJSON)},
		"/no_cache_param":                           {parameters.WithMaxAge(0)},
		"/develop/service/payment/api/capture/url":  nil, // the shared options apply
	})
	if err != nil {
		return err
	}
	fmt.Println(values)
	return nil
}

func main() {
	_ = readAll(context.Background())
}
```

By default `GetParametersByName` fails when any parameter cannot be read.
With `ssm.WithRaiseOnError(false)` it returns the parameters it could read
and, apart, the ones it could not, each with its error. Unlike Python, no
parameter name is reserved for the failures (`_errors`).

```go
values, failed, err := ssm.GetParametersByName[string](ctx, params, ssm.WithRaiseOnError(false))
if err != nil {
	return err // the call itself failed: throttling, access denied
}
for name, cause := range failed {
	log.Printf("parameter %s: %v", name, cause)
}
```

### Setting parameters

`ssm.SetParameter` creates a parameter, or with `ssm.WithOverwrite(true)`
replaces it. A `[]string` is written as a StringList.

```go
package main

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

func write(ctx context.Context) error {
	if _, err := ssm.SetParameter(ctx, "/app/regions", []string{"eu-west-1", "us-east-1"}); err != nil {
		return err
	}
	_, err := ssm.SetParameter(ctx, "/app/api-key", "s3cret",
		ssm.WithParameterType(types.ParameterTypeSecureString),
		ssm.WithOverwrite(true),
	)
	return err
}

func main() {
	_ = write(context.Background())
}
```

### Fetching secrets

```go
package main

import (
	"context"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/secrets"
)

type credentials struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func dbCredentials(ctx context.Context) (credentials, error) {
	return secrets.GetSecret[credentials](ctx, "db-credentials", parameters.WithTransform(parameters.TransformJSON))
}

func main() {
	_, _ = dbCredentials(context.Background())
}
```

### Fetching multiple secrets

`secrets.GetSecretsByName` reads several secrets with `BatchGetSecretValue`,
twenty per call. Secrets are matched by their exact name or ARN. The secrets
that cannot be read do not fail the others: they come back apart, with the
error Secrets Manager gave for each (ResourceNotFoundException,
AccessDeniedException, DecryptionFailure). The call fails only when none can
be read.

```go
values, failed, err := secrets.GetSecretsByName[string](ctx, []string{"api-key", "db-password"})
```

`secrets.WithFilters` narrows the search with Secrets Manager filters, by tag
or description; the names are then matched through a name filter and only
exact matches are kept.

```go
values, failed, err := secrets.GetSecretsByName[string](ctx, []string{"db-password"},
	secrets.WithFilters(types.Filter{Key: types.FilterNameStringTypeTagKey, Values: []string{"team"}}),
)
```

### Setting secrets

`secrets.SetSecret` adds a version to a secret, and creates the secret when
it does not exist yet. A `[]byte` is a binary secret, a string a string
secret, any other value its JSON encoding.

```go
_, err := secrets.SetSecret(ctx, "db-credentials", map[string]string{"user": "admin", "password": "n3w"},
	secrets.WithClientRequestToken(requestID), // idempotent: one version per token
)
```

### Fetching app configurations

```go
package main

import (
	"context"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/appconfig"
)

type config struct {
	LogLevel string `json:"log_level"`
}

func appConfig(ctx context.Context) (config, error) {
	// Profile "my-configuration" of application "my-app" in environment "dev".
	return appconfig.GetAppConfig[config](ctx, "my-configuration", "dev", "my-app",
		parameters.WithTransform(parameters.TransformJSON))
}

func main() {
	_, _ = appConfig(context.Background())
}
```

The first read of a profile takes two API calls, as AppConfig requires; later
reads poll the session, never sooner than the interval AppConfig asks for.
Each application and environment gets its own default provider (Python reuses
the first one for every later call).

### Environment variables

| Setting | Description | Environment variable | Default |
| --- | --- | --- | --- |
| Max age | How long values stay cached, in seconds | `POWERTOOLS_PARAMETERS_MAX_AGE` | `300` |
| SSM decrypt | Whether SSM reads decrypt SecureString parameters | `POWERTOOLS_PARAMETERS_SSM_DECRYPT` | `false` |
| Service name | The default AppConfig application | `POWERTOOLS_SERVICE_NAME` | `service_undefined` |

`parameters.WithMaxAge` and `ssm.WithDecrypt` override them for one read.

## Advanced

### Adjusting cache TTL

```go
endpoint, err := ssm.GetParameter[string](ctx, "/app/endpoint", parameters.WithMaxAge(20*time.Second))
```

A max age of zero or less never caches the value.

### Always fetching the latest

```go
endpoint, err := ssm.GetParameter[string](ctx, "/app/endpoint", parameters.WithForceFetch(true))
```

The fresh value is cached as usual.

### Built-in providers

Create a provider when you need your own SDK client (another region, a role,
retries), and share it across invocations: it is safe for concurrent use.

```go
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/dynamodb"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/secrets"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

var (
	params *ssm.Provider
	vault  *secrets.Provider
	table  *dynamodb.Provider
)

func handler(ctx context.Context, _ map[string]any) (map[string]string, error) {
	password, err := params.Get(ctx, "/app/db/password", ssm.WithDecrypt(true))
	if err != nil {
		return nil, err
	}
	all, err := params.GetMultiple(ctx, "/app", ssm.WithRecursive(true))
	if err != nil {
		return nil, err
	}
	previous, err := vault.Get(ctx, "api-key", secrets.WithVersionStage("AWSPREVIOUS"))
	if err != nil {
		return nil, err
	}
	limits, err := parameters.GetMultiple[string](ctx, table, "limits")
	if err != nil {
		return nil, err
	}
	all["password"], all["previous"], all["limits"] = password, previous, limits["default"]
	return all, nil
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("eu-west-1"))
	if err != nil {
		panic(err)
	}
	params = ssm.FromConfig(cfg)
	vault = secrets.FromConfig(cfg)
	table = dynamodb.FromConfig(cfg, "ParameterTable")
	lambda.Start(handler)
}
```

#### SSM

`ssm.Provider.GetMultiple` reads the direct children of the path only, unless
`ssm.WithRecursive(true)`; the `ssm.GetParameters` shortcut is recursive, as
in Python. `ssm.WithParameterFilters` passes SSM parameter filters.

#### DynamoDB

The table follows Python's schema. A single value is the item whose
partition key `id` is its name, in its `value` attribute; the values under a
path are the items whose partition key is the path, keyed by their sort key
`sk`.

| id | sk | value |
| --- | --- | --- |
| my-param | | my-value |
| my-hash-key | param-a | my-value-a |
| my-hash-key | param-b | my-value-b |

```go
table := dynamodb.FromConfig(cfg, "ParameterTable",
	dynamodb.WithKeyAttribute("pk"), dynamodb.WithSortAttribute("name"), dynamodb.WithValueAttribute("val"))
value, err := table.Get(ctx, "my-param", dynamodb.WithConsistentRead(true))
```

A string attribute is returned as it is, a binary as its bytes, a number as
its text, any other type as its JSON encoding, which `TransformJSON` decodes.
A local DynamoDB is reached through `cfg.BaseEndpoint`.

#### AppConfig

```go
appConfig := appconfig.FromConfig(cfg, "my-app", "prod")
features, err := parameters.Get[map[string]any](ctx, appConfig, "features",
	parameters.WithTransform(parameters.TransformJSON), appconfig.WithMinimumPollInterval(30*time.Second))
```

### Create your own provider

A `Store` has two methods; `parameters.NewProvider` adds the cache and the
transforms.

```go
package main

import (
	"context"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

// vaultStore reads a key/value store such as HashiCorp Vault.
type vaultStore struct {
	read func(ctx context.Context, path string) (map[string]string, error)
}

// mountOptions is an option of this store, carried with the common ones.
type mountOptions struct {
	mount string
}

func withMount(mount string) parameters.Option {
	// The variant keeps values read from different mounts apart in the cache.
	return parameters.WithStoreOption("mount="+mount, func(o *mountOptions) { o.mount = mount })
}

func (v vaultStore) Get(ctx context.Context, name string, opts parameters.StoreOptions) ([]byte, error) {
	o := mountOptions{mount: "secret"}
	opts.Apply(&o)
	data, err := v.read(ctx, o.mount+"/"+name)
	if err != nil {
		return nil, err
	}
	value, ok := data["value"]
	if !ok {
		return nil, parameters.ErrNotFound
	}
	return []byte(value), nil
}

func (v vaultStore) GetMultiple(ctx context.Context, path string, _ parameters.StoreOptions) (map[string][]byte, error) {
	data, err := v.read(ctx, path)
	if err != nil {
		return nil, err
	}
	values := make(map[string][]byte, len(data))
	for name, value := range data {
		values[strings.TrimPrefix(name, path+"/")] = []byte(value)
	}
	return values, nil
}

func main() {
	provider := parameters.NewProvider(vaultStore{read: func(context.Context, string) (map[string]string, error) {
		return map[string]string{"value": "s3cret"}, nil
	}})
	_, _ = provider.Get(context.Background(), "api-key", withMount("kv"))
}
```

### Deserializing values with transforms

| Transform | Value | Go types |
| --- | --- | --- |
| `TransformNone` (default) | as stored | `string`, `[]byte`, `any` (a string) |
| `TransformJSON` | a JSON document | any type the document decodes into |
| `TransformBinary` | base64 | `[]byte`, `string`, `any` (a `[]byte`) |
| `TransformAuto` | by the name's suffix: `.json`, `.binary`, else as stored | as above |

```go
type webhook struct {
	URL string `json:"url"`
}
hook, err := parameters.Get[webhook](ctx, params, "/app/webhook", parameters.WithTransform(parameters.TransformJSON))
cert, err := parameters.Get[[]byte](ctx, params, "/app/cert", parameters.WithTransform(parameters.TransformBinary))
```

A value is cached after its transform, so one name read with two transforms
is fetched twice. Every read decodes a fresh copy: callers never share a map
through the cache.

#### Partial transform failures with GetMultiple

A value whose transform fails is the zero value of its type (Python's
`None`); the others are returned. `parameters.WithRaiseOnTransformError(true)`
fails the whole read instead.

```go
values, err := parameters.GetMultiple[map[string]any](ctx, params, "/app/configs",
	parameters.WithTransform(parameters.TransformJSON))
// values["broken"] is nil when /app/configs/broken is not JSON
```

#### Auto-transform values on suffix

```go
values, err := parameters.GetMultiple[any](ctx, params, "/app", parameters.WithTransform(parameters.TransformAuto))
// "settings.json" decoded from JSON, "key.binary" from base64, the others as stored
```

### Passing additional SDK arguments

Python passes `**sdk_options` to the SDK call. The options that matter are
typed here: `ssm.WithDecrypt`, `ssm.WithRecursive`, `ssm.WithParameterFilters`,
`secrets.WithVersionID`, `secrets.WithVersionStage`, `secrets.WithFilters`,
`appconfig.WithMinimumPollInterval`, `dynamodb.WithConsistentRead`. Those that
change the value read are part of the cache key. Writes take request editors:
`ssm.WithPutParameterInput`, `secrets.WithPutSecretValueInput`,
`secrets.WithCreateSecretInput`.

### Bring your own client

Every provider takes a narrow client interface (`ssm.Client`,
`secrets.Client`, `appconfig.Client`, `dynamodb.Client`) that the SDK client
satisfies: `ssm.New(ssmClient)`. `FromConfig` builds the client from an
`aws.Config`.

## Testing your code

### Mocking parameter values

Give your code a provider, rather than calling the shortcuts, and give your
tests one over a store of their own:

```go
package main

import (
	"context"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

type fixedStore map[string]string

func (s fixedStore) Get(_ context.Context, name string, _ parameters.StoreOptions) ([]byte, error) {
	value, ok := s[name]
	if !ok {
		return nil, parameters.ErrNotFound
	}
	return []byte(value), nil
}

func (fixedStore) GetMultiple(context.Context, string, parameters.StoreOptions) (map[string][]byte, error) {
	return map[string][]byte{}, nil
}

func main() {
	provider := parameters.NewProvider(fixedStore{"/app/endpoint": "https://example.com"})
	endpoint, err := provider.Get(context.Background(), "/app/endpoint")
	fmt.Println(endpoint, err)
}
```

The providers take narrow client interfaces too, so a fake SSM, Secrets
Manager, AppConfigData or DynamoDB client is a few methods.

### Clearing cache

`Provider.ClearCache` empties one provider's cache. `parameters.ClearCaches`
forgets the default providers of the shortcuts, which the next shortcut
builds again with a new SDK client: call it between tests that use the
shortcuts.
