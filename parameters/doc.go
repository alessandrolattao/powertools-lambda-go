// Package parameters reads configuration and secrets with a cache in front:
// the Go side of Powertools' Parameters utility.
//
// The providers live in the packages below this one, one per store:
//
//   - parameters/ssm: Systems Manager Parameter Store, including
//     SecureString decryption, reads by path and by name, and writes;
//   - parameters/secrets: Secrets Manager, single and batch reads, writes;
//   - parameters/appconfig: AppConfig configuration profiles, through
//     AppConfigData sessions;
//   - parameters/dynamodb: a DynamoDB table with Python's schema.
//
// This package holds what they share: the Provider and its cache, the
// transforms, the options of a read, the errors, and the Store interface for
// a provider of your own.
//
// # Reading a value
//
// A provider is created once (in main, or at package level) and shared by
// every invocation; it is safe for concurrent use. Its Get method returns a
// string:
//
//	var params = ssm.FromConfig(cfg)
//
//	func handle(ctx context.Context, event Event) (string, error) {
//		endpoint, err := params.Get(ctx, "/app/endpoint")
//		...
//	}
//
// Values that change shape are read with the generic Get and GetMultiple:
// the caller names the Go type and the transform decodes the value into it.
//
//	cfg, err := parameters.Get[Config](ctx, params, "/app/config",
//		parameters.WithTransform(parameters.TransformJSON))
//
// The transforms are Python's: TransformJSON decodes a JSON document into
// any T; TransformBinary decodes base64 into a []byte (or a string);
// TransformAuto picks one of those from the suffix of the name
// ("config.json", "cert.binary"); TransformNone, the default, returns the
// value as stored, in a string, a []byte or an any. A value read without a
// transform cannot become a struct or a number: that is a *TransformError
// telling to use TransformJSON.
//
// Each package below also has the shortcuts of Python's high-level
// functions, which use a provider built on first use from the default AWS
// configuration: ssm.GetParameter, ssm.GetParameters,
// ssm.GetParametersByName, ssm.SetParameter, secrets.GetSecret,
// secrets.GetSecretsByName, secrets.SetSecret, appconfig.GetAppConfig.
// ClearCaches forgets those default providers.
//
// # Caching
//
// Every value is cached for five minutes, or POWERTOOLS_PARAMETERS_MAX_AGE
// seconds, or what WithMaxAge says for one read; a max age of zero or less
// never caches. WithForceFetch reads the store even when the cache holds the
// value. The cache keeps the value after its transform, apart for each
// transform, as Python does, and apart for each store option that changes
// the value read (a decryption, a version), which Python does not.
// Concurrent reads of a value missing from the cache share one call to the
// store; a caller whose context ends stops waiting without failing the
// others. Every read decodes a fresh copy, so callers never share mutable
// values through the cache.
//
// # Your own store
//
// A Store reads values from a backend: Get returns one value, GetMultiple the
// values under a path. NewProvider puts the cache and the transforms in
// front of it. Options of your store travel with the common ones through
// WithStoreOption, and the store reads them with StoreOptions.Apply.
//
// # Environment variables
//
//   - POWERTOOLS_PARAMETERS_MAX_AGE: the default max age, in seconds (300).
//   - POWERTOOLS_PARAMETERS_SSM_DECRYPT: whether SSM reads decrypt
//     SecureString parameters by default (false).
//   - POWERTOOLS_SERVICE_NAME: the default AppConfig application.
//
// They are read on every call, as in Python; an invalid value is reported on
// standard error and the default applies.
//
// # Errors
//
// A value the store cannot provide is a *GetParameterError (ErrGetParameter),
// which wraps the store's error: errors.As reaches the SDK's own error, for
// example a *types.ParameterNotFound of SSM. A failed transform is a
// *TransformError (ErrTransform). Writes fail with a *SetParameterError or a
// *SetSecretError, a batch of secrets with a *GetSecretError. ErrNotFound
// marks the values a store without its own not-found error lacks.
//
// # Differences from Python
//
//   - The shortcuts live in the provider packages (ssm.GetParameter rather
//     than parameters.get_parameter): this package cannot import them, since
//     they build on it. Powertools for TypeScript has the same layout.
//   - Typed reads replace the str | bytes | dict results: Get[T] and
//     GetMultiple[T]. A value whose transform fails in a read of several
//     values is the zero T, where Python gives None.
//   - Python's **sdk_options become typed options: ssm.WithDecrypt,
//     ssm.WithRecursive, ssm.WithParameterFilters, secrets.WithVersionID,
//     secrets.WithVersionStage, secrets.WithFilters,
//     appconfig.WithMinimumPollInterval, dynamodb.WithConsistentRead, and
//     request editors for the writes.
//   - Fixed: Python's cache ignores the SDK options of a read, so a value
//     read with VersionStage=AWSPREVIOUS answered later reads of the current
//     version, and a decrypted read answered encrypted ones. Here the
//     options that change the value are part of the cache key.
//   - Fixed: a read with WithRaiseOnTransformError fails on a value whose
//     transform failed, even when an earlier tolerant read cached it, where
//     Python returns the cached None.
//   - Base64 decoding skips characters outside the alphabet, as Python's
//     b64decode does; what follows the padding is ignored.
//   - JSON is read as Python's json.loads reads it, repeated object members
//     included (the last one wins), but NaN and Infinity are refused.
//   - An invalid POWERTOOLS_PARAMETERS_MAX_AGE falls back to the default
//     instead of raising ValueError.
//   - The botocore user agent registration is not ported.
package parameters
