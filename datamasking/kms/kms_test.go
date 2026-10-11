package kms

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	mpl "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygenerated"
	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
	esdk "github.com/aws/aws-encryption-sdk/releases/go/encryption-sdk/awscryptographyencryptionsdksmithygenerated"
	esdktypes "github.com/aws/aws-encryption-sdk/releases/go/encryption-sdk/awscryptographyencryptionsdksmithygeneratedtypes"
	"github.com/aws/aws-sdk-go-v2/aws"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// aesKeyring is a raw AES keyring with a fresh key: the AWS Encryption SDK
// without KMS.
func aesKeyring(tb testing.TB) mpltypes.IKeyring {
	tb.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(tb, err)
	keyring, err := withDafnyMu(func() (mpltypes.IKeyring, error) {
		materials, mplErr := mpl.NewClient(mpltypes.MaterialProvidersConfig{})
		if mplErr != nil {
			return nil, mplErr
		}
		return materials.CreateRawAesKeyring(context.Background(), mpltypes.CreateRawAesKeyringInput{
			KeyNamespace: "powertools", KeyName: "test", WrappingKey: key,
			WrappingAlg: mpltypes.AesWrappingAlgAlgAes256GcmIv12Tag16,
		})
	})
	require.NoError(tb, err)
	return keyring
}

func newMasker(tb testing.TB, keyring mpltypes.IKeyring, opts ...Option) (*datamasking.DataMasking, *Provider) {
	tb.Helper()
	provider, err := NewWithKeyring(keyring, opts...)
	require.NoError(tb, err)
	return datamasking.New(datamasking.WithProvider(provider)), provider
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	masker, _ := newMasker(t, aesKeyring(t))
	cases := []struct {
		name string
		data any
		want any
	}{
		{name: "list", data: []any{1, 2, "string", 4.5}, want: []any{int64(1), int64(2), "string", 4.5}},
		{name: "object", data: map[string]any{"a": map[string]any{"1": "hello"}}, want: map[string]any{"a": map[string]any{"1": "hello"}}},
		{name: "string", data: "this is a string", want: "this is a string"},
		{name: "int", data: -1, want: int64(-1)},
		{name: "nil", data: nil, want: nil},
		{name: "unicode", data: "città 😀", want: "città 😀"},
		{name: "large", data: strings.Repeat("x", 100_000), want: strings.Repeat("x", 100_000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ciphertext, err := masker.Encrypt(t.Context(), tc.data)
			require.NoError(t, err)
			_, err = base64.StdEncoding.DecodeString(ciphertext)
			require.NoError(t, err, "the ciphertext is standard base64")
			got, err := masker.Decrypt(t.Context(), ciphertext)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMessageFormat(t *testing.T) {
	t.Parallel()
	keyring := aesKeyring(t)
	masker, _ := newMasker(t, keyring)
	ciphertext, err := masker.Encrypt(t.Context(), map[string]any{"a": 1}, datamasking.WithEncryptionContext(map[string]string{"tenant": "t-1"}))
	require.NoError(t, err)

	// Any AWS Encryption SDK reads the message: here the plain Go SDK, with
	// the same keyring and no Powertools.
	message, err := base64.StdEncoding.DecodeString(ciphertext)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x02, 0x05, 0x78}, message[:3], "message format 2, suite 0x0578")
	client, err := esdk.NewClient(esdktypes.AwsEncryptionSdkConfig{})
	require.NoError(t, err)
	dafnyMu.Lock()
	out, err := client.Decrypt(t.Context(), esdktypes.DecryptInput{Ciphertext: message, Keyring: keyring})
	dafnyMu.Unlock()
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(out.Plaintext))
	assert.Equal(t, "t-1", out.EncryptionContext["tenant"])
	assert.Contains(t, out.EncryptionContext, publicKeyContextKey)
	assert.Len(t, out.EncryptionContext, 2, "Powertools adds nothing to the context")

	// And the other way round.
	dafnyMu.Lock()
	foreign, err := client.Encrypt(t.Context(), esdktypes.EncryptInput{Plaintext: []byte(`["x"]`), Keyring: keyring})
	dafnyMu.Unlock()
	require.NoError(t, err)
	got, err := masker.Decrypt(t.Context(), base64.StdEncoding.EncodeToString(foreign.Ciphertext))
	require.NoError(t, err)
	assert.Equal(t, []any{"x"}, got)
}

func TestEncryptionContext(t *testing.T) {
	t.Parallel()
	masker, _ := newMasker(t, aesKeyring(t))
	ec := map[string]string{"data_classification": "confidential", "data_type": "customer_data"}
	ciphertext, err := masker.Encrypt(t.Context(), []any{1}, datamasking.WithEncryptionContext(ec))
	require.NoError(t, err)

	reordered := map[string]string{"data_type": "customer_data", "data_classification": "confidential"}
	got, err := masker.Decrypt(t.Context(), ciphertext, datamasking.WithEncryptionContext(reordered))
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1)}, got)

	for name, other := range map[string]map[string]string{
		"partial":   {"data_type": "customer_data"},
		"different": {"data_classification": "confidential", "data_type": "other"},
		"larger":    {"data_classification": "confidential", "data_type": "customer_data", "x": "y"},
		"none":      nil,
	} {
		_, mismatch := masker.Decrypt(t.Context(), ciphertext, datamasking.WithEncryptionContext(other))
		require.ErrorIs(t, mismatch, datamasking.ErrContextMismatch, name)
	}

	plain, err := masker.Encrypt(t.Context(), []any{1})
	require.NoError(t, err)
	_, err = masker.Decrypt(t.Context(), plain, datamasking.WithEncryptionContext(map[string]string{"this": "is_secure"}))
	require.ErrorIs(t, err, datamasking.ErrContextMismatch)
}

