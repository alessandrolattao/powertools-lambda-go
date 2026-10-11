package validation

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// validationError describes the first failure of err, the engine's report
// on data, the way Powertools Python's SchemaValidationError does.
func (s *Schema) validationError(data any, err error) error {
	verr, ok := errors.AsType[*jsonschema.ValidationError](err)
	if !ok {
		return fmt.Errorf("validation: %w", err)
	}
	leaf := firstFailure(verr)
	name, path, value := locate(data, leaf.InstanceLocation)
	definition := s.definition(leaf.SchemaURL)
	rule := ruleOf(leaf.ErrorKind)
	var ruleDefinition any
	if def, ok := definition.(map[string]any); ok && rule != "" {
		ruleDefinition = def[rule]
	}
	message := describe(name, leaf, ruleDefinition)
	pathRepr := make([]any, len(path))
	for i, p := range path {
		pathRepr[i] = p
	}
	return &SchemaValidationError{
		Message: fmt.Sprintf("Failed schema validation. Error: %s, Path: %s, Data: %s",
			message, pyRepr(pathRepr), pyStr(value)),
		ValidationMessage: message,
		Name:              name,
		Path:              path,
		Value:             value,
		Definition:        definition,
		Rule:              rule,
		RuleDefinition:    ruleDefinition,
		Err:               verr,
	}
}

// firstFailure picks the failure to report: it descends through the
// failures that only group others (the schema as a whole, "$ref", "allOf")
// and stops at a keyword, choosing the earliest location at each level so
// that the report does not depend on map order.
func firstFailure(e *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(e.Causes) > 0 && grouping(e.ErrorKind) {
		e = slices.MinFunc(e.Causes, compareFailures)
	}
	return e
}

func grouping(k jsonschema.ErrorKind) bool {
	switch k.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference, *kind.AllOf:
		return true
	default:
		return false
	}
}

func compareFailures(a, b *jsonschema.ValidationError) int {
	return cmp.Or(
		slices.CompareFunc(a.InstanceLocation, b.InstanceLocation, compareTokens),
		strings.Compare(a.SchemaURL, b.SchemaURL),
		strings.Compare(ruleOf(a.ErrorKind), ruleOf(b.ErrorKind)),
	)
}

// compareTokens orders location tokens, array indexes by number.
func compareTokens(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return cmp.Compare(ai, bi)
	}
	return strings.Compare(a, b)
}

// ruleOf returns the keyword a failure breaks.
func ruleOf(k jsonschema.ErrorKind) string {
	switch k.(type) {
	case *kind.Not:
		return "not"
	case *kind.Dependency:
		return "dependencies"
	default:
		if path := k.KeywordPath(); len(path) > 0 {
			return path[0]
		}
		return ""
	}
}

// locate renders the location of a value as fastjsonschema names it
// ("data.items[0].id") and as a path, and returns the value.
func locate(data any, location []string) (name string, path []string, value any) {
	var b strings.Builder
	b.WriteString("data")
	path = append(make([]string, 0, len(location)+1), "data")
	value = data
	for _, token := range location {
		path = append(path, token)
		switch v := value.(type) {
		case []any:
			b.WriteString("[" + token + "]")
			value = nil
			if i, err := strconv.Atoi(token); err == nil && i >= 0 && i < len(v) {
				value = v[i]
			}
		case map[string]any:
			b.WriteString("." + token)
			value = v[token]
		default:
			b.WriteString("." + token)
			value = nil
		}
	}
	return b.String(), path, value
}

