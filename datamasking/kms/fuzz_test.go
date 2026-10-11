package kms

import (
	"encoding/base64"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// FuzzDecrypt feeds arbitrary ciphertexts to the provider: whatever arrives
// in an event, Decrypt returns the plaintext or an error, never panics.
func FuzzDecrypt(f *testing.F) {
	provider, err := NewWithKeyring(aesKeyring(f))
	if err != nil {
		f.Fatal(err)
	}
	ciphertext, err := provider.Encrypt(f.Context(), []byte(`{"a":1}`), datamasking.Request{})
	if err != nil {
		f.Fatal(err)
	}
	message, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(message)
	f.Add(message[:len(message)/2])
	f.Add([]byte{})
	f.Add([]byte{2, 5, 0x78})
	f.Add([]byte{1, 0x80})
	f.Fuzz(func(t *testing.T, message []byte) {
		_, _ = provider.Decrypt(t.Context(), base64.StdEncoding.EncodeToString(message), datamasking.Request{}) //nolint:errcheck // the property is the absence of panics
	})
}
