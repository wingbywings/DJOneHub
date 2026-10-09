package remotemedia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHostBridgeAccessBoundary(t *testing.T) {
	bridge, _ := NewHostBridge("call", 1)
	defer bridge.Close()
	for _, test := range []struct {
		address, origin, auth string
		status                int
	}{
		{"192.0.2.1:10", "", "Bearer " + bridge.Binding().Token, 403},
		{"127.0.0.1:10", "https://example.com", "Bearer " + bridge.Binding().Token, 403},
		{"[::1]:10", "", "Bearer invalid", 401},
		{"127.0.0.1:10", "", "", 401},
	} {
		r := httptest.NewRequest("GET", "http://localhost/media", nil)
		r.RemoteAddr = test.address
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Authorization", test.auth)
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		bridge.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%+v: %d", test, w.Code)
		}
	}
}
func TestHostBridgeBoundDuplexAndReplay(t *testing.T) {
	bridge, _ := NewHostBridge("call-uuid", 7)
	defer bridge.Close()
	server := httptest.NewServer(bridge)
	defer server.Close()
	headers := http.Header{"Authorization": []string{"Bearer " + bridge.Binding().Token}}
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var ready struct {
		Type       string
		CallUUID   string `json:"call_uuid"`
		Generation uint64
	}
	if err := conn.ReadJSON(&ready); err != nil || ready.Type != "ready" || ready.CallUUID != "call-uuid" || ready.Generation != 7 {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	duplicate, response, err := websocket.DefaultDialer.Dial(url, headers)
	if duplicate != nil {
		duplicate.Close()
	}
	if err == nil || response.StatusCode != 409 {
		t.Fatal("second host accepted")
	}
	response.Body.Close()
	frame := Frame{Direction: FromModem, Generation: 7, Sequence: 1, SampleIndex: 80}
	frame.PCM[0] = 1234
	data, _ := frame.MarshalBinary()
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := bridge.Receive(ctx)
	if err != nil || got != frame {
		t.Fatalf("receive=%+v err=%v", got, err)
	}
	frame.Direction = ToModem
	frame.PCM[0] = -2345
	if err := bridge.Send(frame); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseFrame(data)
	if err != nil || got != frame {
		t.Fatalf("output=%+v err=%v", got, err)
	}
	// A stale generation is fatal; this single-use bridge cannot resurrect.
	frame.Direction = FromModem
	frame.Generation = 6
	data, _ = frame.MarshalBinary()
	_ = conn.WriteMessage(websocket.BinaryMessage, data)
	if _, _, err = conn.ReadMessage(); err == nil {
		t.Fatal("stale generation did not close bridge")
	}
	if bridge.Ready() {
		t.Fatal("closed bridge reported ready")
	}
	if err := bridge.Send(frame); err != ErrBridgeClosed {
		t.Fatalf("closed send=%v", err)
	}
}
