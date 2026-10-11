// Package ssm reads and writes AWS Systems Manager Parameter Store
// parameters, with the cache and the transforms of package parameters.
//
// Create a Provider once and share it:
//
//	var params = ssm.FromConfig(cfg) // or ssm.New(client)
//
//	host, err := params.Get(ctx, "/app/db/host")
//	password, err := params.Get(ctx, "/app/db/password", ssm.WithDecrypt(true))
//	all, err := params.GetMultiple(ctx, "/app", ssm.WithRecursive(true))
//	cfg, err := parameters.Get[Config](ctx, params, "/app/config",
//		parameters.WithTransform(parameters.TransformJSON))
//
// or use the shortcuts, which build a provider from the default AWS
// configuration on first use: GetParameter, GetParameters,
// GetParametersByName, SetParameter.
//
// # Reads
//
// Get calls GetParameter. GetMultiple calls GetParametersByPath, every page,
// and returns the names relative to the path ("/app/db/host" under "/app"
// is "db/host"). As in Python, Provider.GetMultiple reads only the direct
// children of the path unless WithRecursive(true) is given, while the
// GetParameters shortcut is recursive unless WithRecursive(false) is given.
//
// GetByName reads parameters by their full name, each with its own options
// (transform, decryption, max age), with Python's strategy: the parameters
// that need no decryption go to GetParameters, ten per call; those that need
// it are read one by one, unless all of them do. Every value is cached the
// way Get caches it, so the two share the cache. A failure fails the read,
// or with WithRaiseOnError(false) is returned apart, by name, with the
// values that could be read.
//
// Set writes a parameter with PutParameter: a string as a String, a []string
// as a StringList, WithParameterType for a SecureString; WithOverwrite to
// replace an existing one.
//
// POWERTOOLS_PARAMETERS_SSM_DECRYPT sets the default of WithDecrypt (false).
//
// # Differences from Python
//
//   - GetByName returns the failures apart instead of the reserved "_errors"
//     key, so no parameter name is reserved.
//   - Fixed in GetByName: a parameter whose transform fails is a failure,
//     never a cached None; an unusable KMS key or an SSM failure (InvalidKeyId,
//     InternalServerError) fails the read when it must, instead of dropping
//     the parameters silently; other errors of a call (throttling, access
//     denied) are a *parameters.GetParameterError instead of the raw SDK
//     error; names with a version or label selector, or ARNs, are matched to
//     the parameters SSM answers under their plain name (Python raises
//     KeyError); and a parameter shares the cache entry Get makes for it.
//   - Set accepts a []string for a StringList (Python's annotation promises
//     it, boto3 refuses it) and returns the version and the tier.
//   - Go maps have no order: GetByName reads the names in sorted order.
package ssm
