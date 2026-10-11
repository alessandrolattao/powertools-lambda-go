// Package pyjson writes JSON the way Python's json.dumps does with the
// arguments Powertools Python passes it, so that the documents a Go function
// writes are byte for byte those a Python function writes for the same
// calls.
//
// Two things differ from encoding/json/v2's defaults: floating-point numbers
// keep Python's repr (1.0, 1e+16, 1e-05), and every character outside
// printable ASCII is escaped (ensure_ascii).
package pyjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrNotFinite is returned for a NaN or infinite number, which JSON cannot
// represent (Python would write the invalid literals NaN and Infinity).
var ErrNotFinite = errors.New("NaN and infinite numbers are not valid JSON")

// AppendFloat appends f to dst as Python's repr writes a float: the shortest
// digits that read back as f, with ".0" on whole numbers, and in exponent
// form below 1e-4 and from 1e16 on. bits is 64 or 32.
func AppendFloat(dst []byte, f float64, bits int) []byte {
	if f == 0 {
		if math.Signbit(f) {
			return append(dst, "-0.0"...)
		}
		return append(dst, "0.0"...)
	}
	exp := exponent(f, bits)
	if exp < -4 || exp >= 16 {
		return strconv.AppendFloat(dst, f, 'e', -1, bits)
	}
	start := len(dst)
	dst = strconv.AppendFloat(dst, f, 'f', -1, bits)
	if slices.Contains(dst[start:], '.') {
		return dst
	}
	return append(dst, ".0"...)
}

// exponent returns the decimal exponent of the shortest representation of f.
func exponent(f float64, bits int) int {
	var buf [32]byte
	s := strconv.AppendFloat(buf[:0], f, 'e', -1, bits)
	exp, err := strconv.Atoi(string(s[bytes.LastIndexByte(s, 'e')+1:]))
	if err != nil {
		return 0 // unreachable: strconv writes a valid exponent
	}
	return exp
}

// Float returns f as a JSON number in Python's repr, or ErrNotFinite.
func Float(f float64, bits int) (jsontext.Value, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, ErrNotFinite
	}
	return AppendFloat(nil, f, bits), nil
}

// Options returns the encoding/json/v2 options that marshal values as
// Python's json.dumps would: floats in Python's repr, maps with sorted keys
// (Python keeps insertion order, which a Go map does not have; sorting at
// least makes the output stable), invalid UTF-8 replaced rather than fatal.
func Options() json.Options {
	return json.JoinOptions(
		json.Deterministic(true),
		jsontext.AllowInvalidUTF8(true),
		json.WithMarshalers(json.JoinMarshalers(
			json.MarshalToFunc(func(enc *jsontext.Encoder, f float64) error {
				v, err := Float(f, 64)
				if err != nil {
					return err
				}
				return enc.WriteValue(v)
			}),
			json.MarshalToFunc(func(enc *jsontext.Encoder, f float32) error {
				v, err := Float(float64(f), 32)
				if err != nil {
					return err
				}
				return enc.WriteValue(v)
			}),
		)),
	)
}

// EscapeNonASCII rewrites a JSON document the way json.dumps' ensure_ascii
// writes it: every byte outside printable ASCII (DEL included) becomes a
// \uXXXX escape, characters beyond the Basic Multilingual Plane a surrogate
// pair. Such bytes can only occur inside strings, so the rewrite is safe on
// any document jsontext produced.
func EscapeNonASCII(doc []byte) []byte {
	clean := true
	for _, c := range doc {
		if c >= utf8.RuneSelf || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return doc
	}
	out := make([]byte, 0, len(doc)+len(doc)/4)
	var units []uint16
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == 0x7f:
			out = appendEscape(out, 0x7f)
			i++
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(doc[i:])
			i += size
			units = utf16.AppendRune(units[:0], r)
			for _, x := range units {
				out = appendEscape(out, x)
			}
		}
	}
	return out
}

func appendEscape(dst []byte, x uint16) []byte {
	const hex = "0123456789abcdef"
	return append(dst, '\\', 'u', hex[x>>12&0xf], hex[x>>8&0xf], hex[x>>4&0xf], hex[x&0xf])
}
