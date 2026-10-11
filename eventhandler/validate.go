package eventhandler

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/schema"
)

// validators are the compiled schemas of a typed operation, taken from the
// operation's OpenAPI description so that documentation and validation
// cannot disagree.
type validators struct {
	request  *schema.Schema
	response *schema.Schema
}

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
		paramSchema, ok := param["schema"]
		if !ok {
			continue
		}
		loc := object(props[stringOf(param["in"])])
		if loc == nil {
			loc = map[string]any{keyType: typeObject, keyProperties: map[string]any{}}
			props[stringOf(param["in"])] = loc
		}
		child(loc, keyProperties)[stringOf(param[keyName])] = paramSchema
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
		if s, ok := object(content[ct])["schema"]; ok {
			return s
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
	c := schema.NewCompiler(root)
	v := &validators{}
	var err error
	if v.request, err = c.Compile("#/$defs/request"); err != nil {
		return nil, fmt.Errorf("compile the request schema: %w", err)
	}
	if _, ok := object(root["$defs"])["output"]; ok {
		if v.response, err = c.Compile("#/$defs/output"); err != nil {
			return nil, fmt.Errorf("compile the response schema: %w", err)
		}
	}
	return v, nil
}

// decodeInstance decodes JSON the way the validator reads it, with numbers
// kept exact.
func decodeInstance(data []byte) (any, error) {
	v, err := schema.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	return v, nil
}

// validateRequest validates the bound parameters and the body, skipping the
// parameters binding already reported.
func (v *validators) validateRequest(instance map[string]any, failed map[string]bool) []ValidationErrorDetail {
	out := details(v.request.Validate(instance), nil)
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
	if o.validators.response == nil {
		return nil
	}
	data, err := serialize(req, body)
	if err != nil {
		return err
	}
	instance, err := decodeInstance(data)
	if err != nil {
		return err
	}
	failures := o.validators.response.Validate(instance)
	if len(failures) == 0 {
		return nil
	}
	return &ResponseValidationError{Errors: details(failures, []any{locResponse}), StatusCode: status, Body: body}
}

// details turns the failures of a schema into Pydantic-like details, at
// prefix.
func details(failures []schema.Failure, prefix []any) []ValidationErrorDetail {
	var out []ValidationErrorDetail
	for i := range failures {
		f := &failures[i]
		loc := append(slices.Clone(prefix), f.Loc...)
		switch f.Keyword {
		case "required":
			for _, name := range f.Names {
				out = append(out, ValidationErrorDetail{Loc: append(slices.Clone(loc), name), Type: errTypeMissing, Msg: "Field required"})
			}
		case "additionalProperties":
			for _, name := range f.Names {
				out = append(out, ValidationErrorDetail{Loc: append(slices.Clone(loc), name), Type: "extra_forbidden", Msg: "Extra inputs are not permitted"})
			}
		case "type":
			out = append(out, ValidationErrorDetail{Loc: loc, Type: typeError(f.Got, f.Want), Msg: f.Msg})
		case "format":
			out = append(out, ValidationErrorDetail{Loc: loc, Type: formatError(f.Format), Msg: f.Msg})
		default:
			typ, ok := keywordErrors[f.Keyword]
			if !ok {
				typ = errTypeValue
			}
			out = append(out, ValidationErrorDetail{Loc: loc, Type: typ, Msg: f.Msg})
		}
	}
	return out
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

func typeError(got string, wants []string) string {
	want := ""
	for _, w := range wants {
		if w != typeNull {
			want = w
			break
		}
	}
	switch want {
	case typeInteger:
		switch got {
		case typeString:
			return errTypeIntParsing
		case "number":
			return "int_from_float"
		default:
			return "int_type"
		}
	case "number":
		if got == typeString {
			return "float_parsing"
		}
		return "float_type"
	case typeString:
		return "string_type"
	case "boolean":
		if got == typeString {
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
