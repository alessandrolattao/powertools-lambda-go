package datamasking

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Provider encrypts and decrypts for DataMasking: the Go form of Python's
// BaseProvider. DataMasking turns the data into its JSON document before
// Encrypt and parses the document Decrypt returns, so a provider deals with
// bytes and its own ciphertext encoding only. Implementations must be safe
// for concurrent use.
type Provider interface {
	// Encrypt encrypts plaintext and returns the ciphertext as text.
	Encrypt(ctx context.Context, plaintext []byte, req Request) (string, error)
	// Decrypt reverses Encrypt. It fails with ErrContextMismatch when
	// req.EncryptionContext is not the one the data was encrypted with.
	Decrypt(ctx context.Context, ciphertext string, req Request) ([]byte, error)
}

// Request is what an Encrypt or a Decrypt hands the provider besides the
// data.
type Request struct {
	// EncryptionContext is the non-secret data bound to the ciphertext,
	// which decryption must present again.
	EncryptionContext map[string]string
	// Options are the per-call settings of WithProviderOptions.
	Options []ProviderOption
}

// ProviderOption is a per-call setting only one provider understands: the
// Go form of Python's provider_options. Each provider documents its own.
type ProviderOption interface {
	// Provider names the provider the option is meant for.
	Provider() string
}

// DataMasking erases, encrypts and decrypts data. It holds no state of its
// own: build one at package level, or in main, and share it.
type DataMasking struct {
	provider            Provider
	eraser              Eraser
	raiseOnMissingField bool
	jsonOptions         []json.Options
}

// Option configures a DataMasking.
type Option func(*DataMasking)

// WithProvider sets the encryption provider, typically the AWS Encryption
// SDK one of package datamasking/kms. Without one, DataMasking only erases.
// A provider that also implements Eraser masks values its own way.
func WithProvider(p Provider) Option {
	return func(m *DataMasking) {
		m.provider = p
		m.eraser = nil
		if e, ok := p.(Eraser); ok {
			m.eraser = e
		}
	}
}

// WithRaiseOnMissingField says what to do when an expression of WithFields
// or WithMaskingRules matches nothing: fail with ErrFieldNotFound (true,
// the default), or skip the expression (false). Skipped expressions are
// reported on the Powertools debug log (POWERTOOLS_DEBUG), where Python
// emits a warning.
func WithRaiseOnMissingField(raise bool) Option {
	return func(m *DataMasking) { m.raiseOnMissingField = raise }
}

// WithJSONOptions sets the encoding/json/v2 options used to marshal Go
// values (structs, maps) into JSON and to decode results into the caller's
// type: the Go form of Python's json_serializer and json_deserializer.
func WithJSONOptions(opts ...json.Options) Option {
	return func(m *DataMasking) { m.jsonOptions = append(m.jsonOptions, opts...) }
}

