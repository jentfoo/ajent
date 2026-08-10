package llm

import (
	"fmt"
	"sync"
)

// Providers caches one Provider per vendor so /model switching never rebuilds
// an adapter. It keeps the registry out of front ends' reach.
type Providers struct {
	reg *Registry

	mu sync.Mutex
	m  map[string]Provider
}

// NewProviders returns a provider cache backed by reg.
func NewProviders(reg *Registry) *Providers {
	return &Providers{reg: reg, m: make(map[string]Provider)}
}

// ProviderFor returns the cached provider for m's vendor, building it once.
func (c *Providers) ProviderFor(m Model) (Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if p, ok := c.m[m.Provider]; ok {
		return p, nil
	}
	cfg, flavor, ok := c.reg.ProviderConfigFor(m)
	if !ok {
		return nil, fmt.Errorf("no provider configured for %s", m.Key())
	}
	p, err := NewProvider(m.Provider, cfg, flavor, ProviderOptions{})
	if err != nil {
		return nil, err
	}
	c.m[m.Provider] = p
	return p, nil
}
