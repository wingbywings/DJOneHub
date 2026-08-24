package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func TestTranscriptionDialectUsesCurrentSessionSchema(t *testing.T) {
	dialect := transcriptionDialect{model: "gpt-live-transcribe", delay: "low"}
	raw, err := json.Marshal(dialect.SessionUpdate(voiceagent.SessionConfig{Language: "zh", Instructions: "客服通话"}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, expected := range []string{
		`"type":"transcription"`, `"rate":24000`, `"languages":["zh-cn"]`,
		`"model":"gpt-live-transcribe"`, `"noise_reduction":{"type":"near_field"}`,
		`"threshold":0.65`, `"prefix_padding_ms":400`, `"silence_duration_ms":700`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("session update %s does not contain %s", text, expected)
		}
	}
}

func TestTranscriptionDialectParsesDeltaAndFinal(t *testing.T) {
	dialect := transcriptionDialect{}
	events, err := dialect.ParseEvent([]byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":"你"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "你" || events[0].Final {
		t.Fatalf("delta = %#v", events)
	}
	events, err = dialect.ParseEvent([]byte(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"你好"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "你好" || !events[0].Final {
		t.Fatalf("final = %#v", events)
	}
}
