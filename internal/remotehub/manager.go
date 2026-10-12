// Package remotehub owns the Hub's online signaling connection. Calls are never
// queued across disconnects, and a consumed connection is retired after cleanup.
package remotehub

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
)

type Connection interface {
	Done() <-chan struct{}
	Messages() <-chan callsignal.Message
	Close()
	MaintainAuthorization(context.Context, func(context.Context) (string, error)) error
}

type Config struct {
	RecoverCalls bool // active runner owns bounded reconnect; idle loss still reconnects normally
	Connect      func(context.Context) (Connection, error)
	Refresh      func(context.Context) (string, error)
	// Only explicitly authorized idle requests may create outgoing work.
	Incoming func(context.Context, Connection, callsignal.Message)
}

type Snapshot struct {
	State string `json:"state"` // connecting, online, busy, retrying, stopped
}

type job struct {
	ctx context.Context
	run func(context.Context, Connection)
}

type Manager struct {
	mu         sync.Mutex
	state      string
	connection Connection
	jobs       chan job
	done       chan struct{}
	cancel     context.CancelFunc
}

func Start(parent context.Context, config Config) *Manager {
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{state: "connecting", jobs: make(chan job, 1), done: make(chan struct{}), cancel: cancel}
	go m.loop(ctx, config)
	return m
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{State: m.state}
}

// TryStart reserves the sole live connection without blocking call polling.
// The caller records the attempt for this call UUID even when rejected, so a
// later reconnect never rings an old call. run must honor context and complete
// bounded cellular/media cleanup before returning.
func (m *Manager) TryStart(ctx context.Context, run func(context.Context, Connection)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || run == nil || m.state != "online" || m.connection == nil {
		return false
	}
	select {
	case <-m.connection.Done():
		return false
	default:
	}
	m.state = "busy"
	m.jobs <- job{ctx: ctx, run: run}
	return true
}

func (m *Manager) Close() { m.cancel(); <-m.done }

func (m *Manager) set(state string, c Connection) {
	m.mu.Lock()
	m.state, m.connection = state, c
	m.mu.Unlock()
}

func (m *Manager) loop(ctx context.Context, config Config) {
	defer close(m.done)
	defer m.set("stopped", nil)
	if config.Connect == nil || config.Refresh == nil {
		return
	}
	delay := time.Second
	for ctx.Err() == nil {
		m.set("connecting", nil)
		// Connect implementations must bound credential IO and handshake time,
		// while the established socket uses ctx as its lifetime.
		c, err := config.Connect(ctx)
		if err == nil && c != nil {
			started := time.Now()
			used := m.serve(ctx, c, config.Refresh, config.Incoming, config.RecoverCalls)
			if used || time.Since(started) >= 30*time.Second {
				delay = time.Second
			}
		} else if c != nil {
			c.Close()
		}
		m.set("retrying", nil)
		// Jitter avoids synchronized reconnects after a relay restart.
		timer := time.NewTimer(delay + time.Duration(rand.Int64N(int64(delay/2))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (m *Manager) serve(ctx context.Context, c Connection, refresh func(context.Context) (string, error), incoming func(context.Context, Connection, callsignal.Message), recoverCalls bool) (used bool) {
	authCtx, stopAuth := context.WithCancel(ctx)
	authDone := make(chan struct{})
	go func() { defer close(authDone); _ = c.MaintainAuthorization(authCtx, refresh); c.Close() }()
	defer func() {
		m.set("retrying", nil)
		c.Close()
		stopAuth()
		<-authDone
		// A reservation racing disconnect is discarded, never replayed.
		select {
		case <-m.jobs:
		default:
		}
	}()
	m.set("online", c)
	var j job
	select {
	case <-ctx.Done():
		return false
	case <-c.Done():
		return false
	case message, ok := <-c.Messages():
		if !ok || incoming == nil || message.Type != "dial" {
			return false
		}
		m.mu.Lock()
		if m.state != "online" {
			m.mu.Unlock()
			return false
		}
		m.state = "busy"
		m.mu.Unlock()
		j = job{ctx: ctx, run: func(ctx context.Context, c Connection) { incoming(ctx, c, message) }}
	case j = <-m.jobs:
	}
	{
		used = true
		callCtx, cancel := context.WithCancel(j.ctx)
		stop := context.AfterFunc(ctx, cancel)
		// Propagate signaling loss even if a caller's runner waits on context.
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-c.Done():
				if !recoverCalls {
					cancel()
				}
			case <-callCtx.Done():
			}
		}()
		if ctx.Err() == nil && callCtx.Err() == nil {
			j.run(callCtx, c)
		}
		cancel()
		stop()
		<-watchDone
	}
	return used
}