func TestSameContext(t *testing.T) {
	t.Parallel()
	assert.True(t, sameContext(map[string]string{"a": "b", publicKeyContextKey: "k"}, map[string]string{"a": "b"}))
	assert.True(t, sameContext(nil, nil))
	assert.True(t, sameContext(map[string]string{publicKeyContextKey: "k"}, map[string]string{}))
	assert.False(t, sameContext(map[string]string{"a": "b"}, map[string]string{"a": "c"}))
	assert.False(t, sameContext(map[string]string{"a": "b"}, nil))
	assert.False(t, sameContext(nil, map[string]string{"a": "b"}))
}

func TestDecryptErrors(t *testing.T) {
	t.Parallel()
	masker, _ := newMasker(t, aesKeyring(t))
	ciphertext, err := masker.Encrypt(t.Context(), "secret")
	require.NoError(t, err)
	message, err := base64.StdEncoding.DecodeString(ciphertext)
	require.NoError(t, err)
	tampered := append([]byte(nil), message...)
	tampered[len(tampered)-120] ^= 1

	cases := []struct {
		name       string
		ciphertext string
		want       error
	}{
		{name: "not base64", ciphertext: "hello world", want: datamasking.ErrDecryptValue},
		{name: "empty", ciphertext: "", want: datamasking.ErrDecryptValue},
		{name: "not a message", ciphertext: base64.StdEncoding.EncodeToString([]byte("hello world")), want: datamasking.ErrDecryptValue},
		{name: "truncated", ciphertext: base64.StdEncoding.EncodeToString(message[:len(message)-5]), want: datamasking.ErrDecryptValue},
		{name: "tampered", ciphertext: base64.StdEncoding.EncodeToString(tampered), want: datamasking.ErrDecryptValue},
	}
	for _, tc := range cases {
		_, decryptErr := masker.Decrypt(t.Context(), tc.ciphertext)
		require.ErrorIs(t, decryptErr, tc.want, tc.name)
	}

	other, _ := newMasker(t, aesKeyring(t))
	_, err = other.Decrypt(t.Context(), ciphertext)
	require.ErrorIs(t, err, datamasking.ErrDecryptKey, "another key")
}

