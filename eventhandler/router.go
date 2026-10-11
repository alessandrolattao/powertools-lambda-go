package eventhandler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// ErrDuplicateRoute reports a route registered twice: the same method and a
// path with the same shape, which the second one could never serve.
var ErrDuplicateRoute = errors.New("duplicate route")

// Middleware wraps an http.Handler, net/http's middleware shape. It runs
// before the handler when it calls next, after it when next returns, and
// short-circuits the request when it writes a response without calling next.
type Middleware = func(next http.Handler) http.Handler

// Router routes HTTP requests with Powertools' rules and serves them through
// middlewares, error handlers, CORS and compression. It is an http.Handler:
// the event source adapters (APIGatewayREST, APIGatewayHTTP, ALB,
// FunctionURL, VPCLattice, VPCLatticeV2) turn it into a Lambda handler, and
// http.ListenAndServe serves it locally.
//
// Register routes, middlewares and error handlers before serving: a Router
// is safe for concurrent requests, not for registrations during them.
type Router struct {
	cfg         config
	debug       bool
	routes      []*route
	static      []*route
	dynamic     []*route
	keys        map[string]bool
	middlewares []Middleware
	handlers    []*errorHandler
	notFound    http.Handler
}

// route is a registered route.
type route struct {
	pat     *pattern
	handler http.Handler
	cfg     routeConfig
	// op is the typed operation behind the handler, nil for plain handlers.
	op operationDoc
	// funcName names the handler for the generated operationId.
	funcName string
}

// New returns an empty Router.
func New(opts ...Option) *Router {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Router{cfg: cfg, debug: cfg.debugMode(), keys: map[string]bool{}}
}

// Handle registers h for the requests matching pattern.
//
// A pattern is net/http's "[METHOD ]/path": "GET /todos/{id}" matches one
// path segment into the path value id (read it with r.PathValue),
// "/files/{path...}" matches the rest of the path. Matching follows
// Powertools rather than http.ServeMux: a pattern matches the whole path
// (a trailing slash is not a subtree, "{$}" is accepted and changes
// nothing), patterns without wildcards are tried first and then the others
// in registration order, a method mismatch is a 404 (no 405), GET does not
// serve HEAD, and nothing redirects. A pattern without a method matches every
// method. Registering the same method and path shape twice panics with
// ErrDuplicateRoute, as a conflict does in http.ServeMux.
func (r *Router) Handle(pattern string, h http.Handler, opts ...RouteOption) {
	p, err := parsePattern(pattern)
	if err != nil {
		panic(err)
	}
	r.add(&route{pat: p, handler: h, cfg: newRouteConfig(opts), funcName: handlerName(h)})
}

// HandleFunc registers the handler function h for pattern; see Handle.
func (r *Router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request), opts ...RouteOption) {
	p, err := parsePattern(pattern)
	if err != nil {
		panic(err)
	}
	r.add(&route{pat: p, handler: http.HandlerFunc(h), cfg: newRouteConfig(opts), funcName: funcName(h)})
}

// HandleRegexp registers h for the requests whose method is method (any
// method when empty) and whose whole path matches re, as Powertools Python's
// regular expression rules do. The named groups of re are the path values.
// A route whose expression has groups counts as having wildcards; without,
// it is tried with the literal routes. Regular expression routes are not
// part of the OpenAPI document, which cannot describe them.
func (r *Router) HandleRegexp(method string, re *regexp.Regexp, h http.Handler, opts ...RouteOption) {
	p, err := regexpPattern(method, re)
	if err != nil {
		panic(err)
	}
	r.add(&route{pat: p, handler: h, cfg: newRouteConfig(opts), funcName: handlerName(h)})
}

func (r *Router) add(rt *route) {
	key := rt.pat.key()
	if r.keys[key] {
		panic(fmt.Errorf("%w: %q conflicts with a route registered before", ErrDuplicateRoute, rt.pat.str))
	}
	r.keys[key] = true
	r.routes = append(r.routes, rt)
	if rt.pat.dynamic {
		r.dynamic = append(r.dynamic, rt)
	} else {
		r.static = append(r.static, rt)
	}
}

// Use adds middlewares that run for every request, matched or not
// (including pre-flight and not found responses), in order and before the
// middlewares of the route: Python's use.
func (r *Router) Use(middlewares ...Middleware) {
	r.middlewares = append(r.middlewares, middlewares...)
}

// HandleNotFound sets the handler of the requests no route matches, and of
// the errors that are a *ServiceError with status 404: Python's not_found.
// It runs through the router's middlewares. Without it, such requests get a
// 404 {"statusCode":404,"message":"Not found"}.
func (r *Router) HandleNotFound(h http.Handler) {
	r.notFound = h
}

