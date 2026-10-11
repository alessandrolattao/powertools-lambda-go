package eventsource

import (
	"github.com/aws/aws-lambda-go/events"
)

// APIGatewayProxyRequest is events.APIGatewayProxyRequest (an API Gateway
// REST API proxy request, payload version 1.0) with the members it lacks:
// the payload version and the caller's organization id.
//
// The embedded aws-lambda-go request keeps every other field; RequestContext
// shadows its own, so read the context through e.RequestContext, not
// e.APIGatewayProxyRequest.RequestContext (which stays empty).
type APIGatewayProxyRequest struct {
	events.APIGatewayProxyRequest

	Version        string                        `json:"version,omitempty"`
	RequestContext APIGatewayProxyRequestContext `json:"requestContext"`
}

// APIGatewayProxyRequestContext is events.APIGatewayProxyRequestContext with
// the complete caller identity.
type APIGatewayProxyRequestContext struct {
	events.APIGatewayProxyRequestContext

	Identity APIGatewayRequestIdentity `json:"identity"`
}

// APIGatewayRequestIdentity is events.APIGatewayRequestIdentity with the
// caller's AWS Organizations id.
type APIGatewayRequestIdentity struct {
	events.APIGatewayRequestIdentity

	PrincipalOrgID string `json:"principalOrgId,omitempty"`
}

// APIGatewayCustomAuthorizerRequestTypeRequest is the event of an API
// Gateway REQUEST authorizer: events.APIGatewayCustomAuthorizerRequestTypeRequest
// with the members aws-lambda-go leaves out (payload version, identity
// source, token) and the complete request context. The same type decodes the
// REQUEST authorizer events of REST APIs, of WebSocket APIs (which add the
// connection to the context) and of HTTP APIs with payload version 1.0.
type APIGatewayCustomAuthorizerRequestTypeRequest struct {
	events.APIGatewayCustomAuthorizerRequestTypeRequest

	Version            string                                   `json:"version,omitempty"`
	IdentitySource     string                                   `json:"identitySource,omitempty"`
	AuthorizationToken string                                   `json:"authorizationToken,omitempty"`
	RequestContext     APIGatewayCustomAuthorizerRequestContext `json:"requestContext"`
}

// APIGatewayCustomAuthorizerRequestContext is the request context of a
// REQUEST authorizer event, complete: the members of a REST proxy request
// context plus, for WebSocket APIs, those of the connection.
type APIGatewayCustomAuthorizerRequestContext struct {
	events.APIGatewayCustomAuthorizerRequestTypeRequestContext

	DomainName        string                    `json:"domainName,omitempty"`
	DomainPrefix      string                    `json:"domainPrefix,omitempty"`
	ExtendedRequestID string                    `json:"extendedRequestId,omitempty"`
	Protocol          string                    `json:"protocol,omitempty"`
	RequestTime       string                    `json:"requestTime,omitempty"`
	RequestTimeEpoch  int64                     `json:"requestTimeEpoch,omitempty"`
	Identity          APIGatewayRequestIdentity `json:"identity"`
	ConnectedAt       int64                     `json:"connectedAt,omitempty"`
	ConnectionID      string                    `json:"connectionId,omitempty"`
	EventType         string                    `json:"eventType,omitempty"`
	MessageDirection  string                    `json:"messageDirection,omitempty"`
	RouteKey          string                    `json:"routeKey,omitempty"`
}