func TestProviderOptions(t *testing.T) {
	t.Parallel()
	masker, _ := newMasker(t, aesKeyring(t))

	ciphertext, err := masker.Encrypt(t.Context(), "x", datamasking.WithProviderOptions(
		WithAlgorithmSuite(AlgAES256GCMHKDFSHA512CommitKey), WithFrameLength(1024)))
	require.NoError(t, err)
	message, err := base64.StdEncoding.DecodeString(ciphertext)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x02, 0x04, 0x78}, message[:3], "suite 0x0478")
	got, err := masker.Decrypt(t.Context(), ciphertext)
	require.NoError(t, err)
	assert.Equal(t, "x", got)

	_, err = masker.Encrypt(t.Context(), "x", datamasking.WithProviderOptions(WithAlgorithmSuite(AlgAES256GCMIV12Tag16HKDFSHA384ECDSAP384)))
	require.Error(t, err, "the default policy forbids suites without key commitment")

	_, err = masker.Encrypt(t.Context(), "x", datamasking.WithProviderOptions(WithFrameLength(0)))
	require.Error(t, err)

	_, err = masker.Encrypt(t.Context(), "x", datamasking.WithProviderOptions(otherOption{}))
	require.ErrorIs(t, err, ErrUnknownOption)

	_, err = masker.Decrypt(t.Context(), ciphertext, datamasking.WithProviderOptions(WithFrameLength(1024)))
	require.ErrorIs(t, err, ErrUnknownOption)
}

type otherOption struct{}

func (otherOption) Provider() string { return "other" }

func TestCommitmentPolicy(t *testing.T) {
	t.Parallel()
	keyring := aesKeyring(t)
	legacy, _ := newMasker(t, keyring, WithCommitmentPolicy(ForbidEncryptAllowDecrypt))
	ciphertext, err := legacy.Encrypt(t.Context(), "x", datamasking.WithProviderOptions(WithAlgorithmSuite(AlgAES256GCMIV12Tag16HKDFSHA384ECDSAP384)))
	require.NoError(t, err)

	strict, _ := newMasker(t, keyring)
	_, err = strict.Decrypt(t.Context(), ciphertext)
	require.ErrorIs(t, err, datamasking.ErrDecryptValue, "a message without key commitment")

	lenient, _ := newMasker(t, keyring, WithCommitmentPolicy(RequireEncryptAllowDecrypt))
	got, err := lenient.Decrypt(t.Context(), ciphertext)
	require.NoError(t, err)
	assert.Equal(t, "x", got)
}

func TestMaxEncryptedDataKeys(t *testing.T) {
	t.Parallel()
	first, second := aesKeyring(t), aesKeyring(t)
	both, err := withDafnyMu(func() (mpltypes.IKeyring, error) {
		materials, mplErr := mpl.NewClient(mpltypes.MaterialProvidersConfig{})
		if mplErr != nil {
			return nil, mplErr
		}
		return materials.CreateMultiKeyring(context.Background(), mpltypes.CreateMultiKeyringInput{
			Generator: first, ChildKeyrings: []mpltypes.IKeyring{second},
		})
	})
	require.NoError(t, err)
	masker, _ := newMasker(t, both)
	ciphertext, err := masker.Encrypt(t.Context(), "x")
	require.NoError(t, err)

	// Either key decrypts a message encrypted under both.
	for _, keyring := range []mpltypes.IKeyring{first, second} {
		single, _ := newMasker(t, keyring)
		got, decryptErr := single.Decrypt(t.Context(), ciphertext)
		require.NoError(t, decryptErr)
		assert.Equal(t, "x", got)
	}

	limited, _ := newMasker(t, first, WithMaxEncryptedDataKeys(1))
	_, err = limited.Decrypt(t.Context(), ciphertext)
	require.ErrorIs(t, err, datamasking.ErrDecryptValue, "two data keys, one allowed")
}

