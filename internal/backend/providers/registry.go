// Package providers contains the replacement backend's cohesive harness
// providers and their single registry. Application code selects by name and
// never imports vendor packages.
package providers

import "github.com/tofutools/tclaude/internal/backend/ports"

type Registry struct {
	byName map[string]ports.Provider
}

func NewRegistry(entries ...ports.Provider) Registry {
	registry := Registry{byName: make(map[string]ports.Provider, len(entries))}
	for _, entry := range entries {
		if entry == nil || entry.Name() == "" {
			continue
		}
		if _, exists := registry.byName[entry.Name()]; exists {
			panic("duplicate provider registration: " + entry.Name())
		}
		registry.byName[entry.Name()] = entry
	}
	return registry
}

func (r Registry) Provider(name string) (ports.Provider, bool) {
	provider, ok := r.byName[name]
	return provider, ok
}
