package kms

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	mpl "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygenerated"
	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
	esdk "github.com/aws/aws-encryption-sdk/releases/go/encryption-sdk/awscryptographyencryptionsdksmithygenerated"
	esdktypes "github.com/aws/aws-encryption-sdk/releases/go/encryption-sdk/awscryptographyencryptionsdksmithygeneratedtypes"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// The defaults of Powertools Python's AWSEncryptionSDKProvider.
const (
	// DefaultCacheCapacity is how many data keys the cache holds.
	DefaultCacheCapacity = 100
	// DefaultMaxCacheAge is how long a cached data key serves.
	DefaultMaxCacheAge = 300 * time.Second
	// DefaultMaxMessagesEncrypted is how many messages a cached data key
	// encrypts, 2^32, the most the AWS Encryption SDK allows.
	DefaultMaxMessagesEncrypted = 1 << 32
	// DefaultMaxBytesEncrypted is how many bytes a cached data key encrypts,
	// 2^63-1, the most the AWS Encryption SDK allows.
	DefaultMaxBytesEncrypted = math.MaxInt64
)

// publicKeyContextKey is the pair the signing algorithm suites add to the
// encryption context; decryption ignores it when comparing contexts, as
// Powertools Python does.
const publicKeyContextKey = "aws-crypto-public-key"

// providerName is what the provider's options answer to Provider().
const providerName = "aws-encryption-sdk"

// AlgorithmSuite is an AWS Encryption SDK algorithm suite: Python's
// aws_encryption_sdk.identifiers.Algorithm.
type AlgorithmSuite = mpltypes.ESDKAlgorithmSuiteId

// The algorithm suites. The default is AlgAES256GCMHKDFSHA512CommitKeyECDSAP384;
// the suites without a commitment key need WithCommitmentPolicy.
const (
	AlgAES128GCMIV12Tag16                    = mpltypes.ESDKAlgorithmSuiteIdAlgAes128GcmIv12Tag16NoKdf
	AlgAES192GCMIV12Tag16                    = mpltypes.ESDKAlgorithmSuiteIdAlgAes192GcmIv12Tag16NoKdf
	AlgAES256GCMIV12Tag16                    = mpltypes.ESDKAlgorithmSuiteIdAlgAes256GcmIv12Tag16NoKdf
	AlgAES128GCMIV12Tag16HKDFSHA256          = mpltypes.ESDKAlgorithmSuiteIdAlgAes128GcmIv12Tag16HkdfSha256
	AlgAES192GCMIV12Tag16HKDFSHA256          = mpltypes.ESDKAlgorithmSuiteIdAlgAes192GcmIv12Tag16HkdfSha256
	AlgAES256GCMIV12Tag16HKDFSHA256          = mpltypes.ESDKAlgorithmSuiteIdAlgAes256GcmIv12Tag16HkdfSha256
	AlgAES128GCMIV12Tag16HKDFSHA256ECDSAP256 = mpltypes.ESDKAlgorithmSuiteIdAlgAes128GcmIv12Tag16HkdfSha256EcdsaP256
	AlgAES192GCMIV12Tag16HKDFSHA384ECDSAP384 = mpltypes.ESDKAlgorithmSuiteIdAlgAes192GcmIv12Tag16HkdfSha384EcdsaP384
	AlgAES256GCMIV12Tag16HKDFSHA384ECDSAP384 = mpltypes.ESDKAlgorithmSuiteIdAlgAes256GcmIv12Tag16HkdfSha384EcdsaP384
	AlgAES256GCMHKDFSHA512CommitKey          = mpltypes.ESDKAlgorithmSuiteIdAlgAes256GcmHkdfSha512CommitKey
	AlgAES256GCMHKDFSHA512CommitKeyECDSAP384 = mpltypes.ESDKAlgorithmSuiteIdAlgAes256GcmHkdfSha512CommitKeyEcdsaP384
)

// CommitmentPolicy says which algorithm suites the provider encrypts and
// decrypts with: Python's CommitmentPolicy.
type CommitmentPolicy = mpltypes.ESDKCommitmentPolicy

// The commitment policies. The default, as in every AWS Encryption SDK, is
// RequireEncryptRequireDecrypt.
const (
	ForbidEncryptAllowDecrypt    = mpltypes.ESDKCommitmentPolicyForbidEncryptAllowDecrypt
	RequireEncryptAllowDecrypt   = mpltypes.ESDKCommitmentPolicyRequireEncryptAllowDecrypt
	RequireEncryptRequireDecrypt = mpltypes.ESDKCommitmentPolicyRequireEncryptRequireDecrypt
)

