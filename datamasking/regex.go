package datamasking

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// regexCache holds the compiled patterns for the life of the process, as
// Python's module-level cache does: masks are configured in code, so the
// set of patterns is small and fixed.
var regexCache sync.Map // pattern string -> *regexp.Regexp

func compileRegex(pattern string) (*regexp.Regexp, error) {
	if cached, ok := regexCache.Load(pattern); ok {
		if re, ok := cached.(*regexp.Regexp); ok {
			return re, nil
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRegex, err)
	}
	regexCache.Store(pattern, re)
	return re, nil
}

// regexMask replaces every match of a pattern with a Python replacement
// template: the mask_format of Powertools, re.sub's repl.
type regexMask struct {
	re    *regexp.Regexp
	parts []templatePart
}

// templatePart is either literal text or a reference to a group.
type templatePart struct {
	literal string
	group   int // -1 for a literal
}

func newRegexMask(pattern, format string) (*regexMask, error) {
	re, err := compileRegex(pattern)
	if err != nil {
		return nil, err
	}
	parts, err := parseTemplate(format, re)
	if err != nil {
		return nil, fmt.Errorf("%w: mask format: %w", ErrInvalidRegex, err)
	}
	return &regexMask{re: re, parts: parts}, nil
}

func (m *regexMask) replace(s string) string {
	matches := m.re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range matches {
		b.WriteString(s[last:loc[0]])
		for _, p := range m.parts {
			if p.group < 0 {
				b.WriteString(p.literal)
				continue
			}
			// A group that took no part in the match expands to nothing, as in
			// Python 3.5 and later.
			if start, end := loc[2*p.group], loc[2*p.group+1]; start >= 0 {
				b.WriteString(s[start:end])
			}
		}
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

var (
	errBadEscape     = errors.New("bad escape")
	errGroupRef      = errors.New("invalid group reference")
	errUnknownGroup  = errors.New("unknown group name")
	errMissingGroupB = errors.New("missing < or > in group reference")
)

// parseTemplate reads a replacement template with Python's rules: \1 to
// \99 and \g<n> or \g<name> refer to groups, \0 and three-digit octal
// escapes are characters, \n, \t and the other C escapes are themselves,
// an unknown escape of an ASCII letter is an error, any other unknown
// escape is kept as written, backslash included.
func parseTemplate(format string, re *regexp.Regexp) ([]templatePart, error) {
	var parts []templatePart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, templatePart{literal: lit.String(), group: -1})
			lit.Reset()
		}
	}
	addGroup := func(n int) error {
		if n > re.NumSubexp() {
			return fmt.Errorf("%w %d", errGroupRef, n)
		}
		flush()
		parts = append(parts, templatePart{group: n})
		return nil
	}

	for i := 0; i < len(format); {
		if format[i] != '\\' {
			r, size := utf8.DecodeRuneInString(format[i:])
			lit.WriteRune(r)
			i += size
			continue
		}
		if i+1 >= len(format) {
			return nil, fmt.Errorf("%w (end of template)", errBadEscape)
		}
		n, err := parseEscape(format, i+1, re, &lit, addGroup)
		if err != nil {
			return nil, err
		}
		i = n
	}
	flush()
	return parts, nil
}

// parseEscape reads the escape whose backslash precedes format[at] and
// returns where the template continues.
func parseEscape(format string, at int, re *regexp.Regexp, lit *strings.Builder, addGroup func(int) error) (int, error) {
	c := format[at]
	next := func(i int) byte {
		if i < len(format) {
			return format[i]
		}
		return 0
	}
	switch {
	case c == 'g':
		return parseNamedGroup(format, at+1, re, addGroup)
	case c == '0':
		end := at + 1
		for end < at+3 && isOctal(next(end)) {
			end++
		}
		v, _ := strconv.ParseUint(format[at:end], 8, 8) //nolint:errcheck // at most three octal digits starting with 0
		lit.WriteRune(rune(v))
		return end, nil
	case isDigit(c):
		return parseDigitEscape(format, at, lit, addGroup)
	}
	if r, ok := pyEscapes[c]; ok {
		lit.WriteByte(r)
		return at + 1, nil
	}
	if c < utf8.RuneSelf && isASCIILetter(c) {
		return 0, fmt.Errorf("%w \\%c", errBadEscape, c)
	}
	r, size := utf8.DecodeRuneInString(format[at:])
	lit.WriteByte('\\')
	lit.WriteRune(r)
	return at + size, nil
}

// parseDigitEscape reads what follows a backslash and a digit from 1 to 9:
// a three-digit octal escape, or a group number of one or two digits.
func parseDigitEscape(format string, at int, lit *strings.Builder, addGroup func(int) error) (int, error) {
	digitAt := func(i int) bool { return i < len(format) && isDigit(format[i]) }
	octalAt := func(i int) bool { return i < len(format) && isOctal(format[i]) }
	if !digitAt(at + 1) {
		return at + 1, addGroup(int(format[at] - '0'))
	}
	if octalAt(at) && octalAt(at+1) && octalAt(at+2) {
		v, _ := strconv.ParseUint(format[at:at+3], 8, 16) //nolint:errcheck // three octal digits always parse
		if v > 0o377 {
			return 0, fmt.Errorf("%w: octal escape %s outside of range 0-0o377", errBadEscape, format[at-1:at+3])
		}
		lit.WriteRune(rune(v))
		return at + 3, nil
	}
	n, _ := strconv.Atoi(format[at : at+2]) //nolint:errcheck // two decimal digits always parse
	return at + 2, addGroup(n)
}

// parseNamedGroup reads <name> or <number> after \g.
func parseNamedGroup(format string, at int, re *regexp.Regexp, addGroup func(int) error) (int, error) {
	if at >= len(format) || format[at] != '<' {
		return 0, errMissingGroupB
	}
	end := strings.IndexByte(format[at:], '>')
	if end < 0 {
		return 0, errMissingGroupB
	}
	name := format[at+1 : at+end]
	if name == "" {
		return 0, fmt.Errorf("%w: missing group name", errGroupRef)
	}
	if n, err := strconv.Atoi(name); err == nil && n >= 0 && isDecimal(name) {
		return at + end + 1, addGroup(n)
	}
	n := re.SubexpIndex(name)
	if n < 0 {
		return 0, fmt.Errorf("%w %q", errUnknownGroup, name)
	}
	return at + end + 1, addGroup(n)
}

// pyEscapes are the character escapes a Python template understands.
var pyEscapes = map[byte]byte{
	'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v', '\\': '\\',
}

func isDigit(c byte) bool       { return '0' <= c && c <= '9' }
func isOctal(c byte) bool       { return '0' <= c && c <= '7' }
func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func isDecimal(s string) bool {
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}
