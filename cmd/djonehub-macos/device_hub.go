package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iniwex5/vohive/pkg/smscodec"
)

type managedUSBDevice struct {
	ID          string
	Alias       string
	IMEI        string
	PhysicalID  string
	State       string
	Error       string
	Runtime     *app
	Cancel      context.CancelFunc
	LastSeen    time.Time
	LastAttempt time.Time
	USB         *usbDeviceStatus
}

type usbDeviceHub struct {
	mu                    sync.RWMutex
	reconcileMu           sync.Mutex
	devices               map[string]*managedUSBDevice
	physical              map[string]string
	aliases               map[string]string
	aliasesPath           string
	legacySettingsClaimed bool
	networkOwner          string
	fallback              *app
	ctx                   context.Context
	cancel                context.CancelFunc
}

type usbDeviceSummary struct {
	ID         string           `json:"id"`
	Alias      string           `json:"alias"`
	State      string           `json:"state"`
	Error      string           `json:"error,omitempty"`
	PhysicalID string           `json:"physical_id"`
	IMEIMasked string           `json:"imei_masked,omitempty"`
	USBDevice  *usbDeviceStatus `json:"usb_device,omitempty"`
	LastSeen   time.Time        `json:"last_seen"`
}

func newUSBDeviceHub() *usbDeviceHub {
	ctx, cancel := context.WithCancel(context.Background())
	hub := &usbDeviceHub{
		devices:  make(map[string]*managedUSBDevice),
		physical: make(map[string]string),
		aliases:  make(map[string]string),
		fallback: &app{
			port:             "未检测到 DJI USB 设备",
			discoveryError:   "DJI USB device is not connected",
			smsPollInterval:  8 * time.Second,
			smsAutoCleanupME: true,
			smsReassembler:   smscodec.NewReassembler(),
			callPollInterval: 3 * time.Second,
		},
		ctx:    ctx,
		cancel: cancel,
	}
	hub.loadAliases()
	return hub
}

func (h *usbDeviceHub) start() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-h.ctx.Done():
				return
			case <-ticker.C:
				if err := h.reconcile(); err != nil {
					log.Printf("USB module reconcile failed: %v", err)
				}
			}
		}
	}()
}

func (h *usbDeviceHub) reconcile() error {
	h.reconcileMu.Lock()
	defer h.reconcileMu.Unlock()

	locators, err := listDJIUSBDevices()
	if err != nil {
		return err
	}
	seen := make(map[string]usbDeviceLocator, len(locators))
	for _, locator := range locators {
		physicalID := locator.PhysicalID()
		seen[physicalID] = locator
		h.mu.RLock()
		deviceID, exists := h.physical[physicalID]
		managed := h.devices[deviceID]
		h.mu.RUnlock()
		if exists && managed != nil && managed.State == "ready" {
			h.mu.Lock()
			managed.LastSeen = time.Now()
			managed.USB = locator.Status
			h.mu.Unlock()
			continue
		}
		if exists && managed != nil && time.Since(managed.LastAttempt) < 2*time.Second {
			continue
		}
		h.attach(locator)
	}

	var missing []string
	h.mu.RLock()
	for physicalID := range h.physical {
		if _, ok := seen[physicalID]; !ok {
			missing = append(missing, physicalID)
		}
	}
	h.mu.RUnlock()
	for _, physicalID := range missing {
		h.detachPhysical(physicalID, "DJI USB device disconnected")
	}
	return nil
}

