package auth

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimsAccessors(t *testing.T) {
	t.Parallel()
	raw := `{"iss":"https://idp.example.com/","sub":"user-1","jti":"j1","aud":["a","b"],` +
		`"exp":1800003600,"nbf":1800000000.5,"iat":1800000000,"scp":["orders:read"],` +
		`"tenant":"t1","admin":true,"roles":["x"],"profile":{"name":"Ada"},"none":null}`
	c, err := ParseClaims([]byte(raw))
	require.NoError(t, err)

	assert.Equal(t, "https://idp.example.com/", c.Issuer())
	assert.Equal(t, "user-1", c.Subject())
	assert.Equal(t, "j1", c.ID())
	assert.Equal(t, []string{"a", "b"}, c.Audience())
	assert.Equal(t, time.Unix(1800003600, 0), c.ExpiresAt())
	assert.Equal(t, time.Unix(1800000000, 500_000_000), c.NotBefore())
	assert.Equal(t, time.Unix(1800000000, 0), c.IssuedAt())
	assert.Equal(t, []string{"orders:read"}, c.Scopes())

	tenant, ok := c.GetString("tenant")
	assert.True(t, ok)
	assert.Equal(t, "t1", tenant)
	_, ok = c.GetString("admin")
	assert.False(t, ok)

	admin, ok := c.Get("admin")
	assert.True(t, ok)
	assert.Equal(t, true, admin)
	profile, ok := c.Get("profile")
	assert.True(t, ok)
	assert.Equal(t, map[string]any{"name": "Ada"}, profile)
	_, ok = c.Get("missing")
	assert.False(t, ok)

	assert.True(t, c.Has("none"))
	assert.False(t, c.Has("missing"))
	value, ok := c.Lookup("roles")
	assert.True(t, ok)
	assert.JSONEq(t, `["x"]`, string(value))
	_, ok = c.Lookup("missing")
	assert.False(t, ok)

	assert.JSONEq(t, raw, string(c.Raw()))
	marshalled, err := json.Marshal(c)
	require.NoError(t, err)
	assert.JSONEq(t, raw, string(marshalled))
}

func TestClaimsAs(t *testing.T) {
	t.Parallel()
	type profile struct {
		Subject string   `json:"sub"`
		Tenant  string   `json:"tenant"`
		Roles   []string `json:"roles"`
		Expiry  int64    `json:"exp"`
	}
	c, err := ParseClaims([]byte(`{"sub":"user-1","tenant":"t1","roles":["admin"],"exp":1800000000,"extra":1}`))
	require.NoError(t, err)

	p, err := c.As[profile]()
	require.NoError(t, err)
	assert.Equal(t, profile{Subject: "user-1", Tenant: "t1", Roles: []string{"admin"}, Expiry: 1800000000}, p)

	_, err = c.As[struct {
		Tenant int `json:"tenant"`
	}]()
	require.Error(t, err)
}

func TestClaimsDefaults(t *testing.T) {
	t.Parallel()
	c, err := ParseClaims([]byte(`{"aud":"one","exp":"soon","scope":{"a":1},"iss":1}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"one"}, c.Audience())
	assert.True(t, c.ExpiresAt().IsZero())
	assert.True(t, c.NotBefore().IsZero())
	assert.Nil(t, c.Scopes())
	assert.Empty(t, c.Issuer())

	c, err = ParseClaims([]byte(`{"aud":{"x":1}}`))
	require.NoError(t, err)
	assert.Nil(t, c.Audience())

	for _, payload := range []string{``, `null`, `[]`, `"x"`, `{`, `{"a":1,"a":2}`} {
		_, err := ParseClaims([]byte(payload))
		requireReason(t, err, ReasonInvalidClaims)
	}
}

func TestClaimsAreImmutable(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"sub":"user-1","roles":["a"]}`)
	c, err := ParseClaims(payload)
	require.NoError(t, err)
	copy(payload, `{"sub":"user-2"`)
	assert.Equal(t, "user-1", c.Subject())

	raw := c.Raw()
	raw[8] = 'X'
	value, _ := c.Lookup("roles")
	value[1] = 'X'
	assert.Equal(t, "user-1", c.Subject())
	again, _ := c.Lookup("roles")
	assert.JSONEq(t, `["a"]`, string(again))
}
