package llm

import (
	"fmt"
	"sync"
)

// Providers caches one Provider per endpoint and config era, so /model
// switching never rebuilds an adapter while an edited models.json does. Era-keyed
// entries are never evicted; each edit strands only its adapters. It keeps the
// registry out of front ends' reach.
type Providers struct {
	reg *Registry

	mu sync.Mutex
	m  map[string]Provider
}

// NewProviders returns a provider cache backed by reg.
func NewProviders(reg *Registry) *Providers {
	return &Providers{reg: reg, m: make(map[string]Provider)}
}

// ProviderFor returns the cached provider serving m, building it once.
func (c *Providers) ProviderFor(m Model) (Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// dialect and baseURL split adapters within a provider; the era rebuilds one
	// after an edited models.json, e.g. a rotated credential
	key := m.Provider + "\x00" + m.Caps.Dialect.String() + "\x00" + m.BaseURL + "\x00" +
		c.reg.providerEra(m.Provider)
	if p, ok := c.m[key]; ok {
		return p, nil
	}
	cfg, flavor, ok := c.reg.ProviderConfigFor(m)
	if !ok {
		return nil, fmt.Errorf("no provider configured for %s", m.Key())
	}
	p, err := NewProvider(m.Provider, cfg, flavor, ProviderOptions{Dialect: m.Caps.Dialect, BaseURL: m.BaseURL})
	if err != nil {
		return nil, err
	}
	c.m[key] = p
	return p, nil
}
