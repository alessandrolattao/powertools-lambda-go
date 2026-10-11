package tracing

import (
	"context"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
)

// FuzzLambdaParent feeds trace headers, which come from the invocation's
// caller, to the parent extraction: a malformed header must start a new trace,
// never fail or panic.
func FuzzLambdaParent(f *testing.F) {
	f.Add(testHeader)
	f.Add("Root=1-5759e988-bd862e3fe1be46a994272793")
	f.Add("Root=1-5759e988-bd862e3fe1be46a994272793;Parent=;Sampled=?")
	f.Add("Root=;;;=;Parent=53995c3f42cd8ad8")
	f.Add("")
	f.Fuzz(func(t *testing.T, header string) {
		ctx := context.WithValue(context.Background(), lambdaTraceHeaderKey, header) //nolint:revive,staticcheck // aws-lambda-go's string key
		sc := trace.SpanContextFromContext(withLambdaParent(ctx))
		if sc.IsValid() && !sc.IsRemote() {
			t.Fatalf("a parent from a header is remote: %v", sc)
		}
	})
}

func FuzzParseDaemonAddress(f *testing.F) {
	f.Add("169.254.79.129:2000")
	f.Add("tcp:127.0.0.1:2000 udp:127.0.0.2:2001")
	f.Add("udp:")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		address := parseDaemonAddress(raw)
		if address == "" || strings.ContainsFunc(address, unicode.IsSpace) {
			t.Fatalf("%q gave the address %q", raw, address)
		}
	})
}

func FuzzSanitizeAnnotationKey(f *testing.F) {
	f.Add("order.id")
	f.Add("città ✓")
	f.Fuzz(func(t *testing.T, key string) {
		got := SanitizeAnnotationKey(key)
		if utf8.RuneCountInString(got) != utf8.RuneCountInString(key) {
			t.Fatalf("%q became %q: a character per character", key, got)
		}
		for _, r := range got {
			if r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				t.Fatalf("%q kept %q", key, r)
			}
		}
	})
}
