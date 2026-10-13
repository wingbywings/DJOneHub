package main

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func (h *usbDeviceHub) remoteModuleNames(ids []string) []identity.ModuleChoice {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]identity.ModuleChoice, 0, len(ids))
	for _, id := range ids {
		name := h.aliases[id]
		if device := h.devices[id]; device != nil {
			name = device.Alias
		}
		result = append(result, identity.ModuleChoice{ID: id, Alias: remoteModuleAlias(name)})
	}
	return result
}

// Legacy aliases may contain formatting/control characters. Never let one
// invalid display name block publication of all authorized modules.
func remoteModuleAlias(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return string(runes)
}

func publishRemoteModuleNames(parent context.Context, done <-chan struct{}, origin string, options relayclient.Options, access func() string, names func() []identity.ModuleChoice) {
	publishRemoteModuleNamesEvery(parent, done, origin, options, access, names, 15*time.Second)
}

func publishRemoteModuleNamesEvery(parent context.Context, done <-chan struct{}, origin string, options relayclient.Options, access func() string, names func() []identity.ModuleChoice, interval time.Duration) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	client, err := relayclient.New(origin, options)
	if err != nil {
		return
	}
	defer client.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		request, stop := context.WithTimeout(ctx, 3*time.Second)
		_ = client.PublishModuleNames(request, access(), names())
		stop()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
