// Package appconfig reads AWS AppConfig configuration profiles through
// AppConfigData sessions, with the cache and the transforms of package
// parameters.
//
// A Provider reads the profiles of one application and environment; the name
// of a read is the configuration profile, by name or id:
//
//	var appConfig = appconfig.FromConfig(cfg, "my-app", "prod") // or appconfig.New(client, ...)
//
//	cfg, err := parameters.Get[Config](ctx, appConfig, "my-profile",
//		parameters.WithTransform(parameters.TransformJSON))
//
// The GetAppConfig shortcut builds a provider from the default AWS
// configuration on first use, one per application and environment. An empty
// application means POWERTOOLS_SERVICE_NAME ("service_undefined" when
// unset).
//
// The first read of a profile starts its configuration session
// (StartConfigurationSession, then GetLatestConfiguration: two calls, as
// AppConfig requires); later reads poll the session with the token the
// previous poll returned. AppConfigData answers an empty configuration when
// nothing changed: the last configuration is returned again. The values are
// cached like any other (five minutes by default), and AppConfig is never
// polled sooner than the NextPollIntervalInSeconds it asked for, even with
// parameters.WithForceFetch. The polls of a profile are serialized, since a
// token is valid once.
//
// # Differences from Python
//
//   - Fixed: Python's get_app_config keeps the provider of the first
//     application and environment it was called with, and serves every
//     later call from it. GetAppConfig keeps one provider per application and
//     environment.
//   - Fixed: Python never restarts a session, so a token that expired (they
//     live 24 hours) or that a failed call consumed fails every later read.
//     A token AppConfig refuses (BadRequestException) starts a new session,
//     once.
//   - Fixed: NextPollIntervalInSeconds is honored; Python polls as often as
//     its cache expires, which AppConfig refuses below the session's minimum.
//   - A first answer that is empty is an empty configuration, where Python
//     raises KeyError.
package appconfig
