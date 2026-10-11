// Package datamasking erases, encrypts and decrypts sensitive data: the Go
// port of Powertools for AWS Lambda (Python) data masking utility.
//
// Erasing replaces values irreversibly with "*****" (Mask) or with a mask
// of your own; it needs nothing but memory. Encrypting turns the whole data
// into a ciphertext through a Provider; package datamasking/kms is the AWS
// Encryption SDK provider, with AWS KMS keys, that Powertools uses, and its
// messages decrypt in Powertools Python and the other AWS Encryption SDK
// implementations, and the other way round.
//
// # Erasing
//
// Build one DataMasking and share it:
//
//	var masker = datamasking.New()
//
//	erased, err := masker.Erase(event.Body, datamasking.WithFields("email", "address.street"))
//
// Erase accepts raw JSON ([]byte, jsontext.Value), any Go value that
// marshals to JSON with encoding/json/v2 (structs through their JSON field
// names, maps, aws-lambda-go events) and scalars. With field selection or
// masking rules a string is a JSON document, as in Python. The caller's data
// is never modified. The result is a decoded JSON value (nil, bool, int64,
// float64, string, []any, map[string]any); EraseAs decodes it into a type
// of your choice instead, raw JSON included.
//
// What gets erased:
//
//   - without options, the whole data becomes Mask; an array becomes as
//     many Mask strings as it has elements;
//   - WithFields selects values with JSONPath expressions, applied in
//     order: each selected value is erased the same way (an object becomes
//     Mask, an array a list of Mask), the rest of the data stays;
//   - WithDynamicMask, WithCustomMask and WithRegex mask instead of erasing:
//     objects and arrays keep their shape, every scalar inside is masked as
//     text (see Rule);
//   - WithMaskingRules gives each expression masks of its own, in order,
//     and takes precedence over the other options.
//
// Scalars are masked as the text Python's str() writes, so that masks have
// the same length as in Python: 30 is "30", 4.2 is "4.2", 1e20 is "1e+20",
// true is "True". JSON integers stay int64 and decimals float64, as Python
// keeps int and float apart. With WithMaskingRules every selected value is
// masked as text, objects and arrays as their Python repr ("['Pat',
// 'powertools']"), and null values are left alone.
//
// An expression that matches nothing fails with ErrFieldNotFound, unless
// WithRaiseOnMissingField(false) is set: then it is skipped, and reported on
// the Powertools debug log (POWERTOOLS_DEBUG=true).
//
// # Field expressions
//
// Powertools Python parses expressions with jsonpath-ng's extended parser;
// ojg (github.com/ohler55/ojg) evaluates them here. No Go library speaks
// jsonpath-ng's dialect, so the package parses the part Powertools documents
// and tests, and rejects everything else with ErrInvalidPath rather than
// guess. Accepted:
//
//   - an optional root: card_number and $.card_number are the same; $ alone
//     selects the whole data;
//   - members: name, a.b, names with letters, digits, '_', '@' and '-'
//     (non-existing), quoted names ('1', "a b", with \ escaping the next
//     character), numbers used as names (a.1 is the member "1"), lists of
//     names (a.b,c and ['b','c']);
//   - * and [*] for every member of an object or element of an array;
//   - descendants: category..name, a..'4', a..[0];
//   - indexes and slices: [1], [-1], [0,2], [1:3], [-2:], [::2];
//   - filters on the elements of an array (or the members of an object):
//     [?(@.postcode > 12000)], [?postcode > 12000], [?(@.kind)] (present),
//     [?(!@.kind)] (absent), conditions joined with & and grouped with
//     parentheses; the operators ==, = (the same), !=, <, <=, >, >= and =~
//     (a regular expression, searched anywhere in the text); the values are
//     integers, decimals, true, false, quoted strings and bare words (home
//     is the string "home", as in jsonpath-ng). Inside a filter, @ is the
//     element, and the path after it may use the forms above.
//
// Rejected, because jsonpath-ng extensions have no ojg equivalent: | and
// & between paths, where and wherenot, `this`, `len` and the other named
// operators, arithmetic, sorting, || and && in filters, $ after the start,
// parenthesized paths.
//
// Where both accept an expression, they can still disagree on data of an
// unexpected shape: jsonpath-ng turns a string into an integer to compare
// it with an integer ("12345" > 12000 holds there, not here) and raises an
// error comparing incompatible types, where ojg simply does not match;
// jsonpath-ng's .* skips arrays, and its [*] treats an object as a list of
// one element.
//
// # Masks
//
// See Rule. Regular expressions use Go's RE2 syntax, which has no
// lookarounds and no backreferences; named groups work as (?P<name>...).
// The mask format follows Python's re.sub: \1 to \99, \g<1> and \g<name>
// refer to groups, \0 and three-digit octal escapes are characters, \n and
// the other C escapes are themselves, and $ has no special meaning. Empty
// matches follow Go's rules, which skip an empty match right after a
// non-empty one where Python does not. Patterns are compiled once per
// process.
//
// # Encrypting
//
//	provider, err := kms.NewFromConfig(cfg, []string{keyARN})
//	masker := datamasking.New(datamasking.WithProvider(provider))
//
//	ciphertext, err := masker.Encrypt(ctx, order, datamasking.WithEncryptionContext(map[string]string{
//		"tenant_id": tenantID,
//	}))
//	order, err := masker.DecryptAs[Order](ctx, ciphertext, datamasking.WithEncryptionContext(...))
//
// Encrypt marshals the data to JSON (raw JSON is encrypted as the document
// it is, a string as a JSON string) and returns the provider's ciphertext.
// Decrypt parses the decrypted JSON; DecryptAs decodes it into T. The
// encryption context must be exactly the same on both sides, or decryption
// fails with ErrContextMismatch. WithProviderOptions passes per-call
// settings to the provider, as Python's provider_options.
//
// A Provider deals with bytes; one that also implements Eraser masks values
// its own way, and can fall back to MaskValue.
//
// # Options
//
// New takes WithProvider, WithRaiseOnMissingField (default true) and
// WithJSONOptions, the encoding/json/v2 options for marshalling Go values
// and decoding results (Python's json_serializer and json_deserializer).
// The package reads no environment variable of its own; POWERTOOLS_DEBUG
// turns on the debug log.
//
// # Errors
//
// Every error the package detects matches ErrDataMasking, and one of
// ErrUnsupportedType, ErrEmptyFields, ErrFieldNotFound, ErrInvalidPath,
// ErrMaskField, ErrInvalidRegex, ErrNoProvider, ErrEncryptKey,
// ErrDecryptKey, ErrDecryptValue and ErrContextMismatch. A *PathError names
// the expression that failed.
//
// # Differences from Python
//
//   - Raw JSON ([]byte, jsontext.Value) is the document it holds, never
//     bytes to erase; Go values are seen through encoding/json/v2, not
//     dataclasses, Pydantic or __dict__.
//   - Masking rules are an ordered slice, not a dict, and accept a JSON
//     string as fields do (Python searches the string itself and finds
//     nothing).
//   - $ selects the whole data; in jsonpath-ng it matches but updates
//     nothing.
//   - Field selection needs an object or an array; Python accepts a top
//     level array only inside a JSON string.
//   - The Python repr of an object lists its keys in sorted order, Python
//     in the document's; masks of the same object can differ in where the
//     kept characters fall, not in length.
//   - Regular expressions are compiled when Erase starts, so an invalid one
//     fails even when nothing matches.
//   - Errors of a custom Eraser are tied to the expression with ErrMaskField
//     for fields too, not only for masking rules.
//   - Python raises NotImplementedError when encrypting without a provider;
//     here it is ErrNoProvider. Python's ValueError for empty fields is
//     ErrEmptyFields.
//   - Encryption context values are strings by type, so Python's check that
//     they are is not needed.
//   - Go reads integers beyond int64 as float64, Python keeps them exact.
package datamasking
