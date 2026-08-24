package qwen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func TestSessionUpdateUsesQwenPCMFormat(t *testing.T) {
	raw, err := json.Marshal((dialect{}).SessionUpdate(voiceagent.SessionConfig{
		Language: "zh", Instructions: "询问来意",
	}))
	if err != nil {
		t.Fatal(err)
	}
	value := string(raw)
	for _, expected := range []string{
		`"input_audio_format":"pcm"`,
		`"output_audio_format":"pcm"`,
		`"turn_detection":{"type":"smart_turn"}`,
		`每轮语音尽量控制在 20 秒内`,
	} {
		if !strings.Contains(value, expected) {
			t.Fatalf("session update %s does not contain %s", value, expected)
		}
	}
	if strings.Contains(value, `"pcm16"`) {
		t.Fatalf("session update uses OpenAI format spelling: %s", value)
	}
}

func TestOpeningEventsInjectUserMessageBeforeCreatingResponse(t *testing.T) {
	events := (dialect{}).OpeningEvents(voiceagent.SessionConfig{OpeningPrompt: "主动问候"})
	if len(events) != 2 {
		t.Fatalf("opening events = %#v, want conversation item and response", events)
	}
	item, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(events[1])
	if err != nil {
		t.Fatal(err)
	}
	itemJSON := string(item)
	for _, expected := range []string{
		`"type":"conversation.item.create"`,
		`"type":"message"`,
		`"role":"user"`,
		`"type":"input_text"`,
		`"text":"主动问候"`,
	} {
		if !strings.Contains(itemJSON, expected) {
			t.Fatalf("opening item %s does not contain %s", itemJSON, expected)
		}
	}
	responseJSON := string(response)
	if !strings.Contains(responseJSON, `"type":"response.create"`) {
		t.Fatalf("opening response = %s", responseJSON)
	}
	if strings.Contains(responseJSON, `"instructions"`) {
		t.Fatalf("Qwen response.create cannot carry the injected user prompt: %s", responseJSON)
	}
}
