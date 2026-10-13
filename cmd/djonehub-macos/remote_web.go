package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/relayprofile"
	"github.com/iniwex5/vohive/internal/webadmin"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var errRemoteCallBusy = errors.New("remote management blocked by call or recovery")
var errRemoteScope = errors.New("remote management module or installation scope denied")

type remoteWebJob struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Profile    string     `json:"profile"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	Result     any        `json:"result,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
type remoteWebManager struct {
	auth               *webadmin.Auth
	catalog            *remoteCatalog
	hub                *usbDeviceHub
	ctx                context.Context
	cancel             context.CancelFunc
	factory            func() (*relayprofile.Store, error)
	connect            func(context.Context, string, *relayprofile.Store) (*remoteHubService, error)
	nativeFile         string
	mu                 sync.Mutex
	jobs               map[string]*remoteWebJob
	working            bool
	closed             bool
	active             string
	wg                 sync.WaitGroup
	presence           webPresence
	presenceCancel     context.CancelFunc
	presenceGeneration string
}

func remoteWebOrigin(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" || port == "0" {
		return "", relayprofile.ErrInvalid
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "http://" + net.JoinHostPort(host, port), nil
	}
	return "http://127.0.0.1:" + port, nil
}
func newRemoteWebManager(parent context.Context, hub *usbDeviceHub, dir, origin, nativeFile string, factory func() (*relayprofile.Store, error)) (*remoteWebManager, error) {
	auth, err := webadmin.Open(webadmin.Options{Directory: dir, Origin: origin, Tenant: "local", CookieName: "djonehub_mac_management", LoopbackOnly: true, LocalTickets: true})
	if err != nil {
		return nil, err
	}
	catalog, err := openRemoteCatalog(dir)
	if err != nil {
		auth.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	m := &remoteWebManager{auth: auth, catalog: catalog, hub: hub, ctx: ctx, cancel: cancel, factory: factory, nativeFile: nativeFile, jobs: map[string]*remoteWebJob{}}
	m.connect = func(ctx context.Context, id string, store *relayprofile.Store) (*remoteHubService, error) {
		return startRemoteHubWithStore(ctx, id, nativeFile, store, hub.remoteDial)
	}
	return m, nil
}
func webID(prefix string) string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		panic("system random unavailable")
	}
	return prefix + hex.EncodeToString(data)
}
func webLabel(s string) bool {
	return len(s) <= 128 && utf8.ValidString(s) && !strings.ContainsAny(s, "\r\n\x00")
}
func remoteWebError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errRemoteCallBusy):
		return "call_or_recovery_active"
	case errors.Is(err, errRemoteScope):
		return "scope_denied"
	case errors.Is(err, relayprofile.ErrNotFound):
		return "profile_not_found"
	case errors.Is(err, relayprofile.ErrExists):
		return "profile_exists"
	case errors.Is(err, relayprofile.ErrRecovery):
		return "credential_recovery_required"
	case errors.Is(err, relayprofile.ErrVault):
		return "keychain_unavailable"
	case errors.Is(err, relayprofile.ErrUnsupported):
		return "platform_unsupported"
	case errors.Is(err, relayprofile.ErrBusy):
		return "management_busy"
	case errors.Is(err, relayprofile.ErrInvalid):
		return "invalid_management_state"
	case errors.Is(err, relayclient.ErrRequest):
		return "relay_request_failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "operation_interrupted_check_state"
	default:
		return "management_storage_failed"
	}
}

