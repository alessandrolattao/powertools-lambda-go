package datamasking_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/datamasking"
)

// asJSON renders an Erase result for comparison with an expected document.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v, json.Deterministic(true))
	require.NoError(t, err)
	return string(raw)
}

// The payloads of the Powertools documentation (examples/data_masking/src).
const (
	genericData = `{
		"id": 1, "name": "John Doe", "age": 30, "email": "johndoe@example.com",
		"address": {"street": "123 Main St", "city": "Anytown", "state": "CA", "zip": "12345"},
		"company_address": {"street": "456 ACME Ave", "city": "Anytown", "state": "CA", "zip": "12345"}
	}`
	nestedData = `{"a": {"1": {"None": "hello", "four": "world"}, "b": {"3": {"4": "goodbye", "e": "world"}}}}`
	listData   = `{
		"name": "Carlos", "operation": "non sensitive", "card_number": "1111 2222 3333 4444",
		"address": [
			{"postcode": 12345, "street": "123 Any Street"},
			{"postcode": 67890, "street": "100 Main Street"},
			{"postcode": 11111, "street": "111 Any Drive"}
		]
	}`
	customMaskingData = `{
		"id": 1, "name": "Jane Doe", "age": 30, "email": "janedoe@example.com",
		"address": {"street": "123 Main St", "city": "Anytown", "state": "CA", "zip": "12345", "postcode": 12345, "product": {"name": "Car"}},
		"other_address": [{"postcode": 11345, "street": "123 Any Drive"}, {"postcode": 67890, "street": "100 Main Street,"}],
		"company_address": {"street": "456 ACME Ave", "city": "Anytown", "state": "CA", "zip": "12345"}
	}`
)

func TestEraseWholeData(t *testing.T) {
	t.Parallel()
	type user struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name string
		data any
		opts []datamasking.EraseOption
		want string
	}{
		{name: "int", data: 42, want: `"*****"`},
		{name: "float", data: 4.2, want: `"*****"`},
		{name: "bool", data: true, want: `"*****"`},
		{name: "nil", data: nil, want: `"*****"`},
		{name: "string", data: "this is a string", want: `"*****"`},
		{name: "list keeps its length", data: []any{1, 2, "string", 3}, want: `["*****","*****","*****","*****"]`},
		{name: "list of objects is not traversed", data: []map[string]int{{"a": 1}}, want: `["*****"]`},
		{name: "object", data: map[string]any{"a": map[string]any{"b": "c"}}, want: `"*****"`},
		{name: "struct", data: user{Name: "Pat"}, want: `"*****"`},
		{name: "raw JSON object", data: []byte(nestedData), want: `"*****"`},
		{name: "raw JSON array", data: jsontext.Value(`[1,{"a":2}]`), want: `["*****","*****"]`},
		{name: "custom mask on an int", data: 42, opts: []datamasking.EraseOption{datamasking.WithCustomMask("XX")}, want: `"XX"`},
		{name: "custom mask longer than the text", data: "12345", opts: []datamasking.EraseOption{datamasking.WithCustomMask("XX-XX-XX")}, want: `"XX-XX"`},
		{name: "custom mask shorter than the text", data: "12345", opts: []datamasking.EraseOption{datamasking.WithCustomMask("XX")}, want: `"XX"`},
		{name: "dynamic mask", data: "XYZEKDEDE", opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"*********"`},
		{name: "dynamic mask keeps separators", data: "123 Main-St_4.b", opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"*** ****-**_*.*"`},
		{name: "dynamic mask on empty text", data: "", opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `""`},
		{name: "dynamic mask on a bool", data: true, opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"****"`},
		{name: "dynamic mask on a float", data: 30.0, opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"**"`},
		{name: "dynamic mask on a decimal", data: []byte(`30.0`), opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"**.*"`},
		{name: "dynamic mask counts characters", data: "città", opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"*****"`},
		{name: "masks with nil", data: nil, opts: []datamasking.EraseOption{datamasking.WithDynamicMask()}, want: `"*****"`},
		{
			name: "regex", data: "Hello! My name is John Doe",
			opts: []datamasking.EraseOption{datamasking.WithRegex(`\b[A-Z][a-z]+ [A-Z][a-z]+\b`, "[name]")},
			want: `"Hello! My name is [name]"`,
		},
		{
			name: "regex with groups", data: "johndoe@example.com",
			opts: []datamasking.EraseOption{datamasking.WithRegex(`(.)(.*)(@.*)`, `\1****\3`)},
			want: `"j****@example.com"`,
		},
		{
			name: "a lone pattern is a dynamic mask", data: "abc",
			opts: []datamasking.EraseOption{datamasking.WithRegex(`b`, "")},
			want: `"***"`,
		},
		{
			name: "masks traverse objects", data: map[string]any{"user": map[string]any{"name": "John Doe", "ssn": "123-45-6789"}},
			opts: []datamasking.EraseOption{datamasking.WithCustomMask("REDACTED")},
			want: `{"user":{"name":"REDACTED","ssn":"REDACTED"}}`,
		},
		{name: "masks keep an empty object", data: map[string]any{}, opts: []datamasking.EraseOption{datamasking.WithCustomMask("MASKED")}, want: `{}`},
		{
			name: "masks traverse arrays", data: []string{"name", "phone", "email"},
			opts: []datamasking.EraseOption{datamasking.WithCustomMask("XXX")},
			want: `["XXX","XXX","XXX"]`,
		},
		{
			name: "masks reach nested scalars", data: []byte(`{"a":[1,null,true,{"b":"xy"}]}`),
			opts: []datamasking.EraseOption{datamasking.WithDynamicMask()},
			want: `{"a":["*","*****","****",{"b":"**"}]}`,
		},
	}
	masker := datamasking.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := masker.Erase(tc.data, tc.opts...)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, asJSON(t, got))
		})
	}
}

func TestEraseFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		data   any
		fields []string
		opts   []datamasking.EraseOption
		want   string
	}{
		{
			name: "getting started", data: []byte(genericData), fields: []string{"email", "address.street", "company_address"},
			want: `{"id":1,"name":"John Doe","age":30,"email":"*****",
				"address":{"street":"*****","city":"Anytown","state":"CA","zip":"12345"},"company_address":"*****"}`,
		},
		{
			name: "top keys", data: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444"}`,
			fields: []string{"card_number"},
			want:   `{"name":"Carlos","operation":"non sensitive","card_number":"*****"}`,
		},
		{
			name: "nested key", data: `{"name":"Carlos","address":{"postcode":12345}}`, fields: []string{"address.postcode"},
			want: `{"name":"Carlos","address":{"postcode":"*****"}}`,
		},
		{
			name: "multiple keys", data: `{"address":{"postcode":12345,"street":"123 Any Street"}}`,
			fields: []string{"address.postcode", "address.street"},
			want:   `{"address":{"postcode":"*****","street":"*****"}}`,
		},
		{
			name: "all key items", data: listData, fields: []string{"address"},
			want: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444","address":["*****","*****","*****"]}`,
		},
		{
			name: "complex nested key", data: `{"category":{"subcategory":{"brand":{"product":{"name":"Car"}}}}}`,
			fields: []string{"category..name"},
			want:   `{"category":{"subcategory":{"brand":{"product":{"name":"*****"}}}}}`,
		},
		{
			name: "all fields in a list", data: listData, fields: []string{"address[*].street"},
			want: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444","address":[
				{"postcode":12345,"street":"*****"},{"postcode":67890,"street":"*****"},{"postcode":11111,"street":"*****"}]}`,
		},
		{
			name: "list index", data: listData, fields: []string{"address[1].street"},
			want: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444","address":[
				{"postcode":12345,"street":"123 Any Street"},{"postcode":67890,"street":"*****"},{"postcode":11111,"street":"111 Any Drive"}]}`,
		},
		{
			name: "negative index", data: listData, fields: []string{"address[-1].street"},
			want: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444","address":[
				{"postcode":12345,"street":"123 Any Street"},{"postcode":67890,"street":"100 Main Street"},{"postcode":11111,"street":"*****"}]}`,
		},
		{
			// jsonpath-ng replaces the elements the filter selects; the upstream
			// documentation shows their street erased instead, which is not
			// what Powertools does.
			name: "complex expression", data: listData, fields: []string{"$.address[?(@.postcode > 12000)]"},
			want: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444","address":[
				"*****","*****",{"postcode":11111,"street":"111 Any Drive"}]}`,
		},
		{
			name: "filter then a member", data: listData, fields: []string{"$.address[?(@.postcode > 12000)].street"},
			want: `{"name":"Carlos","operation":"non sensitive","card_number":"1111 2222 3333 4444","address":[
				{"postcode":12345,"street":"*****"},{"postcode":67890,"street":"*****"},{"postcode":11111,"street":"111 Any Drive"}]}`,
		},
		{
			name: "JSON string", data: `{"name": "Carlos", "operation": "non sensitive", "card_number": "1111 2222 3333 4444", "address": {"postcode": 12345}}`,
			fields: []string{"card_number", "address.postcode"},
			want:   `{"name":"Carlos","operation":"non sensitive","card_number":"*****","address":{"postcode":"*****"}}`,
		},
		{
			name: "quoted names and descendants", data: map[string]any{"a": map[string]any{
				"1": map[string]any{"None": "hello", "four": "world"},
				"b": map[string]any{"3": map[string]any{"4": "goodbye", "e": "world"}},
			}},
			fields: []string{"a.'1'.None", "a..'4'"},
			want:   `{"a":{"1":{"None":"*****","four":"world"},"b":{"3":{"4":"*****","e":"world"}}}}`,
		},
		{
			name: "fields and a dynamic mask", data: nestedData, fields: []string{"a.'1'.None", "a..'4'"},
			opts: []datamasking.EraseOption{datamasking.WithDynamicMask()},
			want: `{"a":{"1":{"None":"*****","four":"world"},"b":{"3":{"4":"*******","e":"world"}}}}`,
		},
		{
			name: "fields and a custom mask", data: `{"user":{"contact":{"details":{"address":{"street":"123 Main St","zip":"12345"}}}}}`,
			fields: []string{"user.contact.details.address.zip"}, opts: []datamasking.EraseOption{datamasking.WithCustomMask("xxx")},
			want: `{"user":{"contact":{"details":{"address":{"street":"123 Main St","zip":"xxx"}}}}}`,
		},
		{
			name: "fields and a mask keep the collection", data: `{"user":{"name":["Pat","powertools"]}}`,
			fields: []string{"user.name"}, opts: []datamasking.EraseOption{datamasking.WithDynamicMask()},
			want: `{"user":{"name":["***","**********"]}}`,
		},
		{
			name: "collections keep their type", data: `{"cards":["a","b"]}`, fields: []string{"cards"},
			want: `{"cards":["*****","*****"]}`,
		},
		{
			name: "later fields see earlier results", data: `{"a":{"b":"x"}}`, fields: []string{"a", "a"},
			want: `{"a":"*****"}`,
		},
		{
			name: "a null value is erased", data: `{"a":null}`, fields: []string{"a"},
			want: `{"a":"*****"}`,
		},
		{
			name: "the root", data: `{"a":1}`, fields: []string{"$"},
			want: `"*****"`,
		},
		{
			name: "a top-level array", data: `[{"email":"a@b.c"},{"email":"d@e.f"}]`, fields: []string{"[*].email"},
			want: `[{"email":"*****"},{"email":"*****"}]`,
		},
		{
			name: "hyphenated name", data: `{"non-existing":1}`, fields: []string{"non-existing"},
			want: `{"non-existing":"*****"}`,
		},
		{
			name: "struct", data: struct {
				Name string `json:"name"`
				Age  int    `json:"age"`
			}{Name: "powertools", Age: 42}, fields: []string{"age"},
			want: `{"name":"powertools","age":"*****"}`,
		},
	}
	masker := datamasking.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := append([]datamasking.EraseOption{datamasking.WithFields(tc.fields...)}, tc.opts...)
			got, err := masker.Erase(tc.data, opts...)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, asJSON(t, got))
		})
	}
}

