package secrets_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/secrets"
)

type secret struct {
	text     *string
	binary   []byte
	previous string
	denied   bool
}

// fakeSecrets is an in-memory Secrets Manager.
type fakeSecrets struct {
	mu      sync.Mutex
	secrets map[string]*secret
	putErr  error

	gets    []secretsmanager.GetSecretValueInput
	batches []secretsmanager.BatchGetSecretValueInput
	puts    []secretsmanager.PutSecretValueInput
	creates []secretsmanager.CreateSecretInput
}

func newFake() *fakeSecrets {
	return &fakeSecrets{secrets: map[string]*secret{
		"db":          {text: aws.String(`{"user":"admin","password":"s3cret"}`), previous: `{"user":"admin","password":"old"}`},
		"db-password": {text: aws.String("s3cret")},
		"api-key":     {text: aws.String("k3y")},
		"cert":        {binary: []byte{0xde, 0xad}},
		"locked":      {text: aws.String("x"), denied: true},
	}}
}

func arnOf(name string) string {
	return "arn:aws:secretsmanager:eu-west-1:123456789012:secret:" + name + "-AbCdEf"
}

func (f *fakeSecrets) lookup(id string) (string, *secret) {
	name := id
	if strings.HasPrefix(id, "arn:") {
		name = strings.TrimSuffix(id[strings.LastIndex(id, ":")+1:], "-AbCdEf")
	}
	return name, f.secrets[name]
}

func (f *fakeSecrets) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, *in)
	_, s := f.lookup(aws.ToString(in.SecretId))
	if s == nil {
		return nil, &types.ResourceNotFoundException{Message: aws.String("no such secret")}
	}
	if aws.ToString(in.VersionStage) == "AWSPREVIOUS" {
		return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(s.previous)}, nil
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: s.text, SecretBinary: s.binary}, nil
}

func (f *fakeSecrets) BatchGetSecretValue(_ context.Context, in *secretsmanager.BatchGetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.BatchGetSecretValueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, *in)
	if len(in.SecretIdList) > 20 {
		return nil, errors.New("ValidationException: too many secret ids")
	}
	out := &secretsmanager.BatchGetSecretValueOutput{}
	add := func(name string, s *secret) {
		if s.denied {
			out.Errors = append(out.Errors, types.APIErrorType{SecretId: aws.String(name), ErrorCode: aws.String("AccessDeniedException"), Message: aws.String("denied")})
			return
		}
		out.SecretValues = append(out.SecretValues, types.SecretValueEntry{Name: aws.String(name), ARN: aws.String(arnOf(name)), SecretString: s.text, SecretBinary: s.binary})
	}
	for _, id := range in.SecretIdList {
		name, s := f.lookup(id)
		if s == nil {
			out.Errors = append(out.Errors, types.APIErrorType{SecretId: aws.String(id), ErrorCode: aws.String("ResourceNotFoundException"), Message: aws.String("missing")})
			continue
		}
		add(name, s)
	}
	for _, filter := range in.Filters {
		if filter.Key != types.FilterNameStringTypeName {
			continue
		}
		if len(filter.Values) > 10 {
			return nil, errors.New("ValidationException: too many filter values")
		}
		// Secrets Manager matches the name filter by prefix.
		names := make([]string, 0, len(f.secrets))
		for name := range f.secrets {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if slices.ContainsFunc(filter.Values, func(v string) bool { return strings.HasPrefix(name, v) }) {
				add(name, f.secrets[name])
			}
		}
	}
	return out, nil
}

func (f *fakeSecrets) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, *in)
	if f.putErr != nil {
		return nil, f.putErr
	}
	name, s := f.lookup(aws.ToString(in.SecretId))
	if s == nil {
		return nil, &types.ResourceNotFoundException{Message: aws.String("no such secret")}
	}
	s.text, s.binary = in.SecretString, in.SecretBinary
	return &secretsmanager.PutSecretValueOutput{Name: aws.String(name), ARN: aws.String(arnOf(name)), VersionId: aws.String("v2")}, nil
}

func (f *fakeSecrets) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, *in)
	name := aws.ToString(in.Name)
	f.secrets[name] = &secret{text: in.SecretString, binary: in.SecretBinary}
	return &secretsmanager.CreateSecretOutput{Name: aws.String(name), ARN: aws.String(arnOf(name)), VersionId: aws.String("v1")}, nil
}

