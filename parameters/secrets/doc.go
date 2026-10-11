// Package secrets reads and writes AWS Secrets Manager secrets, with the
// cache and the transforms of package parameters.
//
// Create a Provider once and share it:
//
//	var vault = secrets.FromConfig(cfg) // or secrets.New(client)
//
//	key, err := vault.Get(ctx, "api-key")
//	creds, err := parameters.Get[Credentials](ctx, vault, "db-credentials",
//		parameters.WithTransform(parameters.TransformJSON))
//	previous, err := vault.Get(ctx, "db-password", secrets.WithVersionStage("AWSPREVIOUS"))
//	values, failed, err := vault.GetByName(ctx, []string{"api-key", "db-password"})
//
// or use the shortcuts, which build a provider from the default AWS
// configuration on first use: GetSecret, GetSecretsByName, SetSecret.
//
// Get calls GetSecretValue and returns SecretString, or the bytes of
// SecretBinary. GetByName calls BatchGetSecretValue. Set adds a version to a
// secret with PutSecretValue and creates the secret with CreateSecret when it
// does not exist yet; a []byte value is a SecretBinary, a string a
// SecretString, any other value its JSON encoding.
//
// # Differences from Python
//
//   - Fixed in GetByName: Python finds the secrets through a name filter,
//     which matches by prefix ("db" also returns "db-password") and takes ten
//     names at most. Here the secrets are asked for by their exact name or
//     ARN (SecretIdList), twenty per call and as many calls as needed; with
//     WithFilters the name filter is used, ten names per call, and only exact
//     matches are kept.
//   - Fixed in GetByName: the secrets Secrets Manager could not read
//     (missing, access denied, decryption failure) are returned with their
//     error instead of being dropped silently. The read still succeeds with
//     the others, as in Python, and fails only when none could be read.
//   - Version selectors (WithVersionID, WithVersionStage) are part of the
//     cache key, so a previous version never answers for the current one.
//   - Set accepts any JSON value, not only a dict, and returns the ARN, the
//     name and the version id of either answer.
package secrets
