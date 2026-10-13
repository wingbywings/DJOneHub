package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/desktopopen"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayprofile"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"time"
)

const remoteWebAPI = "/api/remote-management"

// Keep legacy origin-only values while permitting the two supported UI paths.
// No arbitrary redirect target, userinfo, query, fragment or encoded path.
func remoteAdminAddress(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/admin" && u.Path != "/admin/" && u.Path != "/relay-admin" && u.Path != "/relay-admin/" {
		return false
	}
	u.Path = ""
	return identity.HTTPSOrigin(u.String())
}

func webRead(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "invalid_management_request"})
		return false
	}
	return true
}
func (m *remoteWebManager) start(w http.ResponseWriter, kind, profile string, run func(context.Context, *relayprofile.Store) (any, error)) {
	j, err := m.job(kind, profile, run)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": remoteWebError(err)})
		return
	}
	writeJSON(w, 202, j)
}
func (m *remoteWebManager) register(mux *http.ServeMux) {
	sessions := http.NewServeMux()
	m.auth.Register(sessions, remoteWebAPI+"/session")
	mux.Handle(remoteWebAPI+"/session/", m.auth.Guard(sessions))
	require := func(method, path string, handler http.HandlerFunc) {
		mux.Handle(method+" "+path, m.auth.Guard(m.auth.Require(handler)))
	}
	require("GET", remoteWebAPI+"/profiles", func(w http.ResponseWriter, r *http.Request) {
		items, err := m.catalog.list(r.Context())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "management_storage_failed"})
			return
		}
		startup, err := m.catalog.autostart(r.Context())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "management_storage_failed"})
			return
		}
		state := "disabled"
		m.hub.mu.RLock()
		if manager := m.hub.remoteHub; manager != nil {
			state = manager.Snapshot().State
		}
		m.hub.mu.RUnlock()
		var pending *remoteWebJob
		m.mu.Lock()
		for _, j := range m.jobs {
			if j.State == "running" {
				copy := *j
				copy.Result = nil
				pending = &copy
				break
			}
		}
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{"profiles": items, "active_profile": m.activeProfile(), "autostart_profile": startup, "connection_state": state, "modules": m.hub.summaries(), "pending_job": pending, "native_control_configured": m.nativeFile != ""})
	})
	require("GET", remoteWebAPI+"/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := m.jobState(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "job_not_found"})
			return
		}
		if delivery, ok := j.Result.(pairingDelivery); ok && !delivery.Grant.ExpiresAt.After(time.Now()) {
			j.Result = map[string]string{"state": "expired"}
		}
		writeJSON(w, 200, j)
	})
	require("GET", remoteWebAPI+"/profiles/{id}/request", func(w http.ResponseWriter, r *http.Request) {
		entry, err := m.catalog.get(r.Context(), r.PathValue("id"))
		if err != nil || entry.Request == nil {
			writeJSON(w, 404, map[string]string{"error": "profile_request_not_found"})
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="djonehub-registration-request.json"`)
		writeJSON(w, 200, entry.Request)
	})
	require("GET", remoteWebAPI+"/profiles/{id}/presence", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, m.presenceSnapshot(r.PathValue("id")))
	})
	require("POST", remoteWebAPI+"/enrollment-requests", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name     string   `json:"name"`
			RelayURL string   `json:"relay_url"`
			Modules  []string `json:"modules"`
			AdminURL string   `json:"admin_url,omitempty"`
		}
		if !webRead(w, r, &body) {
			return
		}
		if body.AdminURL != "" && !remoteAdminAddress(body.AdminURL) {
			writeJSON(w, 400, map[string]string{"error": "invalid_management_state"})
			return
		}
		m.start(w, "create_request", "", func(ctx context.Context, store *relayprofile.Store) (any, error) {
			result, err := m.create(ctx, store, body.Name, body.RelayURL, body.Modules)
			if err != nil {
				return nil, err
			}
			entry := result.(remoteProfileEntry)
			entry.AdminURL = body.AdminURL
			return entry, m.catalog.save(ctx, entry)
		})
	})
	require("POST", remoteWebAPI+"/profiles/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile string `json:"profile"`
			Name    string `json:"name"`
		}
		if !webRead(w, r, &body) {
			return
		}
		m.start(w, "import_legacy", body.Profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			return m.importLegacy(ctx, store, body.Profile, body.Name)
		})
	})
	require("POST", remoteWebAPI+"/profiles/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action        string                            `json:"action"`
			Name          string                            `json:"name,omitempty"`
			Authorization *identity.EnrollmentAuthorization `json:"authorization,omitempty"`
			AdminURL      string                            `json:"admin_url,omitempty"`
		}
		if !webRead(w, r, &body) {
			return
		}
		id := r.PathValue("id")
		m.start(w, body.Action, id, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			entry, err := m.catalog.get(ctx, id)
			if err != nil {
				return nil, err
			}
			switch body.Action {
			case "admin_url":
				if body.AdminURL != "" && !remoteAdminAddress(body.AdminURL) {
					return nil, relayprofile.ErrInvalid
				}
				entry.AdminURL = body.AdminURL
				return entry, m.catalog.save(ctx, entry)
			case "refresh":
				return m.refresh(ctx, store, id)
			case "rename":
				if body.Name == "" || !webLabel(body.Name) {
					return nil, relayprofile.ErrInvalid
				}
				entry.Label = body.Name
				return entry, m.catalog.save(ctx, entry)
			case "submit":
				if entry.Request == nil {
					return nil, relayprofile.ErrInvalid
				}
				ids := []string{}
				for _, module := range entry.Request.Modules {
					ids = append(ids, module.ID)
				}
				if _, err = m.choices(ids, true, nil); err != nil {
					return nil, err
				}
				return store.SubmitEnrollment(ctx, id)
			case "enroll":
				if body.Authorization == nil {
					return nil, relayprofile.ErrInvalid
				}
				err = store.CompleteEnrollment(ctx, id, *body.Authorization)
				updated, e := m.refresh(context.WithoutCancel(ctx), store, id)
				if err != nil {
					return updated, err
				}
				return updated, e
			case "remove":
				if id == m.activeProfile() {
					return nil, errRemoteCallBusy
				}
				m.hub.mu.RLock()
				inCall := m.hub.remoteHub != nil && m.hub.remoteHub.Snapshot().State == "busy"
				m.hub.mu.RUnlock()
				if inCall {
					return nil, errRemoteCallBusy
				}
				err = store.Remove(ctx, id)
				if err != nil && !errors.Is(err, relayprofile.ErrNotFound) {
					return nil, err
				}
				return map[string]bool{"removed": true}, m.catalog.remove(ctx, id)
			default:
				return nil, relayprofile.ErrInvalid
			}
		})
	})
	require("POST", remoteWebAPI+"/connection", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile   string `json:"profile"`
			AutoStart bool   `json:"auto_start"`
		}
		if !webRead(w, r, &body) {
			return
		}
		m.start(w, "connection", body.Profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			return m.changeConnection(ctx, store, body.Profile, body.AutoStart)
		})
	})
	require("POST", remoteWebAPI+"/pairings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile string   `json:"profile"`
			Name    string   `json:"name"`
			Modules []string `json:"modules"`
		}
		if !webRead(w, r, &body) {
			return
		}
		m.start(w, "create_pairing", body.Profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			return m.pair(ctx, store, body.Profile, body.Name, body.Modules)
		})
	})
	require("POST", remoteWebAPI+"/pairings/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile string `json:"profile"`
			Action  string `json:"action"`
		}
		if !webRead(w, r, &body) {
			return
		}
		id := r.PathValue("id")
		m.start(w, "pairing_"+body.Action, body.Profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			if _, err := m.catalog.get(ctx, body.Profile); err != nil {
				return nil, err
			}
			if body.Action == "status" {
				status, err := store.PairingStatus(ctx, body.Profile, id)
				if err == nil && status.State != "pending" {
					m.erasePairingSecret(id)
				}
				return status, err
			}
			if body.Action == "revoke" {
				err := store.RevokePairing(ctx, body.Profile, id)
				return map[string]bool{"revoked": err == nil}, err
			}
			return nil, relayprofile.ErrInvalid
		})
	})
	require("POST", remoteWebAPI+"/pairings/query", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile string `json:"profile"`
		}
		if !webRead(w, r, &body) {
			return
		}
		m.start(w, "list_pairings", body.Profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			if _, err := m.catalog.get(ctx, body.Profile); err != nil {
				return nil, err
			}
			items, err := store.ManagedPairings(ctx, body.Profile)
			return map[string]any{"pairings": items}, err
		})
	})
	require("POST", remoteWebAPI+"/installations", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile string `json:"profile"`
			Action  string `json:"action"`
			ID      string `json:"id,omitempty"`
			Name    string `json:"name,omitempty"`
		}
		if !webRead(w, r, &body) {
			return
		}
		m.start(w, "installation_"+body.Action, body.Profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
			if body.Action == "list" {
				return m.installations(ctx, store, body.Profile)
			}
			if _, err := m.catalog.get(ctx, body.Profile); err != nil {
				return nil, err
			}
			if body.Action == "rename" {
				if !webLabel(body.Name) {
					return nil, relayprofile.ErrInvalid
				}
				err := store.NameManaged(ctx, body.Profile, "installations", body.ID, body.Name)
				return map[string]bool{"updated": err == nil}, err
			}
			if body.Action == "revoke" {
				err := store.RevokeInstallation(ctx, body.Profile, body.ID)
				return map[string]bool{"revoked": err == nil}, err
			}
			if body.Action == "preview" {
				items, err := store.ManagedInstallations(ctx, body.Profile)
				if err != nil {
					return nil, err
				}
				valid := false
				for _, item := range items {
					if item.ID == body.ID {
						valid = true
					}
				}
				if !valid {
					return nil, errRemoteScope
				}
				return m.references(body.ID), nil
			}
			return nil, relayprofile.ErrInvalid
		})
	})
	require("POST", remoteWebAPI+"/routes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Module string                   `json:"module"`
			Config callcontrol.RemoteConfig `json:"config"`
		}
		if !webRead(w, r, &body) {
			return
		}
		m.start(w, "save_route", m.activeProfile(), func(ctx context.Context, store *relayprofile.Store) (any, error) {
			return m.saveRoute(ctx, store, body.Module, body.Config)
		})
	})
	require("PUT", "/api/devices/{deviceID}/remote-calls/config", func(w http.ResponseWriter, r *http.Request) {
		var config callcontrol.RemoteConfig
		if !webRead(w, r, &config) {
			return
		}
		id := r.PathValue("deviceID")
		m.start(w, "save_route", m.activeProfile(), func(ctx context.Context, store *relayprofile.Store) (any, error) {
			return m.saveRoute(ctx, store, id, config)
		})
	})
	content, _ := fs.Sub(webAssets, "web/remote")
	mux.Handle("GET /remote/", m.auth.Guard(http.StripPrefix("/remote/", http.FileServer(http.FS(content)))))
}
func (m *remoteWebManager) erasePairingSecret(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if delivery, ok := j.Result.(pairingDelivery); ok && delivery.Grant.ID == id {
			delivery.Grant.Secret = ""
			delivery.QRImage = ""
			j.Result = delivery
		}
	}
}

type pairingDelivery struct {
	Version  int            `json:"version"`
	RelayURL string         `json:"relay_url"`
	Grant    identity.Grant `json:"grant"`
	QRImage  string         `json:"qr_image,omitempty"`
	QRError  string         `json:"qr_error,omitempty"`
}

func (m *remoteWebManager) pair(ctx context.Context, store *relayprofile.Store, id, label string, ids []string) (any, error) {
	if !webLabel(label) {
		return nil, relayprofile.ErrInvalid
	}
	entry, err := m.refresh(ctx, store, id)
	if err != nil {
		return nil, err
	}
	if _, err = m.choices(ids, false, entry.Status.Principal.Modules); err != nil {
		return nil, err
	}
	grant, err := store.Pairing(ctx, id, ids)
	if err != nil {
		return nil, err
	}
	if label != "" {
		if err = store.NameManaged(ctx, id, "pairings", grant.ID, label); err != nil {
			return nil, err
		}
	}
	delivery := pairingDelivery{Version: 1, RelayURL: entry.Status.RelayURL, Grant: grant}
	data, _ := json.Marshal(struct {
		Version  int            `json:"version"`
		RelayURL string         `json:"relay_url"`
		Grant    identity.Grant `json:"grant"`
	}{1, delivery.RelayURL, grant})
	if image, e := desktopopen.QRPNG(data); e == nil {
		delivery.QRImage = "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
	} else {
		delivery.QRError = "qr_unavailable_use_file_or_fewer_modules"
	}
	return delivery, nil
}

type remoteReference struct {
	Module string `json:"module"`
	Alias  string `json:"alias"`
	Active bool   `json:"active"`
	AI     bool   `json:"ai_enabled"`
}

func (m *remoteWebManager) references(installation string) []remoteReference {
	m.hub.mu.RLock()
	devices := []struct {
		id, alias string
		a         *app
	}{}
	for id, d := range m.hub.devices {
		if d.Runtime != nil {
			devices = append(devices, struct {
				id, alias string
				a         *app
			}{id, d.Alias, d.Runtime})
		}
	}
	m.hub.mu.RUnlock()
	result := []remoteReference{}
	for _, d := range devices {
		config, _ := d.a.ensureRemoteCalls().snapshot()
		if config.TargetInstallationID == installation {
			result = append(result, remoteReference{d.id, d.alias, d.a.hasActiveCall(), d.a.ensureVoiceAgent().snapshot().Enabled})
		}
	}
	slices.SortFunc(result, func(a, b remoteReference) int {
		if a.Module < b.Module {
			return -1
		}
		if a.Module > b.Module {
			return 1
		}
		return 0
	})
	return result
}