// Jobs outlive the initiating HTTP request. Keychain/network waits never block
// the page listener or hold call-selection locks. Only one credential action runs.
func (m *remoteWebManager) job(kind, profile string, run func(context.Context, *relayprofile.Store) (any, error)) (remoteWebJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.working {
		return remoteWebJob{}, relayprofile.ErrBusy
	}
	for id, j := range m.jobs {
		if j.FinishedAt != nil && time.Since(*j.FinishedAt) > 10*time.Minute {
			delete(m.jobs, id)
		}
	}
	if len(m.jobs) >= 32 {
		oldest := ""
		var at time.Time
		for id, j := range m.jobs {
			if j.FinishedAt != nil && (oldest == "" || j.FinishedAt.Before(at)) {
				oldest = id
				at = *j.FinishedAt
			}
		}
		if oldest == "" {
			return remoteWebJob{}, relayprofile.ErrBusy
		}
		delete(m.jobs, oldest)
	}
	j := &remoteWebJob{ID: webID("job_"), Kind: kind, Profile: profile, State: "running", StartedAt: time.Now().UTC()}
	m.jobs[j.ID] = j
	m.working = true
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
		defer cancel()
		store, err := m.factory()
		var result any
		if err == nil {
			result, err = run(ctx, store)
		}
		now := time.Now().UTC()
		m.mu.Lock()
		defer m.mu.Unlock()
		j.Result = result
		j.Error = remoteWebError(err)
		j.State = "completed"
		if err != nil {
			j.State = "failed"
		}
		j.FinishedAt = &now
		m.working = false
		if delivery, ok := result.(pairingDelivery); ok {
			time.AfterFunc(time.Until(delivery.Grant.ExpiresAt), func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if stored := m.jobs[j.ID]; stored != nil {
					stored.Result = map[string]string{"state": "expired"}
				}
			})
		}
	}()
	return *j, nil
}
func (m *remoteWebManager) jobState(id string) (remoteWebJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return remoteWebJob{}, false
	}
	return *j, true
}
func (m *remoteWebManager) activeProfile() string { m.mu.Lock(); defer m.mu.Unlock(); return m.active }
func (m *remoteWebManager) setActive(id string)   { m.mu.Lock(); m.active = id; m.mu.Unlock() }
func (m *remoteWebManager) refresh(ctx context.Context, store *relayprofile.Store, id string) (remoteProfileEntry, error) {
	entry, err := m.catalog.get(ctx, id)
	if err != nil {
		return entry, err
	}
	status, err := store.Status(id)
	entry.Error = remoteWebError(err)
	entry.CheckedAt = time.Now().UTC()
	if err == nil {
		entry.Status = status
		if request, e := store.EnrollmentRequest(id); e == nil {
			entry.Request = &request
		}
	}
	if e := m.catalog.save(context.WithoutCancel(ctx), entry); e != nil {
		return entry, e
	}
	return entry, err
}
func (m *remoteWebManager) choices(ids []string, ready bool, scope []string) ([]identity.ModuleChoice, error) {
	if len(ids) == 0 || len(ids) > 32 {
		return nil, errRemoteScope
	}
	seen := map[string]bool{}
	result := []identity.ModuleChoice{}
	m.hub.mu.RLock()
	defer m.hub.mu.RUnlock()
	for _, id := range ids {
		device := m.hub.devices[id]
		if seen[id] || device == nil || ready && (device.State != "ready" || device.Runtime == nil) || scope != nil && !slices.Contains(scope, id) {
			return nil, errRemoteScope
		}
		seen[id] = true
		result = append(result, identity.ModuleChoice{ID: id, Alias: device.Alias})
	}
	return result, nil
}
func (m *remoteWebManager) create(ctx context.Context, store *relayprofile.Store, label, origin string, ids []string) (any, error) {
	if label == "" || !webLabel(label) || !identity.HTTPSOrigin(origin) {
		return nil, relayprofile.ErrInvalid
	}
	modules, err := m.choices(ids, true, nil)
	if err != nil {
		return nil, err
	}
	id := webID("web_")
	entry := remoteProfileEntry{Profile: id, Label: label, Status: relayprofile.Status{Name: id, RelayURL: strings.TrimSuffix(origin, "/"), Phase: "creating"}}
	if err = m.catalog.save(ctx, entry); err != nil {
		return nil, err
	}
	_, err = store.BeginEnrollment(ctx, id, origin, label, modules)
	if err != nil {
		entry.Error = remoteWebError(err)
		_ = m.catalog.save(context.WithoutCancel(ctx), entry)
		return entry, err
	}
	return m.refresh(ctx, store, id)
}
func (m *remoteWebManager) importLegacy(ctx context.Context, store *relayprofile.Store, id, label string) (any, error) {
	if !callsignal.ID(id) || len(id) > 64 || !webLabel(label) {
		return nil, relayprofile.ErrInvalid
	}
	status, err := store.Status(id)
	if err != nil {
		return nil, err
	}
	if label == "" {
		label = id
	}
	entry := remoteProfileEntry{Profile: id, Label: label, Status: status, CheckedAt: time.Now().UTC()}
	if req, e := store.EnrollmentRequest(id); e == nil {
		entry.Request = &req
	}
	if old, e := m.catalog.get(ctx, id); e == nil {
		entry.Label = old.Label
		entry.AdminURL = old.AdminURL
	} else if !errors.Is(e, relayprofile.ErrNotFound) {
		return nil, e
	}
	return entry, m.catalog.save(ctx, entry)
}

