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
	if body.Type == "synced" {
		return []voiceagent.Event{{Type: voiceagent.EventSessionReady}}, nil
	}
	if body.Type != "output" {
		return nil, nil
	}
	pcm, err := base64.StdEncoding.DecodeString(body.Audio)
	return []voiceagent.Event{{Type: voiceagent.EventAudio, Audio: pcm}}, err
}

type openingTestDialect struct{ testDialect }

func (openingTestDialect) OpeningEvents(config voiceagent.SessionConfig) []any {
	return []any{
		map[string]any{"type": "conversation.item.create", "prompt": config.OpeningPrompt},
		map[string]any{"type": "response.create"},
	}
}

func TestProviderStartsOpeningEventsInOrderOnlyWhenMediaIsReady(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan []map[string]any, 1)
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
		opening := make([]map[string]any, 2)
		if err := conn.ReadJSON(&opening[0]); err != nil {
			return
		}
		if err := conn.ReadJSON(&opening[1]); err == nil {
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
	case events := <-received:
		t.Fatalf("opening events were sent before Start: %#v", events)
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
	case events := <-received:
		if events[0]["type"] != "conversation.item.create" || events[0]["prompt"] != "greet now" || events[1]["type"] != "response.create" {
			t.Fatalf("opening events = %#v", events)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for opening response")
	}
}

type toolTestDialect struct{ testDialect }

func (toolTestDialect) ToolResult(callID string, output any) []any {
	return []any{
		map[string]any{"type": "conversation.item.create", "call_id": callID, "output": output},
		map[string]any{"type": "response.create"},
	}
}

func TestToolFollowUpWaitsForCurrentResponseDone(t *testing.T) {
	upgrader := websocket.Upgrader{}
	itemReceived := make(chan map[string]any, 1)
	responseReceived := make(chan map[string]any, 1)
	sendDone := make(chan struct{})
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
		if err := conn.WriteJSON(map[string]any{"type": "response.created"}); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "synced"}); err != nil {
			return
		}
		var item map[string]any
		if err := conn.ReadJSON(&item); err != nil {
			return
		}
		itemReceived <- item
		go func() {
			<-sendDone
			_ = conn.WriteJSON(map[string]any{"type": "response.done"})
		}()
		var response map[string]any
		if err := conn.ReadJSON(&response); err == nil {
			responseReceived <- response
		}
	}))
	defer server.Close()
	provider, err := NewProvider(Config{Name: "test", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "key", Dialect: toolTestDialect{}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	select {
	case event := <-session.Events():
		if event.Type != voiceagent.EventSessionReady {
			t.Fatalf("sync event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response state sync")
	}
	if err := session.SubmitToolResult(context.Background(), "call-1", map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-itemReceived:
		if item["type"] != "conversation.item.create" {
			t.Fatalf("tool item = %#v", item)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool result item")
	}
	select {
	case response := <-responseReceived:
		t.Fatalf("follow-up response was sent before response.done: %#v", response)
	case <-time.After(50 * time.Millisecond):
	}
	close(sendDone)
	select {
	case response := <-responseReceived:
		if response["type"] != "response.create" {
			t.Fatalf("follow-up response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queued follow-up response")
	}
}

func TestOpeningIsSkippedAfterCallerSpeechStarts(t *testing.T) {
	s := &session{
		done:          make(chan struct{}),
		openingEvents: []any{map[string]any{"type": "response.create"}},
	}
	if err := s.observeServerEvent([]byte(`{"type":"input_audio_buffer.speech_started"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.started {
		t.Fatal("opening was not marked handled after caller speech")
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
