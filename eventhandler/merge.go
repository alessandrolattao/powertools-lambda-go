package eventhandler

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

// ErrMergeConflict reports an operation defined by several documents, with
// ConflictError: Python's OpenAPIMergeError.
var ErrMergeConflict = errors.New("OpenAPI merge conflict")

// ConflictStrategy says what MergeOpenAPI does when several documents
// define the same operation (method and path): Python's on_conflict.
type ConflictStrategy int

// The conflict strategies.
const (
	// ConflictWarn keeps the first definition and logs a warning, the
	// default.
	ConflictWarn ConflictStrategy = iota
	// ConflictError fails the merge with ErrMergeConflict.
	ConflictError
	// ConflictFirst keeps the first definition silently.
	ConflictFirst
	// ConflictLast keeps the last definition.
	ConflictLast
)

// MergeOpenAPI merges the OpenAPI documents of several functions into one,
// for an API whose routes live in separate Lambda functions: Python's
// OpenAPIMerge. cfg gives the document-level fields; the paths, components
// and tags come from docs (each an OpenAPI JSON document, such as
// Router.OpenAPIJSON writes). Components of the same name are replaced by
// the later document's, as in Python; tags of cfg come first.
//
// Python discovers the handlers by scanning source files; a Go function's
// routes live in its binary, so each function writes its document at build
// time (or exports a function that registers its routes, for a program that
// imports them all) and the documents are merged here.
func MergeOpenAPI(cfg OpenAPIConfig, onConflict ConflictStrategy, docs ...[]byte) ([]byte, error) {
	merged := map[string]any{}
	paths := map[string]any{}
	components := map[string]any{}
	var tags []any
	for i, data := range docs {
		doc := map[string]any{}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("decode OpenAPI document %d: %w", i, err)
		}
		if err := mergePaths(paths, object(doc["paths"]), onConflict); err != nil {
			return nil, err
		}
		for kind, entries := range object(doc["components"]) {
			maps.Copy(child(components, kind), object(entries))
		}
		tags = append(tags, list(doc["tags"])...)
	}
	merged["paths"] = paths
	if len(components) > 0 {
		merged["components"] = components
	}
	if err := decorateDocument(merged, cfg, "3.1.0"); err != nil {
		return nil, err
	}
	if all := mergeTags(cfg.Tags, tags); len(all) > 0 {
		merged["tags"] = all
	}
	return marshalDocument(merged)
}

func mergePaths(target, source map[string]any, onConflict ConflictStrategy) error {
	for _, path := range slices.Sorted(maps.Keys(source)) {
		item := object(source[path])
		existing := object(target[path])
		if existing == nil {
			target[path] = item
			continue
		}
		for _, method := range slices.Sorted(maps.Keys(item)) {
			if _, taken := existing[method]; !taken {
				existing[method] = item[method]
				continue
			}
			switch onConflict {
			case ConflictError:
				return fmt.Errorf("%w: %s %s is defined in multiple schemas", ErrMergeConflict, strings.ToUpper(method), path)
			case ConflictLast:
				existing[method] = item[method]
			case ConflictWarn:
				warnOnce("merge:"+method+" "+path, func(l *slog.Logger) {
					l.Warn("Conflict: an operation is defined in multiple schemas. Keeping first definition.",
						"method", strings.ToUpper(method), "path", path)
				})
			case ConflictFirst:
			}
		}
	}
	return nil
}

// mergeTags lists the configured tags, then the documents' tags not
// configured, once each.
func mergeTags(configured []Tag, found []any) []any {
	seen := map[string]bool{}
	var out []any
	for i := range configured {
		t := &configured[i]
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		m := map[string]any{keyName: t.Name}
		setString(m, keyDescription, t.Description)
		if t.ExternalDocs != nil {
			m["externalDocs"] = externalDocsMap(*t.ExternalDocs)
		}
		out = append(out, m)
	}
	for _, t := range found {
		m := object(t)
		name := stringOf(m[keyName])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, m)
	}
	return out
}
