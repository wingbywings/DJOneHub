package qwen

import (
	"encoding/json"

	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
	"github.com/iniwex5/vohive/internal/voiceagent/transcription"
)

type TranscriptionConfig struct{ APIKey, Endpoint, Model string }

func NewTranscriber(config TranscriptionConfig) (*transcription.Transcriber, error) {
	if config.Endpoint == "" {
		config.Endpoint = "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime"
	}
	if config.Model == "" {
		config.Model = "qwen3-asr-flash-realtime"
	}
	return transcription.New(transcription.Config{Name: "qwen", Endpoint: config.Endpoint, APIKey: config.APIKey, InputSampleRate: 8000, DefaultModel: config.Model, Dialect: transcriptionDialect{}})
}

type transcriptionDialect struct{}

func (transcriptionDialect) SessionUpdate(config voiceagent.SessionConfig) any {
	language := config.Language
	if language == "" {
		language = "zh"
	}
	transcriptionSettings := map[string]any{"language": language}
	if config.Instructions != "" {
		transcriptionSettings["corpus"] = map[string]any{"text": config.Instructions}
	}
	return map[string]any{"type": "session.update", "session": map[string]any{"input_audio_format": "pcm", "sample_rate": 8000, "input_audio_transcription": transcriptionSettings, "turn_detection": map[string]any{"type": "server_vad", "threshold": 0.0, "silence_duration_ms": 400}}}
}
func (transcriptionDialect) InputAudio(audio string) any {
	return map[string]any{"type": "input_audio_buffer.append", "audio": audio}
}
func (transcriptionDialect) Finish() any { return map[string]any{"type": "session.finish"} }
func (transcriptionDialect) ParseEvent(raw []byte) ([]cascade.Transcript, error) {
	var event struct {
		Type, Text, Stash, Transcript string
		Error                         struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	switch event.Type {
	case "input_audio_buffer.speech_started":
		return []cascade.Transcript{{EventType: voiceagent.EventSpeechStarted}}, nil
	case "input_audio_buffer.speech_stopped":
		return []cascade.Transcript{{EventType: voiceagent.EventSpeechStopped}}, nil
	case "conversation.item.input_audio_transcription.text":
		return []cascade.Transcript{{Text: event.Text + event.Stash}}, nil
	case "conversation.item.input_audio_transcription.completed":
		return []cascade.Transcript{{Text: event.Transcript, Final: true}}, nil
	case "error", "conversation.item.input_audio_transcription.failed":
		if event.Error.Message == "" {
			event.Error.Message = "unknown Qwen STT error"
		}
		return []cascade.Transcript{{Err: &qwenSTTError{event.Error.Message}}}, nil
	default:
		return nil, nil
	}
}

type qwenSTTError struct{ message string }

func (e *qwenSTTError) Error() string { return e.message }
