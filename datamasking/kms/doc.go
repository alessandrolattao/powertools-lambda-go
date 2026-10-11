// Package kms is the AWS Encryption SDK provider of package datamasking,
// with AWS KMS keys: the Go port of Powertools for AWS Lambda (Python)
// AWSEncryptionSDKProvider.
//
// It encrypts with envelope encryption: a data key encrypts the data, the
// KMS keys encrypt the data key, and the result is an AWS Encryption SDK
// message (message format 2, framed, signed with ECDSA P-384 by default)
// that Encrypt returns in standard base64. Powertools Python, and any other
// AWS Encryption SDK, decrypts it with the same keys, and this package
// decrypts theirs.
//
//	provider, err := kms.NewFromConfig(cfg, []string{keyARN})
//	masker := datamasking.New(datamasking.WithProvider(provider))
//
// It uses the official AWS Encryption SDK for Go
// (github.com/aws/aws-encryption-sdk/releases/go/encryption-sdk v0.4.0) and
// its Material Providers Library v0.4.0.
//
// # Keys
//
// New and NewFromConfig take KMS key ARNs, as Python's
// StrictAwsKmsMasterKeyProvider does: the first key generates each data key
// (kms:GenerateDataKey), every other key encrypts it too (kms:Encrypt), so
// that any of them decrypts the message (kms:Decrypt). Keys can live in
// other regions and accounts; the provider derives a KMS client per region
// from the one you give it. A key ID or an alias encrypts, but cannot
// decrypt: decryption fails with datamasking.ErrDecryptKey.
//
// NewWithKeyring takes any AWS Encryption SDK keyring instead, a raw AES
// keyring in tests for instance, as Python's key_provider does.
//
// # Data key caching
//
// Encryption reuses data keys, as Python's CachingCryptoMaterialsManager
// does, which the AWS Encryption SDK for Go does not ship: the package
// implements it over the SDK's materials manager interface. Each encryption
// context (and algorithm suite asked for) gets a data key of its own,
// reused until it is older than WithMaxCacheAge (default 300 seconds) or
// has encrypted more than WithMaxMessagesEncrypted messages (default 2^32)
// or WithMaxBytesEncrypted bytes (default 2^63-1); the cache holds
// WithLocalCacheCapacity keys (default 100), dropping the least recently
// used. A message of WithMaxBytesEncrypted bytes or more, and the suites
// without a key derivation function, never use the cache. Concurrent
// encryptions that need the same new key wait for one KMS request.
// Decryption is not cached: it calls KMS every time, as in Python.
//
// # Options
//
// Per call, datamasking.WithProviderOptions takes WithAlgorithmSuite and
// WithFrameLength (Python's provider_options algorithm and frame_length).
// The provider takes the cache options above, WithCommitmentPolicy (default
// RequireEncryptRequireDecrypt, which only allows the suites with key
// commitment) and WithMaxEncryptedDataKeys (default unlimited, as in
// Python; AWS recommends a limit when decrypting messages from sources you
// do not trust).
//
// # Encryption context
//
// The context goes into the message as additional authenticated data and,
// in plaintext, into the KMS calls (and so into CloudTrail): never put
// secrets in it. Decrypt compares the message's context with the one it is
// given, ignoring the aws-crypto-public-key pair the signing suites add,
// and fails with datamasking.ErrContextMismatch unless they are equal.
// Powertools adds no pair of its own.
//
// # Errors
//
// Encryption failures of a keyring or of KMS are datamasking.ErrEncryptKey;
// decryption failures to obtain the data key are datamasking.ErrDecryptKey;
// bad base64, a malformed, tampered or forbidden message, too many data
// keys, are datamasking.ErrDecryptValue. Other failures keep their cause.
//
// # Concurrency and context
//
// A Provider is safe for concurrent use. The generated code of the AWS
// Encryption SDK for Go writes to a variable its runtime shares across
// goroutines whenever a keyring or a materials manager runs, so the package
// runs those under one process-wide lock; message parsing, encryption and
// signatures run in parallel, and the KMS clients the package builds release
// the lock while KMS answers. A keyring given to NewWithKeyring keeps the
// lock for its whole run.
//
// The SDK does not hand the context to KMS: Encrypt and Decrypt return
// early when the context is already done, and a KMS call is bounded by the
// timeouts and retries of the client's aws.Config.
//
// # IAM and sizing
//
// The function needs kms:GenerateDataKey on the first key, kms:Encrypt on
// the others, kms:Decrypt on the keys it decrypts with. Powertools
// recommends separate functions for encrypting and decrypting, and at least
// 1024 MB of memory, since the work is CPU bound.
//
// # Differences from Python
//
//   - The KMS client comes from an aws.Config or an *kms.Client; Python
//     builds it from a botocore session. The Material Providers Library
//     needs the concrete client, so there is no narrow interface to fake:
//     test with NewWithKeyring and a raw AES keyring, or against an
//     emulator.
//   - The commitment policy and the encrypted data key limit are options;
//     Python keeps the SDK defaults.
//   - Base64 is decoded strictly, where Python skips characters outside the
//     alphabet.
//   - Python registers a "data-masking" feature in the KMS user agent; an
//     unofficial port does not pose as Powertools.
package kms
