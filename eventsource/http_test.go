package eventsource

import (
	"encoding/base64"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBody(t *testing.T) {
	t.Parallel()
	encoded := base64.StdEncoding.EncodeToString([]byte(`{"id":7}`))
	cases := []struct {
		name    string
		body    func() ([]byte, error)
		want    string
		wantErr bool
	}{
		{"plain REST", func() ([]byte, error) { return Body(events.APIGatewayProxyRequest{Body: "hi"}) }, "hi", false},
		{"base64 HTTP API", func() ([]byte, error) {
			return Body(events.APIGatewayV2HTTPRequest{Body: encoded, IsBase64Encoded: true})
		}, `{"id":7}`, false},
		{"base64 ALB", func() ([]byte, error) {
			return Body(events.ALBTargetGroupRequest{Body: encoded, IsBase64Encoded: true})
		}, `{"id":7}`, false},
		{"empty base64", func() ([]byte, error) { return Body(events.LambdaFunctionURLRequest{IsBase64Encoded: true}) }, "", false},
		{"invalid base64", func() ([]byte, error) {
			return Body(events.APIGatewayWebsocketProxyRequest{Body: "!!", IsBase64Encoded: true})
		}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.body()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}
}

func TestJSONBody(t *testing.T) {
	t.Parallel()
	type order struct {
		ID int `json:"id"`
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(`{"id":7}`))

	got, err := JSONBody[order](events.APIGatewayV2HTTPRequest{Body: encoded, IsBase64Encoded: true})
	require.NoError(t, err)
	assert.Equal(t, order{ID: 7}, got)

	empty, err := JSONBody[*order](events.APIGatewayProxyRequest{})
	require.NoError(t, err)
	assert.Nil(t, empty)

	_, err = JSONBody[order](events.APIGatewayProxyRequest{Body: "{"})
	require.Error(t, err)
	_, err = JSONBody[order](events.APIGatewayProxyRequest{Body: "!", IsBase64Encoded: true})
	require.Error(t, err)
}

func TestHeaders(t *testing.T) {
	t.Parallel()

	rest := decode[events.APIGatewayProxyRequest](t, "apiGatewayProxyEvent.json")
	value, ok := Header(rest, "header1")
	assert.True(t, ok)
	assert.Equal(t, "value1", value)
	assert.Equal(t, []string{"value1"}, HeaderValues(rest, "HEADER1"))
	_, ok = Header(rest, "missing")
	assert.False(t, ok)
	assert.Nil(t, HeaderValues(rest, "missing"))

	alb := decode[events.ALBTargetGroupRequest](t, "albMultiValueHeadersEvent.json")
	accept, ok := Header(alb, "ACCEPT")
	assert.True(t, ok)
	assert.NotEmpty(t, accept)

	v2 := events.APIGatewayV2HTTPRequest{Headers: map[string]string{"X-Multi": "a,b"}}
	assert.Equal(t, []string{"a", "b"}, HeaderValues(v2, "x-multi"))
	multi, ok := Header(v2, "x-multi")
	assert.True(t, ok)
	assert.Equal(t, "a,b", multi)

	onlyMulti := events.APIGatewayProxyRequest{MultiValueHeaders: map[string][]string{"X-Seen": {"1", "2"}}}
	last, ok := Header(onlyMulti, "x-seen")
	assert.True(t, ok)
	assert.Equal(t, "2", last)
}

func TestQueryParameters(t *testing.T) {
	t.Parallel()

	rest := events.APIGatewayProxyRequest{
		QueryStringParameters:           map[string]string{"single": "a,b", "both": "z"},
		MultiValueQueryStringParameters: map[string][]string{"both": {"x", "z"}, "multi": {"m1", "m2"}},
	}
	v, ok := QueryParameter(rest, "single")
	assert.True(t, ok)
	assert.Equal(t, "a,b", v)
	assert.Equal(t, []string{"a", "b"}, QueryParameterValues(rest, "single"))
	assert.Equal(t, []string{"x", "z"}, QueryParameterValues(rest, "both"))
	v, ok = QueryParameter(rest, "multi")
	assert.True(t, ok)
	assert.Equal(t, "m2", v)
	_, ok = QueryParameter(rest, "SINGLE")
	assert.False(t, ok, "query string names are case-sensitive")
	assert.Nil(t, QueryParameterValues(rest, "missing"))

	url := events.LambdaFunctionURLRequest{QueryStringParameters: map[string]string{"ids": "1,2"}}
	assert.Equal(t, []string{"1", "2"}, QueryParameterValues(url, "ids"))
}

func TestCookies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cookies map[string]string
	}{
		{"HTTP API", Cookies(events.APIGatewayV2HTTPRequest{Cookies: []string{"a=1", "b=2"}})},
		{"Function URL", Cookies(events.LambdaFunctionURLRequest{Cookies: []string{"a=1", "b=2"}})},
		{"REST header", Cookies(events.APIGatewayProxyRequest{Headers: map[string]string{"Cookie": "a=1; b=2"}})},
		{"ALB multi-value", Cookies(events.ALBTargetGroupRequest{MultiValueHeaders: map[string][]string{"cookie": {"a=1", "b=2"}}})},
		{"VPC Lattice v2", Cookies(VPCLatticeEventV2{Headers: map[string]StringValues{"cookie": {"a=1", "b=2"}}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, map[string]string{"a": "1", "b": "2"}, tc.cookies)
		})
	}
	assert.Empty(t, Cookies(events.APIGatewayProxyRequest{}))
}

