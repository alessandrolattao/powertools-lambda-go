// Package swaggerui serves the Swagger UI of an eventhandler.Router's
// OpenAPI document: Powertools for AWS Lambda (Python)'s enable_swagger.
//
//	app := eventhandler.New()
//	// ... routes ...
//	swaggerui.Enable(app)
//
// GET /swagger answers the page, ?format=json the document and
// ?format=oauth2-redirect the OAuth2 redirect page of the UI. The document
// targets the base path of the API (the API Gateway stage), so that "Try it
// out" works behind a stage URL.
//
// The page inlines Swagger UI 5.31.0, the version Powertools Python bundles,
// embedded in this package (about 1.7 MB, so only binaries that import the
// package carry it). WithBaseURL, WithCSSURL and WithJSURL link the assets
// from elsewhere instead. Swagger UI is Copyright SmartBear Software Inc.,
// licensed under the Apache License 2.0: see assets/LICENSE, assets/NOTICE
// and, for the libraries the bundle includes, assets/swagger-ui-bundle.js.LICENSE.txt.
//
// # Options
//
//   - WithPath: where the UI is served (default /swagger).
//   - WithBaseURL, WithCSSURL, WithJSURL: link the assets instead of
//     inlining them (Python's swagger_base_url).
//   - WithPersistAuthorization: keep the authorization across reloads.
//   - WithOAuth2: the OAuth2 configuration of the UI (a client secret needs
//     POWERTOOLS_DEV, since every visitor sees it).
//   - WithMiddlewares: protect the route, which is public otherwise.
//   - WithCompression: gzip the page.
//   - WithOpenAPI: adjust the document (title, servers...).
//   - WithDocument: serve another document, such as a merge of several
//     functions' documents (Python's configure_openapi_merge).
//
// # Environment variables
//
// POWERTOOLS_DEV allows an OAuth2 client secret.
package swaggerui
