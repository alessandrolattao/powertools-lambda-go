package integration

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/smithy-go/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
	"github.com/alessandrolattao/powertools-lambda-go/datamasking/kms"
)

// kmsKey creates a symmetric KMS key in the region of cfg and returns its
// ARN.
func kmsKey(t *testing.T, cfg aws.Config) string {
	t.Helper()
	out, err := awskms.NewFromConfig(cfg).CreateKey(t.Context(), &awskms.CreateKeyInput{
		Description: aws.String("powertools data masking " + t.Name()),
	})
	require.NoError(t, err)
	return aws.ToString(out.KeyMetadata.Arn)
}

// kmsCalls counts the KMS operations a configuration makes.
type kmsCalls struct {
	mu    sync.Mutex
	count map[string]int
}

func (c *kmsCalls) config(base aws.Config) aws.Config {
	c.count = map[string]int{}
	cfg := base.Copy()
	cfg.APIOptions = append(cfg.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("CountKMSCalls",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
				middleware.InitializeOutput, middleware.Metadata, error,
			) {
				c.mu.Lock()
				c.count[middleware.GetOperationName(ctx)]++
				c.mu.Unlock()
				return next.HandleInitialize(ctx, in)
			}), middleware.After)
	})
	return cfg
}

func (c *kmsCalls) get(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[op]
}

func newKMSMasker(t *testing.T, cfg aws.Config, keys ...string) *datamasking.DataMasking {
	t.Helper()
	provider, err := kms.NewFromConfig(cfg, keys)
	require.NoError(t, err)
	return datamasking.New(datamasking.WithProvider(provider))
}

func TestDataMaskingKMSRoundTrip(t *testing.T) {
	t.Parallel()
	key := kmsKey(t, suite.aws)
	masker := newKMSMasker(t, suite.aws, key)
	value := []any{int64(1), int64(2), "string", 4.5}

	encrypted, err := masker.Encrypt(t.Context(), value)
	require.NoError(t, err)
	decrypted, err := masker.Decrypt(t.Context(), encrypted)
	require.NoError(t, err)
	assert.Equal(t, value, decrypted)

	type order struct {
		ID    string  `json:"id"`
		Total float64 `json:"total"`
	}
	encrypted, err = masker.Encrypt(t.Context(), order{ID: "o-1", Total: 12.5})
	require.NoError(t, err)
	o, err := masker.DecryptAs[order](t.Context(), encrypted)
	require.NoError(t, err)
	assert.Equal(t, order{ID: "o-1", Total: 12.5}, o)
}

func TestDataMaskingKMSEncryptionContext(t *testing.T) {
	t.Parallel()
	masker := newKMSMasker(t, suite.aws, kmsKey(t, suite.aws))
	secure := datamasking.WithEncryptionContext(map[string]string{"this": "is_secure"})

	encrypted, err := masker.Encrypt(t.Context(), "value", secure)
	require.NoError(t, err)
	decrypted, err := masker.Decrypt(t.Context(), encrypted, secure)
	require.NoError(t, err)
	assert.Equal(t, "value", decrypted)

	_, err = masker.Decrypt(t.Context(), encrypted, datamasking.WithEncryptionContext(map[string]string{"this": "different_context"}))
	require.ErrorIs(t, err, datamasking.ErrContextMismatch)

	plain, err := masker.Encrypt(t.Context(), "value")
	require.NoError(t, err)
	_, err = masker.Decrypt(t.Context(), plain, secure)
	require.ErrorIs(t, err, datamasking.ErrContextMismatch)
}

func TestDataMaskingKMSMultipleKeys(t *testing.T) {
	t.Parallel()
	// The second key lives in another region: the provider must call KMS
	// there for it.
	otherRegion := suite.aws.Copy()
	otherRegion.Region = "eu-west-1"
	first, second := kmsKey(t, suite.aws), kmsKey(t, otherRegion)
	require.Contains(t, second, ":eu-west-1:")

	encrypted, err := newKMSMasker(t, suite.aws, first, second).Encrypt(t.Context(), "value")
	require.NoError(t, err)

	for _, key := range []string{first, second} {
		decrypted, decryptErr := newKMSMasker(t, suite.aws, key).Decrypt(t.Context(), encrypted)
		require.NoError(t, decryptErr, key)
		assert.Equal(t, "value", decrypted)
	}

	_, err = newKMSMasker(t, suite.aws, kmsKey(t, suite.aws)).Decrypt(t.Context(), encrypted)
	require.ErrorIs(t, err, datamasking.ErrDecryptKey, "a key the message was not encrypted under")
}