// New returns a DataMasking. By default it erases only, and fails on
// expressions that match nothing.
func New(opts ...Option) *DataMasking {
	m := &DataMasking{raiseOnMissingField: true}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// EraseOption configures one Erase.
type EraseOption func(*eraseOptions)

type eraseOptions struct {
	fields    []string
	fieldsSet bool
	rule      Rule
	rules     []Rule
}

// WithFields erases only the values the expressions select, in order, and
// keeps the rest of the data. An expression that selects an object or an
// array erases it whole, unless a mask option is set too.
func WithFields(paths ...string) EraseOption {
	return func(o *eraseOptions) {
		o.fields = append(o.fields, paths...)
		o.fieldsSet = true
	}
}

// WithDynamicMask masks every character but '-', '_', '.' and space,
// keeping the length of the text.
func WithDynamicMask() EraseOption {
	return func(o *eraseOptions) { o.rule.DynamicMask = true }
}

// WithCustomMask replaces the text with mask, cut to the text's length when
// longer.
func WithCustomMask(mask string) EraseOption {
	return func(o *eraseOptions) { o.rule.CustomMask = mask }
}

// WithRegex replaces what pattern (Go RE2 syntax) matches with format,
// where \1 or \g<name> refer to the pattern's groups, as in Python.
func WithRegex(pattern, format string) EraseOption {
	return func(o *eraseOptions) {
		o.rule.RegexPattern = pattern
		o.rule.MaskFormat = format
	}
}

// WithMaskingRules masks each rule's Path with the rule's own masks, rules
// applied in order. It takes precedence over every other option, as Python's
// masking_rules does. Values that are null are left alone.
func WithMaskingRules(rules ...Rule) EraseOption {
	return func(o *eraseOptions) { o.rules = append(o.rules, rules...) }
}

// Erase masks data irreversibly and returns the result as a decoded JSON
// value (nil, bool, int64, float64, string, []any, map[string]any).
//
// Data is raw JSON ([]byte, jsontext.Value), a Go value that marshals to
// JSON (a struct, a map, an aws-lambda-go event), or a scalar. A string is
// a JSON document when fields or rules are given, a plain value otherwise,
// as in Python.
//
// Without options the whole data becomes Mask (an array becomes as many
// Mask strings). WithFields and WithMaskingRules select what to mask; the
// mask options say how (see Rule).
func (m *DataMasking) Erase(data any, opts ...EraseOption) (any, error) {
	var o eraseOptions
	for _, opt := range opts {
		opt(&o)
	}
	switch {
	case len(o.rules) > 0:
		return m.applyRules(data, o.rules)
	case o.fieldsSet:
		return m.applyFields(data, o.fields, o.rule)
	}
	value, err := generic(data, false, m.jsonOptions)
	if err != nil {
		return nil, err
	}
	erase, err := m.eraseFunc(o.rule)
	if err != nil {
		return nil, err
	}
	return erase(value, o.rule)
}

// EraseAs is Erase, with the result decoded into T: a struct whose erased
// fields are strings, a map, or raw JSON ([]byte, jsontext.Value), which
// receives the document itself.
func (m *DataMasking) EraseAs[T any](data any, opts ...EraseOption) (T, error) {
	erased, err := m.Erase(data, opts...)
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeInto[T](erased, m.jsonOptions)
}

// eraseFunc returns what masks one value: the provider's Eraser, or the
// default masks compiled from rule.
func (m *DataMasking) eraseFunc(rule Rule) (func(any, Rule) (any, error), error) {
	if m.eraser != nil {
		return m.eraser.Erase, nil
	}
	compiled, err := compileMask(rule)
	if err != nil {
		return nil, err
	}
	return func(v any, _ Rule) (any, error) { return compiled.erase(v), nil }, nil
}

// container parses data for field selection, which needs an object or an
// array to select from.
func (m *DataMasking) container(data any) (any, error) {
	root, err := generic(data, true, m.jsonOptions)
	if err != nil {
		return nil, err
	}
	switch root.(type) {
	case map[string]any, []any:
		return root, nil
	default:
		return nil, fmt.Errorf("%w: field selection needs a JSON object or array, got %T", ErrUnsupportedType, data)
	}
}

func (m *DataMasking) applyFields(data any, fields []string, rule Rule) (any, error) {
	if len(fields) == 0 {
		return nil, ErrEmptyFields
	}
	root, err := m.container(data)
	if err != nil {
		return nil, err
	}
	erase, err := m.eraseFunc(rule)
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		rule.Path = field
		root, err = m.update(root, rule, func(v any) (any, bool, error) {
			masked, eraseErr := erase(v, rule)
			return masked, true, eraseErr
		})
		if err != nil {
			return nil, err
		}
	}
	return root, nil
}

func (m *DataMasking) applyRules(data any, rules []Rule) (any, error) {
	root, err := m.container(data)
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		erase, err := m.eraseFunc(rule)
		if err != nil {
			return nil, &PathError{Path: rule.Path, Err: ErrInvalidRegex, Cause: err}
		}
		root, err = m.update(root, rule, func(v any) (any, bool, error) {
			if v == nil {
				return nil, false, nil
			}
			// Python masks str(value): rules see every value as text.
			masked, eraseErr := erase(pyStr(v), rule)
			return masked, true, eraseErr
		})
		if err != nil {
			return nil, err
		}
	}
	return root, nil
}

// update replaces every value rule.Path selects in root with what modify
// returns, and returns the new root.
func (m *DataMasking) update(root any, rule Rule, modify func(any) (any, bool, error)) (any, error) {
	path, err := compilePath(rule.Path)
	if err != nil {
		return nil, &PathError{Path: rule.Path, Err: ErrInvalidPath, Cause: err}
	}
	if path.expr == nil {
		// The expression is the root itself.
		masked, changed, modifyErr := modify(root)
		if modifyErr != nil {
			return nil, maskError(rule.Path, modifyErr)
		}
		if changed {
			return masked, nil
		}
		return root, nil
	}
	if len(path.expr.Get(root)) == 0 {
		if m.raiseOnMissingField {
			return nil, &PathError{Path: rule.Path, Err: ErrFieldNotFound}
		}
		debug.Logger().Warn("field or expression not found, skipping it", "path", rule.Path)
		return root, nil
	}
	var firstErr error
	out, err := path.expr.Modify(root, func(v any) (any, bool) {
		if firstErr != nil {
			return v, false
		}
		masked, changed, modifyErr := modify(v)
		if modifyErr != nil {
			firstErr = modifyErr
			return v, false
		}
		return masked, changed
	})
	if firstErr != nil {
		return nil, maskError(rule.Path, firstErr)
	}
	if err != nil {
		return nil, &PathError{Path: rule.Path, Err: ErrInvalidPath, Cause: err}
	}
	return out, nil
}

