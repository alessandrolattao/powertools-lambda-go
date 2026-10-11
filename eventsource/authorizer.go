package eventsource

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
)

// Errors of the API Gateway authorizer helpers.
var (
	// ErrInvalidRouteARN reports a method or route ARN that is not an API
	// Gateway execute-api ARN.
	ErrInvalidRouteARN = errors.New("invalid API Gateway route ARN")
	// ErrInvalidHTTPVerb reports a route whose method is not an HTTPVerb.
	ErrInvalidHTTPVerb = errors.New("invalid HTTP verb")
	// ErrInvalidResourcePath reports a route whose resource does not match
	// ResourcePathPattern.
	ErrInvalidResourcePath = errors.New("invalid resource path")
	// ErrNoStatements reports a policy built without any route.
	ErrNoStatements = errors.New("no statements defined for the policy")
)

// HTTPVerb is the method part of an API Gateway route ARN.
type HTTPVerb string

// The methods an authorizer policy can allow or deny. HTTPVerbAll matches
// every method.
const (
	HTTPVerbGet     HTTPVerb = "GET"
	HTTPVerbPost    HTTPVerb = "POST"
	HTTPVerbPut     HTTPVerb = "PUT"
	HTTPVerbPatch   HTTPVerb = "PATCH"
	HTTPVerbHead    HTTPVerb = "HEAD"
	HTTPVerbDelete  HTTPVerb = "DELETE"
	HTTPVerbOptions HTTPVerb = "OPTIONS"
	HTTPVerbAll     HTTPVerb = "*"
)

// normalize checks that v is one of the HTTPVerb constants. "ALL", the name
// Powertools Python accepts for "*", is accepted too and becomes "*".
func (v HTTPVerb) normalize() (HTTPVerb, error) {
	switch v {
	case HTTPVerbGet, HTTPVerbPost, HTTPVerbPut, HTTPVerbPatch, HTTPVerbHead, HTTPVerbDelete, HTTPVerbOptions, HTTPVerbAll:
		return v, nil
	case "ALL":
		return HTTPVerbAll, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidHTTPVerb, string(v))
	}
}

// ResourcePathPattern is what a resource path of an authorizer policy must
// match, as in Powertools Python.
const ResourcePathPattern = `^[/.a-zA-Z0-9\-_\*\{\}\+]+$`

var resourcePath = regexp.MustCompile(ResourcePathPattern)

// RouteARN is an API Gateway method or route ARN, as an authorizer receives
// it (methodArn, routeArn) and as a policy names the routes it allows:
//
//	arn:<partition>:execute-api:<region>:<account>:<api>/<stage>/<method>/<resource>
//
// WebSocket routes have no method: arn:...:<api>/<stage>/<route>.
type RouteARN struct {
	Partition string
	Region    string
	AccountID string
	APIID     string
	Stage     string
	// Method is empty for WebSocket routes.
	Method HTTPVerb
	// Resource has no leading slash.
	Resource  string
	WebSocket bool
}

// ParseRouteARN parses the ARN of a REST or HTTP API route. Unlike Powertools
// Python, a malformed ARN is an error, not a panic.
func ParseRouteARN(arn string) (RouteARN, error) {
	return parseRouteARN(arn, false)
}

// ParseWebSocketRouteARN parses the ARN of a WebSocket API route.
func ParseWebSocketRouteARN(arn string) (RouteARN, error) {
	return parseRouteARN(arn, true)
}

func parseRouteARN(arn string, websocket bool) (RouteARN, error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return RouteARN{}, fmt.Errorf("%w: %q", ErrInvalidRouteARN, arn)
	}
	path := strings.Split(parts[5], "/")
	minimum := 3
	if websocket {
		minimum = 2
	}
	if len(path) < minimum {
		return RouteARN{}, fmt.Errorf("%w: %q", ErrInvalidRouteARN, arn)
	}
	route := RouteARN{
		Partition: parts[1],
		Region:    parts[3],
		AccountID: parts[4],
		APIID:     path[0],
		Stage:     path[1],
		WebSocket: websocket,
	}
	if websocket {
		route.Resource = strings.Join(path[2:], "/")
		return route, nil
	}
	route.Method = HTTPVerb(path[2])
	route.Resource = strings.Join(path[3:], "/")
	return route, nil
}

// String renders the ARN.
func (r RouteARN) String() string {
	partition := r.Partition
	if partition == "" {
		partition = defaultPartition
	}
	resource := strings.TrimPrefix(r.Resource, "/")
	prefix := "arn:" + partition + ":execute-api:" + r.Region + ":" + r.AccountID + ":" + r.APIID + "/" + r.Stage + "/"
	if r.WebSocket {
		return prefix + resource
	}
	return prefix + string(r.Method) + "/" + resource
}

