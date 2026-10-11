package eventhandler

import (
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	jsv "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// validators are the compiled schemas of a typed operation, taken from the
// operation's OpenAPI description so that documentation and validation
// cannot disagree.
type validators struct {
	request  *jsv.Schema
	response *jsv.Schema
}

const schemaURL = "mem://powertools/operation.json"

// compile describes the operation in an OpenAPI document of its own and
// compiles the schemas of its parameters, body and response.
func (o *operation[In, Out]) compile() (*validators, error) {
	refl := newReflector("3.1.0")
	path := validationPath(o.pat)
	oc, err := refl.NewOperationContext(http.MethodPost, path)
	if err != nil {
		return nil, fmt.Errorf("describe the operation: %w", err)
	}
	o.describeInput(oc)
	addResponse(oc, o.outModel(), http.StatusOK, DefaultResponseDescription)
	if err = refl.AddOperation(oc); err != nil {
		return nil, fmt.Errorf("describe the operation: %w", err)
	}
	doc, err := specMap(refl)
	if err != nil {
		return nil, err
	}
	op := at(doc, "paths", path, "post")
	props := map[string]any{}
	for _, raw := range list(op["parameters"]) {
		param := object(raw)
		schema, ok := param["schema"]
		if !ok {
			continue
		}
		loc := object(props[stringOf(param["in"])])
		if loc == nil {
			loc = map[string]any{keyType: typeObject, keyProperties: map[string]any{}}
			props[stringOf(param["in"])] = loc
		}
		child(loc, keyProperties)[stringOf(param[keyName])] = schema
	}
	if body := mediaSchema(op["requestBody"]); body != nil {
		props[inBody] = body
	}
	defs := map[string]any{"request": map[string]any{keyType: typeObject, keyProperties: props}}
	if resp := mediaSchema(at(op, "responses")["200"]); resp != nil {
		defs["output"] = resp
	}
	for name, s := range at(doc, "components", keySchemas) {
		defs["c:"+name] = s
	}
	root := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "$defs": rewriteRefs(defs)}
	return compileValidators(root)
}

// validationPath is a path template naming the path parameters of p, for the
// reflector, which needs one even for a regular expression route.
func validationPath(p *pattern) string {
	if path, ok := p.openAPIPath(); ok {
		return path
	}
	var b strings.Builder
	for _, name := range p.names() {
		b.WriteString("/{" + name + "}")
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// mediaSchema returns the schema of the JSON or form content of a request
// body or a response.
func mediaSchema(v any) any {
	content := at(v, "content")
	for _, ct := range []string{ContentTypeJSON, formContentType, multipartContentType} {
		if schema, ok := object(content[ct])["schema"]; ok {
			return schema
		}
	}
	return nil
}

// rewriteRefs points the references to components at the $defs of the
// validation document.
func rewriteRefs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if s, ok := child.(string); ok && k == "$ref" {
				x[k] = strings.Replace(s, "#/components/schemas/", "#/$defs/c:", 1)
				continue
			}
			x[k] = rewriteRefs(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = rewriteRefs(child)
		}
		return x
	default:
		return v
	}
}

func compileValidators(root map[string]any) (*validators, error) {
	data, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode the validation schema: %w", err)
	}
	doc, err := jsv.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode the validation schema: %w", err)
	}
	c := jsv.NewCompiler()
	c.DefaultDraft(jsv.Draft2020)
	c.AssertFormat()
	if err = c.AddResource(schemaURL, doc); err != nil {
		return nil, fmt.Errorf("load the validation schema: %w", err)
	}
	v := &validators{}
	if v.request, err = c.Compile(schemaURL + "#/$defs/request"); err != nil {
		return nil, fmt.Errorf("compile the request schema: %w", err)
	}
	if _, ok := object(root["$defs"])["output"]; ok {
		if v.response, err = c.Compile(schemaURL + "#/$defs/output"); err != nil {
			return nil, fmt.Errorf("compile the response schema: %w", err)
		}
	}
	return v, nil
}

// decodeInstance decodes JSON the way the validator reads it, with numbers
// kept exact.
func decodeInstance(data []byte) (any, error) {
	v, err := jsv.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	return v, nil
}

