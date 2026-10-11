package eventhandler

// The members of OpenAPI and JSON Schema objects the package builds and
// reads.
const (
	keyType        = "type"
	keyProperties  = "properties"
	keyTitle       = "title"
	keyName        = "name"
	keyDescription = "description"
	keySchemas     = "schemas"
	typeObject     = "object"
	typeString     = "string"
	typeInteger    = "integer"
	typeArray      = "array"
	typeNull       = "null"
)

// object returns v as a JSON object, nil when it is not one.
func object(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// at walks the objects of v along keys.
func at(v any, keys ...string) map[string]any {
	m := object(v)
	for _, k := range keys {
		m = object(m[k])
	}
	return m
}

// child returns the object m holds under key, adding an empty one when
// there is none.
func child(m map[string]any, key string) map[string]any {
	c := object(m[key])
	if c == nil {
		c = map[string]any{}
		m[key] = c
	}
	return c
}

// list returns v as a JSON array, nil when it is not one.
func list(v any) []any {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	return a
}

// stringOf returns v as a string, "" when it is not one.
func stringOf(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