// Condition is the Condition block of an IAM policy statement: condition
// operators to condition keys to values, for example
//
//	eventsource.Condition{"IpAddress": {"aws:SourceIp": []string{"203.0.113.0/24"}}}
type Condition map[string]map[string]any

// PolicyStatement is a statement of an authorizer policy. Unlike
// events.IAMPolicyStatement it carries a Condition.
type PolicyStatement struct {
	Action    string    `json:"Action"`
	Effect    string    `json:"Effect"`
	Resource  []string  `json:"Resource"`
	Condition Condition `json:"Condition,omitempty"`
}

// PolicyDocument is the IAM policy an authorizer returns.
type PolicyDocument struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

// AuthorizerPolicyResponse is the response of a REST or WebSocket API Lambda
// authorizer: the principal, its policy, and what API Gateway passes on to
// the integration.
type AuthorizerPolicyResponse struct {
	PrincipalID        string         `json:"principalId"`
	PolicyDocument     PolicyDocument `json:"policyDocument"`
	Context            map[string]any `json:"context,omitempty"`
	UsageIdentifierKey string         `json:"usageIdentifierKey,omitempty"`
}

// defaultPartition is the partition of the commercial AWS regions.
const defaultPartition = "aws"

// policyVersion is the IAM policy language version.
const policyVersion = "2012-10-17"

// invokeAction is the action every statement grants or denies.
const invokeAction = "execute-api:Invoke"

// DenyAllResponse returns the response that denies every route to an unknown
// principal, Powertools Python's DENY_ALL_RESPONSE.
func DenyAllResponse() AuthorizerPolicyResponse {
	return AuthorizerPolicyResponse{
		PrincipalID: "deny-all-user",
		PolicyDocument: PolicyDocument{
			Version:   policyVersion,
			Statement: []PolicyStatement{{Action: invokeAction, Effect: "Deny", Resource: []string{"*"}}},
		},
	}
}

// AuthorizerOption configures an authorizer response.
type AuthorizerOption func(*policy)

// WithAuthorizerContext sets the context API Gateway passes to the
// integration ($context.authorizer).
func WithAuthorizerContext(context map[string]any) AuthorizerOption {
	return func(p *policy) { p.context = context }
}

// WithUsageIdentifierKey sets the API key of a usage plan.
func WithUsageIdentifierKey(key string) AuthorizerOption {
	return func(p *policy) { p.usageIdentifierKey = key }
}

type route struct {
	arn        string
	conditions []Condition
}

// policy is what REST and WebSocket authorizer responses share.
type policy struct {
	principalID        string
	target             RouteARN
	context            map[string]any
	usageIdentifierKey string
	allow, deny        []route
}

