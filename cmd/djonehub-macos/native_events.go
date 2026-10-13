package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Events are invalidation hints, never configuration, commands or credentials.
// One pending hint per subscriber coalesces bursts without blocking call work.
type nativeEvents struct {
	webTicket   func() (string, error) // immutable before native listener starts
	done        chan struct{}
	closed      bool
	mu          sync.Mutex
	revision    uint64
	subscribers map[chan struct{}]struct{}
}

func (e *nativeEvents) publish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revision++
	for ch := range e.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (e *nativeEvents) subscribe() (chan struct{}, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || len(e.subscribers) >= 8 {
		return nil, false
	}
	if e.done == nil {
		e.done = make(chan struct{})
	}
	if e.subscribers == nil {
		e.subscribers = make(map[chan struct{}]struct{})
	}
	ch := make(chan struct{}, 1)
	e.subscribers[ch] = struct{}{}
	ch <- struct{}{}
	return ch, true
}
func (e *nativeEvents) unsubscribe(ch chan struct{}) {
	e.mu.Lock()
	delete(e.subscribers, ch)
	e.mu.Unlock()
}
func (e *nativeEvents) shutdown() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done == nil {
		e.done = make(chan struct{})
	}
	return e.done
}
func (e *nativeEvents) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	if e.done == nil {
		e.done = make(chan struct{})
	}
	close(e.done)
}
func (e *nativeEvents) current() uint64 { e.mu.Lock(); defer e.mu.Unlock(); return e.revision }
func (a *app) notifyNative() {
	if a.nativeEvents != nil {
		a.nativeEvents.publish()
	}
}

// Caller applies bootstrap authentication and loopback/origin checks before
// this handler. An initial hint forces a full refresh on every new connection.
func (e *nativeEvents) serve(w http.ResponseWriter, r *http.Request) {
	if id := r.Header.Get("X-DJOneHub-Host-ID"); id == "" || len(id) > 128 {
		writeError(w, http.StatusBadRequest, "native host id required")
		return
	}
	ch, ok := e.subscribe()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "native event subscriber limit")
		return
	}
	defer e.unsubscribe(ch)
	u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn.SetReadLimit(1024)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(30 * time.Second)) })
	go func() { defer cancel(); _, _, _ = conn.ReadMessage() }() // no client application messages
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	done := e.shutdown()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ch:
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if conn.WriteJSON(struct {
				Type     string `json:"type"`
				Revision uint64 `json:"revision"`
			}{"changed", e.current()}) != nil {
				return
			}
			// Bound update rate even when many module transitions happen together.
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case <-heartbeat.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
				return
			}
		}
	}
}
