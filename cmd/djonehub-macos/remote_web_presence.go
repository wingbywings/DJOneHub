package main

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayprofile"
)

type webPresence struct {
	Profile       string                     `json:"profile"`
	State         string                     `json:"state"`
	CheckedAt     time.Time                  `json:"checked_at"`
	Installations []identity.ManagedIdentity `json:"installations"`
}

func (m *remoteWebManager) watchPresence(profile string, observer *relayprofile.InstallationObserver, items []identity.ManagedIdentity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		observer.Close()
		return
	}
	if m.presenceCancel != nil {
		m.presenceCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.presenceCancel = cancel
	generation := webID("presence_")
	m.presenceGeneration = generation
	m.presence = webPresence{Profile: profile, State: "fresh", CheckedAt: time.Now().UTC(), Installations: items}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer observer.Close()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				check, stop := context.WithTimeout(ctx, 3*time.Second)
				items, err := observer.Read(check)
				stop()
				m.mu.Lock()
				if m.presenceGeneration != generation || m.closed {
					m.mu.Unlock()
					return
				}
				if err != nil {
					m.presence.State = "unknown"
					if observer.Expired() {
						m.presence.State = "expired"
					}
					m.presence.Installations = nil
				} else {
					m.presence = webPresence{Profile: profile, State: "fresh", CheckedAt: time.Now().UTC(), Installations: items}
				}
				m.mu.Unlock()
				if err != nil && observer.Expired() {
					return
				} // Explicit reread reauthorizes; no background Vault access.
			}
		}
	}()
}
func (m *remoteWebManager) presenceSnapshot(profile string) webPresence {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.presence.Profile != profile || (m.presence.State == "fresh" && time.Since(m.presence.CheckedAt) > 12*time.Second) {
		return webPresence{Profile: profile, State: "unknown"}
	}
	return m.presence
}
