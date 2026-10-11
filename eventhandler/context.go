package eventhandler

import (
	"context"
	"net/http"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// requestState is what the router knows about the request it serves.
type requestState struct {
	router  *Router
	route   *route
	request *http.Request
	// compress is the compression choice of a typed operation's Response,
	// which wins over the route's.
	compress *bool
}

type stateKey struct{}

func stateFrom(ctx context.Context) *requestState {
	if st, ok := ctx.Value(stateKey{}).(*requestState); ok {
		return st
	}
	return nil
}

// RequestFromContext returns the request a router is serving with ctx, the
// context of the request or one derived from it (the context a typed
// operation receives). It is how a typed operation reads what its input
// struct does not bind, as Python's Request parameter does.
func RequestFromContext(ctx context.Context) (*http.Request, bool) {
	st := stateFrom(ctx)
	if st == nil || st.request == nil {
		return nil, false
	}
	return st.request, true
}

// BasePath returns the path prefix under which the client called the API:
// "/{stage}" for an API Gateway stage URL ("" for the $default stage, a
// custom domain or SAM local), "" for the other sources. The Swagger UI uses
// it for the server of its "Try it out" requests.
func BasePath(ctx context.Context) string {
	if inv := lambdahttp.FromContext(ctx); inv != nil {
		return inv.BasePath
	}
	return ""
}

func eventFrom[E any](ctx context.Context) (E, bool) {
	if inv := lambdahttp.FromContext(ctx); inv != nil {
		if e, ok := inv.Event.(*E); ok {
			return *e, true
		}
	}
	var zero E
	return zero, false
}

// APIGatewayRESTEvent returns the API Gateway REST API (or HTTP API payload
// 1.0) event the request was built from: Python's current_event.
func APIGatewayRESTEvent(ctx context.Context) (events.APIGatewayProxyRequest, bool) {
	return eventFrom[events.APIGatewayProxyRequest](ctx)
}

// APIGatewayHTTPEvent returns the API Gateway HTTP API (payload 2.0) event
// the request was built from.
func APIGatewayHTTPEvent(ctx context.Context) (events.APIGatewayV2HTTPRequest, bool) {
	return eventFrom[events.APIGatewayV2HTTPRequest](ctx)
}

// ALBEvent returns the Application Load Balancer event the request was built
// from.
func ALBEvent(ctx context.Context) (events.ALBTargetGroupRequest, bool) {
	return eventFrom[events.ALBTargetGroupRequest](ctx)
}

// FunctionURLEvent returns the Lambda Function URL event the request was
// built from.
func FunctionURLEvent(ctx context.Context) (events.LambdaFunctionURLRequest, bool) {
	return eventFrom[events.LambdaFunctionURLRequest](ctx)
}

// VPCLatticeEvent returns the VPC Lattice (version 1) event the request was
// built from.
func VPCLatticeEvent(ctx context.Context) (eventsource.VPCLatticeEvent, bool) {
	return eventFrom[eventsource.VPCLatticeEvent](ctx)
}

// VPCLatticeV2Event returns the VPC Lattice version 2 event the request was
// built from.
func VPCLatticeV2Event(ctx context.Context) (eventsource.VPCLatticeEventV2, bool) {
	return eventFrom[eventsource.VPCLatticeEventV2](ctx)
}
