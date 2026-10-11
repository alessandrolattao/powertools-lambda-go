package secrets

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// The limits of one BatchGetSecretValue call.
const (
	// maxSecretIDs is how many secrets SecretIdList names.
	maxSecretIDs = 20
	// maxFilterValues is how many values one filter holds.
	maxFilterValues = 10
)

// The sentinel errors of GetByName, inside a *parameters.GetSecretError.
var (
	// ErrNoNames reports a GetByName without names.
	ErrNoNames = errors.New("you must provide at least one secret name")
	// ErrNoSecretsFound reports a GetByName that could read none of the
	// secrets it was asked for.
	ErrNoSecretsFound = errors.New("no secrets found matching the provided names")
)

// GetByName returns the string value of several secrets: GetByName[string].
func (p *Provider) GetByName(ctx context.Context, names []string, opts ...parameters.Option) (values map[string]string, failed map[string]error, err error) {
	return GetByName[string](ctx, p, names, opts...)
}

// GetByName reads several secrets with BatchGetSecretValue and returns them
// as Ts (see parameters.Get), keyed by the names asked for: Python's
// get_secrets_by_name. Names may be ARNs.
//
// The secrets are asked for by their exact name or ARN (SecretIdList, up to
// twenty per call, as many calls as needed). With WithFilters they are found
// through a name filter instead, as Python does, and only exact matches are
// kept.
//
// As in Python, the secrets that cannot be read do not fail the others:
// they are returned in failed, by name, each with its error (Secrets Manager
// reports ResourceNotFoundException, AccessDeniedException,
// DecryptionFailure as smithy.APIError). The read fails when none of the
// secrets can be read, or when a call fails as a whole. A secret whose
// transform fails is in failed too, unless WithRaiseOnTransformError makes it
// fail the read.
//
// The result is cached under the set of names asked for, as in Python.
func GetByName[T any](ctx context.Context, p *Provider, names []string, opts ...parameters.Option) (values map[string]T, failed map[string]error, err error) {
	if len(names) == 0 {
		return nil, nil, &parameters.GetSecretError{Names: names, Err: ErrNoNames}
	}
	var o core.Options
	for _, opt := range opts {
		opt(&o)
	}
	if o.Transform, err = core.NormalizeTransform(o.Transform); err != nil {
		return nil, nil, &parameters.TransformError{Transform: parameters.Transform(o.Transform), Err: err}
	}
	unique := slices.Compact(slices.Sorted(slices.Values(names)))
	key := core.Key{Name: strings.Join(unique, "|"), Transform: o.Transform, Nested: true, Variant: o.Variant()}
	res, err := core.Fetch(ctx, p.cache, key, o.ResolvedMaxAge(), o.ForceFetch, func(ctx context.Context) (batch, error) {
		return p.fetch(ctx, unique, &o)
	})
	if err != nil {
		if errors.Is(err, parameters.ErrGetSecret) || errors.Is(err, parameters.ErrTransform) {
			return nil, nil, err
		}
		return nil, nil, &parameters.GetSecretError{Names: names, Err: err}
	}
	return convert[T](res, &o)
}

// convert turns the cached values of a GetByName into Ts. A value that
// cannot become a T joins the failures, unless the read must fail on it.
func convert[T any](res batch, o *core.Options) (values map[string]T, failed map[string]error, err error) {
	values = make(map[string]T, len(res.values))
	failed = maps.Clone(res.failed)
	for name, value := range res.values {
		converted, err := core.Convert[T](value)
		if err == nil {
			values[name] = converted
			continue
		}
		transformErr := &parameters.TransformError{Name: name, Transform: parameters.Transform(o.Transform), Err: err}
		if o.RaiseOnTransformError {
			return nil, nil, transformErr
		}
		if failed == nil {
			failed = make(map[string]error)
		}
		failed[name] = transformErr
	}
	return values, failed, nil
}

// batch is what the cache keeps of one GetByName.
type batch struct {
	values map[string]core.Value
	failed map[string]error
}