// lockRoutes freezes route selection only during the short transition commit.
// The device set is captured under hub.mu, then route locks use stable ID order.
func (m *remoteWebManager) lockRoutes() ([]*app, func(), error) {
	for attempt := 0; attempt < 8; attempt++ {
		m.hub.mu.RLock()
		ids := make([]string, 0, len(m.hub.devices))
		for id := range m.hub.devices {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		known := map[string]*app{}
		apps := []*app{}
		for _, id := range ids {
			if a := m.hub.devices[id].Runtime; a != nil {
				known[id] = a
				apps = append(apps, a)
			}
		}
		m.hub.mu.RUnlock()
		for _, a := range apps {
			a.callRouteMu.Lock()
		}
		m.hub.mu.Lock()
		same := true
		count := 0
		for id, d := range m.hub.devices {
			if d.Runtime != nil {
				count++
				if known[id] != d.Runtime {
					same = false
				}
			}
		}
		same = same && count == len(known)
		if same {
			return apps, func() {
				m.hub.mu.Unlock()
				for i := len(apps) - 1; i >= 0; i-- {
					apps[i].callRouteMu.Unlock()
				}
			}, nil
		}
		m.hub.mu.Unlock()
		for i := len(apps) - 1; i >= 0; i-- {
			apps[i].callRouteMu.Unlock()
		}
	}
	return nil, nil, relayprofile.ErrBusy
}
func routesIdle(apps []*app) bool {
	for _, a := range apps {
		if a.hasActiveCall() || a.ensureRemoteJournal().status().RecoveryRequired {
			return false
		}
	}
	return true
}
func (m *remoteWebManager) changeConnection(ctx context.Context, store *relayprofile.Store, id string, autostart bool) (any, error) {
	if id != "" {
		entry, err := m.refresh(ctx, store, id)
		if err != nil {
			return nil, err
		}
		if entry.Status.Phase != "ready" {
			return nil, relayprofile.ErrRecovery
		}
	}
	m.hub.remoteLifecycleMu.Lock()
	defer m.hub.remoteLifecycleMu.Unlock()
	apps, unlock, err := m.lockRoutes()
	if err != nil {
		return nil, err
	}
	if !routesIdle(apps) {
		unlock()
		return nil, errRemoteCallBusy
	}
	old := m.hub.remoteHub
	if old != nil && !old.RetireIdle() {
		unlock()
		return nil, errRemoteCallBusy
	}
	m.hub.remoteHub = nil
	m.setActive("")
	unlock()
	if old != nil {
		old.Close()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		if autostart {
			if err := m.catalog.setAutostart(ctx, ""); err != nil {
				return nil, err
			}
		}
		return map[string]bool{"connected": false}, nil
	}
	// A new call may have arrived while the old listener was closing. Leave it
	// disconnected instead of publishing another identity into that call's route.
	apps, unlock, err = m.lockRoutes()
	if err != nil {
		return nil, err
	}
	if !routesIdle(apps) {
		unlock()
		return nil, errRemoteCallBusy
	}
	unlock()
	manager, err := m.connect(m.ctx, id, store)
	if err != nil {
		return nil, err
	}
	apps, unlock, err = m.lockRoutes()
	if err != nil {
		manager.Close()
		return nil, err
	}
	if !routesIdle(apps) {
		unlock()
		manager.Close()
		return nil, errRemoteCallBusy
	}
	// Publish under the same hub lock used by discovery and dispatch. A manager
	// reservation itself is additionally protected by RetireIdle's atomic gate.
	m.hub.remoteHub = manager
	m.setActive(id)
	if manager.activate != nil {
		manager.activate()
	}
	unlock()
	if autostart {
		if err = m.catalog.setAutostart(ctx, id); err != nil {
			return map[string]bool{"connected": true, "startup_saved": false}, err
		}
	}
	if !autostart {
		if err = m.catalog.setAutostart(ctx, ""); err != nil {
			return map[string]bool{"connected": true, "startup_saved": false}, err
		}
	}
	return map[string]any{"profile": id, "state": manager.Snapshot().State, "startup_saved": autostart}, nil
}
func (m *remoteWebManager) boot(profile string) {
	if profile == "" {
		profile, _ = m.catalog.autostart(m.ctx)
	}
	if profile == "" {
		return
	}
	_, _ = m.job("startup", profile, func(ctx context.Context, store *relayprofile.Store) (any, error) {
		if _, err := m.importLegacy(ctx, store, profile, profile); err != nil {
			return nil, err
		}
		return m.changeConnection(ctx, store, profile, true)
	})
}
func (m *remoteWebManager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
	m.hub.mu.RLock()
	manager := m.hub.remoteHub
	m.hub.mu.RUnlock()
	if manager != nil {
		manager.Close()
	}
	m.auth.Close()
	_ = m.catalog.db.Close()
	m.mu.Lock()
	clear(m.jobs)
	m.mu.Unlock()
}
func (m *remoteWebManager) installations(ctx context.Context, store *relayprofile.Store, id string) (any, error) {
	entry, err := m.refresh(ctx, store, id)
	if err != nil {
		return nil, err
	}
	observer, err := store.InstallationObserver(ctx, id)
	if err != nil {
		return nil, err
	}
	items, err := observer.Read(ctx)
	if err != nil {
		observer.Close()
		return nil, err
	}
	for _, item := range items {
		if item.HubID != entry.Status.Principal.ID || item.Kind != "installation" {
			observer.Close()
			return nil, errRemoteScope
		}
	}
	m.watchPresence(id, observer, items)
	return map[string]any{"profile": id, "installations": items}, nil
}
func (m *remoteWebManager) saveRoute(ctx context.Context, store *relayprofile.Store, module string, config callcontrol.RemoteConfig) (any, error) {
	config, err := callcontrol.NormalizeRemoteConfig(config)
	if err != nil {
		return nil, relayprofile.ErrInvalid
	}
	profile := m.activeProfile()
	if profile == "" && config.Enabled {
		return nil, errRemoteScope
	}
	if config.Enabled || config.TargetInstallationID != "" {
		entry, err := m.refresh(ctx, store, profile)
		if err != nil {
			return nil, err
		}
		if _, err = m.choices([]string{module}, true, entry.Status.Principal.Modules); err != nil {
			return nil, err
		}
		items, err := store.ManagedInstallations(ctx, profile)
		if err != nil {
			return nil, err
		}
		valid := false
		for _, item := range items {
			if item.ID == config.TargetInstallationID && !item.Revoked && item.HubID == entry.Status.Principal.ID && item.Kind == "installation" && slices.Contains(item.Modules, module) {
				valid = true
			}
		}
		if !valid {
			return nil, errRemoteScope
		}
	}
	a, ok := m.hub.runtime(module)
	if !ok {
		return nil, errRemoteScope
	}
	a.callRouteMu.Lock()
	defer a.callRouteMu.Unlock()
	if a.hasActiveCall() || a.ensureRemoteJournal().status().RecoveryRequired {
		return nil, errRemoteCallBusy
	}
	controller := a.ensureRemoteCalls()
	old, _ := controller.snapshot()
	config.Revision = old.Revision + 1
	if err = controller.save(config); err != nil {
		return nil, err
	}
	return map[string]any{"module": module, "config": config, "ai_enabled": a.ensureVoiceAgent().snapshot().Enabled}, nil
}
