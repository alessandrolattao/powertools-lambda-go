package datamasking

import (
	"strings"
)

// Mask is the string that replaces an erased value: Python's
// DATA_MASKING_STRING.
const Mask = "*****"

// preserved are the characters a dynamic mask keeps, so that the shape of
// an address or a date survives.
const preserved = "-_. "

// Rule says how to mask the values an expression selects. With none of the
// masking fields set, a value is erased whole: it becomes Mask, and an
// array becomes as many Mask strings as it had elements. With any of them
// set, objects and arrays keep their shape and every scalar inside is
// masked as text:
//
//   - RegexPattern and MaskFormat together replace what the pattern matches
//     with the format, which refers to groups as Python does (\1, \g<name>);
//   - otherwise CustomMask replaces the text, cut to the text's length when
//     it is longer;
//   - otherwise each character becomes '*', except '-', '_', '.' and space
//     (DynamicMask, or a lone RegexPattern or MaskFormat).
//
// Scalars become text the way Python's str() writes them: 30 is "30", 4.2
// is "4.2", true is "True"; null becomes Mask.
type Rule struct {
	// Path selects the values to mask, in the dialect the package
	// documentation describes. WithMaskingRules needs it; elsewhere it is
	// the expression that selected the value, or empty for the whole data.
	Path string
	// DynamicMask masks every character but '-', '_', '.' and space.
	DynamicMask bool
	// CustomMask replaces the text, cut to the text's length.
	CustomMask string
	// RegexPattern is a regular expression in Go's RE2 syntax.
	RegexPattern string
	// MaskFormat replaces each match of RegexPattern; \1 and \g<name> refer
	// to its groups.
	MaskFormat string
}

// masks reports whether the rule keeps the structure and masks the text,
// rather than erasing the value whole: Python's any([dynamic_mask,
// custom_mask, regex_pattern, mask_format]).
func (r Rule) masks() bool {
	return r.DynamicMask || r.CustomMask != "" || r.RegexPattern != "" || r.MaskFormat != ""
}

// Eraser is implemented by a Provider that masks values its own way: the Go
// form of a Python provider overriding erase. DataMasking hands it every
// selected value (with WithMaskingRules, the value as text, as Python does)
// and the rule that applies; MaskValue is the default it can fall back to.
type Eraser interface {
	Erase(value any, rule Rule) (any, error)
}

// MaskValue masks a decoded JSON value (nil, bool, a number, string, []any,
// map[string]any) as DataMasking does by default; see Rule. It fails only
// on a regular expression or a mask format that does not compile.
func MaskValue(value any, rule Rule) (any, error) {
	m, err := compileMask(rule)
	if err != nil {
		return nil, err
	}
	return m.erase(value), nil
}

// mask is a Rule ready to apply.
type mask struct {
	rule  Rule
	regex *regexMask // set when both RegexPattern and MaskFormat are
}

func compileMask(rule Rule) (*mask, error) {
	m := &mask{rule: rule}
	if rule.RegexPattern != "" && rule.MaskFormat != "" {
		regex, err := newRegexMask(rule.RegexPattern, rule.MaskFormat)
		if err != nil {
			return nil, err
		}
		m.regex = regex
	}
	return m, nil
}

// erase is Python's BaseProvider.erase.
func (m *mask) erase(value any) any {
	if !m.rule.masks() {
		if arr, ok := value.([]any); ok {
			out := make([]any, len(arr))
			for i := range out {
				out[i] = Mask
			}
			return out
		}
		return Mask
	}
	switch v := value.(type) {
	case string:
		return m.text(v)
	case bool, int64, float64:
		return m.text(pyStr(v))
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = m.erase(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = m.erase(item)
		}
		return out
	default:
		return Mask
	}
}

// text masks a scalar rendered as text: Python's _mask_primitive.
func (m *mask) text(s string) string {
	switch {
	case m.regex != nil:
		return m.regex.replace(s)
	case m.rule.CustomMask != "":
		return customMask(s, m.rule.CustomMask)
	default:
		return dynamicMask(s)
	}
}

// customMask is the mask cut to the length of the text when it is longer,
// the whole mask otherwise; lengths count characters, as Python's do.
func customMask(s, pattern string) string {
	n := len([]rune(s))
	runes := []rune(pattern)
	if len(runes) >= n {
		return string(runes[:n])
	}
	return pattern
}

func dynamicMask(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if strings.ContainsRune(preserved, r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('*')
		}
	}
	return b.String()
}
