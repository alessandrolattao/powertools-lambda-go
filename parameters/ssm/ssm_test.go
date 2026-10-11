package ssm_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

// fakeSSM is an in-memory Parameter Store. SecureString values read
// without decryption come back as "encrypted(<value>)".
type fakeSSM struct {
	mu       sync.Mutex
	params   map[string]types.Parameter
	pageSize int
	batchErr error

	gets    []awsssm.GetParameterInput
	batches []awsssm.GetParametersInput
	paths   []awsssm.GetParametersByPathInput
	puts    []awsssm.PutParameterInput
}

func newFakeSSM() *fakeSSM {
	f := &fakeSSM{params: make(map[string]types.Parameter), pageSize: 2}
	f.put("/app/db/host", "db.local", types.ParameterTypeString)
	f.put("/app/db/port", "5432", types.ParameterTypeString)
	f.put("/app/db/password", "s3cret", types.ParameterTypeSecureString)
	f.put("/app/config", `{"feature":true}`, types.ParameterTypeString)
	f.put("/app/deep/er/key", "deep", types.ParameterTypeString)
	f.put("/app/broken", `{`, types.ParameterTypeString)
	return f
}

func (f *fakeSSM) put(name, value string, t types.ParameterType) {
	f.params[name] = types.Parameter{
		Name:    aws.String(name),
		Value:   aws.String(value),
		Type:    t,
		ARN:     aws.String("arn:aws:ssm:eu-west-1:123456789012:parameter" + name),
		Version: 1,
	}
}

func (f *fakeSSM) read(name string, decrypt bool) (types.Parameter, bool) {
	plain, selector, _ := strings.Cut(name, ":")
	if strings.HasPrefix(name, "arn:") {
		plain = name[strings.Index(name, ":parameter")+len(":parameter"):]
		selector = ""
	}
	p, ok := f.params[plain]
	if !ok {
		return p, false
	}
	if p.Type == types.ParameterTypeSecureString && !decrypt {
		p.Value = aws.String("encrypted(" + aws.ToString(p.Value) + ")")
	}
	if selector != "" {
		p.Selector = aws.String(":" + selector)
	}
	return p, true
}

func (f *fakeSSM) GetParameter(_ context.Context, in *awsssm.GetParameterInput, _ ...func(*awsssm.Options)) (*awsssm.GetParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, *in)
	p, ok := f.read(aws.ToString(in.Name), aws.ToBool(in.WithDecryption))
	if !ok {
		return nil, &types.ParameterNotFound{Message: aws.String("not found")}
	}
	return &awsssm.GetParameterOutput{Parameter: &p}, nil
}

func (f *fakeSSM) GetParameters(_ context.Context, in *awsssm.GetParametersInput, _ ...func(*awsssm.Options)) (*awsssm.GetParametersOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, *in)
	if len(in.Names) > 10 {
		return nil, errors.New("ValidationException: too many names")
	}
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := &awsssm.GetParametersOutput{}
	for _, name := range in.Names {
		if p, ok := f.read(name, aws.ToBool(in.WithDecryption)); ok {
			out.Parameters = append(out.Parameters, p)
		} else {
			out.InvalidParameters = append(out.InvalidParameters, name)
		}
	}
	return out, nil
}

func (f *fakeSSM) GetParametersByPath(_ context.Context, in *awsssm.GetParametersByPathInput, _ ...func(*awsssm.Options)) (*awsssm.GetParametersByPathOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, *in)
	path := strings.TrimSuffix(aws.ToString(in.Path), "/") + "/"
	var names []string
	for name := range f.params {
		rest, ok := strings.CutPrefix(name, path)
		if ok && (aws.ToBool(in.Recursive) || !strings.Contains(rest, "/")) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	start := 0
	if in.NextToken != nil {
		var err error
		if start, err = strconv.Atoi(*in.NextToken); err != nil {
			return nil, err
		}
	}
	end := min(start+f.pageSize, len(names))
	out := &awsssm.GetParametersByPathOutput{}
	for _, name := range names[start:end] {
		p, _ := f.read(name, aws.ToBool(in.WithDecryption))
		out.Parameters = append(out.Parameters, p)
	}
	if end < len(names) {
		out.NextToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeSSM) PutParameter(_ context.Context, in *awsssm.PutParameterInput, _ ...func(*awsssm.Options)) (*awsssm.PutParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, *in)
	name := aws.ToString(in.Name)
	old, exists := f.params[name]
	if exists && !aws.ToBool(in.Overwrite) {
		return nil, &types.ParameterAlreadyExists{Message: aws.String("exists")}
	}
	f.put(name, aws.ToString(in.Value), in.Type)
	return &awsssm.PutParameterOutput{Version: old.Version + 1, Tier: in.Tier}, nil
}

