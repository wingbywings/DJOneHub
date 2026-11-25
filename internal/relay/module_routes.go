package relay

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/relay/identity"
)

type moduleRouteSnapshot struct {
	routes map[string]callcontrol.ModuleRouteEntry
	at     time.Time
}

type moduleRouteCatalog struct {
	mu   sync.Mutex
	hubs map[string]moduleRouteSnapshot
}

func (c *moduleRouteCatalog) routes(mux *http.ServeMux, store *identity.Store) {
	mux.HandleFunc("PUT /v1/module-routes", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.Authenticate(r.Context(), identity.Bearer(r.Header.Get("Authorization")))
		if err == nil && p.Kind != "hub" {
			err = identity.ErrUnauthorized
		}
		if err != nil {
			result(w, 200, nil, err)
			return
		}
		var body struct {
			Modules []callcontrol.ModuleRouteEntry `json:"modules"`
		}
		if !decodeLimit(w, r, &body, 65536) {
			return
		}
		if len(body.Modules) != len(p.Modules) {
			result(w, 200, nil, identity.ErrInvalid)
			return
		}
		routes := make(map[string]callcontrol.ModuleRouteEntry, len(body.Modules))
		for _, m := range body.Modules {
			m.ID = strings.TrimSpace(m.ID)
			if !slices.Contains(p.Modules, m.ID) || !validModuleRoute(m) {
				result(w, 200, nil, identity.ErrInvalid)
				return
			}
			if _, exists := routes[m.ID]; exists {
				result(w, 200, nil, identity.ErrInvalid)
				return
			}
			routes[m.ID] = m
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
		c.hubs[p.ID] = moduleRouteSnapshot{routes, time.Now()}
		c.mu.Unlock()
		result(w, 200, map[string]bool{"updated": true}, nil)
	})
	mux.HandleFunc("POST /v1/module-routes", func(w http.ResponseWriter, r *http.Request) {
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
		modules := make([]callcontrol.ModuleRouteEntry, 0, len(p.Modules))
		for _, id := range p.Modules {
			entry := callcontrol.ModuleRouteEntry{ID: id}
			if available {
				if published, found := value.routes[id]; found {
					entry = published
					entry.ID = id
				}
			}
			modules = append(modules, entry)
		}
		c.mu.Unlock()
		result(w, 200, map[string]any{"hub_id": p.HubID, "available": available, "modules": modules}, nil)
	})
}

func validModuleRoute(m callcontrol.ModuleRouteEntry) bool {
	if m.ID == "" || len(m.ID) > 128 {
		return false
	}
	if !m.Known {
		return m.EffectiveRoute == "" && m.TargetInstallationID == "" && !m.AIEnabled && !m.RemoteEnabled
	}
	switch callcontrol.Route(m.EffectiveRoute) {
	case callcontrol.Agent, callcontrol.Remote, callcontrol.Local:
	default:
		return false
	}
	target := strings.TrimSpace(m.TargetInstallationID)
	if len(target) > 128 {
		return false
	}
	for _, ch := range target {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}
	if m.EffectiveRoute == string(callcontrol.Remote) && m.RemoteEnabled && target == "" {
		return false
	}
	if m.EffectiveRoute == string(callcontrol.Agent) && !m.AIEnabled {
		return false
	}
	return true
}
