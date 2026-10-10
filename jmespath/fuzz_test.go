package jmespath

import (
	"testing"
)

// FuzzSearch feeds arbitrary data to the Powertools functions: whatever an
// event carries, they return a value or an error, never panic.
func FuzzSearch(f *testing.F) {
	f.Add(`{"a":"{\"b\":1}"}`, "powertools_json(a)")
	f.Add(`{"a":"eyJiIjoxfQ=="}`, "powertools_base64(a) | powertools_json(@)")
	f.Add(`{"a":"H4sIAAAAAAAA/6tWSlKyMqwFAD7uuZQIAAAA"}`, "powertools_base64_gzip(a)")
	f.Add(`{"Records":[{"body":"{}"}]}`, SQS)
	f.Fuzz(func(_ *testing.T, data, expression string) {
		_, _ = Query([]byte(data), expression) //nolint:errcheck // the property is the absence of panics
	})
}