// validateRequest validates the bound parameters and the body, skipping the
// parameters binding already reported.
func (v *validators) validateRequest(instance map[string]any, failed map[string]bool) []ValidationErrorDetail {
	err := v.request.Validate(instance)
	if err == nil {
		return nil
	}
	ve, ok := errors.AsType[*jsv.ValidationError](err)
	if !ok {
		return []ValidationErrorDetail{{Loc: []any{inBody}, Type: errTypeValue, Msg: err.Error()}}
	}
	var out []ValidationErrorDetail
	collectErrors(ve, instance, nil, &out)
	return slices.DeleteFunc(out, func(d ValidationErrorDetail) bool {
		if len(d.Loc) != 2 || d.Type != errTypeMissing {
			return false
		}
		return failed[failedKey(fmt.Sprint(d.Loc[0]), fmt.Sprint(d.Loc[1]))]
	})
}

// validateResponse validates the body of a typed operation's response, when
// response validation is on for the route.
func (o *operation[In, Out]) validateResponse(req *http.Request, st *requestState, body any) error {
	if st == nil || st.route == nil {
		return nil
	}
	status := st.router.responseValidationStatus(st.route)
	if status == 0 {
		return nil
	}
	if _, raw := body.([]byte); raw {
		return nil
	}
	v, err := o.validators()
	if err != nil || v.response == nil {
		return err
	}
	data, err := serialize(req, body)
	if err != nil {
		return err
	}
	instance, err := decodeInstance(data)
	if err != nil {
		return err
	}
	err = v.response.Validate(instance)
	if err == nil {
		return nil
	}
	var details []ValidationErrorDetail
	if ve, ok := errors.AsType[*jsv.ValidationError](err); ok {
		collectErrors(ve, instance, []any{locResponse}, &details)
	} else {
		details = []ValidationErrorDetail{{Loc: []any{locResponse}, Type: errTypeValue, Msg: err.Error()}}
	}
	return &ResponseValidationError{Errors: details, StatusCode: status, Body: body}
}

// printer writes the messages of validation errors, for logs.
var printer = sync.OnceValue(func() *message.Printer { return message.NewPrinter(language.English) })

// collectErrors turns the engine's error tree into Pydantic-like details.
func collectErrors(ve *jsv.ValidationError, instance any, prefix []any, out *[]ValidationErrorDetail) {
	loc := append(slices.Clone(prefix), instanceLoc(instance, ve.InstanceLocation)...)
	add := func(typ string) {
		*out = append(*out, ValidationErrorDetail{Loc: loc, Type: typ, Msg: ve.ErrorKind.LocalizedString(printer())})
	}
	switch k := ve.ErrorKind.(type) {
	case *kind.Required:
		for _, name := range k.Missing {
			*out = append(*out, ValidationErrorDetail{Loc: append(slices.Clone(loc), name), Type: errTypeMissing, Msg: "Field required"})
		}
	case *kind.AdditionalProperties:
		for _, name := range k.Properties {
			*out = append(*out, ValidationErrorDetail{Loc: append(slices.Clone(loc), name), Type: "extra_forbidden", Msg: "Extra inputs are not permitted"})
		}
	case *kind.Type:
		add(typeError(k))
	case *kind.Format:
		add(formatError(k.Want))
	case *kind.AnyOf, *kind.OneOf:
		causes := slices.DeleteFunc(slices.Clone(ve.Causes), onlyNullMismatch)
		if len(causes) == 0 {
			add(errTypeValue)
		}
		for _, c := range causes {
			collectErrors(c, instance, prefix, out)
		}
	default:
		if typ, ok := keywordError(ve); ok {
			add(typ)
			return
		}
		for _, c := range ve.Causes {
			collectErrors(c, instance, prefix, out)
		}
	}
}

// keywordError maps a failed keyword without causes to its error type.
func keywordError(ve *jsv.ValidationError) (string, bool) {
	if len(ve.Causes) > 0 {
		return "", false
	}
	if kp := ve.ErrorKind.KeywordPath(); len(kp) > 0 {
		if typ, ok := keywordErrors[kp[0]]; ok {
			return typ, true
		}
	}
	return errTypeValue, true
}

