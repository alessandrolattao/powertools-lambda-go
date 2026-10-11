package datamasking

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// pyStr renders a JSON value the way Python's str() does, because Python
// masks that text: the masks keep its length, and a regular expression sees
// it. A number keeps its Python spelling ("30", "4.2", "1e+16"), a boolean
// is "True" or "False", and an object or an array is its Python repr
// ({'a': 1}), with keys in sorted order where Python keeps the document's.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

func writeRepr(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if t {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(pyFloat(t))
	case string:
		b.WriteString(pyQuote(t))
	case []any:
		writeList(b, t)
	case map[string]any:
		writeDict(b, t)
	default:
		// The engine only holds decoded JSON values; anything else comes from
		// a custom Eraser and is rendered the Go way.
		fmt.Fprint(b, t)
	}
}

func writeList(b *strings.Builder, list []any) {
	b.WriteByte('[')
	for i, item := range list {
		if i > 0 {
			b.WriteString(", ")
		}
		writeRepr(b, item)
	}
	b.WriteByte(']')
}

func writeDict(b *strings.Builder, dict map[string]any) {
	b.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(dict)) {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyQuote(k))
		b.WriteString(": ")
		writeRepr(b, dict[k])
	}
	b.WriteByte('}')
}

// pyFloat is Python's repr of a float: the shortest digits that read back
// the same value, in positional notation unless the decimal exponent is
// below -4 or above 16, and always with a fraction or an exponent.
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±xx
	sign := ""
	if sci[0] == '-' {
		sign, sci = "-", sci[1:]
	}
	mantissa, exp, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	e, _ := strconv.Atoi(exp) //nolint:errcheck // FormatFloat always writes a valid exponent
	point := e + 1            // digits are 0.d1d2... times 10^point

	if point <= -4 || point > 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		expSign := "+"
		if e < 0 {
			expSign, e = "-", -e
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, expSign, e)
	}
	switch {
	case point <= 0:
		return sign + "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
	default:
		return sign + digits[:point] + "." + digits[point:]
	}
}

// pyQuote is Python's repr of a str: single quotes unless the text holds a
// single quote and no double quote, backslash escapes for the quote, the
// backslash and the control characters, \x, \u or \U escapes for what is
// not printable.
func pyQuote(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case !unicode.IsPrint(r):
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
