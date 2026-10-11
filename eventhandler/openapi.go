package eventhandler

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/swaggest/openapi-go/openapi31"
)

// Defaults of the OpenAPI document, Powertools Python's with the runtime's
// name.
const (
	DefaultOpenAPITitle        = "Powertools for AWS Lambda (Go) API"
	DefaultAPIVersion          = "1.0.0"
	DefaultResponseDescription = "Successful Response"
)

// ErrSchemaValidation reports an OpenAPI configuration that cannot make a
// valid document: a security requirement naming an unknown scheme, an
// extension whose name does not start with "x-": Python's
// SchemaValidationError.
var ErrSchemaValidation = errors.New("invalid OpenAPI configuration")

// OpenAPIConfig holds the document-level fields of the OpenAPI document:
// Python's OpenAPIConfig.
type OpenAPIConfig struct {
	// Title defaults to DefaultOpenAPITitle.
	Title string
	// Version is the version of the API, DefaultAPIVersion by default.
	Version string
	// Summary is a short summary of the API (OpenAPI 3.1 only).
	Summary string
	// Description is a longer description, in Markdown.
	Description    string
	TermsOfService string
	Contact        *Contact
	License        *License
	// Servers defaults to a single server at "/".
	Servers []Server
	Tags    []Tag
	// SecuritySchemes declares the security schemes, by name.
	SecuritySchemes map[string]SecurityScheme
	// Security applies security requirements to every operation.
	Security     []SecurityRequirement
	ExternalDocs *ExternalDocs
	// Extensions are "x-" fields of the document.
	Extensions map[string]any
}

// OpenAPIOption adjusts the configuration of one document, over the
// router's WithOpenAPI.
type OpenAPIOption func(*OpenAPIConfig)

// Contact is the contact information of the API.
type Contact struct {
	Name  string
	URL   string
	Email string
}

// License is the license of the API.
type License struct {
	Name string
	// Identifier is an SPDX expression (OpenAPI 3.1 only).
	Identifier string
	URL        string
}

// Server is a server of the API.
type Server struct {
	URL         string
	Description string
	Variables   map[string]ServerVariable
	Extensions  map[string]any
}

// ServerVariable is a variable of a server URL template.
type ServerVariable struct {
	Enum        []string
	Default     string
	Description string
}

// Tag describes a tag the operations use.
type Tag struct {
	Name         string
	Description  string
	ExternalDocs *ExternalDocs
}

// ExternalDocs links to documentation elsewhere.
type ExternalDocs struct {
	Description string
	URL         string
}

// SecurityRequirement maps security scheme names to the scopes required;
// several schemes in one requirement are all required.
type SecurityRequirement map[string][]string

// SecurityScheme is a security scheme of the OpenAPI document. The
// constructors below build each kind, as Python's models do.
type SecurityScheme struct {
	// Type is "apiKey", "http", "oauth2", "openIdConnect" or "mutualTLS".
	Type        string
	Description string
	// Name and In locate an API key ("query", "header" or "cookie").
	Name string
	In   string
	// Scheme and BearerFormat describe HTTP authentication.
	Scheme       string
	BearerFormat string
	// Flows are the OAuth2 flows.
	Flows            *OAuthFlows
	OpenIDConnectURL string
	Extensions       map[string]any
}

// OAuthFlows are the flows of an OAuth2 security scheme.
type OAuthFlows struct {
	Implicit          *OAuthFlow
	Password          *OAuthFlow
	ClientCredentials *OAuthFlow
	AuthorizationCode *OAuthFlow
}

// OAuthFlow is one OAuth2 flow.
type OAuthFlow struct {
	AuthorizationURL string
	TokenURL         string
	RefreshURL       string
	// Scopes maps scope names to their descriptions.
	Scopes map[string]string
}

// schemeHTTP is the type of HTTP authentication schemes.
const schemeHTTP = "http"

// APIKeyScheme is an API key passed in a header, a query parameter or a
// cookie: Python's APIKey.
func APIKeyScheme(name, in string) SecurityScheme {
	return SecurityScheme{Type: "apiKey", Name: name, In: in}
}

