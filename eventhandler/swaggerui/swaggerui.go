package swaggerui

import (
	"cmp"
	_ "embed" // the Swagger UI assets
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// Version is the version of the embedded Swagger UI, the one Powertools
// Python 3.35.0 bundles.
const Version = "5.31.0"

// DefaultPath is where the Swagger UI is served.
const DefaultPath = "/swagger"

var (
	//go:embed assets/swagger-ui-bundle.min.js
	bundleJS string
	//go:embed assets/swagger-ui.min.css
	bundleCSS string
)

// ErrClientSecret reports an OAuth2 client secret outside development mode:
// the secret would be served to every visitor of the page.
var ErrClientSecret = errors.New("swaggerui: cannot use an OAuth2 client secret without POWERTOOLS_DEV")

// OAuth2Config configures the OAuth2 authorization of the Swagger UI
// (ui.initOAuth): Python's OAuth2Config.
type OAuth2Config struct {
	// ClientID is the OAuth2 client id.
	ClientID string `json:"clientId,omitempty"`
	// ClientSecret is only allowed with POWERTOOLS_DEV on: it is visible to
	// anyone who loads the page.
	ClientSecret string `json:"clientSecret,omitempty"`
	Realm        string `json:"realm,omitempty"`
	// AppName is the name of the OAuth2 application (required).
	AppName                                   string            `json:"appName"`
	Scopes                                    []string          `json:"scopes,omitempty"`
	AdditionalQueryStringParams               map[string]string `json:"additionalQueryStringParams,omitempty"`
	UseBasicAuthenticationWithAccessCodeGrant bool              `json:"useBasicAuthenticationWithAccessCodeGrant,omitzero"`
	UsePkceWithAuthorizationCodeGrant         bool              `json:"usePkceWithAuthorizationCodeGrant,omitzero"`
}

// Option configures the Swagger UI.
type Option func(*config)

type config struct {
	path        string
	baseURL     string
	cssURL      string
	jsURL       string
	persist     bool
	oauth2      *OAuth2Config
	middlewares []eventhandler.Middleware
	compress    bool
	openapi     []eventhandler.OpenAPIOption
	document    func(r *http.Request) ([]byte, error)
}

// WithPath serves the UI at path instead of DefaultPath.
func WithPath(path string) Option {
	return func(c *config) { c.path = path }
}

// WithBaseURL links the Swagger UI assets from baseURL
// ({baseURL}/swagger-ui-bundle.min.js and {baseURL}/swagger-ui.min.css)
// instead of inlining the embedded ones: Python's swagger_base_url, for a
// lighter page served from a CDN or a bucket.
func WithBaseURL(baseURL string) Option {
	return func(c *config) { c.baseURL = strings.TrimSuffix(baseURL, "/") }
}

// WithCSSURL links the stylesheet from url.
func WithCSSURL(url string) Option {
	return func(c *config) { c.cssURL = url }
}

// WithJSURL links the Swagger UI bundle from url.
func WithJSURL(url string) Option {
	return func(c *config) { c.jsURL = url }
}

// WithPersistAuthorization keeps the authorization the user entered across
// page reloads.
func WithPersistAuthorization() Option {
	return func(c *config) { c.persist = true }
}

// WithOAuth2 configures the OAuth2 authorization of the UI.
func WithOAuth2(cfg OAuth2Config) Option {
	return func(c *config) { c.oauth2 = &cfg }
}

// WithMiddlewares runs middlewares on the Swagger UI route, to protect it:
// the UI is public otherwise.
func WithMiddlewares(middlewares ...eventhandler.Middleware) Option {
	return func(c *config) { c.middlewares = append(c.middlewares, middlewares...) }
}

// WithCompression gzips the page (about 1.7 MB with the inlined assets) for
// clients that accept it.
func WithCompression() Option {
	return func(c *config) { c.compress = true }
}

// WithOpenAPI adjusts the document the UI shows, over the router's
// configuration. Without servers among them, the UI targets the base path
// of the API (the API Gateway stage), so that "Try it out" works.
func WithOpenAPI(opts ...eventhandler.OpenAPIOption) Option {
	return func(c *config) { c.openapi = append(c.openapi, opts...) }
}

// WithDocument serves the document doc returns instead of the router's: a
// merged document of several functions (see eventhandler.MergeOpenAPI),
// Python's configure_openapi_merge.
func WithDocument(doc func(r *http.Request) ([]byte, error)) Option {
	return func(c *config) { c.document = doc }
}

// Enable serves the Swagger UI of r's OpenAPI document at GET /swagger:
// Python's enable_swagger. The route is not part of the document. The same
// path answers ?format=json with the document and ?format=oauth2-redirect
// with the OAuth2 redirect page.
//
// It panics with ErrClientSecret when the OAuth2 configuration has a client
// secret and POWERTOOLS_DEV is off, as Python refuses it.
func Enable(r *eventhandler.Router, opts ...Option) {
	cfg := config{path: DefaultPath}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.oauth2 != nil && cfg.oauth2.ClientSecret != "" {
		if !env.IsDev() {
			panic(ErrClientSecret)
		}
		warnSecret()
	}
	routeOpts := []eventhandler.RouteOption{eventhandler.ExcludeFromSchema(), eventhandler.Middlewares(cfg.middlewares...)}
	if cfg.compress {
		routeOpts = append(routeOpts, eventhandler.Compress())
	}
	r.Handle(http.MethodGet+" "+cfg.path, Handler(r, opts...), routeOpts...)
}

var warnSecret = sync.OnceFunc(func() {
	slog.New(slog.NewJSONHandler(os.Stderr, nil)).Warn(
		"OAuth2Config is using client_secret and POWERTOOLS_DEV is set. This reveals sensitive information. " +
			"DO NOT USE THIS OUTSIDE LOCAL DEVELOPMENT")
})

// Handler returns the handler of the Swagger UI page of r's document, for
// routers that mount it themselves. Enable registers it.
func Handler(r *eventhandler.Router, opts ...Option) http.Handler {
	cfg := config{path: DefaultPath}
	for _, opt := range opts {
		opt(&cfg)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		format := req.URL.Query().Get("format")
		if format == "oauth2-redirect" {
			write(w, req, eventhandler.ContentTypeHTML, oauth2RedirectHTML)
			return
		}
		spec, err := cfg.spec(r, req)
		if err != nil {
			eventhandler.Error(w, req, err)
			return
		}
		// "</" would close the <script> the document is inlined into.
		escaped := strings.ReplaceAll(string(spec), "</", `<\/`)
		if format == "json" {
			write(w, req, eventhandler.ContentTypeJSON, escaped)
			return
		}
		page, err := cfg.page(escaped)
		if err != nil {
			eventhandler.Error(w, req, err)
			return
		}
		write(w, req, eventhandler.ContentTypeHTML, page)
	})
}

