package eventhandler

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// corsRequiredHeaders are always allowed, as in Powertools Python.
var corsRequiredHeaders = []string{"Authorization", "Content-Type", "X-Amz-Date", "X-Api-Key", "X-Amz-Security-Token"}

// CORSConfig is the Cross-Origin Resource Sharing configuration of a router:
// Python's CORSConfig, with its defaults.
type CORSConfig struct {
	// AllowOrigin is the origin allowed to call the API; "*", the default,
	// allows any and should only be used while prototyping.
	AllowOrigin string
	// ExtraOrigins are further allowed origins.
	ExtraOrigins []string
	// AllowHeaders are allowed request headers, on top of Authorization,
	// Content-Type, X-Amz-Date, X-Api-Key and X-Amz-Security-Token.
	AllowHeaders []string
	// ExposeHeaders are the response headers the browser may read
	// (Access-Control-Expose-Headers).
	ExposeHeaders []string
	// MaxAge is how many seconds a pre-flight response may be cached
	// (Access-Control-Max-Age); nil sends no header.
	MaxAge *int
	// AllowCredentials sends Access-Control-Allow-Credentials: true when
	// the allowed origin is not "*".
	AllowCredentials bool
}

func (c *CORSConfig) origins() []string {
	allow := c.AllowOrigin
	if allow == "" {
		allow = "*"
	}
	return append([]string{allow}, c.ExtraOrigins...)
}

// allowedOrigin returns the Access-Control-Allow-Origin value for the
// request origin: the origin itself when listed, "*" when any origin is
// allowed, "" when the origin is not allowed.
func (c *CORSConfig) allowedOrigin(origin string) string {
	if origin == "" {
		return ""
	}
	origins := c.origins()
	if slices.Contains(origins, origin) {
		return origin
	}
	if slices.Contains(origins, "*") {
		return "*"
	}
	return ""
}

// apply adds the CORS headers for the request origin to h. A request without
// an Origin header, or from an origin not allowed, gets none.
func (c *CORSConfig) apply(h http.Header, origin string) {
	allowed := c.allowedOrigin(origin)
	if allowed == "" {
		return
	}
	h.Set("Access-Control-Allow-Origin", allowed)
	if allowed != "*" {
		// The answer depends on the origin: caches must keep one per origin.
		h.Add("Vary", "Origin")
	}
	headers := map[string]bool{}
	for _, name := range append(slices.Clone(corsRequiredHeaders), c.AllowHeaders...) {
		headers[name] = true
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	h.Set("Access-Control-Allow-Headers", strings.Join(names, ","))
	if len(c.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(c.ExposeHeaders, ","))
	}
	if c.MaxAge != nil {
		h.Set("Access-Control-Max-Age", strconv.Itoa(*c.MaxAge))
	}
	if allowed != "*" && c.AllowCredentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
}
