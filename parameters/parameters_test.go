package parameters_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

// fakeStore serves fixed values and counts the reads.
type fakeStore struct {
	mu       sync.Mutex
	values   map[string]string
	multiple map[string]map[string]string
	err      error
	gets     int
	multis   int
	seen     []storeOpts
}

type storeOpts struct {
	version string
}

func withVersion(v string) parameters.Option {
	return parameters.WithStoreOption("version="+v, func(o *storeOpts) { o.version = v })
}

func (s *fakeStore) Get(_ context.Context, name string, opts parameters.StoreOptions) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	var o storeOpts
	opts.Apply(&o)
	s.seen = append(s.seen, o)
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[name+o.version]
	if !ok {
		return nil, parameters.ErrNotFound
	}
	return []byte(v), nil
}

func (s *fakeStore) GetMultiple(_ context.Context, path string, _ parameters.StoreOptions) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.multis++
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string][]byte)
	for k, v := range s.multiple[path] {
		out[k] = []byte(v)
	}
	return out, nil
}

func (s *fakeStore) reads() (gets, multis int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.multis
}

type dbConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func newStore() *fakeStore {
	return &fakeStore{
		values: map[string]string{
			"plain":       "hello",
			"config":      `{"host":"db.local","port":5432}`,
			"config.json": `{"host":"db.local","port":5432}`,
			"key.binary":  "aGVsbG8=",
			"encoded":     "aGVsbG8=",
			"broken":      `{"host":`,
			"plainv2":     "hello again",
		},
		multiple: map[string]map[string]string{
			"/app": {
				"a.json":   `{"x":1}`,
				"b.binary": "aGk=",
				"c":        "text",
				"d.json":   `{`,
			},
		},
	}
}

func TestGetTypes(t *testing.T) {
	t.Parallel()
	p := parameters.NewProvider(newStore())
	ctx := t.Context()

	s, err := p.Get(ctx, "plain")
	require.NoError(t, err)
	assert.Equal(t, "hello", s)

	cfg, err := parameters.Get[dbConfig](ctx, p, "config", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, dbConfig{Host: "db.local", Port: 5432}, cfg)

	generic, err := parameters.Get[map[string]any](ctx, p, "config", parameters.WithTransform(".json"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"host": "db.local", "port": float64(5432)}, generic)

	b, err := parameters.Get[[]byte](ctx, p, "encoded", parameters.WithTransform(parameters.TransformBinary))
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), b)

	a, err := parameters.Get[any](ctx, p, "key.binary", parameters.WithTransform(parameters.TransformAuto))
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), a)

	auto, err := parameters.Get[dbConfig](ctx, p, "config.json", parameters.WithTransform(parameters.TransformAuto))
	require.NoError(t, err)
	assert.Equal(t, "db.local", auto.Host)

	raw, err := parameters.Get[any](ctx, p, "plain")
	require.NoError(t, err)
	assert.Equal(t, "hello", raw)
}

func TestGetErrors(t *testing.T) {
	t.Parallel()
	p := parameters.NewProvider(newStore())
	ctx := t.Context()

	_, err := p.Get(ctx, "missing")
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	require.ErrorIs(t, err, parameters.ErrNotFound)
	getErr, ok := errors.AsType[*parameters.GetParameterError](err)
	require.True(t, ok)
	assert.Equal(t, "missing", getErr.Name)

	_, err = parameters.Get[dbConfig](ctx, p, "broken", parameters.WithTransform(parameters.TransformJSON))
	require.ErrorIs(t, err, parameters.ErrTransform)
	transformErr, ok := errors.AsType[*parameters.TransformError](err)
	require.True(t, ok)
	assert.Equal(t, parameters.TransformJSON, transformErr.Transform)

	_, err = p.Get(ctx, "plain", parameters.WithTransform("yaml"))
	require.ErrorIs(t, err, parameters.ErrTransform)

	_, err = parameters.Get[int](ctx, p, "plain")
	require.ErrorIs(t, err, parameters.ErrTransform, "a number needs the JSON transform")
}