func (h *usbDeviceHub) attach(locator usbDeviceLocator) {
	physicalID := locator.PhysicalID()
	opened, err := openDJIUSBAT(locator)
	if err != nil {
		id := deviceIDFromIdentity(physicalID)
		h.mu.Lock()
		managed := h.devices[id]
		if managed == nil {
			managed = &managedUSBDevice{ID: id, Alias: defaultDeviceAlias(physicalID)}
			h.devices[id] = managed
		}
		managed.PhysicalID = physicalID
		managed.State = "error"
		managed.Error = err.Error()
		managed.LastSeen = time.Now()
		managed.LastAttempt = time.Now()
		managed.USB = locator.Status
		h.physical[physicalID] = id
		h.mu.Unlock()
		return
	}

	imei := probeUSBATIMEI(opened)
	identity := imei
	if identity == "" {
		identity = physicalID
	}
	id := deviceIDFromIdentity(identity)
	h.mu.RLock()
	duplicate := h.devices[id]
	h.mu.RUnlock()
	if duplicate != nil && duplicate.State == "ready" && duplicate.PhysicalID != physicalID {
		// A duplicated or rewritten IMEI must never cause one physical module to
		// replace another live runtime. Keep both manageable and make the second
		// ID deterministic for its current physical attachment.
		id = deviceIDFromIdentity(identity + "@" + physicalID)
	}
	runtime := &app{
		usbAT:            opened,
		usbLocator:       &locator,
		usbDevice:        locator.Status,
		port:             opened.Description(),
		smsPollInterval:  8 * time.Second,
		smsAutoCleanupME: true,
		smsReassembler:   smscodec.NewReassembler(),
		callPollInterval: 3 * time.Second,
	}
	h.configureDeviceSettings(runtime, id)
	deviceCtx, cancel := context.WithCancel(h.ctx)
	runtime.onUSBDetached = func(reason string) {
		go h.detachPhysical(physicalID, reason)
	}
	h.configureNetworkModeGuard(runtime, id)
	runtime.initUSBATESIMManager()

	h.mu.Lock()
	alias := defaultDeviceAlias(physicalID)
	if saved := strings.TrimSpace(h.aliases[id]); saved != "" {
		alias = saved
	}
	if previous := h.devices[id]; previous != nil && strings.TrimSpace(previous.Alias) != "" {
		alias = previous.Alias
		if previous.Cancel != nil {
			previous.Cancel()
		}
	}
	if provisionalID := h.physical[physicalID]; provisionalID != "" && provisionalID != id {
		delete(h.devices, provisionalID)
	}
	h.devices[id] = &managedUSBDevice{
		ID:          id,
		Alias:       alias,
		IMEI:        imei,
		PhysicalID:  physicalID,
		State:       "ready",
		Runtime:     runtime,
		Cancel:      cancel,
		LastSeen:    time.Now(),
		LastAttempt: time.Now(),
		USB:         locator.Status,
	}
	h.physical[physicalID] = id
	h.mu.Unlock()

	go runtime.startSMSPoller(deviceCtx)
	go runtime.startCallPoller(deviceCtx)
	log.Printf("DJI module %s attached as %s (%s)", physicalID, id, maskIMEI(imei))
}

func (h *usbDeviceHub) configureNetworkModeGuard(runtime *app, id string) {
	runtime.authorizeUSBNetMode = func(mode int) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if mode == 1 {
			if h.networkOwner != "" && h.networkOwner != id {
				return errors.New("另一台模块已经被设置为上网模式；请先将其切回短信模式")
			}
			h.networkOwner = id
		}
		return nil
	}
	runtime.releaseUSBNetModeReservation = func(mode int) {
		if mode != 1 {
			return
		}
		h.mu.Lock()
		if h.networkOwner == id {
			h.networkOwner = ""
		}
		h.mu.Unlock()
	}
	runtime.onUSBNetModeChanged = func(mode int) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if mode == 1 {
			h.networkOwner = id
		} else if h.networkOwner == id {
			h.networkOwner = ""
		}
	}
}

func (h *usbDeviceHub) detachPhysical(physicalID, reason string) {
	h.mu.Lock()
	id, ok := h.physical[physicalID]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.physical, physicalID)
	managed := h.devices[id]
	if managed != nil {
		managed.State = "offline"
		managed.Error = reason
		managed.LastSeen = time.Now()
		if h.networkOwner == id {
			h.networkOwner = ""
		}
	}
	h.mu.Unlock()
	if managed == nil {
		return
	}
	if managed.Cancel != nil {
		managed.Cancel()
	}
	if managed.Runtime != nil && managed.Runtime.usbAT != nil {
		managed.Runtime.onUSBDetached = nil
		managed.Runtime.usbAT.Close()
		managed.Runtime.usbAT = nil
	}
	log.Printf("DJI module %s (%s) is offline: %s", id, physicalID, reason)
}

func (h *usbDeviceHub) close() {
	h.cancel()
	h.mu.RLock()
	physicalIDs := make([]string, 0, len(h.physical))
	for physicalID := range h.physical {
		physicalIDs = append(physicalIDs, physicalID)
	}
	h.mu.RUnlock()
	for _, physicalID := range physicalIDs {
		h.detachPhysical(physicalID, "DJOneHub stopped")
	}
}

func probeUSBATIMEI(device *usbAT) string {
	if device == nil {
		return ""
	}
	response, err := device.Command("AT+GSN", 3*time.Second)
	if err != nil {
		return ""
	}
	return regexp.MustCompile(`\b[0-9]{14,16}\b`).FindString(response)
}

func deviceIDFromIdentity(identity string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(identity)))
	return "dji-" + hex.EncodeToString(sum[:6])
}

func defaultDeviceAlias(physicalID string) string {
	return "模块 " + strings.TrimPrefix(physicalID, "usb-")
}

