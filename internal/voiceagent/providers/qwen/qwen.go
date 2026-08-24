package qwen

import (
	"encoding/json"
	"strings"

	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/realtime"
)

type Config struct{ APIKey, Endpoint, Model string }

func New(config Config) (*realtime.Provider, error) {
	if config.Endpoint == "" {
		config.Endpoint = "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime"
	}
	if config.Model == "" {
		config.Model = "qwen-audio-3.0-realtime-flash"
	}
	return realtime.NewProvider(realtime.Config{
		Name: "qwen", Endpoint: config.Endpoint, APIKey: config.APIKey, DefaultModel: config.Model,
		InputSampleRate: 16000, OutputSampleRate: 24000, Dialect: dialect{},
	})
}

type dialect struct{}

func (dialect) SessionUpdate(config voiceagent.SessionConfig) any {
	if config.Voice == "" {
		config.Voice = "Cherry"
	}
	instructions := strings.TrimSpace(config.Instructions)
	if strings.EqualFold(strings.TrimSpace(config.Language), "zh") {
		instructions = "每轮语音尽量控制在 20 秒内；复杂内容先给结论，再分段说明并询问是否继续。\n\n" + instructions
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
		"modalities": []string{"text", "audio"}, "instructions": instructions, "voice": config.Voice,
		// Qwen names raw signed 16-bit little-endian PCM "pcm". "pcm16" is
		// the OpenAI Realtime spelling and is not part of Qwen's protocol.
		"input_audio_format": "pcm", "output_audio_format": "pcm", "tools": tools,
		"input_audio_transcription": map[string]any{"language": config.Language},
		"turn_detection":            map[string]any{"type": "server_vad", "threshold": 0.5, "silence_duration_ms": 500},
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
		"modalities": []string{"text", "audio"}, "instructions": prompt,
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
		"session.created": voiceagent.EventSessionReady, "session.updated": voiceagent.EventSessionReady,
		"input_audio_buffer.speech_started": voiceagent.EventSpeechStarted, "input_audio_buffer.speech_stopped": voiceagent.EventSpeechStopped,
		"conversation.item.input_audio_transcription.delta":     voiceagent.EventInputTranscriptDelta,
		"conversation.item.input_audio_transcription.completed": voiceagent.EventInputTranscriptFinal,
		"response.audio.delta":                                  voiceagent.EventAudio, "response.audio_transcript.delta": voiceagent.EventOutputTranscriptDelta,
		"response.audio_transcript.done": voiceagent.EventOutputTranscriptFinal, "response.done": voiceagent.EventUsage,
	})
}
