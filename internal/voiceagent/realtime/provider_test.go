package realtime

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
)

type testDialect struct{}

func (testDialect) SessionUpdate(voiceagent.SessionConfig) any {
	return map[string]any{"type": "session.update"}
}
func (testDialect) InputAudio(audio string) any {
	return map[string]any{"type": "audio", "audio": audio}
}
func (testDialect) ToolResult(string, any) []any { return nil }
func (testDialect) ParseEvent(raw []byte) ([]voiceagent.Event, error) {
	var body struct{ Type, Audio string }
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body.Type != "output" {
		return nil, nil
	}
	pcm, err := base64.StdEncoding.DecodeString(body.Audio)
	return []voiceagent.Event{{Type: voiceagent.EventAudio, Audio: pcm}}, err
}

type openingTestDialect struct{ testDialect }

func (openingTestDialect) OpeningResponse(config voiceagent.SessionConfig) any {
	return map[string]any{"type": "response.create", "prompt": config.OpeningPrompt}
}

func TestProviderStartsOpeningResponseOnlyWhenMediaIsReady(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan map[string]any, 1)
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
		var opening map[string]any
		if err := conn.ReadJSON(&opening); err == nil {
			received <- opening
		}
	}))
	defer server.Close()
	provider, err := NewProvider(Config{Name: "test", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "key", Dialect: openingTestDialect{}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{OpeningPrompt: "greet now"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	select {
	case event := <-received:
		t.Fatalf("opening response was sent before Start: %#v", event)
	case <-time.After(50 * time.Millisecond):
	}
	starter := session.(voiceagent.SessionStarter)
	if err := starter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := starter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-received:
		if event["type"] != "response.create" || event["prompt"] != "greet now" {
			t.Fatalf("opening event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for opening response")
	}
}

func TestProviderResamplesTelephoneAudioBothDirections(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan int, 1)
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
		raw, _ := base64.StdEncoding.DecodeString(input.Audio)
		received <- len(raw)
		output := make([]byte, 480*2)
		_ = conn.WriteJSON(map[string]any{"type": "output", "audio": base64.StdEncoding.EncodeToString(output)})
		<-time.After(50 * time.Millisecond)
	}))
	defer server.Close()
	provider, err := NewProvider(Config{
		Name: "test", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "key",
		InputSampleRate: 16000, OutputSampleRate: 24000, Dialect: testDialect{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.SendAudio(context.Background(), make([]byte, 160*2)); err != nil {
		t.Fatal(err)
	}
	if size := <-received; size != 640 {
		t.Fatalf("provider input bytes = %d, want 640", size)
	}
	select {
	case event := <-session.Events():
		if event.Type != voiceagent.EventAudio || len(event.Audio) != 320 {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for output audio")
	}
}

func TestResponseDoneAlsoMarksAudioDone(t *testing.T) {
	events, err := ParseCommonEvent(
		[]byte(`{"type":"response.done","response":{"usage":{"total_tokens":9}}}`),
		map[string]voiceagent.EventType{"response.done": voiceagent.EventUsage},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != voiceagent.EventAudioDone || events[1].Type != voiceagent.EventUsage {
		t.Fatalf("events = %#v", events)
	}
	if events[1].Usage["total_tokens"].(float64) != 9 {
		t.Fatalf("usage = %#v", events[1].Usage)
	}
}