func TestGet(t *testing.T) {
	t.Parallel()
	fake := newFake()
	p := secrets.New(fake)
	ctx := t.Context()

	v, err := p.Get(ctx, "api-key")
	require.NoError(t, err)
	assert.Equal(t, "k3y", v)

	cert, err := parameters.Get[[]byte](ctx, p, "cert")
	require.NoError(t, err)
	assert.Equal(t, []byte{0xde, 0xad}, cert, "a binary secret comes back as its bytes")

	type credentials struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	current, err := parameters.Get[credentials](ctx, p, "db", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	previous, err := parameters.Get[credentials](ctx, p, "db", parameters.WithTransform(parameters.TransformJSON), secrets.WithVersionStage("AWSPREVIOUS"))
	require.NoError(t, err)
	assert.Equal(t, "s3cret", current.Password)
	assert.Equal(t, "old", previous.Password, "a version stage is not served the cached current version")
	assert.Equal(t, "AWSPREVIOUS", aws.ToString(fake.gets[3].VersionStage))

	_, err = p.Get(ctx, "db", secrets.WithVersionID("abc"))
	require.NoError(t, err)
	assert.Equal(t, "abc", aws.ToString(fake.gets[4].VersionId))

	_, err = p.Get(ctx, "nope")
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	require.ErrorAs(t, err, new(*types.ResourceNotFoundException))

	_, err = p.GetMultiple(ctx, "/path")
	require.ErrorIs(t, err, errors.ErrUnsupported)
}

func TestGetByName(t *testing.T) {
	t.Parallel()
	fake := newFake()
	p := secrets.New(fake)
	ctx := t.Context()

	values, failed, err := p.GetByName(ctx, []string{"db", "api-key", "missing", "locked", arnOf("cert")})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"db":          `{"user":"admin","password":"s3cret"}`,
		"api-key":     "k3y",
		arnOf("cert"): "\xde\xad",
	}, values, "exact names only: db-password is not db")
	require.Len(t, failed, 2)
	var apiErr smithy.APIError
	require.ErrorAs(t, failed["missing"], &apiErr)
	assert.Equal(t, "ResourceNotFoundException", apiErr.ErrorCode())
	require.ErrorAs(t, failed["locked"], &apiErr)
	assert.Equal(t, "AccessDeniedException", apiErr.ErrorCode())
	assert.Equal(t, []string{"api-key", arnOf("cert"), "db", "locked", "missing"}, fake.batches[0].SecretIdList)
	assert.Empty(t, fake.batches[0].Filters)

	_, _, err = p.GetByName(ctx, []string{"missing", "locked", "db", "api-key", arnOf("cert")})
	require.NoError(t, err)
	assert.Len(t, fake.batches, 1, "the same names in another order are served from the cache")

	_, _, err = p.GetByName(ctx, []string{"missing", "locked"})
	require.ErrorIs(t, err, parameters.ErrGetSecret)
	require.ErrorIs(t, err, secrets.ErrNoSecretsFound)

	_, _, err = p.GetByName(ctx, nil)
	require.ErrorIs(t, err, secrets.ErrNoNames)
}

func TestGetByNameManyNames(t *testing.T) {
	t.Parallel()
	fake := newFake()
	names := make([]string, 0, 45)
	for i := range 45 {
		name := fmt.Sprintf("s%02d", i)
		fake.secrets[name] = &secret{text: aws.String(name)}
		names = append(names, name)
	}
	values, failed, err := secrets.New(fake).GetByName(t.Context(), names)
	require.NoError(t, err)
	assert.Nil(t, failed)
	assert.Len(t, values, 45, "Python's name filter takes ten names at most")
	assert.Len(t, fake.batches, 3, "twenty names per call")
}

func TestGetByNameWithFilters(t *testing.T) {
	t.Parallel()
	fake := newFake()
	tag := types.Filter{Key: types.FilterNameStringTypeTagKey, Values: []string{"team"}}
	values, failed, err := secrets.GetByName[string](t.Context(), secrets.New(fake), []string{"db"}, secrets.WithFilters(tag))
	require.NoError(t, err)
	assert.Nil(t, failed)
	assert.Equal(t, []string{"db"}, slices.Sorted(func(yield func(string) bool) {
		for k := range values {
			if !yield(k) {
				return
			}
		}
	}), "the prefix match db-password is dropped")
	require.Len(t, fake.batches[0].Filters, 2)
	assert.Equal(t, tag, fake.batches[0].Filters[0])
	assert.Empty(t, fake.batches[0].SecretIdList)
}

func TestGetByNameTransforms(t *testing.T) {
	t.Parallel()
	p := secrets.New(newFake())
	values, failed, err := secrets.GetByName[any](t.Context(), p, []string{"db", "api-key"}, parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"user": "admin", "password": "s3cret"}, values["db"])
	require.ErrorIs(t, failed["api-key"], parameters.ErrTransform)

	_, _, err = secrets.GetByName[any](t.Context(), p, []string{"db", "api-key"}, parameters.WithTransform(parameters.TransformJSON), parameters.WithRaiseOnTransformError(true), parameters.WithForceFetch(true))
	require.ErrorIs(t, err, parameters.ErrTransform)
}

func TestSet(t *testing.T) {
	t.Parallel()
	fake := newFake()
	p := secrets.New(fake)
	ctx := t.Context()

	res, err := p.Set(ctx, "api-key", "n3w", secrets.WithClientRequestToken("token-1"))
	require.NoError(t, err)
	assert.Equal(t, secrets.SetResult{ARN: arnOf("api-key"), Name: "api-key", VersionID: "v2"}, res)
	assert.Equal(t, "token-1", aws.ToString(fake.puts[0].ClientRequestToken))
	assert.Empty(t, fake.creates, "an existing secret gets a new version")

	res, err = p.Set(ctx, "fresh", map[string]any{"b": 2, "a": 1}, secrets.WithCreateSecretInput(func(in *secretsmanager.CreateSecretInput) {
		in.Description = aws.String("made by a test")
	}))
	require.NoError(t, err)
	assert.Equal(t, "v1", res.VersionID)
	require.Len(t, fake.creates, 1, "a missing secret is created")
	assert.JSONEq(t, `{"a":1,"b":2}`, aws.ToString(fake.creates[0].SecretString))
	assert.Equal(t, "made by a test", aws.ToString(fake.creates[0].Description))

	_, err = p.Set(ctx, "cert", []byte{1, 2}, secrets.WithPutSecretValueInput(func(in *secretsmanager.PutSecretValueInput) {
		in.VersionStages = []string{"AWSCURRENT"}
	}))
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2}, fake.puts[2].SecretBinary)
	assert.Nil(t, fake.puts[2].SecretString)
	assert.Equal(t, []string{"AWSCURRENT"}, fake.puts[2].VersionStages)

	fake.putErr = errors.New("AccessDeniedException")
	_, err = p.Set(ctx, "api-key", "x")
	require.ErrorIs(t, err, parameters.ErrSetSecret)

	_, err = p.Set(ctx, "bad", func() {})
	require.ErrorIs(t, err, parameters.ErrSetSecret)
}