// Include adds the routes of sub to r, under prefix ("" for none): Python's
// include_router. A "/" route of sub becomes the prefix itself. As in
// Python, the middlewares of sub join r's (they then run for every route of
// r, after the middlewares r had when Include was called), and the error
// handlers of sub replace r's for the same error types. Include copies what
// sub has at the time of the call.
func (r *Router) Include(sub *Router, prefix string) {
	for _, rt := range sub.routes {
		p, err := rt.pat.withPrefix(prefix)
		if err != nil {
			panic(err)
		}
		cp := *rt
		cp.pat = p
		if cp.op != nil {
			cp.op = cp.op.withPattern(p)
			cp.handler = cp.op.handler()
		}
		r.add(&cp)
	}
	r.middlewares = append(r.middlewares, sub.middlewares...)
	for _, h := range sub.handlers {
		r.addErrorHandler(h)
	}
	if sub.notFound != nil {
		r.notFound = sub.notFound
	}
}

// ServeHTTP routes req and writes the response.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	inv := lambdahttp.FromContext(req.Context())
	buf, direct := w.(*lambdahttp.ResponseBuffer)
	if !direct {
		buf = lambdahttp.NewResponseBuffer()
	}

	escaped := r.stripPrefixes(req.URL.EscapedPath())
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		decoded = escaped
	}
	trailing := inv != nil && inv.TrailingSlash
	rt, values := r.match(req.Method, escaped, decoded, trailing)

	st := &requestState{router: r, route: rt}
	// A clone, so that the path values set here never reach the request of
	// an enclosing mux.
	routed := req.Clone(context.WithValue(req.Context(), stateKey{}, st))
	routed.URL.Path, routed.URL.RawPath = decoded, ""
	if escaped != routed.URL.EscapedPath() {
		routed.URL.RawPath = escaped
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		routed.SetPathValue(name, values[name])
	}
	st.request = routed

	var h http.Handler
	if rt != nil {
		routed.Pattern = rt.pat.str
		h = chain(rt.handler, rt.cfg.middlewares)
	} else {
		h = r.notFoundHandler()
	}
	h = chain(h, r.middlewares)

	if r.debug {
		debug.Logger().DebugContext(req.Context(), "event handler: processing request",
			"method", req.Method, "path", decoded, "route", routeName(rt),
			"middlewares", len(r.middlewares)+routeMiddlewares(rt))
	}
	h.ServeHTTP(buf, routed)
	r.finish(buf, routed, st)

	if !direct {
		if err := buf.CopyTo(w); err != nil {
			debug.Logger().DebugContext(req.Context(), "event handler: writing the response failed", "err", err)
		}
	}
}

func routeName(rt *route) string {
	if rt == nil {
		return "not found"
	}
	return rt.pat.str
}

func routeMiddlewares(rt *route) int {
	if rt == nil {
		return 0
	}
	return len(rt.cfg.middlewares)
}

// chain wraps h in middlewares; the first one runs first.
func chain(h http.Handler, middlewares []Middleware) http.Handler {
	for _, mw := range slices.Backward(middlewares) {
		h = mw(h)
	}
	return h
}

func (r *Router) match(method, escaped, decoded string, trailing bool) (matched *route, values map[string]string) {
	for _, list := range [2][]*route{r.static, r.dynamic} {
		for _, rt := range list {
			if rt.pat.method != "" && rt.pat.method != method {
				continue
			}
			var ok bool
			if values, ok = rt.pat.match(escaped, decoded, trailing); ok {
				return rt, values
			}
		}
	}
	return nil, nil
}

// stripPrefixes applies the strip prefixes to the escaped path, with
// Python's rules: the first matching string prefix wins and ends the
// stripping, regular expressions remove every match and go on.
func (r *Router) stripPrefixes(path string) string {
	for _, p := range r.cfg.prefixes {
		if p.re != nil {
			path = p.re.ReplaceAllString(path, "")
			if path == "" {
				return "/"
			}
			continue
		}
		if p.text == "" {
			continue
		}
		if path == p.text {
			return "/"
		}
		if rest, ok := strings.CutPrefix(path, p.text+"/"); ok {
			return "/" + rest
		}
	}
	return path
}

// notFoundHandler answers the requests no route matches: the pre-flight
// response when CORS is on and the method is OPTIONS, the not found handler
// or a 404 otherwise.
func (r *Router) notFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.cfg.cors != nil && req.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", r.corsMethods())
			w.WriteHeader(http.StatusNoContent)
			return
		}
		Error(w, req, NotFound(""))
	})
}

// corsEnabled reports whether the route, nil for the not found route, sends
// CORS headers.
func (r *Router) corsEnabled(rt *route) bool {
	if rt != nil && rt.cfg.cors != nil {
		return *rt.cfg.cors
	}
	return r.cfg.cors != nil
}

// corsMethods lists the methods of the routes with CORS on, plus OPTIONS,
// for Access-Control-Allow-Methods.
func (r *Router) corsMethods() string {
	methods := map[string]bool{http.MethodOptions: true}
	for _, rt := range r.routes {
		if rt.pat.method != "" && r.corsEnabled(rt) {
			methods[rt.pat.method] = true
		}
	}
	return strings.Join(slices.Sorted(maps.Keys(methods)), ",")
}