var (
	// ErrInvalidConfig reports an option, or a key list, the provider
	// cannot work with.
	ErrInvalidConfig = errors.New("datamasking/kms: invalid configuration")
	// ErrUnknownOption reports a provider option of another provider, or an
	// option given to Decrypt, which takes none.
	ErrUnknownOption = errors.New("datamasking/kms: unknown provider option")
)

// Option configures a Provider.
type Option func(*config)

type config struct {
	cacheCapacity        int
	maxCacheAge          time.Duration
	maxMessagesEncrypted uint64
	maxBytesEncrypted    uint64
	commitmentPolicy     CommitmentPolicy
	maxEncryptedDataKeys int64
	now                  func() time.Time
	errs                 []error
}

func (c *config) invalidf(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...)))
}

// WithLocalCacheCapacity sets how many data keys the cache holds, at least
// 1 (default 100): Python's local_cache_capacity.
func WithLocalCacheCapacity(n int) Option {
	return func(c *config) {
		if n < 1 {
			c.invalidf("cache capacity %d is less than 1", n)
		}
		c.cacheCapacity = n
	}
}

// WithMaxCacheAge sets how long a cached data key serves, more than zero
// (default 300 seconds): Python's max_cache_age_seconds.
func WithMaxCacheAge(d time.Duration) Option {
	return func(c *config) {
		if d <= 0 {
			c.invalidf("max cache age %v is not positive", d)
		}
		c.maxCacheAge = d
	}
}

// WithMaxMessagesEncrypted sets how many messages a cached data key
// encrypts, from 1 to 2^32 (the default): Python's max_messages_encrypted.
func WithMaxMessagesEncrypted(n int64) Option {
	return func(c *config) {
		if n < 1 || n > DefaultMaxMessagesEncrypted {
			c.invalidf("max messages encrypted %d is outside [1, 2^32]", n)
			return
		}
		c.maxMessagesEncrypted = uint64(n)
	}
}

// WithMaxBytesEncrypted sets how many bytes a cached data key encrypts,
// from 0 to 2^63-1 (the default): Python's max_bytes_encrypted. A message
// of this many bytes or more is encrypted under a data key of its own.
func WithMaxBytesEncrypted(n int64) Option {
	return func(c *config) {
		if n < 0 {
			c.invalidf("max bytes encrypted %d is negative", n)
			return
		}
		c.maxBytesEncrypted = uint64(n)
	}
}

// WithCommitmentPolicy sets the commitment policy of the AWS Encryption
// SDK client (default RequireEncryptRequireDecrypt, as in Python). The
// algorithm suites without key commitment need a weaker policy.
func WithCommitmentPolicy(p CommitmentPolicy) Option {
	return func(c *config) {
		if !slices.Contains(p.Values(), p) {
			c.invalidf("commitment policy %q", p)
		}
		c.commitmentPolicy = p
	}
}

// WithMaxEncryptedDataKeys limits how many encrypted data keys a message
// may carry, at least 1 (default: no limit, as in Python). AWS recommends a
// limit when decrypting messages from sources you do not trust, so that a
// message cannot make the function call KMS once per key.
func WithMaxEncryptedDataKeys(n int64) Option {
	return func(c *config) {
		if n < 1 {
			c.invalidf("max encrypted data keys %d is less than 1", n)
		}
		c.maxEncryptedDataKeys = n
	}
}

// withClock replaces the clock of the cache, for the tests.
func withClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

func newConfig(opts []Option) (config, error) {
	c := config{
		cacheCapacity:        DefaultCacheCapacity,
		maxCacheAge:          DefaultMaxCacheAge,
		maxMessagesEncrypted: DefaultMaxMessagesEncrypted,
		maxBytesEncrypted:    DefaultMaxBytesEncrypted,
		commitmentPolicy:     RequireEncryptRequireDecrypt,
		now:                  time.Now,
	}
	for _, opt := range opts {
		opt(&c)
	}
	return c, errors.Join(c.errs...)
}

// WithAlgorithmSuite encrypts with another algorithm suite than the
// default: Python's provider_options={"algorithm": ...}. Pass it to
// datamasking.WithProviderOptions.
func WithAlgorithmSuite(suite AlgorithmSuite) datamasking.ProviderOption {
	return algorithmOption(suite)
}

// WithFrameLength sets the frame length of the message, from 1 to 2^32-1
// bytes (default 4096): Python's provider_options={"frame_length": ...}.
// Pass it to datamasking.WithProviderOptions.
func WithFrameLength(n int64) datamasking.ProviderOption {
	return frameLengthOption(n)
}