func TestDataMaskingKMSKeyErrors(t *testing.T) {
	t.Parallel()
	key := kmsKey(t, suite.aws)
	alias := "alias/powertools-" + strings.ToLower(unique(t))
	_, err := awskms.NewFromConfig(suite.aws).CreateAlias(t.Context(), &awskms.CreateAliasInput{
		AliasName: aws.String(alias), TargetKeyId: aws.String(key),
	})
	require.NoError(t, err)

	// An alias encrypts, but only a key ARN decrypts, as in Python.
	byAlias := newKMSMasker(t, suite.aws, alias)
	encrypted, err := byAlias.Encrypt(t.Context(), "value")
	require.NoError(t, err)
	_, err = byAlias.Decrypt(t.Context(), encrypted)
	require.ErrorIs(t, err, datamasking.ErrDecryptKey)
	decrypted, err := newKMSMasker(t, suite.aws, key).Decrypt(t.Context(), encrypted)
	require.NoError(t, err)
	assert.Equal(t, "value", decrypted)

	missing := strings.Replace(key, key[strings.LastIndex(key, "/")+1:], "00000000-0000-0000-0000-000000000000", 1)
	_, err = newKMSMasker(t, suite.aws, missing).Encrypt(t.Context(), "value")
	require.ErrorIs(t, err, datamasking.ErrEncryptKey)
}

func TestDataMaskingKMSDataKeyCache(t *testing.T) {
	t.Parallel()
	var calls kmsCalls
	cfg := calls.config(suite.aws)
	first, second := kmsKey(t, suite.aws), kmsKey(t, suite.aws)
	masker := newKMSMasker(t, cfg, first, second)
	tenantA := datamasking.WithEncryptionContext(map[string]string{"tenant": "a"})
	tenantB := datamasking.WithEncryptionContext(map[string]string{"tenant": "b"})

	var encrypted string
	for range 5 {
		var err error
		encrypted, err = masker.Encrypt(t.Context(), "value", tenantA)
		require.NoError(t, err)
	}
	_, err := masker.Encrypt(t.Context(), "value", tenantB)
	require.NoError(t, err)
	assert.Equal(t, 2, calls.get("GenerateDataKey"), "one data key per context")
	assert.Equal(t, 2, calls.get("Encrypt"), "the second key wraps each data key")

	for range 3 {
		_, err := masker.Decrypt(t.Context(), encrypted, tenantA)
		require.NoError(t, err)
	}
	assert.Equal(t, 3, calls.get("Decrypt"), "decryption is not cached")
}

func TestDataMaskingKMSConcurrent(t *testing.T) {
	t.Parallel()
	// Many invocations at once through KMS keyrings: with -race, this checks
	// the lock around the SDK's generated code and its release during KMS
	// calls.
	masker := newKMSMasker(t, suite.aws, kmsKey(t, suite.aws), kmsKey(t, suite.aws))
	const workers, rounds = 8, 4
	errs := make([]error, workers)
	var done atomic.Int64
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			tenant := datamasking.WithEncryptionContext(map[string]string{"tenant": strings.Repeat("t", i%3+1)})
			for range rounds {
				encrypted, err := masker.Encrypt(t.Context(), map[string]any{"worker": i}, tenant)
				if err != nil {
					errs[i] = err
					return
				}
				decrypted, err := masker.Decrypt(t.Context(), encrypted, tenant)
				if err != nil {
					errs[i] = err
					return
				}
				if got, ok := decrypted.(map[string]any); ok && got["worker"] == int64(i) {
					done.Add(1)
				}
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(workers*rounds), done.Load())
}
