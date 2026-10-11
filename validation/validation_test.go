package validation

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

const orderSchema = `{
	"type": "object",
	"required": ["id", "items"],
	"properties": {
		"id": {"type": "string", "pattern": "^o-"},
		"status": {"type": "string", "default": "pending"},
		"items": {
			"type": "array",
			"items": {
				"type": "object",
				"required": ["sku"],
				"properties": {
					"sku": {"type": "string"},
					"price": {"type": "number", "maximum": 42},
					"qty": {"type": "integer", "default": 1}
				}
			}
		}
	}
}`

// str renders a decoded JSON value, numbers included, as text.
func str(v any) string { return fmt.Sprint(v) }

func gzipBase64(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestValidateReturnsDataWithDefaults(t *testing.T) {
	t.Parallel()
	data, err := Validate([]byte(`{"id":"o-1","items":[{"sku":"a","price":3.5},{"sku":"b","qty":2}]}`), orderSchema)
	require.NoError(t, err)

	order, ok := data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pending", order["status"])
	items, ok := order["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)
	first, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1", str(first["qty"]))
	assert.Equal(t, "3.5", str(first["price"]))
	second, ok := items[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "2", str(second["qty"]))
}

func TestValidateEventKinds(t *testing.T) {
	t.Parallel()
	schema := `{"type":"object","required":["id"]}`
	cases := []struct {
		name  string
		event any
		ok    bool
	}{
		{name: "bytes", event: []byte(`{"id":1}`), ok: true},
		{name: "jsontext", event: jsontext.Value(`{"id":1}`), ok: true},
		{name: "map", event: map[string]any{"id": 1}, ok: true},
		{name: "struct", event: struct {
			ID int `json:"id"`
		}{ID: 1}, ok: true},
		{name: "struct without the member", event: struct {
			Name string `json:"name"`
		}{}, ok: false},
		{name: "a Go string is a JSON string", event: `{"id":1}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate(tc.event, schema)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrSchemaValidation)
			}
		})
	}
}

func TestValidateInvalidEvent(t *testing.T) {
	t.Parallel()
	_, err := Validate([]byte(`{"id":`), `{}`)
	require.ErrorIs(t, err, ErrInvalidEvent)

	_, err = Validate(make(chan int), `{}`)
	require.ErrorIs(t, err, ErrInvalidEvent)
}

func TestSchemaValidationErrorFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		schema         string
		data           string
		message        string
		path           []string
		value          string
		rule           string
		ruleDefinition string
		definition     string
	}{
		{
			name:           "maximum",
			schema:         `{"properties":{"price":{"type":"number","maximum":42}}}`,
			data:           `{"price":60}`,
			message:        "data.price must be smaller than or equal to 42",
			path:           []string{"data", "price"},
			value:          "60",
			rule:           "maximum",
			ruleDefinition: "42",
			definition:     "map[maximum:42 type:number]",
		},
		{
			name:           "required",
			schema:         `{"required":["b","a"]}`,
			data:           `{"c":1}`,
			message:        "data must contain ['a', 'b'] properties",
			path:           []string{"data"},
			value:          "map[c:1]",
			rule:           "required",
			ruleDefinition: "[b a]",
			definition:     "map[required:[b a]]",
		},
		{
			name:           "type in an array",
			schema:         `{"properties":{"items":{"items":{"type":"string"}}}}`,
			data:           `{"items":["a",2]}`,
			message:        "data.items[1] must be string",
			path:           []string{"data", "items", "1"},
			value:          "2",
			rule:           "type",
			ruleDefinition: "string",
			definition:     "map[type:string]",
		},
		{
			name:           "through a reference",
			schema:         `{"properties":{"a":{"$ref":"#/definitions/a"}},"definitions":{"a":{"minLength":3}}}`,
			data:           `{"a":"xy"}`,
			message:        "data.a must be longer than or equal to 3 characters",
			path:           []string{"data", "a"},
			value:          "xy",
			rule:           "minLength",
			ruleDefinition: "3",
			definition:     "map[minLength:3]",
		},
		{
			name:           "enum",
			schema:         `{"enum":["a",1,null]}`,
			data:           `"b"`,
			message:        "data must be one of ['a', 1, None]",
			path:           []string{"data"},
			value:          "b",
			rule:           "enum",
			ruleDefinition: "[a 1 <nil>]",
			definition:     "map[enum:[a 1 <nil>]]",
		},
		{
			name:           "anyOf",
			schema:         `{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
			data:           `true`,
			message:        "data cannot be validated by any definition",
			path:           []string{"data"},
			value:          "true",
			rule:           "anyOf",
			ruleDefinition: "[map[type:string] map[type:integer]]",
			definition:     "map[anyOf:[map[type:string] map[type:integer]]]",
		},
		{
			name:           "oneOf",
			schema:         `{"oneOf":[{"type":"number"},{"type":"integer"}]}`,
			data:           `1`,
			message:        "data must be valid exactly by one definition (2 matches found)",
			path:           []string{"data"},
			value:          "1",
			rule:           "oneOf",
			ruleDefinition: "[map[type:number] map[type:integer]]",
			definition:     "map[oneOf:[map[type:number] map[type:integer]]]",
		},
		{
			name:           "not",
			schema:         `{"not":{"type":"string"}}`,
			data:           `"x"`,
			message:        "data must NOT match a disallowed definition",
			path:           []string{"data"},
			value:          "x",
			rule:           "not",
			ruleDefinition: "map[type:string]",
			definition:     "map[not:map[type:string]]",
		},
		{
			name:           "additionalProperties",
			schema:         `{"properties":{"a":{}},"additionalProperties":false}`,
			data:           `{"a":1,"z":2,"b":3}`,
			message:        "data must not contain ['b', 'z'] properties",
			path:           []string{"data"},
			value:          "map[a:1 b:3 z:2]",
			rule:           "additionalProperties",
			ruleDefinition: "false",
			definition:     "map[additionalProperties:false properties:map[a:map[]]]",
		},
		{
			name:           "false schema",
			schema:         `{"properties":{"a":false}}`,
			data:           `{"a":1}`,
			message:        "data.a must not be there",
			path:           []string{"data", "a"},
			value:          "1",
			rule:           "",
			ruleDefinition: "<nil>",
			definition:     "false",
		},
		{
			name:           "const",
			schema:         `{"const":"x"}`,
			data:           `"y"`,
			message:        "data must be same as const definition: 'x'",
			path:           []string{"data"},
			value:          "y",
			rule:           "const",
			ruleDefinition: "x",
			definition:     "map[const:x]",
		},
		{
			name:           "exclusive minimum keeps the schema's spelling",
			schema:         `{"exclusiveMinimum":1.50}`,
			data:           `1`,
			message:        "data must be bigger than 1.50",
			path:           []string{"data"},
			value:          "1",
			rule:           "exclusiveMinimum",
			ruleDefinition: "1.50",
			definition:     "map[exclusiveMinimum:1.50]",
		},
		{
			name:           "property name with dots and slashes",
			schema:         `{"properties":{"a b/c~d":{"maxItems":1}}}`,
			data:           `{"a b/c~d":[1,2]}`,
			message:        "data.a b/c~d must contain less than or equal to 1 items",
			path:           []string{"data", "a b/c~d"},
			value:          "[1 2]",
			rule:           "maxItems",
			ruleDefinition: "1",
			definition:     "map[maxItems:1]",
		},
		{
			name:           "dependencies",
			schema:         `{"dependencies":{"card":["billing"]}}`,
			data:           `{"card":1}`,
			message:        "data missing dependency billing for card",
			path:           []string{"data"},
			value:          "map[card:1]",
			rule:           "dependencies",
			ruleDefinition: "map[card:[billing]]",
			definition:     "map[dependencies:map[card:[billing]]]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate([]byte(tc.data), tc.schema)
			require.ErrorIs(t, err, ErrSchemaValidation)
			verr, ok := errors.AsType[*SchemaValidationError](err)
			require.True(t, ok)
			assert.Equal(t, tc.message, verr.ValidationMessage)
			assert.Equal(t, tc.path, verr.Path)
			assert.Equal(t, tc.value, str(verr.Value))
			assert.Equal(t, tc.rule, verr.Rule)
			assert.Equal(t, tc.ruleDefinition, str(verr.RuleDefinition))
			assert.Equal(t, tc.definition, str(verr.Definition))
			assert.True(t, strings.HasPrefix(verr.Error(), "Failed schema validation. Error: "+tc.message+", Path: "))

			engine, ok := errors.AsType[*jsonschema.ValidationError](err)
			require.True(t, ok)
			assert.NotEmpty(t, engine.BasicOutput().Errors)
		})
	}
}

func TestSchemaValidationErrorMessage(t *testing.T) {
	t.Parallel()
	_, err := Validate([]byte(`{"order":{"items":[{"price":60}]}}`),
		`{"properties":{"order":{"properties":{"items":{"items":{"properties":{"price":{"maximum":42}}}}}}}}`)
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data.order.items[0].price", verr.Name)
	assert.Equal(t, "Failed schema validation. Error: data.order.items[0].price must be smaller than or equal to 42, "+
		"Path: ['data', 'order', 'items', '0', 'price'], Data: 60", verr.Message)
}

func TestFirstFailureIsDeterministic(t *testing.T) {
	t.Parallel()
	schema := MustCompile(`{"properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}},"required":["d"]}`)
	for range 20 {
		_, err := schema.Validate([]byte(`{"c":1,"b":2,"a":3}`))
		verr, ok := errors.AsType[*SchemaValidationError](err)
		require.True(t, ok)
		assert.Equal(t, "data must contain ['d'] properties", verr.ValidationMessage)
	}
	for range 20 {
		_, err := schema.Validate([]byte(`{"c":1,"b":2,"a":3,"d":0}`))
		verr, ok := errors.AsType[*SchemaValidationError](err)
		require.True(t, ok)
		assert.Equal(t, "data.a must be string", verr.ValidationMessage)
	}
}

func TestEnvelopes(t *testing.T) {
	t.Parallel()
	order := `{"id":"o-1","items":[]}`
	single := `{"type":"object","required":["id"]}`
	list := `{"type":"array","items":` + single + `}`
	cases := []struct {
		name     string
		envelope string
		event    any
		schema   string
	}{
		{name: "API Gateway REST", envelope: APIGatewayREST, event: events.APIGatewayProxyRequest{Body: order}, schema: single},
		{name: "API Gateway HTTP", envelope: APIGatewayHTTP, event: events.APIGatewayV2HTTPRequest{Body: order}, schema: single},
		{
			name: "SQS", envelope: SQS, schema: list,
			event: events.SQSEvent{Records: []events.SQSMessage{{Body: order}, {Body: order}}},
		},
		{
			name: "SNS", envelope: SNS, schema: single,
			event: events.SNSEvent{Records: []events.SNSEventRecord{{SNS: events.SNSEntity{Message: order}}}},
		},
		{name: "EventBridge", envelope: EventBridge, event: events.EventBridgeEvent{Detail: jsontext.Value(order)}, schema: single},
		{name: "scheduled", envelope: CloudWatchEventsScheduled, event: events.EventBridgeEvent{Detail: jsontext.Value(order)}, schema: single},
		{
			name: "Kinesis", envelope: KinesisDataStream, schema: list,
			event: events.KinesisEvent{Records: []events.KinesisEventRecord{{Kinesis: events.KinesisRecord{Data: []byte(order)}}}},
		},
		{
			name: "CloudWatch Logs", envelope: CloudWatchLogs, schema: `{"type":"array","items":{"required":["message"]}}`,
			event: events.CloudwatchLogsEvent{AWSLogs: events.CloudwatchLogsRawData{
				Data: gzipBase64(t, `{"logEvents":[{"id":"1","timestamp":1,"message":"hello"}]}`),
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate(tc.event, tc.schema, WithEnvelope(tc.envelope))
			require.NoError(t, err)
		})
	}
}

func TestEnvelopeErrors(t *testing.T) {
	t.Parallel()
	_, err := Validate([]byte(`{}`), `{}`, WithEnvelope("Records[*"))
	require.ErrorIs(t, err, jmespath.ErrInvalidEnvelope)

	_, err = Validate(events.APIGatewayProxyRequest{Body: "not json"}, `{}`, WithEnvelope(APIGatewayREST))
	require.ErrorIs(t, err, jmespath.ErrInvalidEnvelope)

	// An envelope that selects nothing validates null, as in Python.
	_, err = Validate([]byte(`{}`), `{"type":"object"}`, WithEnvelope("detail"))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data must be object", verr.ValidationMessage)
}

func TestEnvelopeCustomFunctions(t *testing.T) {
	t.Parallel()
	upper := jmespath.Function{
		Name: "upper",
		Args: []jmespath.Arg{{Types: []jmespath.Type{jmespath.TypeString}}},
		Call: func(args []any) (any, error) {
			s, _ := args[0].(string) //nolint:errcheck // the signature guarantees a string
			return strings.ToUpper(s), nil
		},
	}
	data, err := Validate([]byte(`{"name":"ada"}`), `{"const":"ADA"}`, WithEnvelope("upper(name)", jmespath.WithFunctions(upper)))
	require.NoError(t, err)
	assert.Equal(t, "ADA", data)
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		schema string
		data   string
		want   string
	}{
		{
			name:   "nested properties",
			schema: `{"properties":{"a":{"properties":{"b":{"default":{"c":[1]}}}}}}`,
			data:   `{"a":{}}`,
			want:   `{"a":{"b":{"c":[1]}}}`,
		},
		{
			name:   "missing parent stays missing",
			schema: `{"properties":{"a":{"properties":{"b":{"default":1}}}}}`,
			data:   `{}`,
			want:   `{}`,
		},
		{
			name:   "tuple items",
			schema: `{"items":[{"type":"string"},{"default":"x"},{"default":"y"}]}`,
			data:   `["a"]`,
			want:   `["a","x","y"]`,
		},
		{
			name:   "tuple stops at a position without default",
			schema: `{"items":[{"type":"string"},{},{"default":"y"}]}`,
			data:   `["a"]`,
			want:   `["a"]`,
		},
		{
			name:   "prefixItems",
			schema: `{"$schema":"https://json-schema.org/draft/2020-12/schema","prefixItems":[{},{"default":2}],"items":{"properties":{"k":{"default":0}}}}`,
			data:   `[1]`,
			want:   `[1,2]`,
		},
		{
			name:   "items of 2020-12 after the prefix",
			schema: `{"$schema":"https://json-schema.org/draft/2020-12/schema","prefixItems":[{}],"items":{"properties":{"k":{"default":0}}}}`,
			data:   `[{},{}]`,
			want:   `[{},{"k":0}]`,
		},
		{
			name:   "reference and allOf",
			schema: `{"allOf":[{"$ref":"#/definitions/x"}],"definitions":{"x":{"properties":{"r":{"default":true}}}}}`,
			data:   `{}`,
			want:   `{"r":true}`,
		},
		{
			name:   "matching anyOf branch only",
			schema: `{"anyOf":[{"required":["a"],"properties":{"x":{"default":"A"}}},{"required":["b"],"properties":{"x":{"default":"B"}}}]}`,
			data:   `{"b":1}`,
			want:   `{"b":1,"x":"B"}`,
		},
		{
			name:   "if then else",
			schema: `{"if":{"required":["vip"]},"then":{"properties":{"tier":{"default":"gold"}}},"else":{"properties":{"tier":{"default":"basic"}}}}`,
			data:   `{}`,
			want:   `{"tier":"basic"}`,
		},
		{
			name:   "additional and pattern properties",
			schema: `{"patternProperties":{"^p":{"properties":{"d":{"default":1}}}},"additionalProperties":{"properties":{"e":{"default":2}}}}`,
			data:   `{"p1":{},"z":{}}`,
			want:   `{"p1":{"d":1},"z":{"e":2}}`,
		},
		{
			name:   "present values are kept",
			schema: `{"properties":{"a":{"default":1}}}`,
			data:   `{"a":null}`,
			want:   `{"a":null}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateAs[jsontext.Value]([]byte(tc.data), tc.schema)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestDefaultsAreCopied(t *testing.T) {
	t.Parallel()
	schema := MustCompile(`{"properties":{"tags":{"default":["a"]}}}`)
	first, err := schema.Validate([]byte(`{}`))
	require.NoError(t, err)
	m, ok := first.(map[string]any)
	require.True(t, ok)
	tags, ok := m["tags"].([]any)
	require.True(t, ok)
	tags[0] = "changed"

	second, err := schema.Validate([]byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"tags": []any{"a"}}, second)
}

func TestDefaultsDisabled(t *testing.T) {
	t.Parallel()
	got, err := ValidateAs[jsontext.Value]([]byte(`{}`), `{"properties":{"a":{"default":1}}}`, WithDefaults(false))
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(got))
}

func TestCallerDataIsNotChanged(t *testing.T) {
	t.Parallel()
	event := map[string]any{"id": "o-1", "items": []any{}}
	_, err := Validate(event, orderSchema)
	require.NoError(t, err)
	assert.NotContains(t, event, "status")
}

func TestValidateAs(t *testing.T) {
	t.Parallel()
	type item struct {
		SKU   string  `json:"sku"`
		Price float64 `json:"price"`
		Qty   int     `json:"qty"`
	}
	type order struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Items  []item `json:"items"`
	}
	got, err := ValidateAs[order]([]byte(`{"id":"o-1","items":[{"sku":"a","price":3.5}]}`), orderSchema)
	require.NoError(t, err)
	assert.Equal(t, order{ID: "o-1", Status: "pending", Items: []item{{SKU: "a", Price: 3.5, Qty: 1}}}, got)

	_, err = ValidateAs[order]([]byte(`{"id":"x"}`), orderSchema)
	require.ErrorIs(t, err, ErrSchemaValidation)

	_, err = ValidateAs[int]([]byte(`"a"`), `{}`)
	require.Error(t, err)
}

func TestNumbersKeepPrecision(t *testing.T) {
	t.Parallel()
	schema := `{"maximum":12345678901234567890}`
	_, err := Validate([]byte(`12345678901234567890`), schema)
	require.NoError(t, err)
	_, err = Validate([]byte(`12345678901234567891`), schema)
	require.ErrorIs(t, err, ErrSchemaValidation)

	data, err := Validate([]byte(`{"n":12345678901234567891}`), `{}`)
	require.NoError(t, err)
	got, err := ValidateAs[jsontext.Value](data, `{}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":12345678901234567891}`, string(got))
}

func TestFormats(t *testing.T) {
	t.Parallel()
	isAccount := func(v string) bool { return len(v) == 12 && strings.Trim(v, "0123456789") == "" }
	cases := []struct {
		name   string
		schema string
		data   string
		opts   []Option
		ok     bool
	}{
		{name: "custom function accepts", schema: `{"format":"awsaccountid"}`, data: `"123456789012"`, opts: []Option{WithFormat("awsaccountid", isAccount)}, ok: true},
		{name: "custom function rejects", schema: `{"format":"awsaccountid"}`, data: `"1234"`, opts: []Option{WithFormat("awsaccountid", isAccount)}},
		{name: "formats ignore other types", schema: `{"format":"awsaccountid"}`, data: `1234`, opts: []Option{WithFormat("awsaccountid", isAccount)}, ok: true},
		{name: "regexp anchored at start", schema: `{"format":"digits"}`, data: `"12ab"`, opts: []Option{WithFormatRegexp("digits", `\d+`)}, ok: true},
		{name: "regexp rejects", schema: `{"format":"digits"}`, data: `"ab12"`, opts: []Option{WithFormatRegexp("digits", `\d+`)}},
		{name: "custom overrides built-in", schema: `{"format":"email"}`, data: `"anything"`, opts: []Option{WithFormat("email", func(string) bool { return true })}, ok: true},
		{name: "built-in in draft 7", schema: `{"format":"email"}`, data: `"nope"`},
		{name: "asserted in 2019-09", schema: `{"$schema":"https://json-schema.org/draft/2019-09/schema","format":"date"}`, data: `"nope"`},
		{name: "asserted in 2020-12", schema: `{"$schema":"https://json-schema.org/draft/2020-12/schema","format":"uuid"}`, data: `"nope"`},
		{name: "assertion off", schema: `{"properties":{"a":{"format":"email"}}}`, data: `{"a":"nope"}`, opts: []Option{WithFormatAssertion(false)}, ok: true},
		{name: "unknown format ignored", schema: `{"format":"whatever"}`, data: `"x"`, ok: true},
		{name: "content encoding asserted", schema: `{"contentEncoding":"base64"}`, data: `"!!!"`},
		{name: "content media type asserted", schema: `{"contentMediaType":"application/json"}`, data: `"{"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate([]byte(tc.data), tc.schema, tc.opts...)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			verr, ok := errors.AsType[*SchemaValidationError](err)
			require.True(t, ok, "%v", err)
			assert.NotEmpty(t, verr.Rule)
		})
	}
}

func TestFormatMessage(t *testing.T) {
	t.Parallel()
	_, err := Validate([]byte(`{"account":"12"}`), `{"properties":{"account":{"format":"awsaccountid"}}}`,
		WithFormatRegexp("awsaccountid", `^\d{12}$`))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data.account must be awsaccountid", verr.ValidationMessage)
	assert.Equal(t, "format", verr.Rule)
	assert.Equal(t, "awsaccountid", verr.RuleDefinition)
}

func TestDrafts(t *testing.T) {
	t.Parallel()
	// Draft 7 ignores the siblings of "$ref"; 2019-09 applies them.
	schema := `{"definitions":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/definitions/s","maxLength":1}}}`
	data := []byte(`{"a":"long"}`)

	_, err := Validate(data, schema)
	require.NoError(t, err, "the default draft is 7")

	_, err = Validate(data, schema, WithDefaultDraft(Draft2019))
	require.ErrorIs(t, err, ErrSchemaValidation)

	_, err = Validate(data, `{"$schema":"http://json-schema.org/draft-04/schema#","maximum":1,"exclusiveMaximum":true}`)
	require.NoError(t, err)
	_, err = Validate([]byte(`1`), `{"$schema":"http://json-schema.org/draft-04/schema#","maximum":1,"exclusiveMaximum":true}`)
	require.ErrorIs(t, err, ErrSchemaValidation)

	_, err = Validate(data, `{}`, WithDefaultDraft(Draft(5)))
	require.ErrorIs(t, err, ErrInvalidSchema)

	for _, d := range []Draft{Draft4, Draft6, Draft7, Draft2019, Draft2020} {
		_, err := Validate([]byte(`{"a":1}`), `{"properties":{"a":{"type":"integer"}}}`, WithDefaultDraft(d))
		require.NoError(t, err, d.String())
	}
	assert.Equal(t, "draft-07", Draft7.String())
	assert.Equal(t, "draft-04", Draft4.String())
	assert.Equal(t, "draft/2019-09", Draft2019.String())
	assert.Equal(t, "draft/2020-12", Draft2020.String())
	assert.Equal(t, "draft(5)", Draft(5).String())
}

func TestInvalidSchemas(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		schema any
		opts   []Option
	}{
		{name: "not JSON", schema: `{"type":`},
		{name: "breaks the metaschema", schema: `{"type":5}`},
		{name: "bad pattern", schema: `{"pattern":"(?<=a)b"}`},
		{name: "bad format pattern", schema: `{}`, opts: []Option{WithFormatRegexp("x", "(")}},
		{name: "unmarshalable", schema: make(chan int)},
		{name: "nil compiled schema", schema: (*Schema)(nil)},
		{name: "bad resource", schema: `{}`, opts: []Option{WithResource("https://example.com/r.json", `{`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate([]byte(`{}`), tc.schema, tc.opts...)
			require.ErrorIs(t, err, ErrInvalidSchema)
			ierr, ok := errors.AsType[*InvalidSchemaFormatError](err)
			require.True(t, ok)
			assert.Contains(t, ierr.Error(), "Schema received: ")
		})
	}

	_, err := Compile(`{"type":5}`, WithFormatRegexp("custom", "x"))
	ierr, ok := errors.AsType[*InvalidSchemaFormatError](err)
	require.True(t, ok)
	assert.Equal(t, []string{"custom"}, ierr.Formats)
	assert.Equal(t, `{"type":5}`, ierr.Schema)
	assert.Panics(t, func() { MustCompile(`{"type":5}`) })
}

func TestSchemaKinds(t *testing.T) {
	t.Parallel()
	for _, schema := range []any{
		`{"type":"object"}`,
		[]byte(`{"type":"object"}`),
		jsontext.Value(`{"type":"object"}`),
		map[string]any{"type": "object"},
		true,
		MustCompile(`{"type":"object"}`),
	} {
		_, err := Validate([]byte(`{}`), schema)
		require.NoError(t, err, "%T", schema)
	}
	_, err := Validate([]byte(`{}`), false)
	require.ErrorIs(t, err, ErrSchemaValidation)
}

func TestValidateCachesCompiledSchemas(t *testing.T) {
	t.Parallel()
	o := newOptions(nil)
	schema := `{"title":"cache test","type":"object"}`
	first, err := schemaFor(t.Context(), schema, &o)
	require.NoError(t, err)
	second, err := schemaFor(t.Context(), []byte(schema), &o)
	require.NoError(t, err)
	assert.Same(t, first, second)

	other := newOptions([]Option{WithDefaultDraft(Draft2019)})
	third, err := schemaFor(t.Context(), schema, &other)
	require.NoError(t, err)
	assert.NotSame(t, first, third)

	opaque := newOptions([]Option{WithFormat("x", func(string) bool { return true })})
	fourth, err := schemaFor(t.Context(), schema, &opaque)
	require.NoError(t, err)
	fifth, err := schemaFor(t.Context(), schema, &opaque)
	require.NoError(t, err)
	assert.NotSame(t, fourth, fifth, "options with functions are not cached")
}

func TestCacheEviction(t *testing.T) {
	t.Parallel()
	c := newCache[int](2)
	c.put("a", 1)
	c.put("b", 2)
	c.put("a", 10)
	c.put("c", 3)
	_, ok := c.get("a")
	assert.False(t, ok, "the oldest entry goes first")
	v, ok := c.get("b")
	assert.True(t, ok)
	assert.Equal(t, 2, v)
	v, ok = c.get("c")
	assert.True(t, ok)
	assert.Equal(t, 3, v)
}

func TestConcurrentValidation(t *testing.T) {
	t.Parallel()
	schema := MustCompile(orderSchema)
	type outcome struct {
		data           any
		err, envelopes error
	}
	results := make([]outcome, 32)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			event := fmt.Appendf(nil, `{"id":"o-%d","items":[{"sku":"a"}]}`, i)
			results[i].data, results[i].err = schema.Validate(event)
			_, results[i].envelopes = Validate(event, orderSchema, WithEnvelope("items"))
		})
	}
	wg.Wait()
	for _, r := range results {
		require.NoError(t, r.err)
		require.ErrorIs(t, r.envelopes, ErrSchemaValidation)
		m, ok := r.data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "pending", m["status"])
	}
}

func TestPyRepr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want string
	}{
		{nil, "None"},
		{true, "True"},
		{false, "False"},
		{"it's", `"it's"`},
		{`a'b"c`, `'a\'b"c'`},
		{"tab\there\n\\", `'tab\there\n\\'`},
		{"\x01\u00e9", "'\\x01\u00e9'"},
		{"\u2028", `'\u2028'`},
		{"\xff", `'\xff'`},
		{[]any{"a", 1.5, nil}, "['a', 1.5, None]"},
		{map[string]any{"b": 1, "a": []any{}}, "{'a': [], 'b': 1}"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, pyRepr(tc.in), "%#v", tc.in)
	}
}

func TestPyStr(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "plain", pyStr("plain"))
	assert.Equal(t, "{'a': 'b'}", pyStr(map[string]any{"a": "b"}))
	assert.Equal(t, "None", pyStr(nil))
}

// caseInsensitive is a regexp engine that ignores case, to show that the
// engine is pluggable.
func caseInsensitive(pattern string) (Regexp, error) {
	if pattern == "bad" {
		return nil, errors.New("unsupported pattern")
	}
	return regexp.Compile("(?i)" + pattern)
}

func TestRegexpEngine(t *testing.T) {
	t.Parallel()
	_, err := Validate([]byte(`"ABC"`), `{"pattern":"^abc$"}`, WithRegexpEngine(caseInsensitive))
	require.NoError(t, err)
	_, err = Validate([]byte(`"ABC"`), `{"pattern":"^abc$"}`)
	require.ErrorIs(t, err, ErrSchemaValidation)
	_, err = Validate([]byte(`"x"`), `{"pattern":"bad"}`, WithRegexpEngine(caseInsensitive))
	require.ErrorIs(t, err, ErrInvalidSchema)
}

func TestEnvelopeFunctionsDoNotDisableTheSchemaCache(t *testing.T) {
	t.Parallel()
	o := newOptions([]Option{WithEnvelope("a", jmespath.WithFunctions())})
	_, cacheable := o.compileKey([]byte(`{}`))
	assert.True(t, cacheable)
}

func TestValidationMessages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		schema string
		data   string
		want   string
	}{
		{`{"minProperties":2}`, `{"a":1}`, "data must contain at least 2 properties"},
		{`{"maxProperties":0}`, `{"a":1}`, "data must contain less than or equal to 0 properties"},
		{`{"$schema":"https://json-schema.org/draft/2019-09/schema","dependentRequired":{"a":["b"]}}`, `{"a":1}`, "data missing dependency b for a"},
		{`{"propertyNames":{"maxLength":1}}`, `{"ab":1}`, "data must be named by propertyName definition"},
		{`{"minItems":2}`, `[1]`, "data must contain at least 2 items"},
		{`{"uniqueItems":true}`, `[1,1]`, "data must contain unique items"},
		{`{"items":[{}],"additionalItems":false}`, `[1,2]`, "data must contain only specified items"},
		{`{"contains":{"type":"string"}}`, `[1]`, "data must contain one of contains definition"},
		{`{"multipleOf":0.5}`, `0.7`, "data must be multiple of 0.5"},
		{`{"minimum":3}`, `1`, "data must be bigger than or equal to 3"},
		{`{"exclusiveMaximum":3}`, `3`, "data must be smaller than 3"},
		{`{"contentEncoding":"base64"}`, `"!"`, "data must be encoded by base64"},
		{`{"contentMediaType":"application/json"}`, `"{"`, "data must be valid JSON"},
		{`{"items":[{"oneOf":[{"type":"string"},{"type":"number"}]}]}`, `[true]`, "data[0] must be valid exactly by one definition (0 matches found)"},
		{`{"$schema":"https://json-schema.org/draft/2019-09/schema","contains":{"type":"string"},"maxContains":1}`, `["a","b"]`, "data must contain one of contains definition"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			_, err := Validate([]byte(tc.data), tc.schema)
			verr, ok := errors.AsType[*SchemaValidationError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.want, verr.ValidationMessage)
			assert.NotNil(t, verr.Definition)
		})
	}
}

