package appsyncevents

import (
	"errors"
	"fmt"
	"strings"
)

// errInvalidPath is why a channel path cannot be registered.
var errInvalidPath = errors.New("a path starts with '/' and a namespace, its segments are separated by '/', " +
	"and a wildcard is allowed only as the last segment")

// pattern is a channel path a handler is registered for: an exact path such
// as "/default/channel", or a wildcard such as "/default/*" or "/*".
type pattern struct {
	path string
	// wildcard patterns end in "/*"; prefix is what comes before it, empty
	// for "/*".
	wildcard bool
	prefix   string
}

// parsePattern accepts what Powertools Python accepts: "/*", or a namespace
// and optional further segments, none empty nor holding "*", optionally
// followed by "/*".
func parsePattern(path string) (pattern, error) {
	if path == "/*" {
		return pattern{path: path, wildcard: true}, nil
	}
	prefix, wildcard := strings.CutSuffix(path, "/*")
	segments, ok := strings.CutPrefix(prefix, "/")
	if ok {
		for segment := range strings.SplitSeq(segments, "/") {
			if segment == "" || strings.Contains(segment, "*") {
				ok = false
				break
			}
		}
	}
	if !ok {
		return pattern{}, fmt.Errorf("appsyncevents: invalid channel path %q: %w", path, errInvalidPath)
	}
	return pattern{path: path, wildcard: wildcard, prefix: prefix}, nil
}

// matches reports whether the channel path belongs to the pattern. A
// wildcard covers its prefix and everything below it, at a segment
// boundary: "/default/*" matches "/default/a" and "/default/a/b", not
// "/defaults/a" (Powertools Python's regular expression does).
func (p pattern) matches(channel string) bool {
	if !p.wildcard {
		return channel == p.path
	}
	if p.prefix == "" {
		return true
	}
	rest, ok := strings.CutPrefix(channel, p.prefix)
	return ok && (rest == "" || rest[0] == '/')
}

// moreSpecific reports whether p wins over q when both match a channel: an
// exact path wins over any wildcard, a longer wildcard over a shorter one.
// Two different patterns that match the same channel never tie.
func (p pattern) moreSpecific(q pattern) bool {
	if p.wildcard != q.wildcard {
		return !p.wildcard
	}
	return len(p.prefix) > len(q.prefix)
}

// best returns the route whose pattern matches the channel most
// specifically.
func best[F any](routes map[string]route[F], channel string) (route[F], bool) {
	var (
		found route[F]
		ok    bool
	)
	for _, candidate := range routes {
		if candidate.pattern.matches(channel) && (!ok || candidate.pattern.moreSpecific(found.pattern)) {
			found, ok = candidate, true
		}
	}
	return found, ok
}
