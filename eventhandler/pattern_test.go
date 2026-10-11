package eventhandler

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePatternErrors(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"",
		"GET",
		"GET todos",
		"example.com/todos",
		"G(ET /todos",
		"/todos/{id",
		"/todos/x{id}",
		"/todos/{}",
		"/todos/{1d}",
		"/todos/{id}/{id}",
		"/files/{path...}/more",
		"/a/{$}/b",
		"/bad/%zz",
	} {
		_, err := parsePattern(pattern)
		require.ErrorIs(t, err, ErrInvalidPattern, pattern)
	}
}

func TestPatternMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern  string
		path     string
		trailing bool
		values   map[string]string
		ok       bool
	}{
		{"/", "/", false, nil, true},
		{"/", "/x", false, nil, false},
		{"/todos", "/todos", false, nil, true},
		{"/todos", "/todos/", false, nil, false},
		{"/todos", "/todos/", true, nil, true},
		{"/todos", "/todos//", true, nil, true},
		{"/todos/", "/todos", true, nil, true},
		{"/todos/", "/todos/", false, nil, true},
		{"/todos/", "/todos/1", false, nil, false},
		{"/", "///", true, nil, true},
		{"/todos/{id}", "/todos/1", false, map[string]string{"id": "1"}, true},
		{"/todos/{id}", "/todos/", false, nil, false},
		{"/todos/{id}", "/todos/1/2", false, nil, false},
		{"/todos/{id}", "/todos/a%20b", false, map[string]string{"id": "a b"}, true},
		{"/todos/{id}", "/todos/a%2Fb", false, map[string]string{"id": "a/b"}, true},
		{"/todos/{id}/{status}", "/todos/1/done", false, map[string]string{"id": "1", "status": "done"}, true},
		{"/files/{path...}", "/files/a/b/c.txt", false, map[string]string{"path": "a/b/c.txt"}, true},
		{"/files/{path...}", "/files/", false, map[string]string{"path": ""}, true},
		{"/files/{path...}", "/files", false, nil, false},
		{"/files/{path...}", "/files", true, map[string]string{"path": ""}, true},
		{"/a/{$}", "/a/", false, nil, true},
		{"/a/{$}", "/a/b", false, nil, false},
		{"/a%20b", "/a%20b", false, nil, true},
		{"/a%20b", "/a b", false, nil, true},
		{"/x", "/x%zz", false, nil, false},
	}
	for _, tc := range tests {
		p, err := parsePattern(tc.pattern)
		require.NoError(t, err, tc.pattern)
		values, ok := p.match(tc.path, tc.path, tc.trailing)
		assert.Equal(t, tc.ok, ok, "%s ~ %s (trailing %v)", tc.pattern, tc.path, tc.trailing)
		if tc.ok {
			assert.Equal(t, tc.values, values, "%s ~ %s", tc.pattern, tc.path)
		}
	}
}

func TestRegexpPattern(t *testing.T) {
	t.Parallel()
	p, err := regexpPattern("get", regexp.MustCompile(`/users/(?P<user_id>\w+)/files/.+`))
	require.NoError(t, err)
	assert.Equal(t, "GET", p.method)
	assert.True(t, p.dynamic)
	assert.Equal(t, []string{"user_id"}, p.names())

	values, ok := p.match("", "/users/42/files/a/b", false)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"user_id": "42"}, values)

	_, ok = p.match("", "/prefix/users/42/files/a", false)
	assert.False(t, ok, "regular expressions match the whole path")

	static, err := regexpPattern("", regexp.MustCompile(`/files/.+`))
	require.NoError(t, err)
	assert.False(t, static.dynamic, "a rule without groups counts as static, as in Python")

	_, err = regexpPattern("G(T", regexp.MustCompile(`/x`))
	require.ErrorIs(t, err, ErrInvalidPattern)
}

func TestPatternKeyAndOpenAPIPath(t *testing.T) {
	t.Parallel()
	a, err := parsePattern("GET /todos/{id}")
	require.NoError(t, err)
	b, err := parsePattern("GET /todos/{other}")
	require.NoError(t, err)
	assert.Equal(t, a.key(), b.key(), "the wildcard name does not change what matches")

	files, err := parsePattern("/files/{path...}")
	require.NoError(t, err)
	path, ok := files.openAPIPath()
	assert.True(t, ok)
	assert.Equal(t, "/files/{path}", path)

	prefixed, err := a.withPrefix("/v1")
	require.NoError(t, err)
	assert.Equal(t, "GET /v1/todos/{id}", prefixed.str)

	root, err := parsePattern("POST /")
	require.NoError(t, err)
	mounted, err := root.withPrefix("/todos")
	require.NoError(t, err)
	assert.Equal(t, "/todos", mounted.path)
}