// fetch reads names and transforms what it read.
func (p *Provider) fetch(ctx context.Context, names []string, o *core.Options) (batch, error) {
	var ro readOptions
	core.ApplyStore(o.Store, &ro)
	raw, failed, err := p.read(ctx, names, ro.filters)
	if err != nil {
		return batch{}, &parameters.GetSecretError{Names: names, Err: fmt.Errorf("failed to retrieve secrets: %w", err)}
	}
	if len(raw) == 0 {
		causes := make([]error, 0, len(failed))
		for _, name := range slices.Sorted(maps.Keys(failed)) {
			causes = append(causes, failed[name])
		}
		return batch{}, &parameters.GetSecretError{Names: names, Err: fmt.Errorf("%w: %w", ErrNoSecretsFound, errors.Join(causes...))}
	}
	b := batch{values: make(map[string]core.Value, len(raw))}
	for name, data := range raw {
		value, err := core.Apply(name, data, o.Transform)
		if err != nil {
			transformErr := &parameters.TransformError{Name: name, Transform: parameters.Transform(o.Transform), Err: err}
			if o.RaiseOnTransformError {
				return batch{}, transformErr
			}
			failed[name] = transformErr
			continue
		}
		b.values[name] = value
	}
	if len(failed) > 0 {
		b.failed = failed
	}
	return b, nil
}

// read calls BatchGetSecretValue for names: by SecretIdList, or through a
// name filter next to filters. It returns the secrets read, by the name
// asked for, and the ones that failed.
func (p *Provider) read(ctx context.Context, names []string, filters []types.Filter) (raw map[string][]byte, failed map[string]error, err error) {
	asked := make(map[string]bool, len(names))
	for _, name := range names {
		asked[name] = true
	}
	raw = make(map[string][]byte, len(names))
	failed = make(map[string]error)
	if len(filters) == 0 {
		for chunk := range slices.Chunk(names, maxSecretIDs) {
			input := &secretsmanager.BatchGetSecretValueInput{SecretIdList: chunk}
			if err := p.pages(ctx, input, asked, raw, failed); err != nil {
				return nil, nil, err
			}
		}
	} else {
		for chunk := range slices.Chunk(names, maxFilterValues) {
			input := &secretsmanager.BatchGetSecretValueInput{
				Filters: append(slices.Clone(filters), types.Filter{Key: types.FilterNameStringTypeName, Values: chunk}),
			}
			if err := p.pages(ctx, input, asked, raw, failed); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, name := range names {
		_, read := raw[name]
		_, failedAlready := failed[name]
		if !read && !failedAlready {
			failed[name] = &parameters.GetParameterError{Name: name, Err: parameters.ErrNotFound}
		}
	}
	return raw, failed, nil
}

// pages reads every page of one BatchGetSecretValue.
func (p *Provider) pages(ctx context.Context, input *secretsmanager.BatchGetSecretValueInput, asked map[string]bool, raw map[string][]byte, failed map[string]error) error {
	pages := secretsmanager.NewBatchGetSecretValuePaginator(p.client, input)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("secrets manager: %w", err)
		}
		collect(page, asked, raw, failed)
	}
	return nil
}

// collect keeps the secrets and the errors of one page that concern a
// secret asked for.
func collect(page *secretsmanager.BatchGetSecretValueOutput, asked map[string]bool, raw map[string][]byte, failed map[string]error) {
	for _, entry := range page.SecretValues {
		name, ok := askedAs(entry, asked)
		if !ok {
			// A filter matches names by prefix: "db" finds "db-password"
			// too, which nobody asked for.
			continue
		}
		if entry.SecretString != nil {
			raw[name] = []byte(*entry.SecretString)
		} else {
			raw[name] = entry.SecretBinary
		}
	}
	for _, e := range page.Errors {
		if id := aws.ToString(e.SecretId); asked[id] {
			failed[id] = &parameters.GetParameterError{Name: id, Err: &smithy.GenericAPIError{
				Code:    aws.ToString(e.ErrorCode),
				Message: aws.ToString(e.Message),
			}}
		}
	}
}

// askedAs returns the name a secret was asked for by: its name, its ARN, or
// a partial ARN (one without the random suffix Secrets Manager appends).
func askedAs(entry types.SecretValueEntry, asked map[string]bool) (string, bool) {
	name, arn := aws.ToString(entry.Name), aws.ToString(entry.ARN)
	switch {
	case asked[name]:
		return name, true
	case arn != "" && asked[arn]:
		return arn, true
	}
	if i := strings.LastIndexByte(arn, '-'); i > 0 && asked[arn[:i]] {
		return arn[:i], true
	}
	return "", false
}
