package schema

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// format is an asserted string format.
type format struct {
	name  string
	check func(s string) error
}

var (
	errFormat = errors.New("invalid format")

	formats = map[string]func(string) error{
		"date-time":     checkDateTime,
		"date":          checkDate,
		"time":          checkTime,
		"duration":      checkDuration,
		"email":         checkEmail,
		"hostname":      checkHostname,
		"ipv4":          checkIPv4,
		"ipv6":          checkIPv6,
		"uri":           checkURI,
		"iri":           checkURI,
		"uri-reference": checkURIReference,
		"iri-reference": checkURIReference,
		"uuid":          checkUUID,
		"regex":         checkRegex,
	}

	// annotationFormats describe a value without constraining it: the
	// reflector writes them for Go numeric types and files.
	annotationFormats = map[string]bool{
		"int32": true, "int64": true, "float": true, "double": true,
		"byte": true, "binary": true, "password": true,
	}
)

func lookupFormat(name string) (*format, error) {
	if annotationFormats[name] {
		return &format{name: name}, nil
	}
	check, ok := formats[name]
	if !ok {
		return nil, fmt.Errorf("%w: format %q", ErrUnsupported, name)
	}
	return &format{name: name, check: check}, nil
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", errFormat, reason)
}

func invalidErr(err error) error {
	return fmt.Errorf("%w: %w", errFormat, err)
}

// checkUUID accepts the 8-4-4-4-12 hexadecimal form, in any case.
func checkUUID(s string) error {
	groups := strings.Split(s, "-")
	lengths := []int{8, 4, 4, 4, 12}
	if len(groups) != len(lengths) {
		return invalid(fmt.Sprintf("%d groups, want 5", len(groups)))
	}
	for i, g := range groups {
		if len(g) != lengths[i] {
			return invalid(fmt.Sprintf("group %d is %d characters long, want %d", i+1, len(g), lengths[i]))
		}
		if strings.IndexFunc(g, func(r rune) bool { return !isHex(r) }) >= 0 {
			return invalid(fmt.Sprintf("group %d is not hexadecimal", i+1))
		}
	}
	return nil
}

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func isDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// checkDuration accepts RFC 3339's ISO 8601 durations: PnW, or
// P[nY][nM][nD][T[nH][nM][nS]] with at least one element, in order.
func checkDuration(s string) error {
	rest, ok := strings.CutPrefix(s, "P")
	if !ok || rest == "" {
		return invalid("a duration starts with P and has elements")
	}
	if weeks, ok := strings.CutSuffix(rest, "W"); ok {
		if !isDigits(weeks) {
			return invalid("bad week count")
		}
		return nil
	}
	parts := strings.Split(rest, "T")
	if len(parts) > 2 {
		return invalid("more than one T")
	}
	for i, part := range parts {
		if i > 0 && part == "" {
			return invalid("T without time elements")
		}
		units := "YMD"
		if i == 1 {
			units = "HMS"
		}
		if err := durationElements(part, units); err != nil {
			return err
		}
	}
	return nil
}

func durationElements(part, units string) error {
	for part != "" {
		digits := 0
		for digits < len(part) && part[digits] >= '0' && part[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return invalid("element without a number")
		}
		if digits == len(part) {
			return invalid("element without a unit")
		}
		i := strings.IndexByte(units, part[digits])
		if i < 0 {
			return invalid(fmt.Sprintf("unit %q unknown or out of order", part[digits]))
		}
		units, part = units[i+1:], part[digits+1:]
	}
	return nil
}

// checkIPv4 accepts four decimal octets without leading zeros.
func checkIPv4(s string) error {
	octets := strings.Split(s, ".")
	if len(octets) != 4 {
		return invalid(fmt.Sprintf("%d octets, want 4", len(octets)))
	}
	for _, o := range octets {
		if len(o) > 1 && o[0] == '0' {
			return invalid(fmt.Sprintf("octet %q has a leading zero", o))
		}
		n, err := strconv.Atoi(o)
		if err != nil || n < 0 || n > 255 {
			return invalid(fmt.Sprintf("octet %q is not between 0 and 255", o))
		}
	}
	return nil
}

// checkIPv6 accepts an IPv6 address without a zone.
func checkIPv6(s string) error {
	if !strings.Contains(s, ":") {
		return invalid("no colon")
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return invalidErr(err)
	}
	if addr.Zone() != "" {
		return invalid("zones are not part of an address")
	}
	return nil
}

// checkHostname accepts RFC 1123 host names: labels of 1 to 63 letters,
// digits and inner hyphens, 253 characters at most without a final dot.
func checkHostname(s string) error {
	s = strings.TrimSuffix(s, ".")
	if len(s) > 253 {
		return invalid("longer than 253 characters")
	}
	for label := range strings.SplitSeq(s, ".") {
		if err := checkLabel(label); err != nil {
			return err
		}
	}
	return nil
}

