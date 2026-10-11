package eventhandler

import (
	"errors"
	"fmt"
	"go/token"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalidPattern reports a route pattern that cannot be parsed.
var ErrInvalidPattern = errors.New("invalid route pattern")

// errWildcard describes a malformed wildcard segment.
var errWildcard = errors.New("bad wildcard")

// segment is one element of a pattern's path: a literal, a {name} wildcard
// or a trailing {name...} wildcard.
type segment struct {
	literal string
	name    string
	multi   bool
}

func (s segment) wildcard() bool { return s.name != "" }

// pattern is a parsed route pattern: net/http's syntax ("GET /todos/{id}",
// "/files/{path...}", "/{$}") with Powertools' matching rules, or a regular
// expression whose named groups are the path values.
type pattern struct {
	// str is the pattern as registered.
	str string
	// method is the upper-cased method; empty matches every method.
	method string
	// path is the path part of the pattern.
	path string
	segs []segment
	// trimmed is segs without trailing empty segments, for the trailing
	// slash tolerance of REST APIs.
	trimmed []segment
	re      *regexp.Regexp
	// dynamic patterns have wildcards or regexp groups; they are tried after
	// every static one.
	dynamic bool
}

// parsePattern parses a "[METHOD ]/path" pattern.
func parsePattern(s string) (*pattern, error) {
	method, path, found := strings.Cut(s, " ")
	if !found {
		method, path = "", s
	}
	path = strings.TrimLeft(path, " \t")
	if method != "" && !validMethod(method) {
		return nil, fmt.Errorf("%w %q: bad method %q", ErrInvalidPattern, s, method)
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%w %q: the path must start with a slash (host patterns are not supported)", ErrInvalidPattern, s)
	}
	p := &pattern{str: s, method: strings.ToUpper(method), path: path}
	raw := strings.Split(path[1:], "/")
	names := map[string]bool{}
	for i, part := range raw {
		seg, err := parseSegment(part, i == len(raw)-1)
		if err != nil {
			return nil, fmt.Errorf("%w %q: %w", ErrInvalidPattern, s, err)
		}
		if seg.wildcard() {
			if names[seg.name] {
				return nil, fmt.Errorf("%w %q: duplicate wildcard %q", ErrInvalidPattern, s, seg.name)
			}
			names[seg.name] = true
			p.dynamic = true
		}
		p.segs = append(p.segs, seg)
	}
	p.trimmed = trimTrailingEmpty(p.segs)
	return p, nil
}

func parseSegment(part string, last bool) (segment, error) {
	if !strings.Contains(part, "{") && !strings.Contains(part, "}") {
		literal, err := url.PathUnescape(part)
		if err != nil {
			return segment{}, fmt.Errorf("bad escape in %q: %w", part, err)
		}
		return segment{literal: literal}, nil
	}
	if !strings.HasPrefix(part, "{") || !strings.HasSuffix(part, "}") {
		return segment{}, fmt.Errorf("%w %q: a wildcard must be a whole segment", errWildcard, part)
	}
	name := part[1 : len(part)-1]
	if name == "$" {
		if !last {
			return segment{}, fmt.Errorf("%w: {$} must be at the end", errWildcard)
		}
		// "/a/{$}" matches "/a/" exactly, which is what every pattern does
		// here: it is the empty last segment of "/a/".
		return segment{}, nil
	}
	seg := segment{}
	if rest, ok := strings.CutSuffix(name, "..."); ok {
		if !last {
			return segment{}, fmt.Errorf("%w %q: it must be at the end", errWildcard, part)
		}
		name, seg.multi = rest, true
	}
	if !token.IsIdentifier(name) {
		return segment{}, fmt.Errorf("%w name %q", errWildcard, name)
	}
	seg.name = name
	return seg, nil
}

func validMethod(method string) bool {
	return method != "" && strings.IndexFunc(method, func(r rune) bool {
		return r <= ' ' || r >= 0x7f || strings.ContainsRune(`()<>@,;:\"/[]?={}`, r)
	}) < 0
}

func trimTrailingEmpty(segs []segment) []segment {
	end := len(segs)
	for end > 0 && !segs[end-1].wildcard() && segs[end-1].literal == "" {
		end--
	}
	return segs[:end]
}

// regexpPattern builds the pattern of a regular expression route. The
// expression must match the whole path, as Powertools Python anchors route
// rules.
func regexpPattern(method string, re *regexp.Regexp) (*pattern, error) {
	if method != "" && !validMethod(method) {
		return nil, fmt.Errorf("%w %q: bad method %q", ErrInvalidPattern, re.String(), method)
	}
	anchored, err := regexp.Compile(`^(?:` + re.String() + `)$`)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidPattern, re.String(), err)
	}
	upper := strings.ToUpper(method)
	str := re.String()
	if upper != "" {
		str = upper + " " + str
	}
	return &pattern{
		str:    str,
		method: upper,
		path:   re.String(),
		re:     anchored,
		// Python counts a rule with any group, named or not, as dynamic.
		dynamic: anchored.NumSubexp() > 0,
	}, nil
}

