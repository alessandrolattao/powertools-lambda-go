// Package authval holds the configuration checks the auth and oauth2
// packages share, written after Powertools Python's auth_alpha validation
// helpers so that both accept and reject the same values.
package authval

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxPort is the largest TCP port a URL may name.
const maxPort = 65535

// HTTPSURL reports whether raw is a URL Powertools trusts with credentials:
// the https scheme, a host, no user information, no fragment, a valid port,
// and neither whitespace nor control characters anywhere. An issuer must not
// carry a query either, since it is compared byte for byte with the iss
// claim.
func HTTPSURL(raw string, issuer bool) bool {
	if !utf8.ValidString(raw) || strings.ContainsFunc(raw, func(r rune) bool { return r < ' ' || unicode.IsSpace(r) }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (issuer && u.RawQuery != "") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > maxPort {
			return false
		}
	}
	return true
}

// Strings checks that every value holds something other than whitespace,
// and returns them without duplicates, in their first-seen order. It
// reports false when a value is blank.
func Strings(values []string) ([]string, bool) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !NonBlank(v) {
			return nil, false
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, true
}

// Scopes checks that every value is an OAuth scope token, and returns them
// without duplicates, in their first-seen order.
func Scopes(values []string) ([]string, bool) {
	out, ok := Strings(values)
	if !ok {
		return nil, false
	}
	for _, s := range out {
		if !ScopeToken(s) {
			return nil, false
		}
	}
	return out, true
}

// ScopeToken reports whether s is a scope token as RFC 6749 section 3.3
// defines it: printable ASCII except the double quote and the backslash.
func ScopeToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// NonBlank reports whether s holds valid UTF-8 with something other than
// whitespace in it.
func NonBlank(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != ""
}
