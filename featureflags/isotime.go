package featureflags

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// parseISODatetime reads s as Python's datetime.fromisoformat does, in the
// grammar every Python that Powertools supports accepts (3.10 and later):
//
//	YYYY-MM-DD[*HH[:MM[:SS[.fff|.ffffff]]][+HH:MM[:SS[.ffffff]]]]
//
// where * is any single character and the offset may start with + or -.
// Python 3.11 and later accept more (basic format, week dates, fractions of
// any length); a schema that uses those would not validate on Python 3.10.
//
// It returns the wall clock (carried in UTC) and whether an offset was
// given.
func parseISODatetime(s string) (wall time.Time, hasOffset, ok bool) {
	if len(s) < 10 {
		return time.Time{}, false, false
	}
	year, okY := asciiNumber(s[0:4])
	month, okM := asciiNumber(s[5:7])
	day, okD := asciiNumber(s[8:10])
	if !okY || !okM || !okD || s[4] != '-' || s[7] != '-' || !validDate(year, month, day) {
		return time.Time{}, false, false
	}
	if len(s) == 10 {
		return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), false, true
	}
	_, size := utf8.DecodeRuneInString(s[10:])
	clock := s[10+size:]
	offset := ""
	for i := range len(clock) {
		if clock[i] == '+' || clock[i] == '-' {
			clock, offset = clock[:i], clock[i+1:]
			hasOffset = true
			break
		}
	}
	hour, minute, second, micro, ok := parseClock(clock)
	if !ok || hasOffset && !validOffset(offset) {
		return time.Time{}, false, false
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC), hasOffset, true
}

// parseClock reads HH[:MM[:SS[.fff|.ffffff]]].
func parseClock(s string) (hour, minute, second, micro int, ok bool) {
	clock, fraction, hasFraction := strings.Cut(s, ".")
	fields := strings.Split(clock, ":")
	if len(fields) > 3 || hasFraction && len(fields) != 3 {
		return 0, 0, 0, 0, false
	}
	var values [3]int
	for i, field := range fields {
		value, valid := asciiNumber(field)
		if !valid || len(field) != 2 {
			return 0, 0, 0, 0, false
		}
		values[i] = value
	}
	if hasFraction {
		if micro, ok = parseFraction(fraction); !ok {
			return 0, 0, 0, 0, false
		}
	}
	hour, minute, second = values[0], values[1], values[2]
	if hour > 23 || minute > 59 || second > 59 {
		return 0, 0, 0, 0, false
	}
	return hour, minute, second, micro, true
}

// parseFraction reads the milliseconds or microseconds after the seconds:
// Python 3.10 accepts three or six digits.
func parseFraction(fraction string) (micro int, ok bool) {
	value, ok := asciiNumber(fraction)
	switch {
	case !ok:
		return 0, false
	case len(fraction) == 3:
		return value * 1000, true
	case len(fraction) == 6:
		return value, true
	default:
		return 0, false
	}
}

// validOffset checks the offset after its sign: HH:MM, HH:MM:SS or
// HH:MM:SS.ffffff, below 24 hours.
func validOffset(s string) bool {
	switch len(s) {
	case 5, 8, 15:
	default:
		return false
	}
	hours, okH := asciiNumber(s[0:2])
	minutes, okM := asciiNumber(s[3:5])
	if !okH || !okM || s[2] != ':' || hours > 23 || minutes > 59 {
		return false
	}
	if len(s) == 5 {
		return true
	}
	seconds, okS := asciiNumber(s[6:8])
	if !okS || s[5] != ':' || seconds > 59 {
		return false
	}
	if len(s) == 8 {
		return true
	}
	_, okF := asciiNumber(s[9:])
	return okF && s[8] == '.'
}

// asciiNumber reads a non-empty run of ASCII digits.
func asciiNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// validDate checks a proleptic Gregorian date as Python's date does.
func validDate(year, month, day int) bool {
	if year < 1 || year > 9999 || month < 1 || month > 12 || day < 1 {
		return false
	}
	return day <= time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// matchTimeOfDay reports whether s is HH:MM as Python's pattern
// (?:2[0-3]|[01]\d):[0-5]\d matches it in full, and returns the hour and the
// minute. Like Python's \d, the digits may be any Unicode decimal digit,
// which Python's int() then reads.
func matchTimeOfDay(s string) (hour, minute int, ok bool) {
	runes := []rune(s)
	if len(runes) != 5 || runes[2] != ':' {
		return 0, 0, false
	}
	h0, h1, m0, m1 := runes[0], runes[1], runes[3], runes[4]
	switch {
	case h0 == '2' && h1 >= '0' && h1 <= '3':
		hour = 20 + int(h1-'0')
	case (h0 == '0' || h0 == '1') && unicode.IsDigit(h1):
		hour = int(h0-'0')*10 + digitValue(h1)
	default:
		return 0, 0, false
	}
	if m0 < '0' || m0 > '5' || !unicode.IsDigit(m1) {
		return 0, 0, false
	}
	return hour, int(m0-'0')*10 + digitValue(m1), true
}

// digitValue returns the value of a Unicode decimal digit. Unicode keeps the
// digits of every script in runs of ten, from zero to nine.
func digitValue(r rune) int {
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return int(r-start) % 10
}