type algorithmOption AlgorithmSuite

func (algorithmOption) Provider() string { return providerName }

type frameLengthOption int64

func (frameLengthOption) Provider() string { return providerName }

// Provider encrypts and decrypts with the AWS Encryption SDK: the Go form of
// Powertools Python's AWSEncryptionSDKProvider. Its ciphertexts are base64
// encrypted messages that every AWS Encryption SDK decrypts with the same
// keys. It is safe for concurrent use; build one at package level or in
// main, so that its data key cache serves many invocations.
type Provider struct {
	client     *esdk.Client
	encryptCMM mpltypes.ICryptographicMaterialsManager
	decryptCMM mpltypes.ICryptographicMaterialsManager
	cache      *cachingCMM
}

// NewFromConfig returns a provider for the KMS keys (key ARNs), with KMS
// clients built from cfg; see New.
func NewFromConfig(cfg aws.Config, keys []string, opts ...Option) (*Provider, error) {
	return New(awskms.NewFromConfig(cfg), keys, opts...)
}

// New returns a provider that encrypts under every one of keys, KMS key
// ARNs, and decrypts with any of them: the first key generates each data
// key (kms:GenerateDataKey), each other key wraps it too (kms:Encrypt), and
// decryption calls kms:Decrypt with the keys the message names. Keys may
// live in other regions and accounts: the provider derives a client per
// region from client, which it leaves as it is.
//
// Decryption needs key ARNs. A key ID or an alias encrypts, but fails to
// decrypt with datamasking.ErrDecryptKey, as in Python.
func New(client *awskms.Client, keys []string, opts ...Option) (*Provider, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: nil KMS client", ErrInvalidConfig)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no KMS key", ErrInvalidConfig)
	}
	keyring, err := kmsKeyring(client, keys)
	if err != nil {
		return nil, err
	}
	return NewWithKeyring(keyring, opts...)
}

// kmsKeyring builds the keyring that reproduces Python's
// StrictAwsKmsMasterKeyProvider: a KMS keyring per key, the first one the
// generator.
func kmsKeyring(client *awskms.Client, keys []string) (mpltypes.IKeyring, error) {
	return withDafnyMu(func() (mpltypes.IKeyring, error) {
		materials, err := mpl.NewClient(mpltypes.MaterialProvidersConfig{})
		if err != nil {
			return nil, fmt.Errorf("datamasking/kms: material providers: %w", err)
		}
		clients := regionalClients{base: client, byRegion: map[string]*awskms.Client{}}
		keyrings := make([]mpltypes.IKeyring, 0, len(keys))
		for _, key := range keys {
			keyring, keyringErr := materials.CreateAwsKmsKeyring(context.Background(), mpltypes.CreateAwsKmsKeyringInput{
				KmsClient: clients.forKey(key),
				KmsKeyId:  key,
			})
			if keyringErr != nil {
				return nil, fmt.Errorf("%w: KMS key %q: %w", ErrInvalidConfig, key, keyringErr)
			}
			keyrings = append(keyrings, keyring)
		}
		if len(keyrings) == 1 {
			return keyrings[0], nil
		}
		multi, err := materials.CreateMultiKeyring(context.Background(), mpltypes.CreateMultiKeyringInput{
			Generator:     keyrings[0],
			ChildKeyrings: keyrings[1:],
		})
		if err != nil {
			return nil, fmt.Errorf("%w: KMS keys: %w", ErrInvalidConfig, err)
		}
		return multi, nil
	})
}

// regionalClients derives one KMS client per region from the caller's,
// each with the middleware that releases dafnyMu during requests.
type regionalClients struct {
	base     *awskms.Client
	byRegion map[string]*awskms.Client
}

func (r regionalClients) forKey(key string) *awskms.Client {
	region := regionOf(key)
	if c, ok := r.byRegion[region]; ok {
		return c
	}
	c := awskms.New(r.base.Options(), func(o *awskms.Options) {
		if region != "" {
			o.Region = region
		}
		o.APIOptions = append(slices.Clip(o.APIOptions), releaseDuringCall)
	})
	r.byRegion[region] = c
	return c
}

// regionOf is the region of a key ARN, or "" for a key ID or an alias name,
// which use the client's region.
func regionOf(key string) string {
	if parsed, err := arn.Parse(key); err == nil {
		return parsed.Region
	}
	return ""
}

