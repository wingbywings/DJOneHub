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