func (f *fakeSSM) calls() (gets, batches, paths int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gets), len(f.batches), len(f.paths)
}

func TestGet(t *testing.T) {
	t.Parallel()
	fake := newFakeSSM()
	p := ssm.New(fake)
	ctx := t.Context()

	v, err := p.Get(ctx, "/app/db/host")
	require.NoError(t, err)
	assert.Equal(t, "db.local", v)

	secret, err := p.Get(ctx, "/app/db/password", ssm.WithDecrypt(true))
	require.NoError(t, err)
	assert.Equal(t, "s3cret", secret)

	encrypted, err := p.Get(ctx, "/app/db/password")
	require.NoError(t, err)
	assert.Equal(t, "encrypted(s3cret)", encrypted, "decrypted and encrypted reads are cached apart")

	cfg, err := parameters.Get[map[string]bool](ctx, p, "/app/config", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"feature": true}, cfg)

	_, err = p.Get(ctx, "/app/missing")
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	require.ErrorAs(t, err, new(*types.ParameterNotFound), "the SDK error stays reachable")

	gets, _, _ := fake.calls()
	assert.Equal(t, 5, gets)
}

func TestDecryptFromEnvironment(t *testing.T) {
	t.Setenv(env.ParametersSSMDecrypt, "true")
	p := ssm.New(newFakeSSM())
	v, err := p.Get(t.Context(), "/app/db/password")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", v)

	v, err = p.Get(t.Context(), "/app/db/password", ssm.WithDecrypt(false))
	require.NoError(t, err)
	assert.Equal(t, "encrypted(s3cret)", v, "the option wins over the variable")
}

func TestGetMultiple(t *testing.T) {
	t.Parallel()
	fake := newFakeSSM()
	p := ssm.New(fake)
	ctx := t.Context()

	direct, err := p.GetMultiple(ctx, "/app")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"config": `{"feature":true}`, "broken": "{"}, direct, "not recursive by default, as Python's provider")

	all, err := p.GetMultiple(ctx, "/app", ssm.WithRecursive(true), ssm.WithDecrypt(true))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"config":      `{"feature":true}`,
		"broken":      "{",
		"db/host":     "db.local",
		"db/port":     "5432",
		"db/password": "s3cret",
		"deep/er/key": "deep",
	}, all, "names lose the path and its slash, every page is read")

	_, _, paths := fake.calls()
	assert.Equal(t, 1+3, paths, "one page, then three pages of two")
	assert.True(t, aws.ToBool(fake.paths[1].Recursive))
	assert.True(t, aws.ToBool(fake.paths[1].WithDecryption))

	filtered, err := p.GetMultiple(ctx, "/app/db", ssm.WithParameterFilters(types.ParameterStringFilter{Key: aws.String("Type"), Values: []string{"String"}}))
	require.NoError(t, err)
	assert.Len(t, filtered, 3)
	assert.Equal(t, "Type", aws.ToString(fake.paths[len(fake.paths)-1].ParameterFilters[0].Key))
}

