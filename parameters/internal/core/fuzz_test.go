package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"testing"
)

func FuzzDecodeBase64(f *testing.F) {
	for _, seed := range []string{"", "YWJj", "YWJ", "YQ==YQ==", "Y W\nJj", "====", "SGVsbG8sIFdvcmxkIQ==", "\xff\xfe"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := DecodeBase64(data)
		if err != nil {
			return
		}
		// Whatever decodes, re-encodes to the alphabet characters it was
		// decoded from.
		var alphabet []byte
		for _, c := range data {
			if c == '=' {
				break
			}
			if isBase64(c) {
				alphabet = append(alphabet, c)
			}
		}
		if got := base64.RawStdEncoding.EncodeToString(decoded); got != string(alphabet) {
			// The last character of a group may carry bits the decoder
			// drops; compare what those bits decode to instead.
			again, err := base64.RawStdEncoding.DecodeString(got)
			if err != nil || !bytes.Equal(again, decoded) {
				t.Fatalf("%q decoded to %q, which encodes to %q", data, decoded, got)
			}
		}
	})
}

func FuzzApply(f *testing.F) {
	for _, seed := range []string{`{"a":1}`, `{"a":1,"a":2}`, `[`, `"x"`, `aGk=`, "\x00"} {
		f.Add("name.json", []byte(seed))
		f.Add("name.binary", []byte(seed))
		f.Add("name", []byte(seed))
	}
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		for _, transform := range []string{TransformNone, TransformJSON, TransformBinary, TransformAuto} {
			v, err := Apply(name, data, transform)
			if err != nil {
				continue
			}
			if v.Kind == KindJSON && !jsontext.Value(data).IsValid(jsontext.AllowDuplicateNames(true)) {
				t.Fatalf("invalid JSON accepted: %q", data)
			}
			if _, err := Convert[any](v); err != nil && v.Kind != KindJSON {
				t.Fatalf("a %v value does not convert to any: %v", v.Kind, err)
			}
		}
	})
}
