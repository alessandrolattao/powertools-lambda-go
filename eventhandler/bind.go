package eventhandler

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// Where a parameter comes from, as the OpenAPI document and the "loc" of a
// validation error name it.
const (
	inPath   = "path"
	inQuery  = "query"
	inHeader = "header"
	inCookie = "cookie"
	inBody   = "body"
	inForm   = "formData"
)

var locations = []string{inPath, inQuery, inHeader, inCookie, inForm}

// maxMemory bounds the multipart form kept in memory; a Lambda payload is
// smaller anyway.
const maxMemory = 32 << 20

// inputPlan is how a typed operation fills its input struct, computed once
// at registration.
type inputPlan struct {
	typ    reflect.Type
	params []paramField
	body   *bodyField
	form   bool
}

type paramField struct {
	index    []int
	in       string
	name     string
	scalar   reflect.Type
	slice    bool
	ptr      bool
	required bool
	def      *string
	file     bool
	order    int
}

type bodyField struct {
	index    []int
	typ      reflect.Type
	raw      bool
	required bool
}

// ErrInvalidOperation reports the input struct of a typed operation that
// cannot be bound: Register panics with it.
var ErrInvalidOperation = errors.New("invalid typed operation")

// Pydantic's error types the binder reports.
const (
	errTypeMissing    = "missing"
	errTypeValue      = "value_error"
	errTypeIntParsing = "int_parsing"
)

var (
	errMissingBoundary = errors.New("missing boundary in multipart/form-data content-type header")
	errNotBoolean      = errors.New("not a boolean")
	errNotFinite       = errors.New("not a finite number")
)

var (
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	fileHeaderType  = reflect.TypeFor[*multipart.FileHeader]()
	timeType        = reflect.TypeFor[time.Time]()
)

func planInput(t reflect.Type) (*inputPlan, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: the input %s must be a struct", ErrInvalidOperation, t)
	}
	p := &inputPlan{typ: t}
	if err := p.walk(t, nil); err != nil {
		return nil, err
	}
	if p.form && p.body != nil {
		return nil, fmt.Errorf("%w: the input cannot have both a Body and formData fields", ErrInvalidOperation)
	}
	return p, nil
}

func (p *inputPlan) walk(t reflect.Type, prefix []int) error {
	for i := range t.NumField() {
		index := append(append([]int(nil), prefix...), i)
		if err := p.field(t.Field(i), index); err != nil {
			return err
		}
	}
	return nil
}

// field plans one field: a parameter, the body, an embedded struct whose
// fields count as the input's own, or nothing.
func (p *inputPlan) field(f reflect.StructField, index []int) error {
	in, name := fieldLocation(f)
	switch {
	case in == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
		return p.walk(f.Type, index)
	case !f.IsExported():
		return nil
	case in == "":
		if f.Name == "Body" && p.body == nil {
			p.body = &bodyField{index: index, typ: f.Type, raw: f.Type == bytesType, required: f.Type.Kind() != reflect.Pointer}
		}
		return nil
	}
	param, err := planParam(f, index, in, name)
	if err != nil {
		return err
	}
	param.order = len(p.params)
	p.form = p.form || in == inForm
	p.params = append(p.params, param)
	return nil
}

func fieldLocation(f reflect.StructField) (in, name string) {
	for _, loc := range locations {
		if v, ok := f.Tag.Lookup(loc); ok {
			name, _, _ = strings.Cut(v, ",")
			if name == "" {
				name = f.Name
			}
			return loc, name
		}
	}
	return "", ""
}

func planParam(f reflect.StructField, index []int, in, name string) (paramField, error) {
	param := paramField{index: index, in: in, name: name, scalar: f.Type}
	if param.scalar.Kind() == reflect.Slice && param.scalar.Elem() != reflect.TypeFor[byte]() {
		param.slice, param.scalar = true, param.scalar.Elem()
	}
	if !param.slice && param.scalar.Kind() == reflect.Pointer && param.scalar != fileHeaderType {
		param.ptr, param.scalar = true, param.scalar.Elem()
	}
	if param.scalar == fileHeaderType {
		if in != inForm {
			return param, fmt.Errorf("%w: field %s: files are formData fields", ErrInvalidOperation, f.Name)
		}
		param.file = true
	} else if !bindable(param.scalar) {
		return param, fmt.Errorf("%w: field %s: a %s parameter cannot be a %s", ErrInvalidOperation, f.Name, in, f.Type)
	}
	if v, ok := f.Tag.Lookup("required"); ok {
		required, err := strconv.ParseBool(v)
		if err != nil {
			return param, fmt.Errorf("%w: field %s: required tag %q: %w", ErrInvalidOperation, f.Name, v, err)
		}
		param.required = required
	}
	if in == inPath {
		param.required = true
	}
	if v, ok := f.Tag.Lookup("default"); ok {
		param.def = &v
	}
	return param, nil
}