// HTTPScheme is an HTTP authentication scheme ("basic"...): Python's
// HTTPBase.
func HTTPScheme(scheme string) SecurityScheme {
	return SecurityScheme{Type: schemeHTTP, Scheme: scheme}
}

// HTTPBearerScheme is bearer token authentication: Python's HTTPBearer.
func HTTPBearerScheme(bearerFormat string) SecurityScheme {
	return SecurityScheme{Type: schemeHTTP, Scheme: "bearer", BearerFormat: bearerFormat}
}

// OAuth2Scheme is OAuth2 with the given flows: Python's OAuth2.
func OAuth2Scheme(flows OAuthFlows) SecurityScheme {
	return SecurityScheme{Type: "oauth2", Flows: &flows}
}

// OpenIDConnectScheme is OpenID Connect discovery: Python's OpenIdConnect.
func OpenIDConnectScheme(url string) SecurityScheme {
	return SecurityScheme{Type: "openIdConnect", OpenIDConnectURL: url}
}

// MutualTLSScheme is mutual TLS (OpenAPI 3.1 only): Python's MutualTLS.
func MutualTLSScheme() SecurityScheme {
	return SecurityScheme{Type: "mutualTLS"}
}

// OpenAPI returns the OpenAPI 3.1 document of the router's routes: Python's
// get_openapi_schema. Typed operations are described from their input and
// output types; plain handlers by their path parameters and their options.
func (r *Router) OpenAPI(opts ...OpenAPIOption) (*openapi31.Spec, error) {
	doc, err := r.document("3.1.0", opts)
	if err != nil {
		return nil, err
	}
	return specFromMap[openapi31.Spec](doc)
}

// OpenAPIJSON returns the OpenAPI 3.1 document as indented JSON: Python's
// get_openapi_json_schema, what the Swagger UI serves and what a build step
// writes to openapi.json.
func (r *Router) OpenAPIJSON(opts ...OpenAPIOption) ([]byte, error) {
	doc, err := r.document("3.1.0", opts)
	if err != nil {
		return nil, err
	}
	return marshalDocument(doc)
}

// OpenAPI30JSON returns the router's document in OpenAPI 3.0.3, the version
// Amazon Bedrock agents read (see the bedrockagent package), as indented
// JSON.
func (r *Router) OpenAPI30JSON(opts ...OpenAPIOption) ([]byte, error) {
	doc, err := r.document("3.0.3", opts)
	if err != nil {
		return nil, err
	}
	return marshalDocument(doc)
}

// Routes lists the documented operations of the router, for checks such as
// the description Bedrock agents require.
func (r *Router) Routes() []RouteInfo {
	var out []RouteInfo
	for _, rt := range r.routes {
		path, ok := rt.pat.openAPIPath()
		if !ok || rt.cfg.hidden || rt.pat.method == "" {
			continue
		}
		out = append(out, RouteInfo{
			Method: rt.pat.method, Path: path, Pattern: rt.pat.str,
			Summary: rt.cfg.summary, Description: rt.cfg.description, Security: len(rt.cfg.security) > 0,
		})
	}
	return out
}

// RouteInfo describes a documented route.
type RouteInfo struct {
	Method      string
	Path        string
	Pattern     string
	Summary     string
	Description string
	// Security reports whether the route sets security requirements.
	Security bool
}

// documentKeys is the order of the top-level fields of a document, as the
// specification lists them; the fields inside are sorted.
var documentKeys = []string{
	"openapi", "info", "jsonSchemaDialect", "servers", "paths", "webhooks",
	"components", "security", "tags", "externalDocs",
}

