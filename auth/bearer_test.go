package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBearerToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value  string
		token  string
		reason Reason
	}{
		{"", "", ReasonMissingToken},
		{"Bearer abc", "abc", ""},
		{"bearer abc", "abc", ""},
		{"BEARER\tabc", "abc", ""},
		{"  Bearer   abc  ", "abc", ""},
		{"Bearer", "", ReasonInvalidToken},
		{"Bearer a b", "", ReasonInvalidToken},
		{"Basic abc", "", ReasonInvalidToken},
		{"abc", "", ReasonInvalidToken},
		{" ", "", ReasonInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			token, err := bearerToken(tc.value, tc.value != "")
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.token, token)
				return
			}
			requireReason(t, err, tc.reason)
		})
	}
}

func TestEventToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		headers map[string]string
		multi   map[string][]string
		token   string
		reason  Reason
	}{
		{"no headers", nil, nil, "", ReasonMissingToken},
		{"other headers", map[string]string{"Host": "x"}, nil, "", ReasonMissingToken},
		{"canonical", map[string]string{"Authorization": "Bearer a"}, nil, "a", ""},
		{"lower case", map[string]string{"authorization": "Bearer a"}, nil, "a", ""},
		{"two spellings", map[string]string{"Authorization": "Bearer a", "authorization": "Bearer a"}, nil, "", ReasonInvalidToken},
		{"empty value", map[string]string{"authorization": ""}, nil, "", ReasonInvalidToken},
		{"multi only", nil, map[string][]string{"Authorization": {"Bearer a"}}, "a", ""},
		{"multi and single agree", map[string]string{"authorization": "Bearer a"}, map[string][]string{"Authorization": {"Bearer a"}}, "a", ""},
		{"multi and single differ", map[string]string{"authorization": "Bearer a"}, map[string][]string{"Authorization": {"Bearer b"}}, "", ReasonInvalidToken},
		{"multi with two values", nil, map[string][]string{"Authorization": {"Bearer a", "Bearer a"}}, "", ReasonInvalidToken},
		{"multi empty", nil, map[string][]string{"Authorization": {}}, "", ReasonInvalidToken},
		{"multi two spellings", nil, map[string][]string{"Authorization": {"Bearer a"}, "AUTHORIZATION": {"Bearer a"}}, "", ReasonInvalidToken},
		{"wrong scheme", map[string]string{"authorization": "Basic a"}, nil, "", ReasonInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, err := eventToken(tc.headers, tc.multi)
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.token, token)
				return
			}
			requireReason(t, err, tc.reason)
		})
	}
}

func TestVerifyAuthorizationHeaderAndRequest(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)))
	token := key.sign(t, claimsAt(c.Now()), nil)

	claims, err := VerifyAuthorizationHeader(t.Context(), v, "Bearer "+token)
	require.NoError(t, err)
	assert.Equal(t, "user-1", claims.Subject())
	_, err = VerifyAuthorizationHeader(t.Context(), v, "")
	requireReason(t, err, ReasonMissingToken)
	_, err = VerifyAuthorizationHeader(t.Context(), v, "Token "+token)
	requireReason(t, err, ReasonInvalidToken)

	request := func(header http.Header) *http.Request {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com/orders", http.NoBody)
		r.Header = header
		return r
	}
	claims, err = VerifyRequest(v, request(http.Header{"Authorization": {"Bearer " + token}}))
	require.NoError(t, err)
	assert.Equal(t, "user-1", claims.Subject())
	_, err = VerifyRequest(v, request(http.Header{"authorization": {"Bearer " + token}}))
	require.NoError(t, err, "a header map built by hand may hold any spelling")
	_, err = VerifyRequest(v, request(http.Header{}))
	requireReason(t, err, ReasonMissingToken)
	_, err = VerifyRequest(v, request(http.Header{"Authorization": {"Bearer " + token, "Bearer " + token}}))
	requireReason(t, err, ReasonInvalidToken)
	_, err = VerifyRequest(v, request(http.Header{"Authorization": {""}}))
	requireReason(t, err, ReasonInvalidToken)
}

func TestEnforceScopes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		claims   string
		required []string
		reason   Reason
	}{
		{"no scope claim, none required", `{}`, nil, ""},
		{"no scope claim, one required", `{}`, []string{"a"}, ReasonInsufficientScope},
		{"scope string", `{"scope":"a b"}`, []string{"a", "b"}, ""},
		{"double spaces", `{"scope":" a  b "}`, []string{"b"}, ""},
		{"scp list", `{"scp":["a","b"]}`, []string{"b"}, ""},
		{"scopes list", `{"scopes":["a"]}`, []string{"a"}, ""},
		{"scope wins over scp", `{"scope":"a","scp":["b"]}`, []string{"b"}, ReasonInsufficientScope},
		{"missing one", `{"scope":"a"}`, []string{"a", "b"}, ReasonInsufficientScope},
		{"case sensitive", `{"scope":"A"}`, []string{"a"}, ReasonInsufficientScope},
		{"scope number", `{"scope":1}`, nil, ReasonInvalidClaims},
		{"scope null", `{"scope":null}`, nil, ReasonInvalidClaims},
		{"list with a number", `{"scp":["a",1]}`, nil, ReasonInvalidClaims},
		{"invalid scope token", `{"scp":["a\"b"]}`, nil, ReasonInvalidClaims},
		{"tab in a scope", `{"scope":"a\tb"}`, nil, ReasonInvalidClaims},
		{"non-ASCII scope", `{"scope":"café"}`, nil, ReasonInvalidClaims},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims, err := ParseClaims([]byte(tc.claims))
			require.NoError(t, err)
			err = enforceScopes(claims, tc.required)
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			requireReason(t, err, tc.reason)
		})
	}
}
