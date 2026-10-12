package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/iniwex5/vohive/internal/remotecall"
	"github.com/iniwex5/vohive/internal/remotemedia"
)

// Immutable after flag parsing. Opt-in until the packaged launcher migrates.
var nativeControlFile string

type nativeBootstrap struct {
	Version int    `json:"version"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}
type nativeHostReport struct {
	CallUUID   string `json:"call_uuid"`
	Generation uint64 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
	Running    bool   `json:"running"`
}
type remoteNativeHost struct {
	mu      sync.Mutex
	bridge  *remotemedia.HostBridge
	binding remotecall.Binding
	hostID  string
	report  nativeHostReport
	seen    time.Time
}

func (h *remoteNativeHost) claim(hostID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hostID == "" || len(hostID) > 128 {
		return false
	}
	if h.hostID == "" {
		h.hostID = hostID
	}
	return h.hostID == hostID
}
func (h *remoteNativeHost) update(hostID string, report nativeHostReport, now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hostID == "" || hostID != h.hostID || report.CallUUID != h.binding.CallUUID || report.Generation != h.binding.Generation || report.Sequence == 0 {
		return remotecall.ErrBinding
	}
	if report.Sequence <= h.report.Sequence {
		return remotecall.ErrRevision
	}
	// Readiness must confirm both actual UAC startup and the per-call bridge.
	if report.Running && !h.bridge.Ready() {
		return remotemedia.ErrBridgeNotReady
	}
	wasRunning := h.report.Running
	h.report, h.seen = report, now
	if wasRunning && !report.Running {
		h.bridge.Close()
	}
	return nil
}
func (h *remoteNativeHost) ready(binding remotecall.Binding) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return binding == h.binding && h.report.Running && time.Since(h.seen) < 2*time.Second && h.bridge.Ready()
}

// nativeRemoteHost checks ownership independently of the transport token. A
// valid old token cannot reopen media for a ended or replaced call.
func (a *app) nativeRemoteHost() *remoteNativeHost {
	a.remoteSessionMu.Lock()
	handle := a.remoteSession
	a.remoteSessionMu.Unlock()
	if handle == nil || handle.native == nil || handle.session.Snapshot().State == "ended" {
		return nil
	}
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if a.activeCall == nil || a.activeCall.UUID != handle.binding.CallUUID || a.activeCall.State != "active" || a.remoteCallOwner == nil || a.remoteCallOwner.binding != handle.binding {
		return nil
	}
	return handle.native
}

func nativeControlHandler(token string, resolve func(string) (*app, bool), sources ...*nativeEvents) http.Handler {
	events := &nativeEvents{}
	if len(sources) > 0 && sources[0] != nil {
		events = sources[0]
	}
	mux := http.NewServeMux()
	lookup := func(w http.ResponseWriter, r *http.Request) *app {
		a, ok := resolve(r.URL.Query().Get("device_id"))
		if !ok {
			writeError(w, http.StatusConflict, "native device unavailable")
			return nil
		}
		return a
	}
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				writeError(w, http.StatusUnauthorized, "native control credential required")
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /events", auth(events.serve))
	mux.HandleFunc("GET /config", auth(func(w http.ResponseWriter, r *http.Request) {
		a := lookup(w, r)
		if a == nil {
			return
		}
		config, err := a.buildAudioHostConfig()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if config.MediaMode == "remote" {
			host := a.nativeRemoteHost()
			if host == nil || !config.CallActive || config.CallUUID != host.binding.CallUUID {
				config.RouteReady = false
			} else {
				if !host.claim(r.Header.Get("X-DJOneHub-Host-ID")) {
					writeError(w, http.StatusConflict, "another native host owns this generation")
					return
				}
				binding := host.bridge.Binding()
				config.CallUUID, config.MediaGeneration = binding.CallUUID, binding.Generation
				deviceID := a.deviceID
				if deviceID == "" {
					deviceID = "_single"
				}
				config.RemoteMediaURL = "/media/" + url.PathEscape(deviceID) + "/" + url.PathEscape(binding.CallUUID)
				config.RemoteMediaToken = binding.Token
			}
		}
		writeJSON(w, http.StatusOK, config)
	}))
	mux.HandleFunc("POST /status", auth(func(w http.ResponseWriter, r *http.Request) {
		a := lookup(w, r)
		if a == nil {
			return
		}
		host := a.nativeRemoteHost()
		if host == nil {
			writeError(w, http.StatusConflict, "remote native generation ended")
			return
		}
		var report nativeHostReport
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			writeError(w, http.StatusBadRequest, "invalid native report")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid native report")
			return
		}
		if err := host.update(r.Header.Get("X-DJOneHub-Host-ID"), report, time.Now()); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		a.audioHostMu.Lock()
		a.audioHost.Registered, a.audioHost.Running = true, report.Running
		a.audioHost.LastSeen = time.Now()
		a.audioHostMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
	}))
	// Media uses its per-call token, never the bootstrap control credential.
	mux.HandleFunc("GET /media/{deviceID}/{callUUID}", func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("deviceID")
		if deviceID == "_single" {
			deviceID = ""
		}
		a, ok := resolve(deviceID)
		if !ok {
			writeError(w, http.StatusConflict, "native device unavailable")
			return
		}
		host := a.nativeRemoteHost()
		if host == nil || host.binding.CallUUID != r.PathValue("callUUID") {
			writeError(w, http.StatusConflict, "remote native generation ended")
			return
		}
		host.bridge.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "native loopback client required")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Bootstrap is written atomically in a private directory. No token is passed
// in argv, URL, public config or logs. Restart rotates both port and token.
func startNativeControl(path string, resolve func(string) (*app, bool), sources ...*nativeEvents) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("native control file must be absolute")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("native control directory must be owned by this user with mode 0700")
	}
	if old, err := os.Lstat(path); err == nil {
		if !old.Mode().IsRegular() || old.Mode().Perm()&0077 != 0 {
			return nil, errors.New("unsafe existing native control file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Serialize both rotation and removal across server processes. A second
	// process must not replace the bootstrap of a still-running listener.
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	locked := false
	defer func() {
		if !locked {
			lock.Close()
		}
	}()
	lockInfo, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	lockStat, ok := lockInfo.Sys().(*syscall.Stat_t)
	if !ok || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0077 != 0 || int(lockStat.Uid) != os.Getuid() {
		return nil, errors.New("unsafe native control lock file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("native control file is already in use")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		listener.Close()
		return nil, err
	}
	bootstrap := nativeBootstrap{1, "http://" + listener.Addr().String(), hex.EncodeToString(secret)}
	data, _ := json.Marshal(bootstrap)
	file, err := os.CreateTemp(dir, ".native-control-*")
	if err != nil {
		listener.Close()
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		listener.Close()
		return nil, err
	}
	events := &nativeEvents{}
	if len(sources) > 0 && sources[0] != nil {
		events = sources[0]
	}
	server := &http.Server{Handler: nativeControlHandler(bootstrap.Token, resolve, events), ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	go func() { _ = server.Serve(listener) }()
	locked = true
	var once sync.Once
	return func() {
		once.Do(func() {
			events.close()
			_ = server.Close()
			// Do not delete a newer process's rotated bootstrap.
			if current, err := os.ReadFile(path); err == nil && string(current) == string(data) {
				_ = os.Remove(path)
			}
			_ = syscall.Flock(fd, syscall.LOCK_UN)
			_ = lock.Close()
		})
	}, nil
}
