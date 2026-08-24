package voiceagent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry() *Registry { return &Registry{providers: make(map[string]Provider)} }

func (r *Registry) Register(provider Provider) error {
	if provider == nil || strings.TrimSpace(provider.Name()) == "" {
		return fmt.Errorf("voice provider name is required")
	}
	name := strings.ToLower(strings.TrimSpace(provider.Name()))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("voice provider %q is already registered", name)
	}
	r.providers[name] = provider
	return nil
}

func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	provider, ok := r.providers[strings.ToLower(strings.TrimSpace(name))]
	r.mu.RUnlock()
	return provider, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}
