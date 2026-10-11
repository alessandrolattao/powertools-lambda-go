package core

import "sync"

// The registry of default providers: the providers the package-level
// shortcuts (ssm.GetParameter, secrets.GetSecret, appconfig.GetAppConfig)
// build on first use. Each provider package registers how to forget its
// own, so that parameters.ClearCaches can reach them all without importing
// them.
var (
	defaultsMu sync.Mutex
	resetters  []func()
)

// RegisterDefaults adds reset to what ResetDefaults calls.
func RegisterDefaults(reset func()) {
	defaultsMu.Lock()
	defer defaultsMu.Unlock()
	resetters = append(resetters, reset)
}

// ResetDefaults forgets every default provider, so that the next shortcut
// builds a new one, with a new SDK client and an empty cache.
func ResetDefaults() {
	defaultsMu.Lock()
	defer defaultsMu.Unlock()
	for _, reset := range resetters {
		reset()
	}
}

// Defaults holds the default provider of a package, built on first use.
// Building it can fail (no AWS region, for one); a failure is not kept, the
// next use tries again.
type Defaults[K comparable, P any] struct {
	mu        sync.Mutex
	providers map[K]P
}

// Get returns the provider kept under key, building it first if needed.
func (d *Defaults[K, P]) Get(key K, build func() (P, error)) (P, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.providers[key]; ok {
		return p, nil
	}
	p, err := build()
	if err != nil {
		return p, err
	}
	if d.providers == nil {
		d.providers = make(map[K]P)
	}
	d.providers[key] = p
	return p, nil
}

// Reset forgets every provider.
func (d *Defaults[K, P]) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.providers)
}
