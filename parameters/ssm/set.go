package ssm

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

// SetOption configures Provider.Set and SetParameter.
type SetOption func(*setOptions)

type setOptions struct {
	overwrite     bool
	description   string
	parameterType types.ParameterType
	tier          types.ParameterTier
	kmsKeyID      string
	input         []func(*ssm.PutParameterInput)
}

// WithOverwrite replaces the value of a parameter that exists. Without it,
// as in Python, writing an existing parameter fails.
func WithOverwrite(overwrite bool) SetOption {
	return func(o *setOptions) { o.overwrite = overwrite }
}

// WithDescription describes the parameter.
func WithDescription(description string) SetOption {
	return func(o *setOptions) { o.description = description }
}

// WithParameterType sets the type: String (the default), StringList or
// SecureString. A []string value is a StringList unless this says otherwise.
func WithParameterType(t types.ParameterType) SetOption {
	return func(o *setOptions) { o.parameterType = t }
}

// WithTier sets the tier: Standard (the default), Advanced or
// Intelligent-Tiering.
func WithTier(tier types.ParameterTier) SetOption {
	return func(o *setOptions) { o.tier = tier }
}

// WithKMSKeyID encrypts a SecureString with this KMS key instead of the
// account's default one.
func WithKMSKeyID(keyID string) SetOption {
	return func(o *setOptions) { o.kmsKeyID = keyID }
}

// WithPutParameterInput edits the PutParameter request before it is sent,
// for what the other options do not cover (tags, policies, allowed
// pattern, data type): Python's **sdk_options.
func WithPutParameterInput(edit func(*ssm.PutParameterInput)) SetOption {
	return func(o *setOptions) { o.input = append(o.input, edit) }
}

// SetResult is what SSM answers to a write.
type SetResult struct {
	// Version is the new version of the parameter.
	Version int64
	// Tier is the tier the parameter is stored in.
	Tier types.ParameterTier
}

// Set writes the parameter name with PutParameter: Python's set_parameter.
// A []string value is written as a StringList (its items joined with
// commas, which SSM requires); a string as a String unless
// WithParameterType says otherwise. Failures are a
// *parameters.SetParameterError.
func (p *Provider) Set[V string | []string](ctx context.Context, name string, value V, opts ...SetOption) (SetResult, error) {
	o := setOptions{parameterType: types.ParameterTypeString, tier: types.ParameterTierStandard}
	var text string
	switch v := any(value).(type) {
	case []string:
		o.parameterType = types.ParameterTypeStringList
		text = strings.Join(v, ",")
	case string:
		text = v
	}
	for _, opt := range opts {
		opt(&o)
	}
	input := &ssm.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(text),
		Overwrite: aws.Bool(o.overwrite),
		Type:      o.parameterType,
		Tier:      o.tier,
	}
	if o.description != "" {
		input.Description = aws.String(o.description)
	}
	if o.kmsKeyID != "" {
		input.KeyId = aws.String(o.kmsKeyID)
	}
	for _, edit := range o.input {
		edit(input)
	}
	out, err := p.client.PutParameter(ctx, input)
	if err != nil {
		return SetResult{}, &parameters.SetParameterError{Name: name, Err: fmt.Errorf("ssm: %w", err)}
	}
	return SetResult{Version: out.Version, Tier: out.Tier}, nil
}