func checkLabel(label string) error {
	if label == "" || len(label) > 63 {
		return invalid("a label must be 1 to 63 characters long")
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return invalid("a label cannot start or end with a hyphen")
	}
	if i := strings.IndexFunc(label, func(r rune) bool { return !isAlnum(r) && r != '-' }); i >= 0 {
		return invalid(fmt.Sprintf("character %q in a label", label[i]))
	}
	return nil
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// checkEmail accepts local@domain: a dot-atom or quoted local part of 64
// characters at most, a host name or a bracketed IP address, 254
// characters in all.
func checkEmail(s string) error {
	if len(s) > 254 {
		return invalid("longer than 254 characters")
	}
	local, domain, ok := strings.CutLast(s, "@")
	if !ok {
		return invalid("no @")
	}
	if len(local) > 64 {
		return invalid("local part longer than 64 characters")
	}
	if err := checkLocalPart(local); err != nil {
		return err
	}
	if inner, ok := strings.CutPrefix(domain, "["); ok && strings.HasSuffix(inner, "]") {
		inner = strings.TrimSuffix(inner, "]")
		if v6, ok := strings.CutPrefix(inner, "IPv6:"); ok {
			return checkIPv6(v6)
		}
		return checkIPv4(inner)
	}
	return checkHostname(domain)
}

func checkLocalPart(local string) error {
	if len(local) > 1 && strings.HasPrefix(local, `"`) && strings.HasSuffix(local, `"`) {
		if strings.ContainsAny(local[1:len(local)-1], `\"`) {
			return invalid("backslash or quote inside a quoted local part")
		}
		return nil
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return invalid("misplaced dot in the local part")
	}
	if i := strings.IndexFunc(local, func(r rune) bool { return !isAlnum(r) && !strings.ContainsRune(".!#$%&'*+-/=?^_`{|}~", r) }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(local[i:])
		return invalid(fmt.Sprintf("character %q in the local part", r))
	}
	return nil
}

// checkDate accepts RFC 3339 full-date.
func checkDate(s string) error {
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return invalidErr(err)
	}
	return nil
}

// checkTime accepts RFC 3339 full-time: hh:mm:ss, an optional fraction,
// then Z or an offset; a leap second only at 23:59 UTC.
func checkTime(s string) error {
	if len(s) < 9 || s[2] != ':' || s[5] != ':' {
		return invalid("not hh:mm:ss")
	}
	h, m, sec, ok := clock(s[:8])
	if !ok {
		return invalid("bad hour, minute or second")
	}
	rest, err := skipFraction(s[8:])
	if err != nil {
		return err
	}
	if rest != "z" && rest != "Z" {
		offset, err := parseOffset(rest)
		if err != nil {
			return err
		}
		minutes := ((h*60+m-offset)%(24*60) + 24*60) % (24 * 60)
		h, m = minutes/60, minutes%60
	}
	if sec == 60 && (h != 23 || m != 59) {
		return invalid("a leap second must be at 23:59 UTC")
	}
	return nil
}

// skipFraction skips the fraction of a second, if any.
func skipFraction(s string) (string, error) {
	frac, ok := strings.CutPrefix(s, ".")
	if !ok {
		return s, nil
	}
	n := 0
	for n < len(frac) && frac[n] >= '0' && frac[n] <= '9' {
		n++
	}
	if n == 0 {
		return "", invalid("no digits in the fraction")
	}
	return frac[n:], nil
}

// clock parses hh:mm:ss.
func clock(s string) (h, m, sec int, ok bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	vals := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		vals[i] = n
	}
	h, m, sec = vals[0], vals[1], vals[2]
	return h, m, sec, h <= 23 && m <= 59 && sec <= 60
}

// parseOffset parses +hh:mm or -hh:mm into minutes east of UTC.
func parseOffset(s string) (int, error) {
	if len(s) != 6 || (s[0] != '+' && s[0] != '-') || s[3] != ':' {
		return 0, invalid("offset must be Z or ±hh:mm")
	}
	h, errH := strconv.Atoi(s[1:3])
	m, errM := strconv.Atoi(s[4:6])
	if errH != nil || errM != nil || h < 0 || m < 0 || h > 23 || m > 59 {
		return 0, invalid("bad offset")
	}
	offset := h*60 + m
	if s[0] == '-' {
		offset = -offset
	}
	return offset, nil
}

// checkDateTime accepts RFC 3339 date-time.
func checkDateTime(s string) error {
	if len(s) < 20 {
		return invalid("too short")
	}
	if s[10] != 'T' && s[10] != 't' {
		return invalid("no T between date and time")
	}
	if err := checkDate(s[:10]); err != nil {
		return err
	}
	return checkTime(s[11:])
}

// parseURI parses a URI reference, checking IPv6 hosts, which net/url does
// not.
func parseURI(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, invalidErr(err)
	}
	if host := u.Hostname(); strings.Contains(host, ":") {
		if !strings.Contains(u.Host, "[") || !strings.Contains(u.Host, "]") {
			return nil, invalid("IPv6 host not in brackets")
		}
		if err := checkIPv6(host); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// checkURI accepts an absolute URI.
func checkURI(s string) error {
	u, err := parseURI(s)
	if err != nil {
		return err
	}
	if !u.IsAbs() {
		return invalid("relative URI")
	}
	return nil
}

// checkURIReference accepts a URI or a relative reference.
func checkURIReference(s string) error {
	if strings.Contains(s, `\`) {
		return invalid("backslash")
	}
	_, err := parseURI(s)
	return err
}

// checkRegex accepts a regular expression Go (RE2) compiles.
func checkRegex(s string) error {
	if _, err := regexp.Compile(s); err != nil {
		return invalidErr(fmt.Errorf("compile: %w", err))
	}
	return nil
}
