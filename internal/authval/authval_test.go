package authval

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHTTPSURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url    string
		issuer bool
		ok     bool
	}{
		{"https://idp.example.com/", true, true},
		{"HTTPS://idp.example.com", true, true},
		{"https://idp.example.com:443/realms/x", true, true},
		{"https://[::1]:8443/", true, true},
		{"https://idp.example.com/?", true, true},
		{"https://idp.example.com/#", true, true},
		{"https://idp.example.com/keys?tenant=1", false, true},
		{"https://idp.example.com/keys?tenant=1", true, false},
		{"http://idp.example.com/", false, false},
		{"https:idp.example.com", false, false},
		{"https:///path", false, false},
		{"https://user@idp.example.com/", false, false},
		{"https://@idp.example.com/", false, false},
		{"https://idp.example.com/#frag", false, false},
		{"https://idp.example.com:65536/", false, false},
		{"https://idp.example.com:x/", false, false},
		{"https://idp.example.com/a b", false, false},
		{"https://idp.example.com/\x00", false, false},
		{"https://idp.example.com/ ", false, false},
		{"https://[::1/", false, false},
		{"https://idp.example.com/\xff", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.ok, HTTPSURL(tc.url, tc.issuer), "%q issuer=%v", tc.url, tc.issuer)
	}
}

func TestStringsAndScopes(t *testing.T) {
	t.Parallel()
	got, ok := Strings([]string{"b", "a", "b"})
	assert.True(t, ok)
	assert.Equal(t, []string{"b", "a"}, got)
	_, ok = Strings([]string{"a", " \t"})
	assert.False(t, ok)
	got, ok = Strings(nil)
	assert.True(t, ok)
	assert.Empty(t, got)

	got, ok = Scopes([]string{"orders:read", "inventory/write", "orders:read"})
	assert.True(t, ok)
	assert.Equal(t, []string{"orders:read", "inventory/write"}, got)
	for _, bad := range []string{"a b", `a"b`, `a\b`, "café", "\x7f", ""} {
		_, ok = Scopes([]string{bad})
		assert.False(t, ok, "%q", bad)
	}
	assert.True(t, ScopeToken("!#$%&'()*+,-./:;<=>?@[]^_`{|}~"))
}

func TestNonBlank(t *testing.T) {
	t.Parallel()
	assert.True(t, NonBlank("a"))
	assert.False(t, NonBlank(""))
	assert.False(t, NonBlank(" \n"))
	assert.False(t, NonBlank("\xff"))
}
