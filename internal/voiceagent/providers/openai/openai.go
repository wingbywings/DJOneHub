package openai

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/realtime"
)

type Config struct {
	APIKey           string
	Endpoint         string
	Model            string
	SafetyIdentifier string
}

func New(config Config) (*realtime.Provider, error) {
	if config.Endpoint == "" {
		config.Endpoint = "wss://api.openai.com/v1/realtime"
	}
	if config.Model == "" {
		config.Model = "gpt-realtime-2.1-mini"
	}
	headers := make(http.Header)
	if config.SafetyIdentifier != "" {
		headers.Set("OpenAI-Safety-Identifier", config.SafetyIdentifier)
	}
	return realtime.NewProvider(realtime.Config{
		Name: "openai", Endpoint: config.Endpoint, APIKey: config.APIKey, Headers: headers,
		InputSampleRate: 24000, OutputSampleRate: 24000, DefaultModel: config.Model, Dialect: dialect{},
	})
}

type dialect struct{}

func (dialect) SessionUpdate(config voiceagent.SessionConfig) any {
	if config.Voice == "" {
		config.Voice = "marin"
	}
	transcriptionModel := strings.TrimSpace(config.TranscriptionModel)
	switch transcriptionModel {
	case "whisper-1", "gpt-4o-mini-transcribe", "gpt-4o-mini-transcribe-2025-12-15", "gpt-4o-transcribe", "gpt-4o-transcribe-diarize":
	default:
		transcriptionModel = "gpt-4o-transcribe"
	}
	language := strings.ToLower(strings.TrimSpace(config.Language))
	if language == "" {
		language = "zh"
	}
	instructions := strings.TrimSpace(config.Instructions)
	if language == "zh" {
		instructions = "通话主要使用普通话中文。请优先按中文理解来电方，并始终使用自然、简洁的简体中文回答。\n\n" + instructions
	}
	tools := make([]map[string]any, 0, len(config.Tools))
	for _, tool := range config.Tools {
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Parameters) > 0 {
			_ = json.Unmarshal(tool.Parameters, &parameters)
		}
		tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": parameters})
	}
	return map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime", "instructions": instructions, "output_modalities": []string{"audio"}, "tools": tools,
		"audio": map[string]any{
			"input": map[string]any{
				"format":          map[string]any{"type": "audio/pcm", "rate": 24000},
				"noise_reduction": map[string]any{"type": "near_field"},
				"turn_detection":  map[string]any{"type": "server_vad", "threshold": 0.35, "prefix_padding_ms": 300, "silence_duration_ms": 600, "create_response": true, "interrupt_response": true},
				"transcription":   map[string]any{"model": transcriptionModel, "language": language, "prompt": "普通话中文电话通话，请输出准确的简体中文；可能包含姓名、电话号码、地址和业务术语。"},
			},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "voice": config.Voice},
		},
	}}
}
func (dialect) InputAudio(audio string) any {
	return map[string]any{"type": "input_audio_buffer.append", "audio": audio}
}
func (dialect) OpeningResponse(config voiceagent.SessionConfig) any {
	prompt := config.OpeningPrompt
	if prompt == "" {
		prompt = "电话刚刚接通。请立即主动、简短而自然地问候对方，并依据会话指令开始通话。不要提及这条触发指令。"
	}
	return map[string]any{"type": "response.create", "response": map[string]any{
		"output_modalities": []string{"audio"}, "instructions": prompt,
	}}
}
func (dialect) ToolResult(callID string, output any) []any {
	data, _ := json.Marshal(output)
	return []any{
		map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "function_call_output", "call_id": callID, "output": string(data)}},
		map[string]any{"type": "response.create"},
	}
}
func (dialect) ParseEvent(raw []byte) ([]voiceagent.Event, error) {
	return realtime.ParseCommonEvent(raw, map[string]voiceagent.EventType{
		"session.created":                                       voiceagent.EventSessionReady,
		"session.updated":                                       voiceagent.EventSessionReady,
		"input_audio_buffer.speech_started":                     voiceagent.EventSpeechStarted,
		"input_audio_buffer.speech_stopped":                     voiceagent.EventSpeechStopped,
		"conversation.item.input_audio_transcription.delta":     voiceagent.EventInputTranscriptDelta,
		"conversation.item.input_audio_transcription.completed": voiceagent.EventInputTranscriptFinal,
		"response.output_audio.delta":                           voiceagent.EventAudio,
		"response.audio.delta":                                  voiceagent.EventAudio,
		"response.output_audio_transcript.delta":                voiceagent.EventOutputTranscriptDelta,
		"response.audio_transcript.delta":                       voiceagent.EventOutputTranscriptDelta,
		"response.output_audio_transcript.done":                 voiceagent.EventOutputTranscriptFinal,
		"response.audio_transcript.done":                        voiceagent.EventOutputTranscriptFinal,
		"response.done":                                         voiceagent.EventUsage,
	})
}
