package datamasking_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// fakeProvider "encrypts" into base64 and binds the encryption context to
// the ciphertext, as the fake of the Powertools tests does.
type fakeProvider struct{}

func (fakeProvider) Encrypt(_ context.Context, plaintext []byte, req datamasking.Request) (string, error) {
	ec, err := jsontextMarshal(req.EncryptionContext)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ec) + "." + base64.StdEncoding.EncodeToString(plaintext), nil
}

func (fakeProvider) Decrypt(_ context.Context, ciphertext string, req datamasking.Request) ([]byte, error) {
	ecPart, dataPart, ok := strings.Cut(ciphertext, ".")
	if !ok {
		return nil, datamasking.ErrDecryptValue
	}
	ec, err := base64.StdEncoding.DecodeString(ecPart)
	if err != nil {
		return nil, datamasking.ErrDecryptValue
	}
	want, err := jsontextMarshal(req.EncryptionContext)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(ec, want) {
		return nil, datamasking.ErrContextMismatch
	}
	plaintext, err := base64.StdEncoding.DecodeString(dataPart)
	if err != nil {
		return nil, datamasking.ErrDecryptValue
	}
	return plaintext, nil
}

// jsontextMarshal writes a context with sorted keys.
func jsontextMarshal(ec map[string]string) ([]byte, error) {
	if ec == nil {
		ec = map[string]string{}
	}
	return json.Marshal(ec, json.Deterministic(true))
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()
	type order struct {
		ID    string  `json:"id"`
		Total float64 `json:"total"`
	}
	cases := []struct {
		name string
		data any
		want any
	}{
		{name: "int", data: -1, want: int64(-1)},
		{name: "float", data: -1.11, want: -1.11},
		{name: "bool", data: true, want: true},
		{name: "nil", data: nil, want: nil},
		{name: "string", data: "this is a string", want: "this is a string"},
		{name: "JSON string stays a string", data: `{"a":1}`, want: `{"a":1}`},
		{name: "list", data: []any{1, 2, "a string", 3.4}, want: []any{int64(1), int64(2), "a string", 3.4}},
		{name: "dict", data: map[string]any{"a": map[string]any{"1": map[string]any{"None": "hello"}}}, want: map[string]any{"a": map[string]any{"1": map[string]any{"None": "hello"}}}},
		{name: "struct", data: order{ID: "o-1", Total: 12.5}, want: map[string]any{"id": "o-1", "total": 12.5}},
		{name: "raw JSON", data: []byte(` {"b": [1, 2] , "a": "x"} `), want: map[string]any{"b": []any{int64(1), int64(2)}, "a": "x"}},
		{name: "raw JSON value", data: jsontext.Value(`[true]`), want: []any{true}},
	}
	masker := datamasking.New(datamasking.WithProvider(fakeProvider{}))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ciphertext, err := masker.Encrypt(t.Context(), tc.data)
			require.NoError(t, err)
			got, err := masker.Decrypt(t.Context(), ciphertext)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEncryptionContext(t *testing.T) {
	t.Parallel()
	masker := datamasking.New(datamasking.WithProvider(fakeProvider{}))
	ec := map[string]string{"data_classification": "confidential", "tenant_id": "a06bf973"}

	ciphertext, err := masker.Encrypt(t.Context(), map[string]any{"a": 1}, datamasking.WithEncryptionContext(ec))
	require.NoError(t, err)

	got, err := masker.Decrypt(t.Context(), ciphertext, datamasking.WithEncryptionContext(ec))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": int64(1)}, got)

	_, err = masker.Decrypt(t.Context(), ciphertext, datamasking.WithEncryptionContext(map[string]string{"data_classification": "confidential"}))
	require.ErrorIs(t, err, datamasking.ErrContextMismatch)

	_, err = masker.Decrypt(t.Context(), ciphertext)
	require.ErrorIs(t, err, datamasking.ErrContextMismatch)
}

func TestDecryptAs(t *testing.T) {
	t.Parallel()
	type order struct {
		ID string `json:"id"`
	}
	masker := datamasking.New(datamasking.WithProvider(fakeProvider{}))
	ciphertext, err := masker.Encrypt(t.Context(), order{ID: "o-1"})
	require.NoError(t, err)

	o, err := masker.DecryptAs[order](t.Context(), ciphertext)
	require.NoError(t, err)
	assert.Equal(t, order{ID: "o-1"}, o)

	raw, err := masker.DecryptAs[[]byte](t.Context(), ciphertext)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"o-1"}`, string(raw))

	_, err = masker.DecryptAs[int](t.Context(), ciphertext)
	require.ErrorIs(t, err, datamasking.ErrDecryptValue)

	_, err = masker.DecryptAs[order](t.Context(), "not a ciphertext")
	require.ErrorIs(t, err, datamasking.ErrDecryptValue)
}

func TestCryptoErrors(t *testing.T) {
	t.Parallel()
	masker := datamasking.New()
	_, err := masker.Encrypt(t.Context(), "hello world")
	require.ErrorIs(t, err, datamasking.ErrNoProvider)
	_, err = masker.Decrypt(t.Context(), "hello world")
	require.ErrorIs(t, err, datamasking.ErrNoProvider)

	withProvider := datamasking.New(datamasking.WithProvider(fakeProvider{}))
	_, err = withProvider.Encrypt(t.Context(), make(chan int))
	require.ErrorIs(t, err, datamasking.ErrUnsupportedType)
	_, err = withProvider.Encrypt(t.Context(), []byte(`{"a":`))
	require.ErrorIs(t, err, datamasking.ErrUnsupportedType)

	notJSON := base64.StdEncoding.EncodeToString([]byte("{}")) + "." + base64.StdEncoding.EncodeToString([]byte("not json"))
	_, err = withProvider.Decrypt(t.Context(), notJSON)
	require.ErrorIs(t, err, datamasking.ErrDecryptValue)
}

// recordingProvider keeps what it is asked to do.
type recordingProvider struct {
	fakeProvider

	got *datamasking.Request
}

func (p recordingProvider) Encrypt(ctx context.Context, plaintext []byte, req datamasking.Request) (string, error) {
	*p.got = req
	return p.fakeProvider.Encrypt(ctx, plaintext, req)
}

type namedOption string

func (o namedOption) Provider() string { return string(o) }

func TestProviderOptionsReachTheProvider(t *testing.T) {
	t.Parallel()
	var got datamasking.Request
	masker := datamasking.New(datamasking.WithProvider(recordingProvider{got: &got}))
	ec := map[string]string{"k": "v"}
	_, err := masker.Encrypt(t.Context(), 1, datamasking.WithEncryptionContext(ec), datamasking.WithProviderOptions(namedOption("test")))
	require.NoError(t, err)
	assert.Equal(t, ec, got.EncryptionContext)
	assert.Equal(t, []datamasking.ProviderOption{namedOption("test")}, got.Options)

	ec["k"] = "changed"
	assert.Equal(t, "v", got.EncryptionContext["k"], "the context is copied")
}