func TestGetByNameBatches(t *testing.T) {
	t.Parallel()
	fake := newFakeSSM()
	for i := range 23 {
		fake.put(fmt.Sprintf("/batch/p%02d", i), strconv.Itoa(i), types.ParameterTypeString)
	}
	p := ssm.New(fake)
	params := make(map[string][]parameters.Option)
	for i := range 23 {
		params[fmt.Sprintf("/batch/p%02d", i)] = nil
	}
	values, failed, err := p.GetByName(t.Context(), params)
	require.NoError(t, err)
	assert.Nil(t, failed)
	assert.Len(t, values, 23)
	assert.Equal(t, "7", values["/batch/p07"])

	_, batches, _ := fake.calls()
	assert.Equal(t, 3, batches, "ten names per GetParameters call")
	assert.False(t, aws.ToBool(fake.batches[0].WithDecryption))

	_, _, err = p.GetByName(t.Context(), params)
	require.NoError(t, err)
	_, batches, _ = fake.calls()
	assert.Equal(t, 3, batches, "the second read is served from the cache")

	v, err := p.Get(t.Context(), "/batch/p11")
	require.NoError(t, err)
	assert.Equal(t, "11", v)
	gets, _, _ := fake.calls()
	assert.Equal(t, 0, gets, "Get finds what GetByName cached")
}

func TestGetByNameDecryption(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fake := newFakeSSM()
	values, _, err := ssm.GetByName[string](ctx, ssm.New(fake), map[string][]parameters.Option{
		"/app/db/host":     nil,
		"/app/db/password": {ssm.WithDecrypt(true)},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"/app/db/host": "db.local", "/app/db/password": "s3cret"}, values)
	gets, batches, _ := fake.calls()
	assert.Equal(t, 1, gets, "a parameter that needs decryption among others is read alone")
	assert.Equal(t, 1, batches)
	assert.Equal(t, []string{"/app/db/host"}, fake.batches[0].Names)

	fake = newFakeSSM()
	values, _, err = ssm.GetByName[string](ctx, ssm.New(fake), map[string][]parameters.Option{
		"/app/db/host":     nil,
		"/app/db/password": nil,
	}, ssm.WithDecrypt(true))
	require.NoError(t, err)
	assert.Equal(t, "s3cret", values["/app/db/password"])
	gets, batches, _ = fake.calls()
	assert.Equal(t, 0, gets)
	assert.Equal(t, 1, batches, "when all need decryption they are read together")
	assert.True(t, aws.ToBool(fake.batches[0].WithDecryption))

	fake = newFakeSSM()
	values, _, err = ssm.GetByName[string](ctx, ssm.New(fake), map[string][]parameters.Option{
		"/app/db/host":     {ssm.WithDecrypt(false)},
		"/app/db/password": nil,
	}, ssm.WithDecrypt(true))
	require.NoError(t, err)
	assert.Equal(t, "s3cret", values["/app/db/password"])
	assert.Equal(t, "db.local", values["/app/db/host"], "a parameter's own decrypt option wins")
}

func TestGetByNameTransforms(t *testing.T) {
	t.Parallel()
	values, failed, err := ssm.GetByName[any](t.Context(), ssm.New(newFakeSSM()), map[string][]parameters.Option{
		"/app/config":  nil,
		"/app/db/port": {parameters.WithTransform(parameters.TransformNone)},
		"/app/db/host": {parameters.WithTransform(parameters.TransformBinary)},
	}, parameters.WithTransform(parameters.TransformJSON), ssm.WithRaiseOnError(false))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"feature": true}, values["/app/config"])
	assert.InDelta(t, 5432, values["/app/db/port"], 0, "an empty transform falls back to the shared one, as in Python")
	require.Contains(t, failed, "/app/db/host")
	assert.ErrorIs(t, failed["/app/db/host"], parameters.ErrTransform, "a failed transform is a failure, not a nil value")
}

