package kms_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	mpl "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygenerated"
	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
	"github.com/alessandrolattao/powertools-lambda-go/datamasking/kms"
)

func ExampleNewFromConfig() {
	// In a function, cfg comes from config.LoadDefaultConfig(ctx).
	cfg := aws.Config{Region: "eu-west-1"}

	// Build the provider once, outside the handler, so that its data key
	// cache serves every invocation.
	provider, err := kms.NewFromConfig(cfg,
		[]string{"arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"},
		kms.WithMaxCacheAge(5*time.Minute),
		kms.WithMaxMessagesEncrypted(200),
	)
	if err != nil {
		panic(err)
	}
	masker := datamasking.New(datamasking.WithProvider(provider))
	fmt.Println(masker != nil)
	// Output: true
}

// rawAESKeyring stands in for KMS in tests: an AWS Encryption SDK keyring
// with a local key.
func rawAESKeyring() mpltypes.IKeyring {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	materials, err := mpl.NewClient(mpltypes.MaterialProvidersConfig{})
	if err != nil {
		panic(err)
	}
	keyring, err := materials.CreateRawAesKeyring(context.Background(), mpltypes.CreateRawAesKeyringInput{
		KeyNamespace: "tests", KeyName: "local", WrappingKey: key,
		WrappingAlg: mpltypes.AesWrappingAlgAlgAes256GcmIv12Tag16,
	})
	if err != nil {
		panic(err)
	}
	return keyring
}

func ExampleNewWithKeyring() {
	ctx := context.Background()
	provider, err := kms.NewWithKeyring(rawAESKeyring())
	if err != nil {
		panic(err)
	}
	masker := datamasking.New(datamasking.WithProvider(provider))
	tenant := datamasking.WithEncryptionContext(map[string]string{"tenant_id": "a06bf973"})

	encrypted, err := masker.Encrypt(ctx, map[string]any{"card_number": "1111 2222 3333 4444"}, tenant)
	if err != nil {
		panic(err)
	}
	type card struct {
		Number string `json:"card_number"`
	}
	decrypted, err := masker.DecryptAs[card](ctx, encrypted, tenant)
	if err != nil {
		panic(err)
	}
	fmt.Println(decrypted.Number)
	// Output: 1111 2222 3333 4444
}

func ExampleWithAlgorithmSuite() {
	ctx := context.Background()
	provider, err := kms.NewWithKeyring(rawAESKeyring())
	if err != nil {
		panic(err)
	}
	masker := datamasking.New(datamasking.WithProvider(provider))

	// An unsigned suite: smaller and faster messages, without the ECDSA
	// signature of the default suite.
	encrypted, err := masker.Encrypt(ctx, "secret", datamasking.WithProviderOptions(
		kms.WithAlgorithmSuite(kms.AlgAES256GCMHKDFSHA512CommitKey),
	))
	if err != nil {
		panic(err)
	}
	decrypted, err := masker.Decrypt(ctx, encrypted)
	if err != nil {
		panic(err)
	}
	fmt.Println(decrypted)
	// Output: secret
}
