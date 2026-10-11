package validation

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"
)

// MaxRemoteSchemaSize bounds the documents the HTTP loader reads, so that a
// misbehaving server cannot exhaust the function's memory.
const MaxRemoteSchemaSize = 8 << 20

// defaultHTTPTimeout bounds a fetch by the HTTP loader when no client is
// given: schemas are compiled while the function initializes.
const defaultHTTPTimeout = 10 * time.Second

// urlLoaders returns the loaders by URL scheme: the HTTP loader, when asked
// for, under the explicit ones.
func (o *options) urlLoaders() map[string]Loader {
	loaders := map[string]Loader{}
	if o.httpLoader {
		load := httpLoader(o.httpClient)
		loaders["http"] = load
		loaders["https"] = load
	}
	maps.Copy(loaders, o.loaders)
	return loaders
}

// httpLoader fetches schema documents with GET requests through client.
func httpLoader(client *http.Client) Loader {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return func(ctx context.Context, location string) (any, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "application/schema+json, application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch: %w", err)
		}
		defer func() { _ = resp.Body.Close() }() //nolint:errcheck // the body has been read; closing it has nothing to report
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%w: %s", ErrFetch, resp.Status)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxRemoteSchemaSize+1))
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		if len(body) > MaxRemoteSchemaSize {
			return nil, fmt.Errorf("%w: larger than %d bytes", ErrFetch, MaxRemoteSchemaSize)
		}
		return body, nil
	}
}