func TestNumberRendering(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "42", number(nil, big.NewRat(42, 1)))
	assert.Equal(t, "0.5", number(nil, big.NewRat(1, 2)))
	assert.Equal(t, "?", number(nil, nil))
}

func TestDefinitionLookup(t *testing.T) {
	t.Parallel()
	s := &Schema{docs: map[string]any{rootURL: map[string]any{"a": []any{"x", map[string]any{"b": 1}}}}}
	assert.Equal(t, "1", str(s.definition(rootURL+"#/a/1/b")))
	assert.Nil(t, s.definition(rootURL+"#/a/9"))
	assert.Nil(t, s.definition(rootURL+"#/a/0/b"))
	assert.Nil(t, s.definition("https://elsewhere#/a"))
	assert.NotNil(t, s.definition(rootURL+"#"))
}

func TestLocate(t *testing.T) {
	t.Parallel()
	data := map[string]any{"a": []any{map[string]any{"b": "v"}}}
	name, path, value := locate(data, []string{"a", "0", "b"})
	assert.Equal(t, "data.a[0].b", name)
	assert.Equal(t, []string{"data", "a", "0", "b"}, path)
	assert.Equal(t, "v", value)

	name, _, value = locate(data, []string{"a", "7", "b"})
	assert.Equal(t, "data.a[7].b", name)
	assert.Nil(t, value)
}