func TestParseCookies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header string
		want   map[string]string
	}{
		{"", map[string]string{}},
		{"a=1", map[string]string{"a": "1"}},
		{" a = 1 ; b=2=3;;flag; a=4", map[string]string{"a": "4", "b": "2=3"}},
		{`quoted="v"`, map[string]string{"quoted": `"v"`}},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ParseCookies(tc.header))
		})
	}
}

func TestPathAndMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		path   string
		method string
	}{
		{"REST", Path(events.APIGatewayProxyRequest{Path: "/orders"}), Method(events.APIGatewayProxyRequest{HTTPMethod: "POST"})},
		{"HTTP API stage", Path(httpAPI("/prod/orders", "prod")), Method(httpAPI("/prod/orders", "prod"))},
		{"HTTP API $default", Path(httpAPI("/orders", "$default")), Method(httpAPI("/orders", "$default"))},
		{"HTTP API stage not in path", Path(httpAPI("/orders", "prod")), Method(httpAPI("/orders", "prod"))},
		{"VPC Lattice v1", Path(VPCLatticeEvent{RawPath: "/orders"}), Method(VPCLatticeEvent{Method: "POST"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, "/orders", tc.path)
			assert.Equal(t, "POST", tc.method)
		})
	}
	assert.Equal(t, "/production/orders", Path(httpAPI("/production/orders", "prod")), "a stage only strips a whole segment")
}

func httpAPI(rawPath, stage string) events.APIGatewayV2HTTPRequest {
	return events.APIGatewayV2HTTPRequest{
		RawPath: rawPath,
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			Stage: stage,
			HTTP:  events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: "POST"},
		},
	}
}

func TestHTTPSamples(t *testing.T) {
	t.Parallel()

	v2 := decode[events.APIGatewayV2HTTPRequest](t, "apiGatewayProxyV2Event.json")
	assert.Empty(t, Cookies(v2), "the sample's cookies have no value, which the lenient parser skips")
	assert.Equal(t, "/my/path", Path(v2))
	assert.Equal(t, "POST", Method(v2))

	url := decode[events.LambdaFunctionURLRequest](t, "lambdaFunctionUrlEvent.json")
	assert.Equal(t, "/", Path(url))

	ws := decode[events.APIGatewayWebsocketProxyRequest](t, "apiGatewayWebSocketApiMessage.json")
	body, err := Body(ws)
	require.NoError(t, err)
	assert.NotEmpty(t, body)

	alb := decode[events.ALBTargetGroupRequest](t, "albEvent.json")
	assert.Equal(t, "/lambda", Path(alb))
	assert.Equal(t, "GET", Method(alb))
}