// definition returns the part of the schema documents at location, an
// absolute URL whose fragment is a JSON pointer, or nil.
func (s *Schema) definition(location string) any {
	base, fragment, _ := strings.Cut(location, "#")
	doc, ok := s.docs[base]
	if !ok {
		return nil
	}
	if unescaped, err := url.PathUnescape(fragment); err == nil {
		fragment = unescaped
	}
	if fragment == "" || fragment == "/" {
		return doc
	}
	for token := range strings.SplitSeq(strings.TrimPrefix(fragment, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch v := doc.(type) {
		case map[string]any:
			doc = v[token]
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(v) {
				return nil
			}
			doc = v[i]
		default:
			return nil
		}
	}
	return doc
}

// describe words a failure the way fastjsonschema does, with the location
// name in front: "data.price must be smaller than or equal to 42".
func describe(name string, e *jsonschema.ValidationError, ruleDefinition any) string {
	if msg, ok := describeValue(e.ErrorKind, ruleDefinition); ok {
		return name + " must " + msg
	}
	if msg, ok := describeStructure(e.ErrorKind); ok {
		return name + " " + msg
	}
	// A failure fastjsonschema does not know: the engine's words.
	line, _, _ := strings.Cut(e.Error(), "\n")
	if _, msg, found := strings.Cut(line, "': "); found {
		line = msg
	}
	return name + " " + line
}

// describeValue words the failures about a value's type or content.
func describeValue(k jsonschema.ErrorKind, ruleDefinition any) (string, bool) {
	switch k := k.(type) {
	case *kind.Type:
		return "be " + strings.Join(k.Want, " or "), true
	case *kind.MinLength:
		return fmt.Sprintf("be longer than or equal to %d characters", k.Want), true
	case *kind.MaxLength:
		return fmt.Sprintf("be shorter than or equal to %d characters", k.Want), true
	case *kind.Pattern:
		return "match pattern " + k.Want, true
	case *kind.Format:
		return "be " + k.Want, true
	case *kind.Enum:
		return "be one of " + pyRepr(k.Want), true
	case *kind.Const:
		return "be same as const definition: " + pyRepr(k.Want), true
	case *kind.ContentEncoding:
		return "be encoded by " + k.Want, true
	case *kind.ContentMediaType:
		if k.Want == "application/json" {
			return "be valid JSON", true
		}
		return "be " + k.Want, true
	default:
		return describeNumber(k, ruleDefinition)
	}
}

// describeNumber words the failures about a number's range.
func describeNumber(k jsonschema.ErrorKind, ruleDefinition any) (string, bool) {
	switch k := k.(type) {
	case *kind.Minimum:
		return "be bigger than or equal to " + number(ruleDefinition, k.Want), true
	case *kind.ExclusiveMinimum:
		return "be bigger than " + number(ruleDefinition, k.Want), true
	case *kind.Maximum:
		return "be smaller than or equal to " + number(ruleDefinition, k.Want), true
	case *kind.ExclusiveMaximum:
		return "be smaller than " + number(ruleDefinition, k.Want), true
	case *kind.MultipleOf:
		return "be multiple of " + number(ruleDefinition, k.Want), true
	default:
		return "", false
	}
}

// describeStructure words the failures about an object's members.
func describeStructure(k jsonschema.ErrorKind) (string, bool) {
	switch k := k.(type) {
	case *kind.Required:
		return "must contain " + pyRepr(sortedAny(k.Missing)) + " properties", true
	case *kind.AdditionalProperties:
		return "must not contain " + pyRepr(sortedAny(k.Properties)) + " properties", true
	case *kind.MinProperties:
		return fmt.Sprintf("must contain at least %d properties", k.Want), true
	case *kind.MaxProperties:
		return fmt.Sprintf("must contain less than or equal to %d properties", k.Want), true
	case *kind.Dependency:
		return fmt.Sprintf("missing dependency %s for %s", strings.Join(k.Missing, ", "), k.Prop), true
	case *kind.DependentRequired:
		return fmt.Sprintf("missing dependency %s for %s", strings.Join(k.Missing, ", "), k.Prop), true
	case *kind.PropertyNames:
		return "must be named by propertyName definition", true
	default:
		return describeComposite(k)
	}
}

// describeComposite words the failures about an array's items and those of
// the combining keywords.
func describeComposite(k jsonschema.ErrorKind) (string, bool) {
	switch k := k.(type) {
	case *kind.MinItems:
		return fmt.Sprintf("must contain at least %d items", k.Want), true
	case *kind.MaxItems:
		return fmt.Sprintf("must contain less than or equal to %d items", k.Want), true
	case *kind.UniqueItems:
		return "must contain unique items", true
	case *kind.AdditionalItems:
		return "must contain only specified items", true
	case *kind.Contains, *kind.MinContains, *kind.MaxContains:
		return "must contain one of contains definition", true
	case *kind.AnyOf:
		return "cannot be validated by any definition", true
	case *kind.OneOf:
		return fmt.Sprintf("must be valid exactly by one definition (%d matches found)", len(k.Subschemas)), true
	case *kind.Not:
		return "must NOT match a disallowed definition", true
	case *kind.FalseSchema:
		return "must not be there", true
	default:
		return "", false
	}
}

// number renders a keyword's number as the schema spells it, or else from
// the engine's rational.
func number(ruleDefinition any, want *big.Rat) string {
	if n, ok := ruleDefinition.(fmt.Stringer); ok {
		return n.String()
	}
	if want == nil {
		return "?"
	}
	if want.IsInt() {
		return want.Num().String()
	}
	f, _ := want.Float64()
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func sortedAny(names []string) []any {
	sorted := slices.Sorted(slices.Values(names))
	out := make([]any, len(sorted))
	for i, n := range sorted {
		out[i] = n
	}
	return out
}

// pyStr renders a decoded JSON value the way Python's str does, as the
// f-string of Python's message: a string as it is, anything else as repr.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyRepr renders a decoded JSON value the way Python's repr renders the
// same value, for messages that match Powertools Python's.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyString(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := slices.Sorted(maps.Keys(x))
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyString(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case fmt.Stringer: // json.Number
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// pyString quotes s the way Python's repr quotes a str.
func pyString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i-1])
		case strconv.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