func TestGetByNameErrors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	params := map[string][]parameters.Option{"/app/db/host": nil, "/app/missing": nil, "/app/gone": nil}

	_, _, err := ssm.New(newFakeSSM()).GetByName(ctx, params)
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	require.ErrorIs(t, err, parameters.ErrNotFound)
	assert.Contains(t, err.Error(), "/app/missing")
	assert.Contains(t, err.Error(), "/app/gone", "the error names every missing parameter")

	values, failed, err := ssm.New(newFakeSSM()).GetByName(ctx, params, ssm.WithRaiseOnError(false))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"/app/db/host": "db.local"}, values)
	assert.Len(t, failed, 2)
	require.ErrorIs(t, failed["/app/missing"], parameters.ErrNotFound)

	fake := newFakeSSM()
	fake.batchErr = &types.InvalidKeyId{Message: aws.String("bad key")}
	_, failed, err = ssm.New(fake).GetByName(ctx, params, ssm.WithRaiseOnError(false))
	require.NoError(t, err)
	assert.Len(t, failed, 3, "an unusable key fails the whole call")
	_, _, err = ssm.New(fake).GetByName(ctx, params)
	require.ErrorIs(t, err, parameters.ErrGetParameter, "and fails the read when it must (Python drops them)")

	fake.batchErr = errors.New("ThrottlingException")
	_, _, err = ssm.New(fake).GetByName(ctx, params, ssm.WithRaiseOnError(false))
	require.ErrorIs(t, err, parameters.ErrGetParameter, "an error of the call fails the read in any mode")
}

func TestGetByNameSelectorsAndARNs(t *testing.T) {
	t.Parallel()
	arn := "arn:aws:ssm:eu-west-1:123456789012:parameter/app/db/port"
	values, failed, err := ssm.New(newFakeSSM()).GetByName(t.Context(), map[string][]parameters.Option{
		"/app/db/host:1": nil,
		arn:              nil,
	})
	require.NoError(t, err)
	assert.Nil(t, failed)
	assert.Equal(t, map[string]string{"/app/db/host:1": "db.local", arn: "5432"}, values)
}

func TestSet(t *testing.T) {
	t.Parallel()
	fake := newFakeSSM()
	p := ssm.New(fake)
	ctx := t.Context()

	res, err := p.Set(ctx, "/new", "value", ssm.WithDescription("a new one"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Version)
	put := fake.puts[0]
	assert.Equal(t, types.ParameterTypeString, put.Type)
	assert.Equal(t, types.ParameterTierStandard, put.Tier)
	assert.False(t, aws.ToBool(put.Overwrite))
	assert.Equal(t, "a new one", aws.ToString(put.Description))
	assert.Nil(t, put.KeyId)

	_, err = p.Set(ctx, "/new", "again")
	require.ErrorIs(t, err, parameters.ErrSetParameter)
	require.ErrorAs(t, err, new(*types.ParameterAlreadyExists))

	res, err = p.Set(ctx, "/new", []string{"a", "b"}, ssm.WithOverwrite(true), ssm.WithTier(types.ParameterTierAdvanced))
	require.NoError(t, err)
	assert.Equal(t, int64(2), res.Version)
	put = fake.puts[2]
	assert.Equal(t, "a,b", aws.ToString(put.Value))
	assert.Equal(t, types.ParameterTypeStringList, put.Type)
	assert.Equal(t, types.ParameterTierAdvanced, res.Tier)

	_, err = p.Set(ctx, "/secure", "s3cret",
		ssm.WithParameterType(types.ParameterTypeSecureString),
		ssm.WithKMSKeyID("alias/mine"),
		ssm.WithPutParameterInput(func(in *awsssm.PutParameterInput) { in.DataType = aws.String("text") }),
	)
	require.NoError(t, err)
	put = fake.puts[3]
	assert.Equal(t, types.ParameterTypeSecureString, put.Type)
	assert.Equal(t, "alias/mine", aws.ToString(put.KeyId))
	assert.Equal(t, "text", aws.ToString(put.DataType))
}

func TestMaxAgePerParameter(t *testing.T) {
	t.Parallel()
	fake := newFakeSSM()
	p := ssm.New(fake)
	params := map[string][]parameters.Option{
		"/app/db/host": {parameters.WithMaxAge(0)},
		"/app/db/port": {parameters.WithMaxAge(time.Hour)},
	}
	for range 2 {
		_, _, err := p.GetByName(t.Context(), params)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"/app/db/host"}, fake.batches[1].Names, "a zero max age is honored per parameter")
}
