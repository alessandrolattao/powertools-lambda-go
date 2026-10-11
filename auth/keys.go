package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/lestrrat-go/jwx/v4/jwk"
)

// keySet is a JSON Web Key Set as Powertools reads it: the raw members of
// every key, for the matching rules, next to the public key each one
// decodes to. A key that does not decode stays in the set: it only matters
// if a token selects it.
type keySet struct {
	keys []setKey
}

type setKey struct {
	members map[string]jsontext.Value
	kid     string
	hasKID  bool

	// public is the verification key, nil when the JWK is not a usable
	// RSA, EC or OKP key; curve names the curve of EC and OKP keys.
	public any
	curve  string
}

// parseKeySet reads a JWKS document: an object whose keys member is a list
// of objects. Anything else is an unusable set.
func parseKeySet(doc map[string]jsontext.Value) (*keySet, error) {
	rawKeys, ok := doc["keys"]
	if !ok || rawKeys.Kind() != '[' {
		return nil, fmt.Errorf("%w: a JWKS must hold a list of keys", ErrInvalidConfig)
	}
	var list []jsontext.Value
	if err := json.Unmarshal(rawKeys, &list); err != nil {
		return nil, fmt.Errorf("%w: a JWKS must hold a list of keys", ErrInvalidConfig)
	}
	set := &keySet{keys: make([]setKey, 0, len(list))}
	for _, raw := range list {
		var members map[string]jsontext.Value
		if raw.Kind() != '{' || json.Unmarshal(raw, &members) != nil {
			return nil, fmt.Errorf("%w: every key of a JWKS must be an object", ErrInvalidConfig)
		}
		key := setKey{members: members}
		key.kid, key.hasKID = jsonString(members["kid"])
		key.public, key.curve = decodePublicKey(members)
		set.keys = append(set.keys, key)
	}
	return set, nil
}

// parseKeySetJSON reads a JWKS document from its JSON text.
func parseKeySetJSON(raw []byte) (*keySet, error) {
	var doc map[string]jsontext.Value
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("%w: a JWKS must be a JSON object", ErrInvalidConfig)
	}
	return parseKeySet(doc)
}

// hasKID reports whether any key carries kid, usable or not: the cache only
// refreshes for a kid it has never seen.
func (s *keySet) hasKID(kid string) bool {
	return slices.ContainsFunc(s.keys, func(k setKey) bool { return k.hasKID && k.kid == kid })
}

// signingKey selects the one key that may verify a token with the given
// header. Zero or several matches, or a match that does not fit the
// algorithm, make the token invalid: Powertools never guesses.
func (s *keySet) signingKey(alg Algorithm, kid string) (any, error) {
	var match *setKey
	for i := range s.keys {
		if !s.keys[i].matches(alg, kid) {
			continue
		}
		if match != nil {
			return nil, errInvalidToken()
		}
		match = &s.keys[i]
	}
	if match == nil || !match.fits(alg) {
		return nil, errInvalidToken()
	}
	return match.public, nil
}

// matches applies Powertools' selection rules: the same kid, an asymmetric
// key type, and the restrictions the provider published (alg, use,
// key_ops) allowing a signature check with alg.
func (k *setKey) matches(alg Algorithm, kid string) bool {
	if !k.hasKID || k.kid != kid {
		return false
	}
	switch kty, _ := jsonString(k.members[memberKty]); kty {
	case "RSA", "EC", "OKP":
	default:
		return false
	}
	if v, ok := k.members["alg"]; ok && v.Kind() != 'n' {
		if s, _ := jsonString(v); s != string(alg) {
			return false
		}
	}
	if v, ok := k.members["use"]; ok && v.Kind() != 'n' {
		if s, _ := jsonString(v); s != "sig" {
			return false
		}
	}
	if v, ok := k.members["key_ops"]; ok {
		var ops []jsontext.Value
		if v.Kind() != '[' || json.Unmarshal(v, &ops) != nil {
			return false
		}
		return slices.ContainsFunc(ops, func(op jsontext.Value) bool {
			s, _ := jsonString(op)
			return s == "verify"
		})
	}
	return true
}

// fits reports whether the key can verify alg: the key type of its family,
// and for elliptic curves the one curve RFC 7518 binds the algorithm to.
func (k *setKey) fits(alg Algorithm) bool {
	switch alg {
	case RS256, RS384, RS512, PS256, PS384, PS512:
		_, ok := k.public.(*rsa.PublicKey)
		return ok
	case ES256, ES384, ES512, ES256K:
		_, ok := k.public.(*ecdsa.PublicKey)
		return ok && k.curve == curveOf[alg]
	case EdDSA:
		_, ok := k.public.(ed25519.PublicKey)
		return ok
	default:
		return false
	}
}

// curveOf is the curve each ECDSA algorithm requires.
var curveOf = map[Algorithm]string{
	ES256:  "P-256",
	ES384:  "P-384",
	ES512:  "P-521",
	ES256K: "secp256k1",
}

// publicMembers are the members that make up each public key. Only they
// reach the JWK parser: the selection rules already read kid, alg, use and
// key_ops, which Powertools ignores when it builds the key, and a private
// JWK yields its public half.
var publicMembers = map[string][]string{
	"RSA": {memberKty, "n", "e"},
	"EC":  {memberKty, "crv", "x", "y"},
	"OKP": {memberKty, "crv", "x"},
}

// memberKty is the JWK member naming the key type.
const memberKty = "kty"

// decodePublicKey turns one JWK into the public key that verifies with it,
// checking its parameters. It returns nil for anything that is not a
// well-formed RSA, EC or OKP key.
func decodePublicKey(members map[string]jsontext.Value) (public any, curve string) {
	kty, _ := jsonString(members[memberKty])
	names, ok := publicMembers[kty]
	if !ok {
		return nil, ""
	}
	minimal := make(map[string]jsontext.Value, len(names))
	for _, name := range names {
		if value, ok := members[name]; ok {
			minimal[name] = value
		}
	}
	raw, err := json.Marshal(minimal)
	if err != nil {
		return nil, ""
	}
	key, err := jwk.ParseKey(raw)
	if err != nil || key.Validate() != nil {
		return nil, ""
	}
	exported, err := jwk.Export[any](key)
	if err != nil {
		return nil, ""
	}
	curve, _ = jsonString(members["crv"])
	return exported, curve
}
