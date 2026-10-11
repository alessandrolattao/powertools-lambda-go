// Package disabled tells whether Powertools metrics are switched off, with
// the rule Powertools Python applies to every metrics provider.
package disabled

import "github.com/alessandrolattao/powertools-lambda-go/internal/env"

// Is reports whether metrics are off: POWERTOOLS_METRICS_DISABLED is true,
// or POWERTOOLS_DEV is on and POWERTOOLS_METRICS_DISABLED is unset. Setting
// POWERTOOLS_METRICS_DISABLED to false keeps metrics on in development mode.
func Is() bool {
	if env.Get(env.MetricsDisabled) != "" {
		return env.Bool(env.MetricsDisabled, false)
	}
	return env.IsDev()
}
