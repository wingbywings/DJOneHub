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
	ID           string
	Alias        string
	IMEI         string
	PhysicalID   string
	State        string
	Error        string
	Runtime      *app
	Cancel       context.CancelFunc
	LastSeen     time.Time
	LastAttempt  time.Time
	MissingSince time.Time
	Generation   uint64
	Locator      usbDeviceLocator
	USB          *usbDeviceStatus
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
	voiceOwner            string
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
	Voice      voicePreparation `json:"voice"`
	LastSeen   time.Time        `json:"last_seen"`
}

type voicePreparation struct {
	State                  string `json:"state"`
	Detail                 string `json:"detail"`
	USBLocationID          string `json:"usb_location_id,omitempty"`
	AudioInterface         bool   `json:"audio_interface"`
	ADBInterfaceAdvertised bool   `json:"adb_interface_advertised"`
}

const usbReenumerationGrace = 12 * time.Second

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
		state := ""
		lastAttempt := time.Time{}
		previousLocator := usbDeviceLocator{}
		hasRuntime := false
		if managed != nil {
			state = managed.State
			lastAttempt = managed.LastAttempt
			previousLocator = managed.Locator
			hasRuntime = managed.Runtime != nil
		}
		h.mu.RUnlock()
		if exists && managed != nil && state == "ready" && sameUSBEnumeration(previousLocator, locator) {
			h.mu.Lock()
			managed.LastSeen = time.Now()
			managed.USB = locator.Status
			managed.Locator = locator
			h.mu.Unlock()
			continue
		}
		if exists && managed != nil && state == "ready" {
			h.beginPhysicalReconnect(physicalID, "USB device identity changed during re-enumeration")
			state = "reconnecting"
		}
		if exists && managed != nil && time.Since(lastAttempt) < 2*time.Second {
			continue
		}
		if exists && managed != nil && hasRuntime && (state == "reconnecting" || state == "error") {
			h.resumePhysicalRuntime(locator)
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
		h.beginPhysicalReconnect(physicalID, "DJI USB device is re-enumerating or disconnected")
	}
	h.expirePhysicalReconnects(time.Now())
	return nil
}

func sameUSBEnumeration(a, b usbDeviceLocator) bool {
	return sameUSBDevice(a, b) && a.VendorID == b.VendorID && a.ProductID == b.ProductID && a.Address == b.Address
}

// beginPhysicalReconnect closes handles tied to the old USB enumeration while
// retaining the logical device record. A mode change can therefore return on
// the same physical port without losing its alias, device ID or setup progress.
func (h *usbDeviceHub) beginPhysicalReconnect(physicalID, reason string) {
	h.mu.Lock()
	id, ok := h.physical[physicalID]
	managed := h.devices[id]
	if !ok || managed == nil || managed.State == "offline" {
		h.mu.Unlock()
		return
	}
	if managed.State != "reconnecting" {
		managed.State = "reconnecting"
		managed.MissingSince = time.Now()
		managed.Generation++
	}
	managed.Error = reason
	runtime := managed.Runtime
	h.mu.Unlock()

	if runtime != nil {
		runtime.moduleVoiceOpMu.Lock()
		runtime.closeModuleVoiceSessionLocked()
		runtime.operationMu.Lock()
		if runtime.usbAT != nil {
			runtime.usbAT.Close()
			runtime.usbAT = nil
		}
		runtime.usbDevice = nil
		runtime.port = "正在等待模块重新枚举"
		runtime.discoveryError = reason
		runtime.usbATBackoffUntil = time.Now().Add(2 * time.Second)
		runtime.usbATBackoffErr = reason
		runtime.operationMu.Unlock()
		runtime.moduleVoiceOpMu.Unlock()
	}
}

func (h *usbDeviceHub) resumePhysicalRuntime(locator usbDeviceLocator) {
	physicalID := locator.PhysicalID()
	h.mu.RLock()
	id := h.physical[physicalID]
	managed := h.devices[id]
	var runtime *app
	if managed != nil {
		runtime = managed.Runtime
	}
	h.mu.RUnlock()
	if runtime == nil {
		h.attach(locator)
		return
	}

	opened, err := openDJIUSBAT(locator)
	if err != nil {
		h.recordAttachError(locator, err)
		return
	}
	runtime.moduleVoiceOpMu.Lock()
	runtime.closeModuleVoiceSessionLocked()
	runtime.operationMu.Lock()
	if runtime.usbAT != nil {
		runtime.usbAT.Close()
	}
	runtime.usbAT = opened
	runtime.usbLocator = &locator
	runtime.usbDevice = locator.Status
	runtime.port = opened.Description()
	runtime.discoveryError = ""
	runtime.usbATBackoffUntil = time.Time{}
	runtime.usbATBackoffErr = ""
	runtime.callMu.Lock()
	runtime.callConfigured = false
	runtime.callMu.Unlock()
	runtime.initUSBATESIMManager()
	runtime.operationMu.Unlock()
	runtime.moduleVoiceOpMu.Unlock()

	h.mu.Lock()
	if current := h.devices[id]; current == managed {
		managed.State = "ready"
		managed.Error = ""
		managed.MissingSince = time.Time{}
		managed.LastSeen = time.Now()
		managed.LastAttempt = time.Now()
		managed.Locator = locator
		managed.USB = locator.Status
	}
	h.mu.Unlock()
	log.Printf("DJI module %s resumed after USB re-enumeration", id)
	go runtime.resumeModuleSetup(h.ctx)
	go runtime.warmModuleVoiceIfReady()
}