var keywordErrors = map[string]string{
	"minLength":        "string_too_short",
	"maxLength":        "string_too_long",
	"pattern":          "string_pattern_mismatch",
	"minimum":          "greater_than_equal",
	"exclusiveMinimum": "greater_than",
	"maximum":          "less_than_equal",
	"exclusiveMaximum": "less_than",
	"multipleOf":       "multiple_of",
	"enum":             "enum",
	"const":            "literal_error",
	"minItems":         "too_short",
	"maxItems":         "too_long",
	"minProperties":    "too_short",
	"maxProperties":    "too_long",
}

// onlyNullMismatch reports a union branch that failed only because the value
// is not null: the nullable side of an optional value, which says nothing
// about what is wrong.
func onlyNullMismatch(ve *jsv.ValidationError) bool {
	if t, ok := ve.ErrorKind.(*kind.Type); ok {
		return len(t.Want) == 1 && t.Want[0] == typeNull
	}
	if len(ve.Causes) == 1 {
		return onlyNullMismatch(ve.Causes[0])
	}
	return false
}

func typeError(k *kind.Type) string {
	want := ""
	for _, w := range k.Want {
		if w != typeNull {
			want = w
			break
		}
	}
	switch want {
	case typeInteger:
		switch k.Got {
		case typeString:
			return errTypeIntParsing
		case "number":
			return "int_from_float"
		default:
			return "int_type"
		}
	case "number":
		if k.Got == typeString {
			return "float_parsing"
		}
		return "float_type"
	case typeString:
		return "string_type"
	case "boolean":
		if k.Got == typeString {
			return "bool_parsing"
		}
		return "bool_type"
	case typeObject:
		return "model_attributes_type"
	case typeArray:
		return "list_type"
	case "":
		return "none_required"
	default:
		return errTypeValue
	}
}

func formatError(format string) string {
	switch format {
	case "date-time":
		return "datetime_parsing"
	case "date":
		return "date_parsing"
	case "time":
		return "time_parsing"
	case "duration":
		return "time_delta_parsing"
	case "uuid":
		return "uuid_parsing"
	case "uri", "uri-reference", "iri", "iri-reference":
		return "url_parsing"
	case "ipv4":
		return "ip_v4_address"
	case "ipv6":
		return "ip_v6_address"
	default:
		return errTypeValue
	}
}

// instanceLoc converts the engine's location to Python's: array indexes as
// ints.
func instanceLoc(instance any, path []string) []any {
	loc := make([]any, 0, len(path))
	cur := instance
	for _, seg := range path {
		switch c := cur.(type) {
		case []any:
			i, err := strconv.Atoi(seg)
			if err == nil && i >= 0 && i < len(c) {
				loc = append(loc, i)
				cur = c[i]
				continue
			}
			loc = append(loc, seg)
			cur = nil
		case map[string]any:
			loc = append(loc, seg)
			cur = c[seg]
		default:
			loc = append(loc, seg)
			cur = nil
		}
	}
	return loc
}

var locationRank = map[string]int{inPath: 0, inQuery: 1, inHeader: 2, inCookie: 3, inBody: 4, locResponse: 5}

// locResponse is where a response validation error is.
const locResponse = "response"

// sortDetails orders the errors as Python reports them: path, query,
// header, cookie, body; parameters in declaration order.
func sortDetails(details []ValidationErrorDetail, plan *inputPlan) {
	order := map[string]int{}
	for _, p := range plan.params {
		loc := p.in
		if loc == inForm {
			loc = inBody
		}
		order[loc+"/"+p.name] = p.order
	}
	key := func(d ValidationErrorDetail) (int, int, string) {
		if len(d.Loc) == 0 {
			return 99, 0, ""
		}
		where := fmt.Sprint(d.Loc[0])
		rank, ok := locationRank[where]
		if !ok {
			rank = 98
		}
		pos := 0
		if len(d.Loc) > 1 {
			pos = order[where+"/"+fmt.Sprint(d.Loc[1])]
		}
		return rank, pos, fmt.Sprint(d.Loc)
	}
	slices.SortStableFunc(details, func(a, b ValidationErrorDetail) int {
		ra, pa, sa := key(a)
		rb, pb, sb := key(b)
		return cmp.Or(cmp.Compare(ra, rb), cmp.Compare(pa, pb), cmp.Compare(sa, sb))
	})
}