func TestEraseFieldsDoesNotModifyTheInput(t *testing.T) {
	t.Parallel()
	data := map[string]any{"a": map[string]any{"b": "secret"}}
	_, err := datamasking.New().Erase(data, datamasking.WithFields("a.b"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": map[string]any{"b": "secret"}}, data)
}

func TestEraseMaskingRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		data  any
		rules []datamasking.Rule
		want  string
	}{
		{
			name: "documentation example", data: customMaskingData,
			rules: []datamasking.Rule{
				{Path: "email", RegexPattern: `(.)(.*)(@.*)`, MaskFormat: `\1****\3`},
				{Path: "age", DynamicMask: true},
				{Path: "address.zip", CustomMask: "xxx"},
				{Path: "$.other_address[?(@.postcode > 12000)]", CustomMask: "Masked"},
			},
			want: `{"id":1,"name":"Jane Doe","age":"**","email":"j****@example.com",
				"address":{"street":"123 Main St","city":"Anytown","state":"CA","zip":"xxx","postcode":12345,"product":{"name":"Car"}},
				"other_address":[{"postcode":11345,"street":"123 Any Drive"},"Masked"],
				"company_address":{"street":"456 ACME Ave","city":"Anytown","state":"CA","zip":"12345"}}`,
		},
		{
			name: "complex rules", data: map[string]any{
				"email": "johndoe@example.com", "age": 30,
				"address": map[string]any{"zip": 13000, "street": "123 Main St", "details": map[string]any{"name": "Home", "type": "Primary"}},
			},
			rules: []datamasking.Rule{
				{Path: "email", RegexPattern: "(.)(.*)(@.*)", MaskFormat: `\1****\3`},
				{Path: "age", DynamicMask: true},
				{Path: "address.zip", CustomMask: "xxx"},
			},
			want: `{"email":"j****@example.com","age":"**","address":{"zip":"xxx","street":"123 Main St","details":{"name":"Home","type":"Primary"}}}`,
		},
		{
			name: "nested fields", data: `{"user":{"name":"John Doe","ssn":"123-45-6789","address":{"street":"123 Main St","zip":"12345"}}}`,
			rules: []datamasking.Rule{{Path: "user.ssn", CustomMask: "XXX-XX-XXXX"}, {Path: "user.address.zip", CustomMask: "00000"}},
			want:  `{"user":{"name":"John Doe","ssn":"XXX-XX-XXXX","address":{"street":"123 Main St","zip":"00000"}}}`,
		},
		{
			name: "a list is masked as its text", data: `{"user":{"name":["Pat","powertools"]}}`,
			rules: []datamasking.Rule{{Path: "user.name", CustomMask: "NO-NAME"}},
			want:  `{"user":{"name":"NO-NAME"}}`,
		},
		{
			// Python masks str(['Pat', 'powertools']): 21 characters, the
			// brackets, quotes and comma included.
			name: "a list is masked as its Python text", data: `{"user":{"name":["Pat","powertools"]}}`,
			rules: []datamasking.Rule{{Path: "user.name", DynamicMask: true}},
			want:  `{"user":{"name":"******* *************"}}`,
		},
		{
			name: "an object is masked as its Python text", data: `{"a":{"k":"v w"}}`,
			rules: []datamasking.Rule{{Path: "a", DynamicMask: true}},
			want:  `{"a":"***** ** ***"}`,
		},
		{
			name: "a rule without masks erases", data: `{"a":{"b":1},"c":[1,2]}`,
			rules: []datamasking.Rule{{Path: "a"}, {Path: "c"}},
			want:  `{"a":"*****","c":"*****"}`,
		},
		{
			name: "null values stay", data: `{"a":null,"b":[null,"x"]}`,
			rules: []datamasking.Rule{{Path: "a", DynamicMask: true}, {Path: "b[*]", DynamicMask: true}},
			want:  `{"a":null,"b":[null,"*"]}`,
		},
		{
			name: "numbers keep their Python spelling", data: `{"i":12345,"f":1.5,"e":1e20,"t":true}`,
			rules: []datamasking.Rule{{Path: "i", DynamicMask: true}, {Path: "f", DynamicMask: true}, {Path: "e", DynamicMask: true}, {Path: "t", DynamicMask: true}},
			want:  `{"i":"*****","f":"*.*","e":"*****","t":"****"}`,
		},
		{
			name: "rules override fields", data: `{"a":"x","b":"y"}`,
			rules: []datamasking.Rule{{Path: "a", CustomMask: "Z"}},
			want:  `{"a":"Z","b":"y"}`,
		},
	}
	masker := datamasking.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := masker.Erase(tc.data, datamasking.WithFields("b"), datamasking.WithMaskingRules(tc.rules...))
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, asJSON(t, got))
		})
	}
}