// key identifies the pattern for duplicate detection: two patterns with the
// same key match the same requests, so the second could never be reached.
func (p *pattern) key() string {
	if p.re != nil {
		return p.method + " re:" + p.path
	}
	var b strings.Builder
	b.WriteString(p.method)
	b.WriteByte(' ')
	for _, s := range p.segs {
		b.WriteByte('/')
		switch {
		case s.multi:
			b.WriteString("{...}")
		case s.wildcard():
			b.WriteString("{}")
		default:
			b.WriteString(url.PathEscape(s.literal))
		}
	}
	return b.String()
}

// names returns the wildcard names of the pattern, in order.
func (p *pattern) names() []string {
	if p.re != nil {
		return slices.DeleteFunc(p.re.SubexpNames()[1:], func(n string) bool { return n == "" })
	}
	var names []string
	for _, s := range p.segs {
		if s.wildcard() {
			names = append(names, s.name)
		}
	}
	return names
}

// openAPIPath renders the path as an OpenAPI path template, or reports false
// for a regular expression, which has none.
func (p *pattern) openAPIPath() (string, bool) {
	if p.re != nil {
		return "", false
	}
	var b strings.Builder
	for _, s := range p.segs {
		b.WriteByte('/')
		if s.wildcard() {
			b.WriteString("{" + s.name + "}")
			continue
		}
		b.WriteString(s.literal)
	}
	return b.String(), true
}

// withPrefix returns the pattern mounted under prefix, as including a router
// does. A "/" pattern becomes the prefix itself.
func (p *pattern) withPrefix(prefix string) (*pattern, error) {
	if prefix == "" {
		return p, nil
	}
	if p.re != nil {
		re, err := regexp.Compile(regexp.QuoteMeta(prefix) + p.path)
		if err != nil {
			return nil, fmt.Errorf("%w: prefix %q: %w", ErrInvalidPattern, prefix, err)
		}
		return regexpPattern(p.method, re)
	}
	path := prefix + p.path
	if p.path == "/" {
		path = prefix
	}
	if p.method != "" {
		path = p.method + " " + path
	}
	return parsePattern(path)
}

// match reports whether the pattern matches the escaped path, and the path
// values. With trailing set, trailing slashes are ignored on both sides.
func (p *pattern) match(escapedPath, decodedPath string, trailing bool) (map[string]string, bool) {
	if p.re != nil {
		path := decodedPath
		if trailing {
			path = trimSlashes(path)
		}
		m := p.re.FindStringSubmatch(path)
		if m == nil {
			return nil, false
		}
		values := map[string]string{}
		for i, name := range p.re.SubexpNames() {
			if name != "" && i > 0 {
				values[name] = m[i]
			}
		}
		return values, true
	}
	segs := p.segs
	if trailing {
		segs = p.trimmed
		escapedPath = trimSlashes(escapedPath)
	}
	return matchSegments(segs, escapedPath, trailing)
}

func trimSlashes(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func matchSegments(segs []segment, escapedPath string, trailing bool) (map[string]string, bool) {
	rest := strings.TrimPrefix(escapedPath, "/")
	parts := strings.Split(rest, "/")
	if trailing && rest == "" {
		parts = nil
	}
	values := map[string]string{}
	for i, seg := range segs {
		if seg.multi {
			return matchRest(seg, parts[min(i, len(parts)):], i < len(parts) || trailing, values)
		}
		if i >= len(parts) {
			return nil, false
		}
		value, ok := unescape(parts[i])
		if !ok || !seg.accepts(value) {
			return nil, false
		}
		if seg.wildcard() {
			values[seg.name] = value
		}
	}
	if len(parts) != len(segs) {
		return nil, false
	}
	return nonEmpty(values), true
}

// matchRest matches a {name...} wildcard with the remaining segments.
// "/files/{p...}" matches "/files/" but not "/files", as in net/http, unless
// trailing slashes do not count: present says whether the path reaches the
// wildcard.
func matchRest(seg segment, parts []string, present bool, values map[string]string) (map[string]string, bool) {
	if !present {
		return nil, false
	}
	value, ok := unescape(strings.Join(parts, "/"))
	if !ok {
		return nil, false
	}
	values[seg.name] = value
	return values, true
}

// accepts reports whether the segment matches the unescaped value: a
// wildcard matches any non-empty one.
func (s segment) accepts(value string) bool {
	if s.wildcard() {
		return value != ""
	}
	return value == s.literal
}

func nonEmpty(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return values
}

func unescape(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	value, err := url.PathUnescape(s)
	return value, err == nil
}
