package qwen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func TestTranscriptionDialectKeepsTelephoneSampleRate(t *testing.T) {
	raw, err := json.Marshal((transcriptionDialect{}).SessionUpdate(voiceagent.SessionConfig{Language: "zh"}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, expected := range []string{`"sample_rate":8000`, `"input_audio_format":"pcm"`, `"type":"server_vad"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("session update %s does not contain %s", text, expected)
		}
	}
}

func TestTranscriptionDialectParsesPreviewAndFinal(t *testing.T) {
	dialect := transcriptionDialect{}
	events, err := dialect.ParseEvent([]byte(`{"type":"conversation.item.input_audio_transcription.text","text":"北京","stash":"天气"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "北京天气" || events[0].Final {
		t.Fatalf("preview = %#v", events)
	}
	events, err = dialect.ParseEvent([]byte(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"北京天气很好"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "北京天气很好" || !events[0].Final {
		t.Fatalf("final = %#v", events)
	}
}
