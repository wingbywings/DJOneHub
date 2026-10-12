package remotemedia

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var ErrBridgeClosed = errors.New("remote media bridge is closed")
var ErrBridgeNotReady = errors.New("remote media host is not ready")

type BridgeBinding struct {
	CallUUID   string
	Generation uint64
	Token      string
}
type timedFrame struct {
	frame Frame
	at    time.Time
}

// HostBridge is a single-use loopback PCM transport. The future call coordinator
// must create it only after granting remote ownership, and Close on call end.
// It is intentionally not registered on the public/local app router yet.
// Reconnect requires a new bridge, token and media generation, never replay.
type HostBridge struct {
	mu                 sync.Mutex
	binding            BridgeBinding
	conn               *websocket.Conn
	used, closed       bool
	done               chan struct{}
	incoming, outgoing chan timedFrame
	outgoingGuard      FrameGuard
}

func NewHostBridge(callUUID string, generation uint64) (*HostBridge, error) {
	if callUUID == "" || len(callUUID) > 128 || generation == 0 {
		return nil, ErrFrame
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	return &HostBridge{binding: BridgeBinding{callUUID, generation, hex.EncodeToString(token)}, done: make(chan struct{}), incoming: make(chan timedFrame, 6), outgoing: make(chan timedFrame, 6), outgoingGuard: FrameGuard{Generation: generation, Direction: ToModem}}, nil
}

// Binding contains a secret. Only send it on an authenticated loopback control
// channel; do not expose it in ordinary status, browser config or logs.
func (b *HostBridge) Binding() BridgeBinding { return b.binding }
func (b *HostBridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.done)
	if b.conn != nil {
		_ = b.conn.Close()
	}
}
func (b *HostBridge) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil && !b.closed
}

// Drop queued PCM without resetting generation or anti-replay guards.
func (b *HostBridge) DiscardQueuedAudio() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, queue := range []chan timedFrame{b.incoming, b.outgoing} {
		for n := 0; n < cap(queue); n++ {
			select {
			case <-queue:
			default:
			}
		}
	}
}
func (b *HostBridge) Receive(ctx context.Context) (Frame, error) {
	for {
		select {
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		case <-b.done:
			return Frame{}, ErrBridgeClosed
		case queued := <-b.incoming:
			if time.Since(queued.at) > 60*time.Millisecond {
				continue
			}
			select {
			case <-b.done:
				return Frame{}, ErrBridgeClosed
			default:
				return queued.frame, nil
			}
		}
	}
}
func (b *HostBridge) Send(frame Frame) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBridgeClosed
	}
	if b.conn == nil {
		return ErrBridgeNotReady
	}
	if err := b.outgoingGuard.Accept(frame); err != nil {
		return err
	}
	offerLatest(b.outgoing, timedFrame{frame, time.Now()})
	return nil
}

// Caller is the only producer for each channel (Send serializes its writers).
func offerLatest(queue chan timedFrame, frame timedFrame) {
	select {
	case queue <- frame:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- frame:
	default:
	}
}
func (b *HostBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || r.Header.Get("Origin") != "" {
		http.Error(w, "loopback native host required", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.binding.Token)) != 1 {
		http.Error(w, "invalid host credential", http.StatusUnauthorized)
		return
	}
	b.mu.Lock()
	if b.used || b.closed {
		b.mu.Unlock()
		http.Error(w, "media generation expired", http.StatusConflict)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		b.mu.Unlock()
		return
	}
	b.used = true
	b.conn = conn
	b.mu.Unlock()
	defer b.Close()
	conn.SetReadLimit(FrameSize)
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if err = conn.WriteJSON(map[string]any{"type": "ready", "version": 1, "call_uuid": b.binding.CallUUID, "generation": b.binding.Generation, "sample_rate": SampleRate, "frame_samples": FrameSamples}); err != nil {
		return
	}
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); defer b.Close(); b.write(conn) }()
	defer func() { b.Close(); <-writerDone }()
	guard := FrameGuard{Generation: b.binding.Generation, Direction: FromModem}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.TextMessage {
			if string(data) == `{"type":"health"}` {
				continue
			}
			return
		}
		if kind != websocket.BinaryMessage {
			return
		}
		frame, err := ParseFrame(data)
		if err != nil || guard.Accept(frame) != nil {
			return
		}
		offerLatest(b.incoming, timedFrame{frame, time.Now()})
	}
}
func (b *HostBridge) write(conn *websocket.Conn) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-tick.C:
			// One PCM frame per real clock tick; never drain a backlog in a burst.
			for {
				select {
				case frame := <-b.outgoing:
					if time.Since(frame.at) > 60*time.Millisecond {
						continue
					}
					data, _ := frame.frame.MarshalBinary()
					_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
					if conn.WriteMessage(websocket.BinaryMessage, data) != nil {
						return
					}
				default:
				}
				break
			}
		}
	}
}