func TestGetCaches(t *testing.T) {
	t.Parallel()
	store := newStore()
	p := parameters.NewProvider(store)
	ctx := t.Context()

	for range 3 {
		_, err := p.Get(ctx, "plain")
		require.NoError(t, err)
	}
	gets, _ := store.reads()
	assert.Equal(t, 1, gets)

	_, err := p.Get(ctx, "plain", parameters.WithForceFetch(true))
	require.NoError(t, err)
	_, err = p.Get(ctx, "plain")
	require.NoError(t, err)
	gets, _ = store.reads()
	assert.Equal(t, 2, gets, "a forced read refreshes the cache")

	_, err = parameters.Get[[]byte](ctx, p, "plain", parameters.WithTransform(parameters.TransformBinary))
	require.Error(t, err)
	_, err = p.Get(ctx, "config", parameters.WithTransform(parameters.TransformJSON))
	require.Error(t, err, "a JSON object is not a string")
	_, err = parameters.Get[dbConfig](ctx, p, "config", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	gets, _ = store.reads()
	assert.Equal(t, 4, gets, "each transform is cached apart; a failed transform is not cached")

	p.ClearCache()
	_, err = p.Get(ctx, "plain")
	require.NoError(t, err)
	gets, _ = store.reads()
	assert.Equal(t, 5, gets)
}

func TestGetZeroMaxAge(t *testing.T) {
	t.Parallel()
	store := newStore()
	p := parameters.NewProvider(store)
	for range 2 {
		_, err := p.Get(t.Context(), "plain", parameters.WithMaxAge(0))
		require.NoError(t, err)
	}
	gets, _ := store.reads()
	assert.Equal(t, 2, gets)
}

func TestMaxAgeFromEnvironment(t *testing.T) {
	t.Setenv(env.ParametersMaxAge, "0")
	store := newStore()
	p := parameters.NewProvider(store)
	for range 2 {
		_, err := p.Get(t.Context(), "plain")
		require.NoError(t, err)
	}
	gets, _ := store.reads()
	assert.Equal(t, 2, gets, "POWERTOOLS_PARAMETERS_MAX_AGE=0 disables the cache")

	_, err := p.Get(t.Context(), "plain", parameters.WithMaxAge(time.Minute))
	require.NoError(t, err)
	_, err = p.Get(t.Context(), "plain", parameters.WithMaxAge(time.Minute))
	require.NoError(t, err)
	gets, _ = store.reads()
	assert.Equal(t, 3, gets, "the option wins over the variable")
}

func TestStoreOptionsVaryTheCache(t *testing.T) {
	t.Parallel()
	store := newStore()
	p := parameters.NewProvider(store)
	ctx := t.Context()

	v1, err := p.Get(ctx, "plain")
	require.NoError(t, err)
	v2, err := p.Get(ctx, "plain", withVersion("v2"))
	require.NoError(t, err)
	assert.Equal(t, "hello", v1)
	assert.Equal(t, "hello again", v2, "another version is not served from the cache")

	_, err = p.Get(ctx, "plain", withVersion("v2"))
	require.NoError(t, err)
	gets, _ := store.reads()
	assert.Equal(t, 2, gets)
	assert.Equal(t, "v2", store.seen[1].version)
}

func TestGetMultiple(t *testing.T) {
	t.Parallel()
	store := newStore()
	p := parameters.NewProvider(store)
	ctx := t.Context()

	values, err := parameters.GetMultiple[any](ctx, p, "/app", parameters.WithTransform(parameters.TransformAuto))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"a.json":   map[string]any{"x": float64(1)},
		"b.binary": []byte("hi"),
		"c":        "text",
		"d.json":   nil,
	}, values, "a failed transform gives the zero value, as Python gives None")

	_, err = parameters.GetMultiple[any](ctx, p, "/app", parameters.WithTransform(parameters.TransformAuto), parameters.WithRaiseOnTransformError(true))
	require.ErrorIs(t, err, parameters.ErrTransform)

	text, err := p.GetMultiple(ctx, "/app")
	require.NoError(t, err)
	assert.Equal(t, "aGk=", text["b.binary"])

	_, err = p.GetMultiple(ctx, "/app")
	require.NoError(t, err)
	_, multis := store.reads()
	assert.Equal(t, 2, multis, "auto and none are cached apart; the raising read found the failure in the cache")

	_, err = parameters.GetMultiple[int](ctx, p, "/app")
	require.ErrorIs(t, err, parameters.ErrTransform)
}

func TestStoreErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	errDown := errors.New("store down")
	p := parameters.NewProvider(&fakeStore{err: errDown})
	_, err := p.Get(t.Context(), "a")
	require.ErrorIs(t, err, errDown)
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	_, err = p.GetMultiple(t.Context(), "/a")
	require.ErrorIs(t, err, errDown)
	require.ErrorIs(t, err, parameters.ErrGetParameter)
}

func TestCanceledContext(t *testing.T) {
	t.Parallel()
	p := parameters.NewProvider(newStore())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := p.Get(ctx, "plain")
	require.ErrorIs(t, err, context.Canceled)
}

func TestConcurrentReads(t *testing.T) {
	t.Parallel()
	store := newStore()
	p := parameters.NewProvider(store)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			cfg, err := parameters.Get[map[string]any](t.Context(), p, "config", parameters.WithTransform(parameters.TransformJSON))
			assert.NoError(t, err)
			cfg["host"] = "mutated by a caller"
		})
	}
	wg.Wait()
	cfg, err := parameters.Get[map[string]any](t.Context(), p, "config", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, "db.local", cfg["host"], "callers never share the cached value")
}

func TestErrorMessages(t *testing.T) {
	t.Parallel()
	cause := errors.New("boom")
	for _, tc := range []struct {
		err      error
		sentinel error
		text     string
	}{
		{&parameters.GetParameterError{Name: "a", Err: cause}, parameters.ErrGetParameter, `get parameter "a": boom`},
		{&parameters.TransformError{Name: "a", Transform: parameters.TransformJSON, Err: cause}, parameters.ErrTransform, `parameter "a": unable to transform value using "json" transform: boom`},
		{&parameters.SetParameterError{Name: "a", Err: cause}, parameters.ErrSetParameter, `error setting parameter "a": boom`},
		{&parameters.GetSecretError{Names: []string{"a"}, Err: cause}, parameters.ErrGetSecret, `get secrets ["a"]: boom`},
		{&parameters.SetSecretError{Name: "a", Err: cause}, parameters.ErrSetSecret, `error setting secret "a": boom`},
	} {
		require.EqualError(t, tc.err, tc.text)
		require.ErrorIs(t, tc.err, tc.sentinel)
		require.ErrorIs(t, tc.err, cause)
	}
}