func (h *usbDeviceHub) loadAliases() {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	h.aliasesPath = filepath.Join(configDir, "DJOneHub", "device-aliases.json")
	data, err := os.ReadFile(h.aliasesPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &h.aliases)
	if h.aliases == nil {
		h.aliases = make(map[string]string)
	}
}

func (h *usbDeviceHub) persistAliasesLocked() error {
	if h.aliasesPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(h.aliasesPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h.aliases, "", "  ")
	if err != nil {
		return err
	}
	temporary := h.aliasesPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, h.aliasesPath)
}

func (h *usbDeviceHub) configureDeviceSettings(runtime *app, deviceID string) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	directory := filepath.Join(configDir, "DJOneHub", "devices", deviceID)
	runtime.barkSettingsPath = filepath.Join(directory, "bark-settings.json")

	h.mu.Lock()
	claimLegacy := !h.legacySettingsClaimed
	if claimLegacy {
		h.legacySettingsClaimed = true
	}
	h.mu.Unlock()
	if !claimLegacy {
		return
	}
	if _, err := os.Stat(runtime.barkSettingsPath); err == nil {
		return
	}
	legacyPath := filepath.Join(configDir, "DJOneHub", "bark-settings.json")
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(runtime.barkSettingsPath, data, 0o600); err == nil {
		log.Printf("migrated legacy Bark settings to device %s", deviceID)
	}
}

func maskIMEI(imei string) string {
	if len(imei) < 7 {
		return imei
	}
	return imei[:4] + strings.Repeat("•", len(imei)-7) + imei[len(imei)-3:]
}

func (h *usbDeviceHub) summaries() []usbDeviceSummary {
	h.mu.RLock()
	out := make([]usbDeviceSummary, 0, len(h.devices))
	for _, device := range h.devices {
		out = append(out, usbDeviceSummary{
			ID: device.ID, Alias: device.Alias, State: device.State, Error: device.Error,
			PhysicalID: device.PhysicalID, IMEIMasked: maskIMEI(device.IMEI),
			USBDevice: device.USB, LastSeen: device.LastSeen,
		})
	}
	h.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == "ready"
		}
		return out[i].Alias < out[j].Alias
	})
	return out
}

func (h *usbDeviceHub) runtime(id string) (*app, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	device := h.devices[id]
	returnRuntime := device != nil && device.State == "ready" && device.Runtime != nil
	if !returnRuntime {
		return nil, false
	}
	return device.Runtime, true
}

func (h *usbDeviceHub) defaultRuntime() *app {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.devices))
	for id, device := range h.devices {
		if device.State == "ready" && device.Runtime != nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return h.fallback
	}
	sort.Strings(ids)
	return h.devices[ids[0]].Runtime
}

func (h *usbDeviceHub) routes(assets embed.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, h.summaries())
	})
	mux.HandleFunc("PATCH /api/devices/{deviceID}", h.renameDevice)
	mux.HandleFunc("/api/devices/{deviceID}/{rest...}", h.dispatchDeviceAPI)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		h.defaultRuntime().routes().ServeHTTP(w, r)
	})
	content, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(content)))
	return securityHeaders(mux)
}

func (h *usbDeviceHub) renameDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alias string `json:"alias"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Alias = strings.TrimSpace(body.Alias)
	if body.Alias == "" || len([]rune(body.Alias)) > 80 {
		writeError(w, http.StatusBadRequest, "alias must contain 1 to 80 characters")
		return
	}
	h.mu.Lock()
	device := h.devices[r.PathValue("deviceID")]
	if device == nil {
		h.mu.Unlock()
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	device.Alias = body.Alias
	h.aliases[device.ID] = body.Alias
	persistErr := h.persistAliasesLocked()
	h.mu.Unlock()
	if persistErr != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist device alias")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": device.ID, "alias": device.Alias})
}

func (h *usbDeviceHub) dispatchDeviceAPI(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.runtime(r.PathValue("deviceID"))
	if !ok {
		writeError(w, http.StatusConflict, "device is offline or unavailable")
		return
	}
	rest := strings.TrimPrefix(r.PathValue("rest"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "API path is required")
		return
	}
	cloned := r.Clone(r.Context())
	cloned.URL.Path = "/api/" + rest
	cloned.URL.RawPath = ""
	runtime.routes().ServeHTTP(w, cloned)
}

func serveUSBDeviceHub(hub *usbDeviceHub, listen string) {
	server := &http.Server{
		Addr: listen, Handler: hub.routes(webAssets), ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer hub.close()
	hub.start()
	log.Printf("DJOneHub multi-module mode")
	log.Printf("Open http://%s", listen)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
}
