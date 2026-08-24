package openai

import (
	"encoding/json"

	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
	"github.com/iniwex5/vohive/internal/voiceagent/transcription"
)

type TranscriptionConfig struct{ APIKey, Endpoint, Model, Delay string }

func NewTranscriber(config TranscriptionConfig) (*transcription.Transcriber, error) {
	if config.Endpoint == "" {
		config.Endpoint = "wss://api.openai.com/v1/realtime"
	}
	if config.Model == "" {
		config.Model = "gpt-live-transcribe"
	}
	if config.Delay == "" {
		config.Delay = "low"
	}
	return transcription.New(transcription.Config{Name: "openai", Endpoint: config.Endpoint, APIKey: config.APIKey, InputSampleRate: 24000, DefaultModel: config.Model, Dialect: transcriptionDialect{model: config.Model, delay: config.Delay}})
}

type transcriptionDialect struct{ model, delay string }

func (d transcriptionDialect) SessionUpdate(config voiceagent.SessionConfig) any {
	language := config.Language
	if language == "zh" {
		language = "zh-cn"
	}
	model := config.Model
	if model == "" {
		model = d.model
	}
	settings := map[string]any{"model": model, "delay": d.delay}
	if language != "" {
		settings["languages"] = []string{language}
	}
	if config.Instructions != "" {
		settings["prompt"] = config.Instructions
	}
	return map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription", "audio": map[string]any{"input": map[string]any{
		"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "transcription": settings,
		"turn_detection": map[string]any{"type": "server_vad", "threshold": 0.5, "prefix_padding_ms": 300, "silence_duration_ms": 500},
	}}}}
}
func (transcriptionDialect) InputAudio(audio string) any {
	return map[string]any{"type": "input_audio_buffer.append", "audio": audio}
}
func (transcriptionDialect) Finish() any { return nil }
func (transcriptionDialect) ParseEvent(raw []byte) ([]cascade.Transcript, error) {
	var event struct {
		Type, Delta, Transcript string
		Error                   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	switch event.Type {
	case "input_audio_buffer.speech_started":
		return []cascade.Transcript{{EventType: voiceagent.EventSpeechStarted}}, nil
	case "input_audio_buffer.speech_stopped":
		return []cascade.Transcript{{EventType: voiceagent.EventSpeechStopped}}, nil
	case "conversation.item.input_audio_transcription.delta":
		return []cascade.Transcript{{Text: event.Delta}}, nil
	case "conversation.item.input_audio_transcription.completed":
		return []cascade.Transcript{{Text: event.Transcript, Final: true}}, nil
	case "error", "conversation.item.input_audio_transcription.failed":
		return []cascade.Transcript{{Err: providerEventError(event.Error)}}, nil
	default:
		return nil, nil
	}
}

func providerEventError(raw json.RawMessage) error {
	if len(raw) == 0 {
		return &eventError{message: "unknown provider error"}
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Message == "" {
		body.Message = string(raw)
	}
	return &eventError{message: body.Message}
}

type eventError struct{ message string }

func (e *eventError) Error() string { return e.message }