// maskError keeps an error of the package as it is, and ties any other one,
// from a custom Eraser, to the expression: Python's "Failed to mask field
// at path".
func maskError(path string, err error) error {
	if errors.Is(err, ErrDataMasking) {
		return err
	}
	return &PathError{Path: path, Err: ErrMaskField, Cause: err}
}

// CryptoOption configures one Encrypt or Decrypt.
type CryptoOption func(*Request)

// WithEncryptionContext binds non-secret key-value pairs to the ciphertext
// (additional authenticated data). Decrypt must be given exactly the same
// pairs, or it fails with ErrContextMismatch. Never put secrets in it: AWS
// KMS logs it in CloudTrail.
func WithEncryptionContext(ec map[string]string) CryptoOption {
	return func(r *Request) {
		if r.EncryptionContext == nil {
			r.EncryptionContext = map[string]string{}
		}
		maps.Copy(r.EncryptionContext, ec)
	}
}

// WithProviderOptions passes per-call settings to the provider: the Go form
// of Python's provider_options.
func WithProviderOptions(opts ...ProviderOption) CryptoOption {
	return func(r *Request) { r.Options = append(r.Options, opts...) }
}

func request(opts []CryptoOption) Request {
	var r Request
	for _, opt := range opts {
		opt(&r)
	}
	return r
}

// Encrypt encrypts the JSON document of data with the provider and returns
// the provider's ciphertext (for the AWS Encryption SDK provider, a base64
// encrypted message). Data is a Go value that marshals to JSON, or raw JSON
// ([]byte, jsontext.Value) encrypted as the document it is; a string is a
// JSON string, as in Python.
func (m *DataMasking) Encrypt(ctx context.Context, data any, opts ...CryptoOption) (string, error) {
	if m.provider == nil {
		return "", ErrNoProvider
	}
	plaintext, err := m.document(data)
	if err != nil {
		return "", err
	}
	ciphertext, err := m.provider.Encrypt(ctx, plaintext, request(opts))
	if err != nil {
		return "", fmt.Errorf("datamasking: encrypt: %w", err)
	}
	return ciphertext, nil
}

// Decrypt decrypts what Encrypt returned and parses the JSON document into
// a decoded JSON value (nil, bool, int64, float64, string, []any,
// map[string]any).
func (m *DataMasking) Decrypt(ctx context.Context, ciphertext string, opts ...CryptoOption) (any, error) {
	plaintext, err := m.decrypt(ctx, ciphertext, opts)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(plaintext)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecryptValue, err)
	}
	return v, nil
}

// DecryptAs is Decrypt, with the document decoded into T; raw JSON targets
// ([]byte, jsontext.Value) receive the document itself.
func (m *DataMasking) DecryptAs[T any](ctx context.Context, ciphertext string, opts ...CryptoOption) (T, error) {
	plaintext, err := m.decrypt(ctx, ciphertext, opts)
	if err != nil {
		var zero T
		return zero, err
	}
	out, err := unmarshalInto[T](plaintext, m.jsonOptions)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrDecryptValue, err)
	}
	return out, nil
}

func (m *DataMasking) decrypt(ctx context.Context, ciphertext string, opts []CryptoOption) ([]byte, error) {
	if m.provider == nil {
		return nil, ErrNoProvider
	}
	plaintext, err := m.provider.Decrypt(ctx, ciphertext, request(opts))
	if err != nil {
		return nil, fmt.Errorf("datamasking: decrypt: %w", err)
	}
	return plaintext, nil
}

// document is the JSON the provider encrypts.
func (m *DataMasking) document(data any) ([]byte, error) {
	var raw []byte
	switch v := data.(type) {
	case jsontext.Value:
		raw = slices.Clone(v)
	case []byte:
		raw = slices.Clone(v)
	default:
		out, err := json.Marshal(data, m.jsonOptions...)
		if err != nil {
			return nil, fmt.Errorf("%w: %T: %w", ErrUnsupportedType, data, err)
		}
		return out, nil
	}
	value := jsontext.Value(raw)
	if !value.IsValid(jsontext.AllowDuplicateNames(true)) {
		return nil, fmt.Errorf("%w: invalid JSON", ErrUnsupportedType)
	}
	if err := value.Compact(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedType, err)
	}
	return value, nil
}
