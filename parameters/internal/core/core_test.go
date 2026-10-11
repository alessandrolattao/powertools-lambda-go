package core

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

func TestNormalizeTransform(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"json", "json"}, {"binary", "binary"}, {"auto", "auto"}, {".json", "json"}, {".binary", "binary"},
	} {
		got, err := NormalizeTransform(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	_, err := NormalizeTransform("yaml")
	require.ErrorIs(t, err, ErrUnknownTransform)
}

func TestResolveTransform(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, transform, want string }{
		{"config.json", TransformAuto, TransformJSON},
		{"key.binary", TransformAuto, TransformBinary},
		{"a.b.json", TransformAuto, TransformJSON},
		{"plain", TransformAuto, TransformNone},
		{"file.yaml", TransformAuto, TransformNone},
		// Python's rsplit gives the whole name when it has no dot.
		{"json", TransformAuto, TransformJSON},
		{"binary", TransformAuto, TransformBinary},
		{"config.json", TransformNone, TransformNone},
		{"config", TransformBinary, TransformBinary},
	} {
		assert.Equal(t, tc.want, ResolveTransform(tc.name, tc.transform), tc.name+" "+tc.transform)
	}
}

func TestDecodeBase64LikePython(t *testing.T) {
	t.Parallel()
	// The expectations are what Python's base64.b64decode returns.
	for _, tc := range []struct {
		in   string
		want string
		err  bool
	}{
		{in: "YWJj", want: "abc"},
		{in: "YWJ", err: true},
		{in: "YWJj====", want: "abc"},
		{in: "Y W\nJj", want: "abc"},
		{in: "YWJj!", want: "abc"},
		{in: "YQ", err: true},
		{in: "YQ=", err: true},
		{in: "YQ==", want: "a"},
		{in: "Y===", err: true},
		{in: "", want: ""},
		{in: "=", want: ""},
		{in: "YWJj\x00", want: "abc"},
		{in: "SGVsbG8sIFdvcmxkIQ==", want: "Hello, World!"},
	} {
		got, err := DecodeBase64([]byte(tc.in))
		if tc.err {
			require.ErrorIs(t, err, ErrIncorrectPadding, "%q", tc.in)
			continue
		}
		require.NoError(t, err, "%q", tc.in)
		assert.Equal(t, tc.want, string(got), "%q", tc.in)
	}
}

func TestApply(t *testing.T) {
	t.Parallel()
	v, err := Apply("k", []byte(`{"a":1}`), TransformJSON)
	require.NoError(t, err)
	assert.Equal(t, KindJSON, v.Kind)

	// json.loads keeps the last of repeated members.
	v, err = Apply("k", []byte(`{"a":1,"a":2}`), TransformJSON)
	require.NoError(t, err)
	got, err := Convert[map[string]int](v)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 2}, got)

	_, err = Apply("k", []byte(`{"a":`), TransformJSON)
	require.ErrorIs(t, err, ErrInvalidJSON)

	v, err = Apply("k", []byte("aGk="), TransformBinary)
	require.NoError(t, err)
	assert.Equal(t, Value{Data: []byte("hi"), Kind: KindBinary}, v)

	v, err = Apply("k.binary", []byte("aGk="), TransformAuto)
	require.NoError(t, err)
	assert.Equal(t, KindBinary, v.Kind)

	v, err = Apply("k", []byte("aGk="), TransformAuto)
	require.NoError(t, err)
	assert.Equal(t, Value{Data: []byte("aGk="), Kind: KindText}, v)
}

type named string

type config struct {
	Region string `json:"region"`
}

func TestConvert(t *testing.T) {
	t.Parallel()
	text := Value{Data: []byte("hello"), Kind: KindText}
	binary := Value{Data: []byte{0, 1}, Kind: KindBinary}

	s, err := Convert[string](text)
	require.NoError(t, err)
	assert.Equal(t, "hello", s)

	n, err := Convert[named](text)
	require.NoError(t, err)
	assert.Equal(t, named("hello"), n)

	b, err := Convert[[]byte](text)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), b)
	b[0] = 'X'
	assert.Equal(t, "hello", string(text.Data), "the cached bytes are not shared")

	a, err := Convert[any](text)
	require.NoError(t, err)
	assert.Equal(t, "hello", a)

	a, err = Convert[any](binary)
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 1}, a)

	_, err = Convert[int](text)
	require.ErrorIs(t, err, ErrUnsupportedType)

	c, err := Convert[config](Value{Data: []byte(`{"region":"eu-west-1","other":1}`), Kind: KindJSON})
	require.NoError(t, err)
	assert.Equal(t, config{Region: "eu-west-1"}, c)

	js, err := Convert[string](Value{Data: []byte(`"quoted"`), Kind: KindJSON})
	require.NoError(t, err)
	assert.Equal(t, "quoted", js)

	_, err = Convert[string](Value{Data: []byte(`{}`), Kind: KindJSON})
	require.Error(t, err)

	zero, err := Convert[map[string]any](Value{Kind: KindFailed})
	require.NoError(t, err)
	assert.Nil(t, zero)
}

func TestOptions(t *testing.T) {
	t.Setenv(env.ParametersMaxAge, "")
	o := Options{}
	assert.Equal(t, DefaultMaxAge, o.ResolvedMaxAge())

	t.Setenv(env.ParametersMaxAge, "60")
	assert.Equal(t, time.Minute, o.ResolvedMaxAge())

	t.Setenv(env.ParametersMaxAge, "a minute")
	assert.Equal(t, DefaultMaxAge, o.ResolvedMaxAge(), "an invalid value falls back")

	o = Options{MaxAge: 0, MaxAgeSet: true}
	assert.Equal(t, time.Duration(0), o.ResolvedMaxAge(), "an explicit zero wins")

	type mine struct{ calls []string }
	o = Options{Store: []StoreOption{
		{Variant: "a=1", Apply: func(m *mine) { m.calls = append(m.calls, "first") }},
		{Variant: "", Apply: func(*int) {}},
		{Variant: "b=2", Apply: func(m *mine) { m.calls = append(m.calls, "second") }},
	}}
	assert.Equal(t, "a=1|b=2", o.Variant())
	var m mine
	ApplyStore(o.Store, &m)
	assert.Equal(t, []string{"first", "second"}, m.calls)
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	var d Defaults[string, *int]
	builds := 0
	errBuild := errors.New("no region")
	_, err := d.Get("a", func() (*int, error) { builds++; return nil, errBuild })
	require.ErrorIs(t, err, errBuild)

	p1, err := d.Get("a", func() (*int, error) { builds++; return new(1), nil })
	require.NoError(t, err)
	p2, err := d.Get("a", func() (*int, error) { builds++; return new(2), nil })
	require.NoError(t, err)
	assert.Same(t, p1, p2)
	other, err := d.Get("b", func() (*int, error) { builds++; return new(3), nil })
	require.NoError(t, err)
	assert.Equal(t, 3, *other)
	assert.Equal(t, 3, builds, "a failure is not kept, a success is")

	d.Reset()
	p3, err := d.Get("a", func() (*int, error) { return new(4), nil })
	require.NoError(t, err)
	assert.Equal(t, 4, *p3)
}