type failingEraser struct{ err error }

func (failingEraser) Encrypt(_ context.Context, _ []byte, _ datamasking.Request) (string, error) {
	return "", nil
}

func (failingEraser) Decrypt(_ context.Context, _ string, _ datamasking.Request) ([]byte, error) {
	return nil, nil
}

func (f failingEraser) Erase(_ any, _ datamasking.Rule) (any, error) { return nil, f.err }

func TestEraseErrors(t *testing.T) {
	t.Parallel()
	errMock := errors.New("mock error")
	cases := []struct {
		name     string
		masker   *datamasking.DataMasking
		data     any
		opts     []datamasking.EraseOption
		want     error
		wantPath string
	}{
		{
			name: "unsupported data type", masker: datamasking.New(), data: 42,
			opts: []datamasking.EraseOption{datamasking.WithFields("this.field")}, want: datamasking.ErrUnsupportedType,
		},
		{
			name: "empty fields", masker: datamasking.New(), data: 42,
			opts: []datamasking.EraseOption{datamasking.WithFields()}, want: datamasking.ErrEmptyFields,
		},
		{
			name: "missing field", masker: datamasking.New(), data: `{"3":{"1":{"None":"hello"}}}`,
			opts: []datamasking.EraseOption{datamasking.WithFields("'3'..True")}, want: datamasking.ErrFieldNotFound, wantPath: "'3'..True",
		},
		{
			name: "missing masking rule", masker: datamasking.New(), data: `{"name":"Ana"}`,
			opts: []datamasking.EraseOption{datamasking.WithMaskingRules(datamasking.Rule{Path: "$.missing_field", DynamicMask: true})},
			want: datamasking.ErrFieldNotFound, wantPath: "$.missing_field",
		},
		{
			name: "invalid masking path", masker: datamasking.New(), data: `{"user":{"ssn":"1"}}`,
			opts: []datamasking.EraseOption{datamasking.WithMaskingRules(datamasking.Rule{Path: "user.ssn...", CustomMask: "X"})},
			want: datamasking.ErrInvalidPath, wantPath: "user.ssn...",
		},
		{
			name: "invalid field path", masker: datamasking.New(), data: `{"a":1}`,
			opts: []datamasking.EraseOption{datamasking.WithFields("a | b")}, want: datamasking.ErrInvalidPath, wantPath: "a | b",
		},
		{
			name: "invalid regex", masker: datamasking.New(), data: "test123",
			opts: []datamasking.EraseOption{datamasking.WithRegex("[", "X")}, want: datamasking.ErrInvalidRegex,
		},
		{
			name: "invalid mask format", masker: datamasking.New(), data: "test123",
			opts: []datamasking.EraseOption{datamasking.WithRegex("(t)", `\2`)}, want: datamasking.ErrInvalidRegex,
		},
		{
			name: "invalid regex in a rule", masker: datamasking.New(), data: `{"a":"b"}`,
			opts: []datamasking.EraseOption{datamasking.WithMaskingRules(datamasking.Rule{Path: "a", RegexPattern: "(", MaskFormat: "x"})},
			want: datamasking.ErrInvalidRegex, wantPath: "a",
		},
		{
			name: "eraser failure", masker: datamasking.New(datamasking.WithProvider(failingEraser{err: errMock})), data: `{"value":"test"}`,
			opts: []datamasking.EraseOption{datamasking.WithMaskingRules(datamasking.Rule{Path: "value"})},
			want: datamasking.ErrMaskField, wantPath: "value",
		},
		{
			name: "invalid JSON", masker: datamasking.New(), data: []byte(`{"a":`),
			want: datamasking.ErrUnsupportedType,
		},
		{
			name: "unmarshalable value", masker: datamasking.New(), data: make(chan int),
			want: datamasking.ErrUnsupportedType,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.masker.Erase(tc.data, tc.opts...)
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, datamasking.ErrDataMasking)
			if tc.wantPath != "" {
				pathErr, ok := errors.AsType[*datamasking.PathError](err)
				require.True(t, ok)
				assert.Equal(t, tc.wantPath, pathErr.Path)
			}
		})
	}
	t.Run("eraser failure keeps its cause", func(t *testing.T) {
		t.Parallel()
		masker := datamasking.New(datamasking.WithProvider(failingEraser{err: errMock}))
		_, err := masker.Erase(`{"value":"test"}`, datamasking.WithMaskingRules(datamasking.Rule{Path: "value"}))
		require.ErrorIs(t, err, errMock)
		assert.Contains(t, err.Error(), `failed to mask field at path "value"`)
	})
}

