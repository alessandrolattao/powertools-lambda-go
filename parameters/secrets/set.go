package secrets

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

// SetOption configures Provider.Set and SetSecret.
type SetOption func(*setOptions)

type setOptions struct {
	clientRequestToken string
	put                []func(*secretsmanager.PutSecretValueInput)
	create             []func(*secretsmanager.CreateSecretInput)
}

// WithClientRequestToken makes the write idempotent: Secrets Manager
// creates one version per token, and the token becomes the version id.
func WithClientRequestToken(token string) SetOption {
	return func(o *setOptions) { o.clientRequestToken = token }
}

// WithPutSecretValueInput edits the PutSecretValue request before it is
// sent (version stages, for one): Python's **sdk_options for the update.
func WithPutSecretValueInput(edit func(*secretsmanager.PutSecretValueInput)) SetOption {
	return func(o *setOptions) { o.put = append(o.put, edit) }
}

// WithCreateSecretInput edits the CreateSecret request sent when the secret
// does not exist yet (description, KMS key, tags): Python's **sdk_options
// for the creation.
func WithCreateSecretInput(edit func(*secretsmanager.CreateSecretInput)) SetOption {
	return func(o *setOptions) { o.create = append(o.create, edit) }
}

// SetResult is what Secrets Manager answers to a write.
type SetResult struct {
	ARN       string
	Name      string
	VersionID string
}

// Set writes the secret name: Python's set_secret. A string is written as
// SecretString, a []byte as SecretBinary, anything else as its JSON encoding
// in SecretString (Python accepts a dict).
//
// As in Python, Set first adds a version to the secret (PutSecretValue) and
// creates the secret (CreateSecret) only when it does not exist, the cheaper
// path for a secret that is written often. Failures are a
// *parameters.SetSecretError.
func (p *Provider) Set(ctx context.Context, name string, value any, opts ...SetOption) (SetResult, error) {
	var o setOptions
	for _, opt := range opts {
		opt(&o)
	}
	var (
		secretString *string
		secretBinary []byte
	)
	switch v := value.(type) {
	case string:
		secretString = aws.String(v)
	case []byte:
		secretBinary = v
	default:
		encoded, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return SetResult{}, &parameters.SetSecretError{Name: name, Err: fmt.Errorf("encode the value as JSON: %w", err)}
		}
		secretString = aws.String(string(encoded))
	}
	var token *string
	if o.clientRequestToken != "" {
		token = aws.String(o.clientRequestToken)
	}

	put := &secretsmanager.PutSecretValueInput{SecretId: aws.String(name), SecretString: secretString, SecretBinary: secretBinary, ClientRequestToken: token}
	for _, edit := range o.put {
		edit(put)
	}
	updated, err := p.client.PutSecretValue(ctx, put)
	if err == nil {
		return SetResult{ARN: aws.ToString(updated.ARN), Name: aws.ToString(updated.Name), VersionID: aws.ToString(updated.VersionId)}, nil
	}
	if !errors.As(err, new(*types.ResourceNotFoundException)) {
		return SetResult{}, &parameters.SetSecretError{Name: name, Err: fmt.Errorf("secrets manager: %w", err)}
	}

	create := &secretsmanager.CreateSecretInput{Name: aws.String(name), SecretString: secretString, SecretBinary: secretBinary, ClientRequestToken: token}
	for _, edit := range o.create {
		edit(create)
	}
	created, err := p.client.CreateSecret(ctx, create)
	if err != nil {
		return SetResult{}, &parameters.SetSecretError{Name: name, Err: fmt.Errorf("secrets manager: %w", err)}
	}
	return SetResult{ARN: aws.ToString(created.ARN), Name: aws.ToString(created.Name), VersionID: aws.ToString(created.VersionId)}, nil
}
