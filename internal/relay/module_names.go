package relay

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iniwex5/vohive/internal/relay/identity"
)

type moduleNameSnapshot struct {
	names map[string]string
	at    time.Time
}
type moduleNameCatalog struct {
	mu   sync.Mutex
	hubs map[string]moduleNameSnapshot
}

func (c *moduleNameCatalog) routes(mux *http.ServeMux, store *identity.Store) {
	mux.HandleFunc("PUT /v1/module-names", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.Authenticate(r.Context(), identity.Bearer(r.Header.Get("Authorization")))
		if err == nil && p.Kind != "hub" {
			err = identity.ErrUnauthorized
		}
		if err != nil {
			result(w, 200, nil, err)
			return
		}
		var body struct {
			Modules []identity.ModuleChoice `json:"modules"`
		}
		if !decodeLimit(w, r, &body, 32768) {
			return
		}
		if len(body.Modules) != len(p.Modules) {
			result(w, 200, nil, identity.ErrInvalid)
			return
		}
		names := make(map[string]string, len(body.Modules))
		for _, m := range body.Modules {
			if !slices.Contains(p.Modules, m.ID) || !validModuleAlias(m.Alias) {
				result(w, 200, nil, identity.ErrInvalid)
				return
			}
			if _, exists := names[m.ID]; exists {
				result(w, 200, nil, identity.ErrInvalid)
				return
			}
			names[m.ID] = strings.TrimSpace(m.Alias)
		}
		c.mu.Lock()
		for id, value := range c.hubs {
			if time.Since(value.at) > 2*time.Minute {
				delete(c.hubs, id)
			}
		}
		if _, exists := c.hubs[p.ID]; !exists && len(c.hubs) >= 1024 {
			c.mu.Unlock()
			result(w, 200, nil, identity.ErrCapacity)
			return
		}
		c.hubs[p.ID] = moduleNameSnapshot{names, time.Now()}
		c.mu.Unlock()
		result(w, 200, map[string]bool{"updated": true}, nil)
	})
	mux.HandleFunc("POST /v1/module-names", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decode(w, r, &body) {
			return
		}
		p, err := store.Authenticate(r.Context(), identity.Bearer(r.Header.Get("Authorization")))
		if err != nil {
			result(w, 200, nil, err)
			return
		}
		c.mu.Lock()
		value, ok := c.hubs[p.HubID]
		available := ok && time.Since(value.at) <= 2*time.Minute
		modules := make([]identity.ModuleChoice, 0, len(p.Modules))
		for _, id := range p.Modules {
			name := ""
			if available {
				name = value.names[id]
			}
			modules = append(modules, identity.ModuleChoice{ID: id, Alias: name})
		}
		c.mu.Unlock()
		result(w, 200, map[string]any{"hub_id": p.HubID, "available": available, "modules": modules}, nil)
	})
}

func validModuleAlias(value string) bool {
	return utf8.ValidString(value) && len(value) <= 320 && utf8.RuneCountInString(value) <= 80 && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}
