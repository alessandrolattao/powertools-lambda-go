package kms

import (
	"errors"
	"fmt"

	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
	primitives "github.com/aws/aws-cryptographic-material-providers-library/releases/go/primitives/awscryptographyprimitivessmithygeneratedtypes"
	esdktypes "github.com/aws/aws-encryption-sdk/releases/go/encryption-sdk/awscryptographyencryptionsdksmithygeneratedtypes"
	"github.com/aws/smithy-go"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// encryptError maps a failure of the SDK to the errors of datamasking, as
// Python maps GenerateKeyError to DataMaskingEncryptKeyError: a keyring or
// KMS refusing the key is ErrEncryptKey, anything else keeps its cause.
func encryptError(err error) error {
	if keyError(err) {
		return fmt.Errorf("%w: %w", datamasking.ErrEncryptKey, err)
	}
	return fmt.Errorf("aws encryption sdk: encrypt: %w", err)
}

// decryptError maps a failure of the SDK to the errors of datamasking, as
// Python maps DecryptKeyError to DataMaskingDecryptKeyError and malformed
// messages to DataMaskingDecryptValueError.
func decryptError(err error) error {
	switch {
	case messageError(err):
		return fmt.Errorf("%w: %w", datamasking.ErrDecryptValue, err)
	case keyError(err):
		return fmt.Errorf("%w: %w", datamasking.ErrDecryptKey, err)
	default:
		return fmt.Errorf("aws encryption sdk: decrypt: %w", err)
	}
}

// keyError reports a keyring that could not obtain, wrap or unwrap a data
// key: a KMS error, or a failure of the Material Providers Library such as
// "no encrypted data key found to match".
func keyError(err error) bool {
	return isType[smithy.APIError](err) ||
		isType[mpltypes.AwsCryptographicMaterialProvidersException](err) ||
		isType[mpltypes.CollectionOfErrors](err)
}

// messageError reports a message the SDK cannot read or authenticate:
// malformed, truncated, tampered with, carrying too many data keys, or
// written under an algorithm suite the commitment policy forbids.
func messageError(err error) bool {
	return isType[esdktypes.AwsEncryptionSdkException](err) ||
		isType[primitives.AwsCryptographicPrimitivesError](err) ||
		isType[mpltypes.InvalidAlgorithmSuiteInfoOnDecrypt](err) ||
		isType[mpltypes.InvalidAlgorithmSuiteInfo](err)
}

// isType reports whether err, or an error it wraps, is a T.
func isType[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}