func TestConfigErrors(t *testing.T) {
	t.Parallel()
	keyring := aesKeyring(t)
	for _, opts := range [][]Option{
		{WithLocalCacheCapacity(0)},
		{WithMaxCacheAge(0)},
		{WithMaxMessagesEncrypted(0)},
		{WithMaxMessagesEncrypted(DefaultMaxMessagesEncrypted + 1)},
		{WithMaxBytesEncrypted(-1)},
		{WithCommitmentPolicy("NONE")},
		{WithMaxEncryptedDataKeys(0)},
	} {
		_, err := NewWithKeyring(keyring, opts...)
		require.ErrorIs(t, err, ErrInvalidConfig)
	}
	_, err := NewWithKeyring(nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
	_, err = New(nil, []string{"arn"})
	require.ErrorIs(t, err, ErrInvalidConfig)
	_, err = NewFromConfig(aws.Config{Region: "us-east-1"}, nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
	_, err = NewFromConfig(aws.Config{Region: "us-east-1"}, []string{""})
	require.ErrorIs(t, err, ErrInvalidConfig)

	_, err = NewWithKeyring(keyring, WithLocalCacheCapacity(1), WithMaxCacheAge(1), WithMaxMessagesEncrypted(1),
		WithMaxBytesEncrypted(0), WithCommitmentPolicy(RequireEncryptAllowDecrypt), WithMaxEncryptedDataKeys(1))
	require.NoError(t, err)
}

func TestNewBuildsKMSKeyrings(t *testing.T) {
	t.Parallel()
	client := awskms.NewFromConfig(aws.Config{Region: "eu-west-1"})
	provider, err := New(client, []string{
		"arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		"arn:aws:kms:eu-central-1:444455556666:key/0987dcba-09fe-87dc-65ba-ab0987654321",
		"alias/my-key",
	})
	require.NoError(t, err)
	assert.NotNil(t, provider)
	assert.Equal(t, "eu-west-1", client.Options().Region, "the caller's client is left as it is")
	assert.Equal(t, "us-east-1", regionOf("arn:aws:kms:us-east-1:111122223333:key/x"))
	assert.Empty(t, regionOf("1234abcd-12ab-34cd-56ef-1234567890ab"))
}

func TestCanceledContext(t *testing.T) {
	t.Parallel()
	masker, _ := newMasker(t, aesKeyring(t))
	ciphertext, err := masker.Encrypt(t.Context(), "x")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = masker.Encrypt(ctx, "x")
	require.ErrorIs(t, err, context.Canceled)
	_, err = masker.Decrypt(ctx, ciphertext)
	require.ErrorIs(t, err, context.Canceled)
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	// Signed suites, cache misses (one context per goroutine) and hits, and
	// decryptions, all at once: the race detector checks the locking.
	masker, _ := newMasker(t, aesKeyring(t), WithMaxMessagesEncrypted(2))
	const workers, rounds = 8, 5
	results := make([][]any, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			ec := map[string]string{"worker": strings.Repeat("w", i+1)}
			for range rounds {
				ciphertext, err := masker.Encrypt(t.Context(), i, datamasking.WithEncryptionContext(ec))
				if err != nil {
					errs[i] = err
					return
				}
				got, err := masker.Decrypt(t.Context(), ciphertext, datamasking.WithEncryptionContext(ec))
				if err != nil {
					errs[i] = err
					return
				}
				results[i] = append(results[i], got)
			}
		})
	}
	wg.Wait()
	for i := range workers {
		require.NoError(t, errs[i])
		assert.Len(t, results[i], rounds)
		for _, got := range results[i] {
			assert.Equal(t, int64(i), got)
		}
	}
}

func TestEncryptUsesTheCache(t *testing.T) {
	t.Parallel()
	keyring := aesKeyring(t)
	masker, provider := newMasker(t, keyring)
	for _, tenant := range []string{"a", "a", "b", "a"} {
		_, err := masker.Encrypt(t.Context(), "x", datamasking.WithEncryptionContext(map[string]string{"tenant": tenant}))
		require.NoError(t, err)
	}
	assert.Equal(t, 2, provider.cache.len(), "one data key per encryption context")

	uncached, provider := newMasker(t, keyring, WithMaxBytesEncrypted(0))
	_, err := uncached.Encrypt(t.Context(), "x")
	require.NoError(t, err)
	assert.Equal(t, 0, provider.cache.len())
}