func newPolicy(principalID string, target RouteARN, websocket bool, opts []AuthorizerOption) policy {
	if target.Partition == "" {
		target.Partition = defaultPartition
	}
	target.WebSocket = websocket
	p := policy{principalID: principalID, target: target}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

func (p *policy) add(allow bool, r route) {
	if allow {
		p.allow = append(p.allow, r)
	} else {
		p.deny = append(p.deny, r)
	}
}

func (p *policy) build() (AuthorizerPolicyResponse, error) {
	if len(p.allow) == 0 && len(p.deny) == 0 {
		return AuthorizerPolicyResponse{}, ErrNoStatements
	}
	statements := statementsFor("Allow", p.allow)
	statements = append(statements, statementsFor("Deny", p.deny)...)
	return AuthorizerPolicyResponse{
		PrincipalID:        p.principalID,
		PolicyDocument:     PolicyDocument{Version: policyVersion, Statement: statements},
		Context:            p.context,
		UsageIdentifierKey: p.usageIdentifierKey,
	}, nil
}

// statementsFor groups the routes of one effect: the unconditioned ones in a
// single statement, each conditioned one in its own.
func statementsFor(effect string, routes []route) []PolicyStatement {
	var statements []PolicyStatement
	plain := PolicyStatement{Action: invokeAction, Effect: effect}
	for _, r := range routes {
		if len(r.conditions) == 0 {
			plain.Resource = append(plain.Resource, r.arn)
			continue
		}
		statements = append(statements, PolicyStatement{
			Action: invokeAction, Effect: effect, Resource: []string{r.arn}, Condition: mergeConditions(r.conditions),
		})
	}
	if len(plain.Resource) > 0 {
		statements = append(statements, plain)
	}
	return statements
}

// mergeConditions folds several condition blocks into the one object IAM
// expects. Powertools Python emits them as a list, which is not valid policy
// grammar.
func mergeConditions(conditions []Condition) Condition {
	merged := Condition{}
	for _, c := range conditions {
		for operator, keys := range c {
			if merged[operator] == nil {
				merged[operator] = map[string]any{}
			}
			maps.Copy(merged[operator], keys)
		}
	}
	return merged
}

// AuthorizerResponse builds the IAM policy a REST API Lambda authorizer
// returns, Powertools Python's APIGatewayAuthorizerResponse. Create it with
// NewAuthorizerResponse, add routes, then Build.
type AuthorizerResponse struct {
	policy policy
}

// NewAuthorizerResponse starts the response for principalID. The target
// gives the partition, region, account, API and stage of the routes; it is
// usually the parsed methodArn of the event:
//
//	arn, err := eventsource.ParseRouteARN(event.MethodArn)
//	response := eventsource.NewAuthorizerResponse("user-1", arn)
//
// Unlike Powertools Python's from_route_arn, the partition of the ARN is
// kept, so that policies work in China and GovCloud regions.
func NewAuthorizerResponse(principalID string, target RouteARN, opts ...AuthorizerOption) *AuthorizerResponse {
	return &AuthorizerResponse{policy: newPolicy(principalID, target, false, opts)}
}

// AllowAllRoutes allows every method on every resource.
func (r *AuthorizerResponse) AllowAllRoutes() {
	r.policy.add(true, route{arn: r.arn(HTTPVerbAll, "*")})
}

// DenyAllRoutes denies every method on every resource.
func (r *AuthorizerResponse) DenyAllRoutes() {
	r.policy.add(false, route{arn: r.arn(HTTPVerbAll, "*")})
}

// AllowRoute allows method on resource ("*" for every resource), optionally
// only under conditions.
func (r *AuthorizerResponse) AllowRoute(method HTTPVerb, resource string, conditions ...Condition) error {
	return r.addRoute(true, method, resource, conditions)
}

// DenyRoute denies method on resource, optionally only under conditions.
func (r *AuthorizerResponse) DenyRoute(method HTTPVerb, resource string, conditions ...Condition) error {
	return r.addRoute(false, method, resource, conditions)
}

func (r *AuthorizerResponse) addRoute(allow bool, method HTTPVerb, resource string, conditions []Condition) error {
	method, err := method.normalize()
	if err != nil {
		return err
	}
	if !resourcePath.MatchString(resource) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidResourcePath, resource, ResourcePathPattern)
	}
	r.policy.add(allow, route{arn: r.arn(method, resource), conditions: conditions})
	return nil
}

func (r *AuthorizerResponse) arn(method HTTPVerb, resource string) string {
	target := r.policy.target
	target.Method = method
	target.Resource = resource
	return target.String()
}

// Build returns the response, with the Allow statements first. It fails with
// ErrNoStatements when no route was added.
func (r *AuthorizerResponse) Build() (AuthorizerPolicyResponse, error) {
	return r.policy.build()
}

// WebSocketAuthorizerResponse builds the IAM policy a WebSocket API Lambda
// authorizer returns, Powertools Python's
// APIGatewayAuthorizerResponseWebSocket: routes have no method.
type WebSocketAuthorizerResponse struct {
	policy policy
}

// NewWebSocketAuthorizerResponse starts the response for principalID, for
// the API and stage of target (usually the parsed methodArn of the $connect
// event).
func NewWebSocketAuthorizerResponse(principalID string, target RouteARN, opts ...AuthorizerOption) *WebSocketAuthorizerResponse {
	return &WebSocketAuthorizerResponse{policy: newPolicy(principalID, target, true, opts)}
}

// AllowAllRoutes allows every route.
func (r *WebSocketAuthorizerResponse) AllowAllRoutes() {
	r.AllowRoute("*")
}

// DenyAllRoutes denies every route.
func (r *WebSocketAuthorizerResponse) DenyAllRoutes() {
	r.DenyRoute("*")
}

// AllowRoute allows the route, optionally only under conditions.
func (r *WebSocketAuthorizerResponse) AllowRoute(routeKey string, conditions ...Condition) {
	r.policy.add(true, route{arn: r.arn(routeKey), conditions: conditions})
}

// DenyRoute denies the route, optionally only under conditions.
func (r *WebSocketAuthorizerResponse) DenyRoute(routeKey string, conditions ...Condition) {
	r.policy.add(false, route{arn: r.arn(routeKey), conditions: conditions})
}

func (r *WebSocketAuthorizerResponse) arn(routeKey string) string {
	target := r.policy.target
	target.Method = ""
	target.Resource = routeKey
	return target.String()
}

// Build returns the response, with the Allow statements first. It fails with
// ErrNoStatements when no route was added.
func (r *WebSocketAuthorizerResponse) Build() (AuthorizerPolicyResponse, error) {
	return r.policy.build()
}