// marshalDocument encodes an OpenAPI document as indented JSON.
func marshalDocument(doc map[string]any) ([]byte, error) {
	keys := slices.DeleteFunc(slices.Clone(documentKeys), func(k string) bool {
		_, ok := doc[k]
		return !ok
	})
	for _, k := range slices.Sorted(maps.Keys(doc)) {
		if !slices.Contains(documentKeys, k) {
			keys = append(keys, k)
		}
	}
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf, jsontext.WithIndent("  "), json.Deterministic(true))
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
	}
	for _, k := range keys {
		if err := enc.WriteToken(jsontext.String(k)); err != nil {
			return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
		}
		if err := json.MarshalEncode(enc, doc[k]); err != nil {
			return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func specFromMap[S any](doc map[string]any) (*S, error) {
	data, err := json.Marshal(doc, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
	}
	spec := new(S)
	if err := json.Unmarshal(data, spec); err != nil {
		return nil, fmt.Errorf("decode the OpenAPI document: %w", err)
	}
	return spec, nil
}

// newReflector returns a swaggest reflector for the OpenAPI version, set up
// the way the event handler names and reads Go types.
func newReflector(version string) openapi.Reflector {
	var refl openapi.Reflector
	if strings.HasPrefix(version, "3.0") {
		r := openapi3.NewReflector()
		r.Spec.Openapi = version
		configureReflector(&r.Reflector)
		refl = r
	} else {
		r := openapi31.NewReflector()
		r.Spec.Openapi = version
		configureReflector(&r.Reflector)
		refl = r
	}
	return refl
}

var defNameCleaner = regexp.MustCompile(`[\w./-]*[./]`)

func configureReflector(r *jsonschema.Reflector) {
	r.InlineDefinition(new(multipart.FileHeader))
	// encoding/json/v2 encodes fields without a json tag under their Go
	// name: the schemas must say so too.
	r.DefaultOptions = append(r.DefaultOptions, jsonschema.ProcessWithoutTags,
		// A pointer to a struct can be null: say so next to its reference,
		// as Python's Optional fields do.
		func(rc *jsonschema.ReflectContext) { rc.EnvelopNullability = true },
		// encoding/json/v2 writes nil slices and maps as [] and {}, never
		// null.
		jsonschema.InterceptNullability(func(params jsonschema.InterceptNullabilityParams) {
			if params.NullAdded && (params.Type.Kind() == reflect.Slice || params.Type.Kind() == reflect.Map) {
				params.Schema.RemoveType(jsonschema.Null)
			}
			// An uploaded file is there or missing, never null.
			if params.Type == fileHeaderType {
				*params.Schema = params.OrigSchema
				params.Schema.RemoveType(jsonschema.Null)
				return
			}
			// The reflector keeps the type of a reference it wrapped in
			// anyOf with null, which would forbid the null.
			if len(params.Schema.AnyOf) > 0 && params.Schema.Ref == nil {
				params.Schema.Type = nil
			}
		}),
		jsonschema.InterceptDefName(func(t reflect.Type, defaultName string) string {
			if name, ok := builtinDefNames[t]; ok {
				return name
			}
			// Components are named after the Go type, without its package,
			// as Python names them after the model class.
			name := defNameCleaner.ReplaceAllString(t.Name(), "")
			name = strings.NewReplacer("[", "", "]", "", ",", "").Replace(name)
			if name == "" {
				return defaultName
			}
			return name
		}))
}

// document builds the OpenAPI document of version as a JSON object.
func (r *Router) document(version string, opts []OpenAPIOption) (map[string]any, error) {
	cfg := r.cfg.openapi
	for _, opt := range opts {
		opt(&cfg)
	}
	refl := newReflector(version)
	ids := map[string]bool{}
	var described []*route
	for _, rt := range r.routes {
		ok, err := r.describeRoute(refl, rt, ids)
		if err != nil {
			return nil, err
		}
		if ok {
			described = append(described, rt)
		}
	}
	doc, err := specMap(refl)
	if err != nil {
		return nil, err
	}
	if err := r.decorateOperations(doc, described, cfg); err != nil {
		return nil, err
	}
	if err := decorateDocument(doc, cfg, version); err != nil {
		return nil, err
	}
	if strings.HasPrefix(version, "3.0") {
		nullable30(doc)
	}
	return doc, nil
}

// nullable30 rewrites the anyOf branches that only allow null into OpenAPI
// 3.0's nullable, as Python does for Bedrock agents.
func nullable30(v any) {
	switch x := v.(type) {
	case map[string]any:
		if branches := list(x["anyOf"]); branches != nil {
			kept := slices.DeleteFunc(slices.Clone(branches), nullOnly)
			if len(kept) < len(branches) {
				x["anyOf"], x["nullable"] = kept, true
			}
		}
		for _, child := range x {
			nullable30(child)
		}
	case []any:
		for _, child := range x {
			nullable30(child)
		}
	}
}

// nullOnly reports a schema that allows only null, in either version's
// spelling.
func nullOnly(v any) bool {
	m := object(v)
	if len(m) != 1 {
		return false
	}
	if nullable, ok := m["nullable"].(bool); ok && nullable {
		return true
	}
	return m[keyType] == typeNull
}

func specMap(refl openapi.Reflector) (map[string]any, error) {
	data, err := json.Marshal(refl.SpecSchema())
	if err != nil {
		return nil, fmt.Errorf("encode the OpenAPI document: %w", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode the OpenAPI document: %w", err)
	}
	return doc, nil
}

// describeRoute adds the operation of rt to the reflector, and reports
// whether the route is part of the document.
func (r *Router) describeRoute(refl openapi.Reflector, rt *route, ids map[string]bool) (bool, error) {
	path, ok := rt.pat.openAPIPath()
	if !ok || rt.cfg.hidden || rt.pat.method == "" {
		return false, nil
	}
	method := rt.pat.method
	oc, err := refl.NewOperationContext(method, path)
	if err != nil {
		return false, fmt.Errorf("describe %s %s: %w", method, path, err)
	}
	summary := rt.cfg.summary
	if summary == "" {
		summary = method + " " + path
	}
	oc.SetSummary(summary)
	if rt.cfg.description != "" {
		oc.SetDescription(rt.cfg.description)
	}
	if len(rt.cfg.tags) > 0 {
		oc.SetTags(rt.cfg.tags...)
	}
	if rt.cfg.deprecated {
		oc.SetIsDeprecated(true)
	}
	id := rt.cfg.operationID
	if id == "" {
		id = generateOperationID(rt.funcName, path, method)
	}
	if ids[id] {
		warnOnce("duplicate-operation-id:"+id, func(l *slog.Logger) {
			l.Warn("Duplicate Operation ID in the OpenAPI document", "operationId", id, "route", rt.pat.str)
		})
	}
	ids[id] = true
	oc.SetID(id)

	responseValidation := r.responseValidationStatus(rt)
	if rt.op != nil {
		rt.op.describe(oc, rt, responseValidation)
	} else {
		describePlain(oc, rt)
	}
	if err := refl.AddOperation(oc); err != nil {
		return false, fmt.Errorf("describe %s %s: %w", method, path, err)
	}
	return true, nil
}

func (r *Router) responseValidationStatus(rt *route) int {
	if rt.cfg.responseValidation != 0 {
		return rt.cfg.responseValidation
	}
	return r.cfg.responseValidation
}

var nonWord = regexp.MustCompile(`\W`)

// generateOperationID follows Python: the function name and the path, with
// non-word characters as underscores, then the method.
func generateOperationID(funcName, path, method string) string {
	id := nonWord.ReplaceAllString(funcName+path, "_")
	if funcName == "" {
		id = strings.TrimLeft(id, "_")
	}
	return id + "_" + strings.ToLower(method)
}

// describePlain describes a plain handler: its path parameters, as strings,
// and its responses.
func describePlain(oc openapi.OperationContext, rt *route) {
	if names := rt.pat.names(); len(names) > 0 {
		fields := make([]reflect.StructField, len(names))
		for i, name := range names {
			fields[i] = reflect.StructField{
				Name: fmt.Sprintf("P%d", i),
				Type: reflect.TypeFor[string](),
				Tag:  reflect.StructTag(fmt.Sprintf(`path:%q`, name)),
			}
		}
		oc.AddReqStructure(reflect.New(reflect.StructOf(fields)).Interface())
	}
	if len(rt.cfg.responses) > 0 {
		for _, doc := range rt.cfg.responses {
			addResponse(oc, doc.model, doc.status, doc.description)
		}
		return
	}
	addResponse(oc, nil, rt.cfg.status, successDescription(rt))
}

func successDescription(rt *route) string {
	if rt.cfg.responseDescription != "" {
		return rt.cfg.responseDescription
	}
	return DefaultResponseDescription
}

func addResponse(oc openapi.OperationContext, model any, status int, description string) {
	oc.AddRespStructure(model, func(cu *openapi.ContentUnit) {
		cu.HTTPStatus = status
		cu.Description = description
		if model == nil {
			return
		}
		if t := reflect.TypeOf(model); t == bytesType || t == reflect.PointerTo(bytesType) {
			cu.Structure, cu.ContentType, cu.Format = nil, "application/octet-stream", "binary"
		}
	})
}

var bytesType = reflect.TypeFor[[]byte]()

// decorateOperations sets what the reflector does not: security,
// extensions, a required request body, the empty JSON content of plain
// handlers.
func (r *Router) decorateOperations(doc map[string]any, routes []*route, cfg OpenAPIConfig) error {
	for _, rt := range routes {
		path, _ := rt.pat.openAPIPath()
		if op := at(doc, "paths", path, strings.ToLower(rt.pat.method)); op != nil {
			if err := decorateOperation(op, rt, cfg); err != nil {
				return fmt.Errorf("route %q: %w", rt.pat.str, err)
			}
		}
	}
	return nil
}

func decorateOperation(op map[string]any, rt *route, cfg OpenAPIConfig) error {
	if len(rt.cfg.security) > 0 {
		if err := checkSecurity(rt.cfg.security, cfg.SecuritySchemes); err != nil {
			return err
		}
		op["security"] = securityList(rt.cfg.security)
	}
	if err := addExtensions(op, rt.cfg.extensions); err != nil {
		return err
	}
	if body := object(op["requestBody"]); body != nil && rt.op != nil && rt.op.bodyRequired() {
		body["required"] = true
	}
	if rt.op == nil {
		emptyJSONContent(op)
	}
	return nil
}

// emptyJSONContent gives the responses of a plain handler without a model
// the empty application/json content Python documents.
func emptyJSONContent(op map[string]any) {
	for _, resp := range object(op["responses"]) {
		m := object(resp)
		if m == nil {
			continue
		}
		if _, has := m["content"]; !has {
			m["content"] = map[string]any{ContentTypeJSON: map[string]any{}}
		}
	}
}

func checkSecurity(reqs []SecurityRequirement, schemes map[string]SecurityScheme) error {
	if len(schemes) == 0 {
		return fmt.Errorf("%w: security requirements without security schemes", ErrSchemaValidation)
	}
	for _, req := range reqs {
		for name := range req {
			if _, ok := schemes[name]; !ok {
				return fmt.Errorf("%w: security scheme %q is not declared", ErrSchemaValidation, name)
			}
		}
	}
	return nil
}

func securityList(reqs []SecurityRequirement) []any {
	out := make([]any, len(reqs))
	for i, req := range reqs {
		m := map[string]any{}
		for name, scopes := range req {
			if scopes == nil {
				scopes = []string{}
			}
			m[name] = scopes
		}
		out[i] = m
	}
	return out
}

// decorateDocument sets the document-level fields.
func decorateDocument(doc map[string]any, cfg OpenAPIConfig, version string) error {
	is31 := strings.HasPrefix(version, "3.1")
	doc["openapi"] = version
	doc["info"] = infoMap(cfg, is31)
	servers, err := serverList(cfg.Servers)
	if err != nil {
		return err
	}
	doc["servers"] = servers
	if len(cfg.Security) > 0 {
		if err := checkSecurity(cfg.Security, cfg.SecuritySchemes); err != nil {
			return err
		}
		doc["security"] = securityList(cfg.Security)
	}
	if tags := mergeTags(cfg.Tags, nil); len(tags) > 0 {
		doc["tags"] = tags
	}
	if cfg.ExternalDocs != nil {
		doc["externalDocs"] = externalDocsMap(*cfg.ExternalDocs)
	}
	if err := addExtensions(doc, cfg.Extensions); err != nil {
		return err
	}
	components := child(doc, "components")
	schemas := child(components, keySchemas)
	maps.Copy(schemas, validationComponents(schemas))
	if len(cfg.SecuritySchemes) > 0 {
		sec := map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(cfg.SecuritySchemes)) {
			scheme := cfg.SecuritySchemes[name]
			m, err := securitySchemeMap(&scheme, is31)
			if err != nil {
				return fmt.Errorf("security scheme %q: %w", name, err)
			}
			sec[name] = m
		}
		components["securitySchemes"] = sec
	}
	child(doc, "paths")
	return nil
}

// serverList renders the servers, a single "/" when there are none.
func serverList(servers []Server) ([]any, error) {
	if len(servers) == 0 {
		servers = []Server{{URL: "/"}}
	}
	out := make([]any, len(servers))
	for i := range servers {
		m, err := serverMap(servers[i])
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func infoMap(cfg OpenAPIConfig, is31 bool) map[string]any {
	title, version := cfg.Title, cfg.Version
	if title == "" {
		title = DefaultOpenAPITitle
	}
	if version == "" {
		version = DefaultAPIVersion
	}
	info := map[string]any{"title": title, "version": version}
	if is31 {
		setString(info, "summary", cfg.Summary)
	}
	setString(info, keyDescription, cfg.Description)
	setString(info, "termsOfService", cfg.TermsOfService)
	if c := cfg.Contact; c != nil {
		m := map[string]any{}
		setString(m, keyName, c.Name)
		setString(m, "url", c.URL)
		setString(m, "email", c.Email)
		info["contact"] = m
	}
	if l := cfg.License; l != nil {
		m := map[string]any{keyName: l.Name}
		if is31 {
			setString(m, "identifier", l.Identifier)
		}
		setString(m, "url", l.URL)
		info["license"] = m
	}
	return info
}

func serverMap(s Server) (map[string]any, error) {
	m := map[string]any{"url": s.URL}
	setString(m, keyDescription, s.Description)
	if len(s.Variables) > 0 {
		vars := map[string]any{}
		for name, v := range s.Variables {
			vm := map[string]any{"default": v.Default}
			if len(v.Enum) > 0 {
				vm["enum"] = v.Enum
			}
			setString(vm, keyDescription, v.Description)
			vars[name] = vm
		}
		m["variables"] = vars
	}
	if err := addExtensions(m, s.Extensions); err != nil {
		return nil, err
	}
	return m, nil
}

func externalDocsMap(d ExternalDocs) map[string]any {
	m := map[string]any{"url": d.URL}
	setString(m, keyDescription, d.Description)
	return m
}

func securitySchemeMap(s *SecurityScheme, is31 bool) (map[string]any, error) {
	m := map[string]any{keyType: s.Type}
	setString(m, keyDescription, s.Description)
	switch s.Type {
	case "apiKey":
		m[keyName], m["in"] = s.Name, s.In
	case schemeHTTP:
		m["scheme"] = s.Scheme
		setString(m, "bearerFormat", s.BearerFormat)
	case "oauth2":
		flows := map[string]any{}
		if s.Flows != nil {
			addFlow(flows, "implicit", s.Flows.Implicit)
			addFlow(flows, "password", s.Flows.Password)
			addFlow(flows, "clientCredentials", s.Flows.ClientCredentials)
			addFlow(flows, "authorizationCode", s.Flows.AuthorizationCode)
		}
		m["flows"] = flows
	case "openIdConnect":
		m["openIdConnectUrl"] = s.OpenIDConnectURL
	case "mutualTLS":
		if !is31 {
			return nil, fmt.Errorf("%w: mutualTLS needs OpenAPI 3.1", ErrSchemaValidation)
		}
	default:
		return nil, fmt.Errorf("%w: unknown security scheme type %q", ErrSchemaValidation, s.Type)
	}
	if err := addExtensions(m, s.Extensions); err != nil {
		return nil, err
	}
	return m, nil
}

func addFlow(flows map[string]any, name string, f *OAuthFlow) {
	if f == nil {
		return
	}
	scopes := map[string]any{}
	for k, v := range f.Scopes {
		scopes[k] = v
	}
	m := map[string]any{"scopes": scopes}
	setString(m, "authorizationUrl", f.AuthorizationURL)
	setString(m, "tokenUrl", f.TokenURL)
	setString(m, "refreshUrl", f.RefreshURL)
	flows[name] = m
}

func addExtensions(m, ext map[string]any) error {
	for _, name := range slices.Sorted(maps.Keys(ext)) {
		if !strings.HasPrefix(name, "x-") {
			return fmt.Errorf("%w: extension %q must start with x-", ErrSchemaValidation, name)
		}
		m[name] = ext[name]
	}
	return nil
}

func setString(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// The bodies of validation failures, as the OpenAPI document describes
// them.
type httpValidationError struct {
	StatusCode int                     `json:"statusCode"`
	Detail     []validationErrorSchema `json:"detail"`
}

type responseValidationError httpValidationError

type validationErrorSchema struct {
	Loc  []any  `json:"loc"`
	Type string `json:"type"`
}

// The names of the validation error components, Python's.
const (
	validationErrorName         = "ValidationError"
	httpValidationErrorName     = "HTTPValidationError"
	responseValidationErrorName = "ResponseValidationError"
)

var builtinDefNames = map[reflect.Type]string{
	reflect.TypeFor[httpValidationError]():     httpValidationErrorName,
	reflect.TypeFor[responseValidationError](): responseValidationErrorName,
	reflect.TypeFor[validationErrorSchema]():   validationErrorName,
}

// validationComponents are Python's schemas of the validation error bodies,
// without the msg Python declares required and never sends, and with the
// statusCode it sends and never declares. The response validation schema is
// kept only when an operation refers to it.
func validationComponents(existing map[string]any) map[string]any {
	ref := "#/components/schemas/" + validationErrorName
	detail := func(title string) map[string]any {
		return map[string]any{
			keyTitle: title,
			keyType:  typeObject,
			keyProperties: map[string]any{
				"statusCode": map[string]any{keyTitle: "Status Code", keyType: typeInteger},
				"detail":     map[string]any{keyTitle: "Detail", keyType: typeArray, "items": map[string]any{"$ref": ref}},
			},
		}
	}
	location := map[string]any{"anyOf": []any{map[string]any{keyType: typeString}, map[string]any{keyType: typeInteger}}}
	out := map[string]any{
		validationErrorName: map[string]any{
			keyTitle: validationErrorName,
			keyType:  typeObject,
			keyProperties: map[string]any{
				"loc":   map[string]any{keyTitle: "Location", keyType: typeArray, "items": location},
				keyType: map[string]any{keyTitle: "Error Type", keyType: typeString},
			},
			"required": []any{"loc", keyType},
		},
		httpValidationErrorName: detail(httpValidationErrorName),
	}
	if _, ok := existing[responseValidationErrorName]; ok {
		out[responseValidationErrorName] = detail(responseValidationErrorName)
	}
	return out
}

// describeResponseValidation documents the response of a failed response
// validation.
func describeValidationResponses(oc openapi.OperationContext, responseValidation int) {
	oc.AddRespStructure(new(httpValidationError), func(cu *openapi.ContentUnit) {
		cu.HTTPStatus = http.StatusUnprocessableEntity
		cu.Description = "Validation Error"
	})
	if responseValidation != 0 && responseValidation != http.StatusUnprocessableEntity {
		oc.AddRespStructure(new(responseValidationError), func(cu *openapi.ContentUnit) {
			cu.HTTPStatus = responseValidation
			cu.Description = "Response Validation Error"
		})
	}
}
