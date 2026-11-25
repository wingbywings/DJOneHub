package main

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
	"github.com/iniwex5/vohive/internal/relayclient"
)

func (h *usbDeviceHub) remoteModuleRoutes(ids []string) []callcontrol.ModuleRouteEntry {
	result := make([]callcontrol.ModuleRouteEntry, 0, len(ids))
	for _, id := range ids {
		entry := callcontrol.ModuleRouteEntry{ID: id}
		a, ok := h.runtime(id)
		if !ok {
			result = append(result, entry)
			continue
		}
		a.callRouteMu.Lock()
		config, _ := a.ensureRemoteCalls().snapshot()
		agent := a.ensureVoiceAgent().snapshot()
		route := callcontrol.SelectRoute(agent.Enabled, agent.Revision, config)
		a.callRouteMu.Unlock()
		entry.Known = true
		entry.EffectiveRoute = string(route.Mode)
		entry.TargetInstallationID = route.TargetInstallationID
		entry.AIEnabled = agent.Enabled
		entry.RemoteEnabled = config.Enabled
		result = append(result, entry)
	}
	return result
}

func publishRemoteModuleRoutes(parent context.Context, done <-chan struct{}, origin string, options relayclient.Options, access func() string, routes func() []callcontrol.ModuleRouteEntry) {
	publishRemoteModuleRoutesEvery(parent, done, origin, options, access, routes, 15*time.Second)
}

func publishRemoteModuleRoutesEvery(parent context.Context, done <-chan struct{}, origin string, options relayclient.Options, access func() string, routes func() []callcontrol.ModuleRouteEntry, interval time.Duration) {
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
		_ = client.PublishModuleRoutes(request, access(), routes())
		stop()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