func bindable(t reflect.Type) bool {
	if reflect.PointerTo(t).Implements(textUnmarshaler) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

// binding is the state of one request being bound.
type binding struct {
	req      *http.Request
	st       *requestState
	value    reflect.Value
	instance map[string]any
	details  []ValidationErrorDetail
	// failed lists the parameters already reported, by location and name,
	// so that schema validation does not report them as missing too.
	failed map[string]bool
	query  url.Values
	form   url.Values
	files  map[string][]*multipart.FileHeader
	body   []byte
	// commaQuery splits the values of list query parameters on commas.
	commaQuery bool
}

// failParam reports a parameter that cannot be bound, at loc (path, query,
// header, cookie, body), name, then the index of a list item.
func (b *binding) failParam(loc, name, typ, msg string, input any, index ...any) {
	b.details = append(b.details, ValidationErrorDetail{Loc: append([]any{loc, name}, index...), Type: typ, Msg: msg, Input: input})
	b.failed[failedKey(loc, name)] = true
}

// failBody reports a body that cannot be read, at "body" then where.
func (b *binding) failBody(typ, msg string, where ...any) {
	b.details = append(b.details, ValidationErrorDetail{Loc: append([]any{inBody}, where...), Type: typ, Msg: msg})
}

func failedKey(loc, name string) string {
	return loc + "/" + name
}

// bind fills the input struct from the request and validates it.
func (o *operation[In, Out]) bind(ctx context.Context, req *http.Request, st *requestState) (In, error) {
	var in In
	inv := lambdahttp.FromContext(ctx)
	b := &binding{
		req: req, st: st, value: reflect.ValueOf(&in).Elem(),
		instance: map[string]any{}, failed: map[string]bool{},
		commaQuery: inv != nil && inv.CommaQuery,
	}
	if err := b.readBody(o.plan); err != nil {
		return in, err
	}
	for _, f := range o.plan.params {
		b.bindParam(f)
	}
	jsonBody, hasJSON := b.bindBody(o.plan)
	v, err := o.validators()
	if err != nil {
		return in, err
	}
	b.details = append(b.details, v.validateRequest(b.instance, b.failed)...)
	if len(b.details) > 0 {
		sortDetails(b.details, o.plan)
		return in, &RequestValidationError{Errors: b.details, Body: b.body}
	}
	if hasJSON {
		if err := b.decodeBody(o.plan.body, jsonBody); err != nil {
			return in, err
		}
	}
	return in, nil
}

// readBody reads the body when the operation has one, and parses forms.
func (b *binding) readBody(plan *inputPlan) error {
	if plan.body == nil && !plan.form {
		return nil
	}
	if b.req.Body != nil {
		data, err := io.ReadAll(b.req.Body)
		if err != nil {
			return fmt.Errorf("read the request body: %w", err)
		}
		b.body = data
	}
	if plan.body != nil && plan.body.raw {
		return nil
	}
	contentType := strings.TrimSpace(b.req.Header.Get("Content-Type"))
	if plan.body != nil {
		if contentType == "" || strings.HasPrefix(contentType, ContentTypeJSON) {
			return nil
		}
		return &UnsupportedContentTypeError{ContentType: contentType}
	}
	switch {
	case strings.HasPrefix(contentType, formContentType):
		form, err := url.ParseQuery(string(b.body))
		if err != nil {
			return &RequestValidationError{Errors: []ValidationErrorDetail{{Loc: []any{inBody}, Type: "form_invalid", Msg: err.Error()}}, Body: b.body}
		}
		b.form = form
	case strings.HasPrefix(contentType, multipartContentType):
		return b.readMultipart(contentType)
	case contentType == "" && len(b.body) == 0:
		b.form = url.Values{}
	default:
		return &UnsupportedContentTypeError{ContentType: contentType}
	}
	return nil
}

func (b *binding) readMultipart(contentType string) error {
	invalid := func(err error) error {
		return &RequestValidationError{Errors: []ValidationErrorDetail{{Loc: []any{inBody}, Type: "multipart_invalid", Msg: err.Error()}}, Body: b.body}
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return invalid(err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		// Python raises an unhandled ValueError here; a request without a
		// boundary is a bad request, not a crash.
		return invalid(errMissingBoundary)
	}
	form, err := multipart.NewReader(bytes.NewReader(b.body), boundary).ReadForm(maxMemory)
	if err != nil {
		return invalid(err)
	}
	b.form, b.files = form.Value, form.File
	return nil
}

// values returns the raw values of a parameter, and whether it is present.
func (b *binding) values(f paramField) ([]string, bool) {
	switch f.in {
	case inPath:
		v := b.req.PathValue(f.name)
		return []string{v}, true
	case inQuery:
		if b.query == nil {
			b.query = b.req.URL.Query()
		}
		vs, ok := b.query[f.name]
		if ok && f.slice && b.commaQuery {
			vs = splitCommas(vs)
		}
		return vs, ok
	case inHeader:
		vs := b.req.Header.Values(f.name)
		if f.slice {
			vs = splitCommas(vs)
		}
		return vs, len(vs) > 0
	case inCookie:
		cookies := b.req.CookiesNamed(f.name)
		vs := make([]string, len(cookies))
		for i, c := range cookies {
			vs[i] = c.Value
		}
		return vs, len(vs) > 0
	default:
		vs, ok := b.form[f.name]
		return vs, ok
	}
}

func splitCommas(values []string) []string {
	var out []string
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}

// location returns the instance object of a parameter location.
func (b *binding) location(in string) map[string]any {
	key := in
	if in == inForm {
		key = inBody
	}
	m, ok := b.instance[key].(map[string]any)
	if !ok {
		m = map[string]any{}
		b.instance[key] = m
	}
	return m
}

func (b *binding) bindParam(f paramField) {
	field := b.value.FieldByIndex(f.index)
	if f.file {
		b.bindFile(f, field)
		return
	}
	loc := f.in
	if loc == inForm {
		loc = inBody
	}
	raw, present := b.values(f)
	if !present && f.def != nil {
		raw, present = []string{*f.def}, true
		if f.slice {
			raw = splitCommas(raw)
		}
	}
	if !present {
		if f.required {
			b.failParam(loc, f.name, errTypeMissing, "Field required", nil)
		}
		return
	}
	if !f.slice {
		raw = raw[:1]
	}
	values, items, ok := b.coerceAll(f, loc, raw)
	if !ok {
		b.failed[failedKey(loc, f.name)] = true
		return
	}
	switch {
	case f.slice:
		s := reflect.MakeSlice(field.Type(), len(values), len(values))
		for i, v := range values {
			s.Index(i).Set(v)
		}
		field.Set(s)
		b.location(f.in)[f.name] = items
	case f.ptr:
		ptr := reflect.New(f.scalar)
		ptr.Elem().Set(values[0])
		field.Set(ptr)
		b.location(f.in)[f.name] = items[0]
	default:
		field.Set(values[0])
		b.location(f.in)[f.name] = items[0]
	}
}

// coerceAll converts the raw values of a parameter, reporting each failure.
func (b *binding) coerceAll(f paramField, loc string, raw []string) ([]reflect.Value, []any, bool) {
	items := make([]any, 0, len(raw))
	values := make([]reflect.Value, 0, len(raw))
	for i, r := range raw {
		v, inst, typ, err := coerce(r, f.scalar)
		if err != nil {
			var index []any
			if f.slice {
				index = []any{i}
			}
			b.failParam(loc, f.name, typ, err.Error(), r, index...)
			continue
		}
		items = append(items, inst)
		values = append(values, v)
	}
	return values, items, len(values) == len(raw)
}

func (b *binding) bindFile(f paramField, field reflect.Value) {
	files := b.files[f.name]
	if len(files) == 0 {
		if f.required {
			b.failParam(inBody, f.name, errTypeMissing, "Field required", nil)
		}
		return
	}
	// The schema of a file is a binary string: the file name stands for it.
	if f.slice {
		field.Set(reflect.ValueOf(files))
		names := make([]any, len(files))
		for i, fh := range files {
			names[i] = fh.Filename
		}
		b.location(f.in)[f.name] = names
		return
	}
	field.Set(reflect.ValueOf(files[0]))
	b.location(f.in)[f.name] = files[0].Filename
}

// coerce converts a raw parameter value to t. It returns the Go value, the
// value for schema validation and, on failure, the Pydantic error type.
func coerce(raw string, t reflect.Type) (value reflect.Value, instance any, errType string, err error) {
	ptr := reflect.New(t)
	if u, ok := reflect.TypeAssert[encoding.TextUnmarshaler](ptr); ok {
		if err := u.UnmarshalText([]byte(raw)); err != nil {
			typ := errTypeValue
			if t == timeType {
				typ = "datetime_parsing"
			}
			return reflect.Value{}, nil, typ, fmt.Errorf("parse %q as %s: %w", raw, t, err)
		}
		return ptr.Elem(), raw, "", nil
	}
	v := ptr.Elem()
	switch t.Kind() {
	case reflect.Bool:
		parsed, ok := env.ParseBool(raw)
		if !ok {
			return v, nil, "bool_parsing", fmt.Errorf("%q: %w", raw, errNotBoolean)
		}
		v.SetBool(parsed)
		return v, parsed, "", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(raw, 10, t.Bits())
		if err != nil {
			return v, nil, errTypeIntParsing, fmt.Errorf("parse %q as an integer: %w", raw, err)
		}
		v.SetInt(parsed)
		return v, parsed, "", nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(raw, 10, t.Bits())
		if err != nil {
			return v, nil, errTypeIntParsing, fmt.Errorf("parse %q as a non-negative integer: %w", raw, err)
		}
		v.SetUint(parsed)
		return v, parsed, "", nil
	case reflect.Float32, reflect.Float64:
		return coerceFloat(raw, v)
	default:
		v.SetString(raw)
		return v, raw, "", nil
	}
}

func coerceFloat(raw string, v reflect.Value) (value reflect.Value, instance any, errType string, err error) {
	parsed, err := strconv.ParseFloat(raw, v.Type().Bits())
	if err != nil {
		return v, nil, "float_parsing", fmt.Errorf("parse %q as a number: %w", raw, err)
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return v, nil, "finite_number", fmt.Errorf("%q: %w", raw, errNotFinite)
	}
	v.SetFloat(parsed)
	return v, parsed, "", nil
}

// bindBody checks the JSON body and puts it in the validation instance. It
// returns the body to decode once validation passes.
func (b *binding) bindBody(plan *inputPlan) ([]byte, bool) {
	body := plan.body
	if body == nil {
		return nil, false
	}
	if body.raw {
		b.value.FieldByIndex(body.index).SetBytes(b.body)
		return nil, false
	}
	if len(bytes.TrimSpace(b.body)) == 0 {
		if body.required {
			b.failBody(errTypeMissing, "Field required")
		}
		return nil, false
	}
	var syntax jsontext.Value
	if err := json.Unmarshal(b.body, &syntax); err != nil {
		var offset int64
		if se, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
			offset = se.ByteOffset
		}
		b.failBody("json_invalid", "JSON decode error: "+err.Error(), offset)
		return nil, false
	}
	instance, err := decodeInstance(b.body)
	if err != nil {
		b.failBody("json_invalid", "JSON decode error: "+err.Error())
		return nil, false
	}
	b.instance[inBody] = instance
	return b.body, true
}

// decodeBody decodes the validated body into the Body field.
func (b *binding) decodeBody(body *bodyField, data []byte) error {
	deserialize := defaultConfig().deserializer
	if b.st != nil {
		deserialize = b.st.router.cfg.deserializer
	}
	ptr := reflect.New(body.typ)
	if err := deserialize(data, ptr.Interface()); err != nil {
		return &RequestValidationError{
			Errors: []ValidationErrorDetail{{Loc: []any{inBody}, Type: "model_attributes_type", Msg: err.Error()}},
			Body:   data,
		}
	}
	b.value.FieldByIndex(body.index).Set(ptr.Elem())
	return nil
}