func (h *usbDeviceHub) expirePhysicalReconnects(now time.Time) {
	var expired []string
	h.mu.RLock()
	for physicalID, id := range h.physical {
		managed := h.devices[id]
		if managed != nil && managed.State == "reconnecting" && !managed.MissingSince.IsZero() && now.Sub(managed.MissingSince) >= usbReenumerationGrace {
			expired = append(expired, physicalID)
		}
	}
	h.mu.RUnlock()
	for _, physicalID := range expired {
		h.detachPhysical(physicalID, "DJI USB device did not return after re-enumeration")
	}
}

func (h *usbDeviceHub) attach(locator usbDeviceLocator) {
	physicalID := locator.PhysicalID()
	opened, err := openDJIUSBAT(locator)
	if err != nil {
		h.recordAttachError(locator, err)
		return
	}

	imei := probeUSBATIMEI(opened)
	h.mu.RLock()
	existingID := h.physical[physicalID]
	existingDevice := h.devices[existingID]
	h.mu.RUnlock()
	if imei == "" && existingDevice != nil && existingDevice.IMEI != "" {
		// A freshly re-enumerated module can answer AT a moment before AT+GSN.
		// Keep its known identity instead of replacing it with a port-derived ID.
		imei = existingDevice.IMEI
	}
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
		deviceID:         id,
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
		go h.beginPhysicalReconnect(physicalID, reason)
	}
	h.configureNetworkModeGuard(runtime, id)
	h.configureVoiceRouteGuard(runtime, id)
	runtime.initUSBATESIMManager()

	h.mu.Lock()
	alias := defaultDeviceAlias(physicalID)
	generation := uint64(1)
	if saved := strings.TrimSpace(h.aliases[id]); saved != "" {
		alias = saved
	}
	if previous := h.devices[id]; previous != nil && strings.TrimSpace(previous.Alias) != "" {
		alias = previous.Alias
		generation = previous.Generation + 1
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
		Generation:  generation,
		Locator:     locator,
		USB:         locator.Status,
	}
	h.physical[physicalID] = id
	h.mu.Unlock()

	go runtime.startSMSPoller(deviceCtx)
	go runtime.startCallPoller(deviceCtx)
	go runtime.resumeModuleSetup(deviceCtx)
	go runtime.warmModuleVoiceIfReady()
	log.Printf("DJI module %s attached as %s (%s)", physicalID, id, maskIMEI(imei))
}

func (h *usbDeviceHub) recordAttachError(locator usbDeviceLocator, attachErr error) {
	physicalID := locator.PhysicalID()
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.physical[physicalID]
	if id == "" {
		id = deviceIDFromIdentity(physicalID)
	}
	managed := h.devices[id]
	if managed == nil {
		managed = &managedUSBDevice{ID: id, Alias: defaultDeviceAlias(physicalID)}
		h.devices[id] = managed
	}
	managed.PhysicalID = physicalID
	managed.State = "error"
	managed.Error = attachErr.Error()
	managed.LastSeen = time.Now()
	managed.LastAttempt = time.Now()
	managed.MissingSince = time.Time{}
	managed.Locator = locator
	managed.USB = locator.Status
	h.physical[physicalID] = id
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

func (h *usbDeviceHub) configureVoiceRouteGuard(runtime *app, id string) {
	runtime.authorizeVoiceRoute = func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.voiceOwner != "" && h.voiceOwner != id {
			return errors.New("另一台模块正在使用语音音频；请先结束该通话")
		}
		h.voiceOwner = id
		return nil
	}
	runtime.releaseVoiceRoute = func() {
		h.mu.Lock()
		if h.voiceOwner == id {
			h.voiceOwner = ""
		}
		h.mu.Unlock()
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
		if h.voiceOwner == id {
			h.voiceOwner = ""
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
		managed.Runtime.usbAT.Close()
		managed.Runtime.usbAT = nil
	}
	if managed.Runtime != nil {
		managed.Runtime.resetModuleVoiceSession()
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
	runtime.voiceAgentSettingsPath = filepath.Join(directory, "voice-agent.json")
	runtime.moduleSetupPath = filepath.Join(directory, "voice-setup-state.json")
	runtime.moduleSetupBackupDir = filepath.Join(directory, "module-backups")
	if err := runtime.loadModuleSetupState(); err != nil {
		log.Printf("load voice setup state for %s: %v", deviceID, err)
	}

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
			USBDevice: device.USB, Voice: voicePreparationFor(device.State, device.USB), LastSeen: device.LastSeen,
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

func voicePreparationFor(deviceState string, usb *usbDeviceStatus) voicePreparation {
	preparation := voicePreparation{
		State:  "offline",
		Detail: "模块当前离线",
	}
	if usb == nil {
		if deviceState == "reconnecting" {
			preparation.State = "reconnecting"
			preparation.Detail = "正在等待模块完成 USB 重枚举"
		}
		return preparation
	}
	preparation.USBLocationID = usb.LocationID
	for _, usbInterface := range usb.Interfaces {
		if usbInterface.Class == 1 {
			preparation.AudioInterface = true
		}
		if usbInterface.Subclass == 66 || (usbInterface.Number == 6 && usbInterface.Class == 255) {
			preparation.ADBInterfaceAdvertised = true
		}
	}
	if deviceState == "reconnecting" {
		preparation.State = "reconnecting"
		preparation.Detail = "正在等待模块完成 USB 重枚举"
		return preparation
	}
	if preparation.AudioInterface {
		preparation.State = "usb_audio_available"
		preparation.Detail = "已检测到 USB 音频接口，仍需完成通话组件初始化"
		return preparation
	}
	preparation.State = "module_configuration_required"
	preparation.Detail = "当前 USB 模式未提供音频接口"
	return preparation
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
