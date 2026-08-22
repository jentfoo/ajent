package llm

import (
	"fmt"
	"sync"
)

// Providers caches one Provider per endpoint so /model switching never rebuilds
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

// ProviderFor returns the cached provider serving m, building it once.
func (c *Providers) ProviderFor(m Model) (Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// two models on one provider may differ in dialect or endpoint, so the vendor
	// name alone would collapse them onto whichever adapter was built first
	key := m.Provider + "\x00" + m.Caps.Dialect.String() + "\x00" + m.BaseURL
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
