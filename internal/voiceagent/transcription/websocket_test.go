package transcription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
)

type testDialect struct{}

func (testDialect) SessionUpdate(voiceagent.SessionConfig) any {
	return map[string]any{"type": "session.update"}
}
func (testDialect) InputAudio(audio string) any {
	return map[string]any{"type": "audio", "audio": audio}
}
func (testDialect) Finish() any { return nil }
func (testDialect) ParseEvent(raw []byte) ([]cascade.Transcript, error) {
	var body struct{ Type, Text string }
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body.Type != "final" {
		return nil, nil
	}
	return []cascade.Transcript{{Text: body.Text, Final: true}}, nil
}

func TestWebSocketTranscriberResamplesAndEmitsFinal(t *testing.T) {
	upgrader := websocket.Upgrader{}
	inputBytes := make(chan int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var update map[string]any
		if err := conn.ReadJSON(&update); err != nil {
			return
		}
		var input struct {
			Audio string `json:"audio"`
		}
		if err := conn.ReadJSON(&input); err != nil {
			return
		}
		audio, _ := base64.StdEncoding.DecodeString(input.Audio)
		inputBytes <- len(audio)
		_ = conn.WriteJSON(map[string]any{"type": "final", "text": "你好"})
		time.Sleep(30 * time.Millisecond)
	}))
	defer server.Close()
	transcriber, err := New(Config{Name: "test", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "key", InputSampleRate: 24000, Dialect: testDialect{}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := transcriber.Open(context.Background(), voiceagent.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.SendAudio(context.Background(), make([]byte, 160*2)); err != nil {
		t.Fatal(err)
	}
	if size := <-inputBytes; size != 960 {
		t.Fatalf("input bytes = %d, want 960", size)
	}
	select {
	case event := <-session.Events():
		if !event.Final || event.Text != "你好" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final transcript")
	}
}