func (c *config) spec(r *eventhandler.Router, req *http.Request) ([]byte, error) {
	if c.document != nil {
		return c.document(req)
	}
	base := eventhandler.BasePath(req.Context())
	if base == "" {
		base = "/"
	}
	opts := append([]eventhandler.OpenAPIOption{func(o *eventhandler.OpenAPIConfig) {
		o.Servers = []eventhandler.Server{{URL: base}}
	}}, c.openapi...)
	return r.OpenAPIJSON(opts...)
}

func write(w http.ResponseWriter, req *http.Request, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	// G705: the page is the Swagger UI's; the document it embeds is JSON
	// whose "</" are escaped.
	if _, err := w.Write([]byte(body)); err != nil { //nolint:gosec // see above
		debug.Logger().DebugContext(req.Context(), "swaggerui: writing the page failed", "err", err)
	}
}

// page renders Python's Swagger UI page.
func (c *config) page(spec string) (string, error) {
	css := "<style>" + bundleCSS + "</style>"
	js := "<script>" + bundleJS + "</script>"
	cssURL, jsURL := c.cssURL, c.jsURL
	if c.baseURL != "" {
		cssURL = cmp.Or(cssURL, c.baseURL+"/swagger-ui.min.css")
		jsURL = cmp.Or(jsURL, c.baseURL+"/swagger-ui-bundle.min.js")
	}
	if cssURL != "" {
		css = "<link rel='stylesheet' type='text/css' href='" + attr(cssURL) + "'>"
	}
	if jsURL != "" {
		js = "<script src='" + attr(jsURL) + "'></script>"
	}
	oauth2 := ""
	if c.oauth2 != nil {
		data, err := json.Marshal(c.oauth2, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
		if err != nil {
			return "", fmt.Errorf("swaggerui: encode the OAuth2 configuration: %w", err)
		}
		oauth2 = "ui.initOAuth(" + string(data) + ");"
	}
	persist := "false"
	if c.persist {
		persist = "true"
	}
	return strings.NewReplacer(
		"{css}", css,
		"{js}", js,
		"{spec}", spec,
		"{persist}", persist,
		"{oauth2}", oauth2,
	).Replace(pageTemplate), nil
}

// attr escapes a URL for a single-quoted HTML attribute.
func attr(s string) string {
	return strings.NewReplacer("&", "&amp;", "'", "&#39;", "<", "&lt;", ">", "&gt;").Replace(s)
}

const pageTemplate = `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Swagger UI</title>
    <meta
      http-equiv="Cache-control"
      content="no-cache, no-store, must-revalidate"
    />
    {css}
</head>

<body>
    <div id="swagger-ui">
        Loading...
    </div>
</body>

{js}

<script>
  var currentUrl = new URL(window.location.href);
  var baseUrl = currentUrl.protocol + "//" + currentUrl.host + currentUrl.pathname;

  var swaggerUIOptions = {
    dom_id: "#swagger-ui",
    docExpansion: "list",
    deepLinking: true,
    filter: true,
    layout: "BaseLayout",
    showExtensions: true,
    showCommonExtensions: true,
    spec: {spec},
    presets: [
      SwaggerUIBundle.presets.apis,
      SwaggerUIBundle.SwaggerUIStandalonePreset
    ],
    plugins: [
      SwaggerUIBundle.plugins.DownloadUrl
    ],
    withCredentials: true,
    persistAuthorization: {persist},
    oauth2RedirectUrl: baseUrl + "?format=oauth2-redirect",
  }

  var ui = SwaggerUIBundle(swaggerUIOptions)
  ui.specActions.updateUrl(currentUrl.pathname + "?format=json");
  {oauth2}
</script>
</html>`

// oauth2RedirectHTML is Swagger UI's oauth2-redirect.html, as Python serves
// it.
const oauth2RedirectHTML = `<!doctype html>
<html lang="en-US">
<head>
    <title>Swagger UI: OAuth2 Redirect</title>
</head>
<body>
<script>
    'use strict';
    function run () {
        var oauth2 = window.opener.swaggerUIRedirectOauth2;
        var sentState = oauth2.state;
        var redirectUrl = oauth2.redirectUrl;
        var isValid, qp, arr;

        if (/code|token|error/.test(window.location.hash)) {
            qp = window.location.hash.substring(1).replace('?', '&');
        } else {
            qp = location.search.substring(1);
        }

        arr = qp.split("&");
        arr.forEach(function (v,i,_arr) { _arr[i] = '"' + v.replace('=', '":"') + '"';});
        qp = qp ? JSON.parse('{' + arr.join() + '}',
                function (key, value) {
                    return key === "" ? value : decodeURIComponent(value);
                }
        ) : {};

        isValid = qp.state === sentState;

        if ((
          oauth2.auth.schema.get("flow") === "accessCode" ||
          oauth2.auth.schema.get("flow") === "authorizationCode" ||
          oauth2.auth.schema.get("flow") === "authorization_code"
        ) && !oauth2.auth.code) {
            if (!isValid) {
                oauth2.errCb({
                    authId: oauth2.auth.name,
                    source: "auth",
                    level: "warning",
                    message: "Authorization may be unsafe, passed state was changed in server. The passed state wasn't returned from auth server."
                });
            }

            if (qp.code) {
                delete oauth2.state;
                oauth2.auth.code = qp.code;
                oauth2.callback({auth: oauth2.auth, redirectUrl: redirectUrl});
            } else {
                let oauthErrorMsg;
                if (qp.error) {
                    oauthErrorMsg = "["+qp.error+"]: " +
                        (qp.error_description ? qp.error_description+ ". " : "no accessCode received from the server. ") +
                        (qp.error_uri ? "More info: "+qp.error_uri : "");
                }

                oauth2.errCb({
                    authId: oauth2.auth.name,
                    source: "auth",
                    level: "error",
                    message: oauthErrorMsg || "[Authorization failed]: no accessCode received from the server."
                });
            }
        } else {
            oauth2.callback({auth: oauth2.auth, token: qp, isValid: isValid, redirectUrl: redirectUrl});
        }
        window.close();
    }

    if (document.readyState !== 'loading') {
        run();
    } else {
        document.addEventListener('DOMContentLoaded', function () {
            run();
        });
    }
</script>
</body>
</html>`
