package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func TestOpeningResponseRequestsAudio(t *testing.T) {
	raw, err := json.Marshal((dialect{}).OpeningResponse(voiceagent.SessionConfig{OpeningPrompt: "主动问候"}))
	if err != nil {
		t.Fatal(err)
	}
	value := string(raw)
	for _, expected := range []string{`"type":"response.create"`, `"output_modalities":["audio"]`, `"instructions":"主动问候"`} {
		if !strings.Contains(value, expected) {
			t.Fatalf("opening response %s does not contain %s", value, expected)
		}
	}
}

func TestSessionUpdateOptimizesMandarinTelephoneRecognition(t *testing.T) {
	raw, err := json.Marshal((dialect{}).SessionUpdate(voiceagent.SessionConfig{
		Language: "zh", Instructions: "询问来意", TranscriptionModel: "qwen-invalid-model",
	}))
	if err != nil {
		t.Fatal(err)
	}
	value := string(raw)
	for _, expected := range []string{
		`"language":"zh"`,
		`"model":"gpt-4o-transcribe"`,
		`"noise_reduction":{"type":"near_field"}`,
		`"threshold":0.35`,
		`普通话中文`,
		`简体中文`,
	} {
		if !strings.Contains(value, expected) {
			t.Fatalf("session update %s does not contain %s", value, expected)
		}
	}
}
