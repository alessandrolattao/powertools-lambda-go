# Data masking

The data masking utility can encrypt, decrypt, or irreversibly erase
sensitive information to protect data confidentiality. It is the Go port of
the [Powertools for AWS Lambda (Python) data masking utility](https://docs.powertools.aws.dev/lambda/python/latest/utilities/data_masking/):
package `datamasking` erases, package `datamasking/kms` encrypts and decrypts
with the AWS Encryption SDK and AWS KMS keys.

## Key features

* Encrypt, decrypt, or irreversibly erase data with ease
* Erase sensitive information in one or more fields within nested data, with
  the JSONPath expressions Powertools Python accepts
* Seamless integration with the [AWS Encryption SDK](https://docs.aws.amazon.com/encryption-sdk/latest/developer-guide/introduction.html)
  for industry and AWS security best practices: messages encrypted in Go
  decrypt in Python, and the other way round

## Terminology

**Erasing** replaces sensitive information **irreversibly** with a
non-sensitive placeholder (`*****`), or with a customized mask. It happens in
memory and cannot be undone.

**Encrypting** transforms plaintext into ciphertext with a cryptographic key,
so that only those allowed to use the key can read it back. **Decrypting**
transforms the ciphertext back into plaintext.

**Encryption context** is non-secret `key=value` data bound to the ciphertext,
such as `tenant_id:<id>`, that decryption must present again.

**[Encrypted message](https://docs.aws.amazon.com/encryption-sdk/latest/developer-guide/message-format.html)**
is the portable structure the AWS Encryption SDK writes: the encrypted data,
the encrypted data keys, and everything needed to authenticate and decrypt
it.

**[Envelope encryption](https://docs.aws.amazon.com/encryption-sdk/latest/developer-guide/concepts.html#envelope-encryption)**
uses two keys: a data key encrypts the data, a KMS key (the master key)
encrypts the data key.

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go
```

Erasing needs `datamasking` only. Encrypting needs `datamasking/kms`, which
brings in the AWS Encryption SDK for Go and its Material Providers Library:
about 7.4 MB of stripped linux/arm64 binary and 3 ms of package
initialization on top of `datamasking`. Go only links what you import, so a
function that only erases does not carry it.

### Required resources

Encryption needs a symmetric KMS key. The function needs `kms:GenerateDataKey`
on the first key it encrypts with, `kms:Encrypt` on the others (see
[multiple keys](#using-multiple-keys)), and `kms:Decrypt` on the keys it
decrypts with.

We recommend at least 1024 MB of memory (the work is CPU bound) and separate
functions for encrypting and decrypting.

```yaml
Resources:
  DataMaskingKey:
    Type: AWS::KMS::Key
    Properties:
      KeyPolicy:
        Version: "2012-10-17"
        Statement:
          - Effect: Allow
            Principal:
              AWS: !Sub arn:aws:iam::${AWS::AccountId}:root
            Action: kms:*
            Resource: "*"
  EncryptFunction:
    Type: AWS::Serverless::Function
    Properties:
      Runtime: provided.al2023
      Architectures: [arm64]
      Handler: bootstrap
      MemorySize: 1024
      Environment:
        Variables:
          KMS_KEY_ARN: !GetAtt DataMaskingKey.Arn
      Policies:
        - Statement:
            - Effect: Allow
              Action: [kms:GenerateDataKey, kms:Encrypt]
              Resource: !GetAtt DataMaskingKey.Arn
```

### Erasing data

Erasing replaces the data with `*****`. Without `WithFields` the whole data
is erased: an array keeps its length, every element becoming `*****`;
anything else becomes the string `*****`.

```go
var masker = datamasking.New()

func handler(ctx context.Context, event events.APIGatewayV2HTTPRequest) (any, error) {
	return masker.Erase([]byte(event.Body), datamasking.WithFields("email", "address.street", "company_address"))
}
```

=== "Input"

    ```json
    {
      "name": "John Doe",
      "email": "johndoe@example.com",
      "address": {"street": "123 Main St", "zip": "12345"},
      "company_address": {"street": "456 ACME Ave", "zip": "12345"}
    }
    ```

=== "Output"

    ```json
    {
      "name": "John Doe",
      "email": "*****",
      "address": {"street": "*****", "zip": "12345"},
      "company_address": "*****"
    }
    ```

`Erase` accepts raw JSON (`[]byte`, `jsontext.Value`), any Go value that
`encoding/json/v2` marshals (structs through their JSON field names, maps,
`aws-lambda-go` events), and scalars. The caller's data is never modified.
It returns a decoded JSON value; `EraseAs` decodes the result into a type of
your choice, raw JSON included:

```go
type customer struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

c, err := masker.EraseAs[customer](customer{Name: "Pat", Email: "pat@example.com"}, datamasking.WithFields("email"))
// c.Email == "*****"

raw, err := masker.EraseAs[jsontext.Value](`{"card_number": "1111 2222 3333 4444"}`, datamasking.WithFields("card_number"))
// {"card_number":"*****"}
```

#### Custom masking

Masks keep the structure of objects and arrays and mask every scalar inside
as text:

| Option | Behavior | Example |
| --- | --- | --- |
| `WithDynamicMask()` | each character becomes `*`, except `-`, `_`, `.` and space | `"123 Main St"` gives `"*** **** **"` |
| `WithCustomMask(mask)` | the mask replaces the text, cut to its length when longer | `"XX"` on `"12345"` gives `"XX"` |
| `WithRegex(pattern, format)` | what the pattern matches is replaced by the format, with `\1` or `\g<name>` for groups | `(.)(.*)(@.*)` and `\1****\3` give `"j****@example.com"` |

```go
masker.Erase(body, datamasking.WithFields("address.street"), datamasking.WithDynamicMask())
masker.Erase(body, datamasking.WithFields("address.zip"), datamasking.WithCustomMask("XX"))
masker.Erase(body, datamasking.WithFields("email"), datamasking.WithRegex(`(.)(.*)(@.*)`, `\1****\3`))
```

Numbers and booleans are masked as the text Python's `str()` writes, so that
masks have the same length as in Python: `30` gives `"**"`, `true` gives
`"****"`.

`WithMaskingRules` gives each field its own masks. Rules apply in order, and
take precedence over the other options:

```go
erased, err := masker.Erase(body, datamasking.WithMaskingRules(
	datamasking.Rule{Path: "email", RegexPattern: `(.)(.*)(@.*)`, MaskFormat: `\1****\3`},
	datamasking.Rule{Path: "age", DynamicMask: true},
	datamasking.Rule{Path: "address.zip", CustomMask: "xxx"},
	datamasking.Rule{Path: "$.other_address[?(@.postcode > 12000)]", CustomMask: "Masked"},
))
```

=== "Input"

    ```json
    {
      "age": 30,
      "email": "janedoe@example.com",
      "address": {"zip": "12345"},
      "other_address": [{"postcode": 11345}, {"postcode": 67890}]
    }
    ```

=== "Output"

    ```json
    {
      "age": "**",
      "email": "j****@example.com",
      "address": {"zip": "xxx"},
      "other_address": [{"postcode": 11345}, "Masked"]
    }
    ```

With rules, every selected value is masked as text, objects and arrays as
their Python representation (`['Pat', 'powertools']`), and `null` values are
left alone, as in Python.

### Encrypting data

Encrypting needs a provider. `kms.NewFromConfig` builds the AWS Encryption SDK
provider for your KMS keys; build it once, outside the handler, so that its
data key cache serves every invocation:

```go
var masker *datamasking.DataMasking

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	provider, err := kms.NewFromConfig(cfg, []string{os.Getenv("KMS_KEY_ARN")})
	if err != nil {
		panic(err)
	}
	masker = datamasking.New(datamasking.WithProvider(provider))
	lambda.Start(handler)
}

func handler(ctx context.Context, event map[string]any) (map[string]string, error) {
	encrypted, err := masker.Encrypt(ctx, event["body"])
	if err != nil {
		return nil, err
	}
	return map[string]string{"body": encrypted}, nil
}
```

`Encrypt` marshals the data to JSON and returns a base64 AWS Encryption SDK
message, the same format Powertools Python returns. Raw JSON is encrypted as
the document it is; a Go string is a JSON string, as in Python.

### Decrypting data

Decryption works with KMS key **ARNs** only: a key ID or an alias fails with
`datamasking.ErrDecryptKey`.

```go
func handler(ctx context.Context, event map[string]string) (any, error) {
	return masker.Decrypt(ctx, event["body"])
}
```

`Decrypt` returns a decoded JSON value; `DecryptAs` decodes into your type:

```go
type order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

o, err := masker.DecryptAs[order](ctx, encrypted)
```

### Encryption context for integrity and authenticity

The encryption context is additional authenticated data: decryption must
present exactly the same pairs, or it fails with
`datamasking.ErrContextMismatch`.

!!! danger "Important considerations"
    1. **Exact match on decrypt.** Do not use values you cannot provide again,
       such as timestamps.
    2. **Never put secrets in it.** KMS logs it in plaintext in CloudTrail.

```go
tenant := datamasking.WithEncryptionContext(map[string]string{
	"data_classification": "confidential",
	"data_type":           "customer-data",
	"tenant_id":           "a06bf973-0734-4b53-9072-39d7ac5b2cba",
})

encrypted, err := masker.Encrypt(ctx, data, tenant)
decrypted, err := masker.Decrypt(ctx, encrypted, tenant)
```

### Choosing parts of your data

`WithFields` takes JSONPath expressions in the dialect of
[jsonpath-ng](https://github.com/h2non/jsonpath-ng#jsonpath-syntax), the one
Powertools Python uses, evaluated by [ojg](https://github.com/ohler55/ojg).
Expressions apply in order, so later ones see what earlier ones erased.

| Scenario | Expression |
| --- | --- |
| Top keys | `card_number` |
| Nested key | `address.postcode` |
| Multiple keys | `address.postcode`, `address.street` |
| All key items (an array becomes `["*****", ...]`, an object `"*****"`) | `address` |
| Complex nested key | `category..name` |
| All fields in a list | `address[*].street` |
| An index, from the end | `address[1].street`, `address[-1].street` |
| Complex expressions (the matching elements are erased) | `$.address[?(@.postcode > 12000)]` |
| Quoted names | `a.'1'.None`, `a..'4'` |

The leading `$` is optional. The package accepts the part of jsonpath-ng
that Powertools documents and tests, and rejects the rest with
`datamasking.ErrInvalidPath`:

* members: `name`, `a.b`, names with `-`, `_` and `@`, quoted names (`'a b'`,
  `"it's"`), numbers as names (`a.1`), lists of names (`a.b,c`, `['b','c']`);
* `*` and `[*]`; descendants `..`;
* indexes and slices: `[0]`, `[-1]`, `[0,2]`, `[1:3]`, `[-2:]`, `[::2]`;
* filters: `[?(@.postcode > 12000)]`, `[?postcode > 12000]`, `[?(@.kind)]`,
  `[?(!@.kind)]`, joined with `&` and grouped with parentheses, with `==`,
  `=`, `!=`, `<`, `<=`, `>`, `>=` and `=~` against integers, decimals, `true`,
  `false`, quoted strings and bare words.

Not supported: `|`, `&` between paths, `where`, `wherenot`, named operators
(`` `len` ``, `` `this` ``), arithmetic and sorting. Comparisons do not
convert types: jsonpath-ng reads `"12345"` as a number to compare it with
`12000`, ojg does not.

An expression that matches nothing fails with `datamasking.ErrFieldNotFound`.
With `datamasking.WithRaiseOnMissingField(false)` it is skipped instead, and
reported on the Powertools debug log (`POWERTOOLS_DEBUG=true`).

#### JSON

A JSON string is parsed when fields or rules are given, and the result is
the parsed, updated document:

```go
masker.Erase(`{"card_number": "1111 2222 3333 4444", "address": {"postcode": 12345}}`,
	datamasking.WithFields("card_number", "address.postcode"))
// map[address:map[postcode:*****] card_number:*****]
```

## Advanced

### Data serialization

Go values are turned into JSON with `encoding/json/v2` before masking and
encrypting, which stands for Python's support of Pydantic models,
dataclasses and objects with `dict()`. JSON integers stay `int64` and
decimals `float64`, as Python keeps `int` and `float` apart.
`datamasking.WithJSONOptions` passes `encoding/json/v2` options to every
marshal and unmarshal, the Go form of Python's custom serializer and
deserializer:

```go
masker := datamasking.New(
	datamasking.WithProvider(provider),
	datamasking.WithJSONOptions(json.FormatNilSliceAsNull(true)),
)
```

### Using multiple keys

You can encrypt under several KMS keys, from several accounts and regions,
for higher availability: the first key generates the data key, every other
key encrypts it too, and any of them decrypts the message.

```go
provider, err := kms.NewFromConfig(cfg, []string{
	os.Getenv("KMS_KEY_ARN_1"),
	os.Getenv("KMS_KEY_ARN_2"),
})
```

### Providers

#### AWS Encryption SDK

| Option | Default | Description |
| --- | --- | --- |
| `kms.WithLocalCacheCapacity` | `100` | how many data keys the cache holds |
| `kms.WithMaxCacheAge` | `300s` | how long a cached data key serves |
| `kms.WithMaxMessagesEncrypted` | `4294967296` | how many messages a cached data key encrypts |
| `kms.WithMaxBytesEncrypted` | `9223372036854775807` | how many bytes a cached data key encrypts |
| `kms.WithCommitmentPolicy` | `RequireEncryptRequireDecrypt` | which algorithm suites encrypt and decrypt |
| `kms.WithMaxEncryptedDataKeys` | no limit | how many data keys a message may carry |

```go
provider, err := kms.NewFromConfig(cfg, []string{os.Getenv("KMS_KEY_ARN")},
	kms.WithLocalCacheCapacity(200),
	kms.WithMaxCacheAge(400*time.Second),
	kms.WithMaxMessagesEncrypted(200),
	kms.WithMaxBytesEncrypted(2000),
)
```

##### Passing additional SDK arguments

`datamasking.WithProviderOptions` is the escape hatch Python calls
`provider_options`. The AWS Encryption SDK provider accepts the algorithm
suite and the frame length:

```go
encrypted, err := masker.Encrypt(ctx, data, datamasking.WithProviderOptions(
	kms.WithAlgorithmSuite(kms.AlgAES256GCMHKDFSHA512CommitKey),
))
```

#### Your own provider

A provider implements `datamasking.Provider`: it receives the JSON document
of the data and returns its own ciphertext text. A provider that also
implements `datamasking.Eraser` masks values its own way, and can fall back
to `datamasking.MaskValue`.

### Data masking request flow

Erasing happens in memory, with no provider.

Encrypting asks KMS for a data key (`GenerateDataKey`, plus `Encrypt` for
every other key), caches it, encrypts the data with it and writes the
encrypted message. Decrypting asks KMS to decrypt the data key (`Decrypt`),
checks authenticity and integrity, decrypts the data and discards the key.

#### Caching encrypt operations

Without caching, every encryption would ask KMS for a new data key. The
provider reuses a data key until it exceeds one of the thresholds above;
each encryption context gets a data key of its own. Decryption is never
cached. The AWS Encryption SDK for Go has no caching materials manager, so
the package implements the Python SDK's on the Go SDK's materials manager
interface.

#### Concurrency

The provider is safe for concurrent use, including on Lambda Managed
Instances. The AWS Encryption SDK for Go is generated from Dafny, and its
runtime writes to a variable it shares across goroutines whenever a keyring
runs; the provider runs keyrings under one process-wide lock, and its KMS
clients release the lock while KMS answers, so the rest of the work runs in
parallel.

## Testing your code

### Testing erase operation

Erasing is pure in-memory work: test it as is.

```go
func TestHandler(t *testing.T) {
	masker := datamasking.New()
	got, err := masker.Erase(map[string]any{"testkey": "testvalue"}, datamasking.WithFields("testkey"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"testkey": "*****"}, got)
}
```

### Testing encryption without KMS

`kms.NewWithKeyring` builds the same provider over any AWS Encryption SDK
keyring, such as a raw AES keyring with a local key:

```go
func rawAESKeyring(t *testing.T) mpltypes.IKeyring {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	materials, err := mpl.NewClient(mpltypes.MaterialProvidersConfig{})
	require.NoError(t, err)
	keyring, err := materials.CreateRawAesKeyring(t.Context(), mpltypes.CreateRawAesKeyringInput{
		KeyNamespace: "tests", KeyName: "local", WrappingKey: key,
		WrappingAlg: mpltypes.AesWrappingAlgAlgAes256GcmIv12Tag16,
	})
	require.NoError(t, err)
	return keyring
}

func TestEncrypt(t *testing.T) {
	provider, err := kms.NewWithKeyring(rawAESKeyring(t))
	require.NoError(t, err)
	masker := datamasking.New(datamasking.WithProvider(provider))

	encrypted, err := masker.Encrypt(t.Context(), map[string]any{"card": "1111"})
	require.NoError(t, err)
	decrypted, err := masker.Decrypt(t.Context(), encrypted)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"card": "1111"}, decrypted)
}
```

Here `mpl` and `mpltypes` are the `awscryptographymaterialproviderssmithygenerated`
and `awscryptographymaterialproviderssmithygeneratedtypes` packages of
`github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl`.
For KMS itself, test against an emulator, as this repository does with Floci.

## Differences from Python

* Masking rules are an ordered slice of `datamasking.Rule`, not a dict, and
  accept a JSON string as fields do.
* `$` selects and erases the whole data; jsonpath-ng matches it but updates
  nothing.
* Field selection needs a JSON object or array.
* Objects masked as text list their keys in sorted order.
* Regular expressions use Go's RE2 syntax (no lookarounds, no
  backreferences); mask formats follow Python's `re.sub`.
* The commitment policy and the encrypted data key limit are options.
* No user agent feature is registered: an unofficial port does not pose as
  Powertools.