func TestEraseSkipsMissingFields(t *testing.T) {
	t.Parallel()
	masker := datamasking.New(datamasking.WithRaiseOnMissingField(false))
	data := `{"3":{"1":{"None":"hello"}},"name":"Ana"}`

	got, err := masker.Erase(data, datamasking.WithFields("non-existing", "name"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"3":{"1":{"None":"hello"}},"name":"*****"}`, asJSON(t, got))

	got, err = masker.Erase(data, datamasking.WithMaskingRules(datamasking.Rule{Path: "$.missing_field", DynamicMask: true}))
	require.NoError(t, err)
	assert.JSONEq(t, data, asJSON(t, got))
}

func TestEraseAs(t *testing.T) {
	t.Parallel()
	type user struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	masker := datamasking.New()

	u, err := masker.EraseAs[user](user{Name: "Pat", Email: "pat@example.com"}, datamasking.WithFields("email"))
	require.NoError(t, err)
	assert.Equal(t, user{Name: "Pat", Email: datamasking.Mask}, u)

	raw, err := masker.EraseAs[[]byte]([]byte(`{"b":"x","a":"y"}`), datamasking.WithFields("a"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":"*****","b":"x"}`, string(raw))

	value, err := masker.EraseAs[jsontext.Value](`{"a":"y"}`, datamasking.WithFields("a"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":"*****"}`, string(value))

	s, err := masker.EraseAs[string](42)
	require.NoError(t, err)
	assert.Equal(t, datamasking.Mask, s)

	_, err = masker.EraseAs[user](`{"name":1}`, datamasking.WithFields("email"))
	require.ErrorIs(t, err, datamasking.ErrFieldNotFound)

	type typed struct {
		Age int `json:"age"`
	}
	_, err = masker.EraseAs[typed](typed{Age: 3}, datamasking.WithFields("age"))
	require.ErrorIs(t, err, datamasking.ErrUnsupportedType)
}

func TestMaskValue(t *testing.T) {
	t.Parallel()
	got, err := datamasking.MaskValue(map[string]any{"a": "bc"}, datamasking.Rule{DynamicMask: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": "**"}, got)

	got, err = datamasking.MaskValue([]any{"x", "y"}, datamasking.Rule{})
	require.NoError(t, err)
	assert.Equal(t, []any{datamasking.Mask, datamasking.Mask}, got)

	_, err = datamasking.MaskValue("x", datamasking.Rule{RegexPattern: "(", MaskFormat: "y"})
	require.ErrorIs(t, err, datamasking.ErrInvalidRegex)
}