// NewWithKeyring returns a provider over any AWS Encryption SDK keyring, a
// raw AES keyring in tests for instance, with the provider's data key cache
// and the rest of its behavior: the Go form of Python's key_provider.
//
// The provider runs the keyring under a lock, as the Dafny-generated
// keyrings of the Material Providers Library need; the KMS keyrings New
// builds release it while KMS answers, a keyring of yours keeps it.
func NewWithKeyring(keyring mpltypes.IKeyring, opts ...Option) (*Provider, error) {
	if keyring == nil {
		return nil, fmt.Errorf("%w: nil keyring", ErrInvalidConfig)
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	sdkConfig := esdktypes.AwsEncryptionSdkConfig{CommitmentPolicy: &cfg.commitmentPolicy}
	if cfg.maxEncryptedDataKeys > 0 {
		sdkConfig.MaxEncryptedDataKeys = &cfg.maxEncryptedDataKeys
	}
	client, err := withDafnyMu(func() (*esdk.Client, error) { return esdk.NewClient(sdkConfig) })
	if err != nil {
		return nil, fmt.Errorf("%w: AWS Encryption SDK: %w", ErrInvalidConfig, err)
	}
	cmm, err := withDafnyMu(func() (mpltypes.ICryptographicMaterialsManager, error) {
		materials, mplErr := mpl.NewClient(mpltypes.MaterialProvidersConfig{})
		if mplErr != nil {
			return nil, mplErr //nolint:wrapcheck // wrapped once below
		}
		return materials.CreateDefaultCryptographicMaterialsManager(context.Background(),
			mpltypes.CreateDefaultCryptographicMaterialsManagerInput{Keyring: keyring})
	})
	if err != nil {
		return nil, fmt.Errorf("%w: materials manager: %w", ErrInvalidConfig, err)
	}
	locked := lockedCMM{inner: cmm}
	cache := newCachingCMM(locked, cfg)
	return &Provider{client: client, encryptCMM: cache, decryptCMM: locked, cache: cache}, nil
}

// Encrypt encrypts plaintext into an AWS Encryption SDK message bound to
// the request's encryption context, and returns it in standard base64, as
// Powertools Python does. Data keys come from the cache when they may. It
// accepts the options WithAlgorithmSuite and WithFrameLength.
//
// The AWS Encryption SDK for Go does not hand ctx to KMS: Encrypt returns
// early when ctx is already done, and a KMS call is bounded by the client's
// own timeouts and retries.
func (p *Provider) Encrypt(ctx context.Context, plaintext []byte, req datamasking.Request) (string, error) {
	in := esdktypes.EncryptInput{
		Plaintext:         plaintext,
		EncryptionContext: req.EncryptionContext,
		MaterialsManager:  p.encryptCMM,
	}
	for _, opt := range req.Options {
		switch o := opt.(type) {
		case algorithmOption:
			in.AlgorithmSuiteId = new(AlgorithmSuite(o))
		case frameLengthOption:
			in.FrameLength = new(int64(o))
		default:
			return "", fmt.Errorf("%w: %T for provider %q", ErrUnknownOption, opt, opt.Provider())
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := p.client.Encrypt(ctx, in)
	if err != nil {
		return "", encryptError(err)
	}
	return base64.StdEncoding.EncodeToString(out.Ciphertext), nil
}

// Decrypt decodes and decrypts a message of Encrypt, or of any AWS
// Encryption SDK under the provider's keys, and checks that its encryption
// context is exactly the request's, apart from the public key the signing
// suites add. It calls KMS every time: decryption is not cached, as in
// Python.
func (p *Provider) Decrypt(ctx context.Context, ciphertext string, req datamasking.Request) ([]byte, error) {
	if len(req.Options) > 0 {
		return nil, fmt.Errorf("%w: decrypt takes no provider option", ErrUnknownOption)
	}
	message, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %w", datamasking.ErrDecryptValue, err)
	}
	if len(message) == 0 {
		// The SDK dereferences nil on an empty message.
		return nil, fmt.Errorf("%w: empty message", datamasking.ErrDecryptValue)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out, err := p.client.Decrypt(ctx, esdktypes.DecryptInput{Ciphertext: message, MaterialsManager: p.decryptCMM})
	if err != nil {
		return nil, decryptError(err)
	}
	if !sameContext(out.EncryptionContext, req.EncryptionContext) {
		return nil, datamasking.ErrContextMismatch
	}
	return out.Plaintext, nil
}

// sameContext compares the context of a message with the expected one,
// ignoring the public key of the signing suites.
func sameContext(actual, expected map[string]string) bool {
	n := 0
	for k, v := range actual {
		if k == publicKeyContextKey {
			continue
		}
		if want, ok := expected[k]; !ok || want != v {
			return false
		}
		n++
	}
	return n == len(expected)
}
