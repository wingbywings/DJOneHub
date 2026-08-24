package minimax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
)

type TextConfig struct {
	APIKey, Endpoint, Model string
	Client                  *http.Client
}
type TextModel struct{ config TextConfig }

func NewTextModel(config TextConfig) *TextModel {
	if config.Endpoint == "" {
		config.Endpoint = "https://api.minimax.io/v1/chat/completions"
	}
	if config.Model == "" {
		config.Model = "MiniMax-M2.7-highspeed"
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 45 * time.Second}
	}
	return &TextModel{config: config}
}

func (m *TextModel) Complete(ctx context.Context, history []cascade.Message, tools []voiceagent.Tool) (string, *voiceagent.ToolCall, map[string]any, error) {
	if m.config.APIKey == "" {
		return "", nil, nil, fmt.Errorf("MiniMax API key is not configured")
	}
	messages := minimaxMessages(history)
	toolValues := minimaxTools(tools)
	payload := map[string]any{"model": m.config.Model, "messages": messages, "stream": false}
	if len(toolValues) > 0 {
		payload["tools"] = toolValues
		payload["tool_choice"] = "auto"
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := m.config.Client.Do(req)
	if err != nil {
		return "", nil, nil, fmt.Errorf("MiniMax text request: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", nil, nil, err
	}
	if response.StatusCode/100 != 2 {
		return "", nil, nil, fmt.Errorf("MiniMax text HTTP %d: %s", response.StatusCode, string(data))
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage    map[string]any `json:"usage"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", nil, nil, fmt.Errorf("decode MiniMax text response: %w", err)
	}
	if result.BaseResp.StatusCode != 0 {
		return "", nil, nil, fmt.Errorf("MiniMax text error %d: %s", result.BaseResp.StatusCode, result.BaseResp.StatusMsg)
	}
	if len(result.Choices) == 0 {
		return "", nil, nil, fmt.Errorf("MiniMax text response has no choices")
	}
	message := result.Choices[0].Message
	if len(message.ToolCalls) > 0 {
		call := message.ToolCalls[0]
		return message.Content, &voiceagent.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments)}, result.Usage, nil
	}
	return message.Content, nil, result.Usage, nil
}

func minimaxMessages(history []cascade.Message) []map[string]any {
	messages := make([]map[string]any, 0, len(history))
	for _, message := range history {
		value := map[string]any{"role": message.Role, "content": message.Content}
		if message.Role != "tool" {
			value["name"] = message.Role
		}
		if message.ToolCallID != "" {
			value["tool_call_id"] = message.ToolCallID
		}
		if message.ToolCall != nil {
			value["tool_calls"] = []map[string]any{{"id": message.ToolCall.ID, "type": "function", "function": map[string]any{"name": message.ToolCall.Name, "arguments": string(message.ToolCall.Arguments)}}}
		}
		messages = append(messages, value)
	}
	return messages
}

func minimaxTools(tools []voiceagent.Tool) []map[string]any {
	values := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Parameters) > 0 {
			_ = json.Unmarshal(tool.Parameters, &parameters)
		}
		values = append(values, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": parameters}})
	}
	return values
}

func (m *TextModel) Stream(ctx context.Context, history []cascade.Message) (<-chan cascade.TextStreamEvent, error) {
	if m.config.APIKey == "" {
		return nil, fmt.Errorf("MiniMax API key is not configured")
	}
	payload := map[string]any{
		"model":           m.config.Model,
		"messages":        minimaxMessages(history),
		"stream":          true,
		"stream_options":  map[string]any{"include_usage": true},
		"reasoning_split": true,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	response, err := m.config.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MiniMax streaming text request: %w", err)
	}
	if response.StatusCode/100 != 2 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("MiniMax streaming text HTTP %d: %s", response.StatusCode, string(data))
	}
	events := make(chan cascade.TextStreamEvent, 32)
	go func() {
		defer close(events)
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		accumulated := ""
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if raw == "" || raw == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage    map[string]any `json:"usage"`
				BaseResp struct {
					StatusCode int    `json:"status_code"`
					StatusMsg  string `json:"status_msg"`
				} `json:"base_resp"`
			}
			if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
				events <- cascade.TextStreamEvent{Err: fmt.Errorf("decode MiniMax stream: %w", err)}
				return
			}
			if chunk.BaseResp.StatusCode != 0 {
				events <- cascade.TextStreamEvent{Err: fmt.Errorf("MiniMax stream error %d: %s", chunk.BaseResp.StatusCode, chunk.BaseResp.StatusMsg)}
				return
			}
			event := cascade.TextStreamEvent{Usage: chunk.Usage}
			if len(chunk.Choices) > 0 {
				content := chunk.Choices[0].Delta.Content
				if accumulated != "" && strings.HasPrefix(content, accumulated) {
					event.Delta = strings.TrimPrefix(content, accumulated)
					accumulated = content
				} else {
					event.Delta = content
					accumulated += content
				}
			}
			if event.Delta != "" || event.Usage != nil {
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			events <- cascade.TextStreamEvent{Err: fmt.Errorf("read MiniMax stream: %w", err)}
		}
	}()
	return events, nil
}

type SpeechConfig struct {
	APIKey, Endpoint, Model, Voice string
	Dialer                         *websocket.Dialer
}
type Synthesizer struct{ config SpeechConfig }

func NewSynthesizer(config SpeechConfig) *Synthesizer {
	if config.Endpoint == "" {
		config.Endpoint = "wss://api.minimax.io/ws/v1/t2a_v2"
	}
	if config.Model == "" {
		config.Model = "speech-2.8-turbo"
	}
	if config.Voice == "" {
		config.Voice = "male-qn-qingse"
	}
	if config.Dialer == nil {
		config.Dialer = websocket.DefaultDialer
	}
	return &Synthesizer{config: config}
}
func (s *Synthesizer) Synthesize(ctx context.Context, text, voice string) (<-chan []byte, <-chan error, error) {
	if s.config.APIKey == "" {
		return nil, nil, fmt.Errorf("MiniMax API key is not configured")
	}
	headers := http.Header{"Authorization": []string{"Bearer " + s.config.APIKey}}
	conn, response, err := s.config.Dialer.DialContext(ctx, s.config.Endpoint, headers)
	if err != nil {
		if response != nil {
			return nil, nil, fmt.Errorf("MiniMax TTS HTTP %d: %w", response.StatusCode, err)
		}
		return nil, nil, err
	}
	_, connected, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var hello struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(connected, &hello)
	if hello.Event != "connected_success" {
		conn.Close()
		return nil, nil, fmt.Errorf("MiniMax TTS did not confirm connection")
	}
	chosen := voice
	if chosen == "" {
		chosen = s.config.Voice
	}
	start := map[string]any{"event": "task_start", "model": s.config.Model, "voice_setting": map[string]any{"voice_id": chosen, "speed": 1, "vol": 1, "pitch": 0}, "audio_setting": map[string]any{"sample_rate": 8000, "bitrate": 128000, "format": "pcm", "channel": 1}}
	if err := conn.WriteJSON(start); err != nil {
		conn.Close()
		return nil, nil, err
	}
	_, started, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var acknowledged struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(started, &acknowledged)
	if acknowledged.Event != "task_started" {
		conn.Close()
		return nil, nil, fmt.Errorf("MiniMax TTS task was not started")
	}
	if err := conn.WriteJSON(map[string]any{"event": "task_continue", "text": text}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	audio := make(chan []byte, 16)
	failures := make(chan error, 1)
	go func() {
		defer close(audio)
		defer close(failures)
		defer conn.Close()
		defer conn.WriteJSON(map[string]any{"event": "task_finish"})
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				failures <- err
				return
			}
			var event struct {
				Event   string `json:"event"`
				IsFinal bool   `json:"is_final"`
				Data    struct {
					Audio string `json:"audio"`
				} `json:"data"`
				BaseResp struct {
					StatusCode int    `json:"status_code"`
					StatusMsg  string `json:"status_msg"`
				} `json:"base_resp"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				failures <- err
				return
			}
			if event.BaseResp.StatusCode != 0 {
				failures <- fmt.Errorf("MiniMax TTS error %d: %s", event.BaseResp.StatusCode, event.BaseResp.StatusMsg)
				return
			}
			if event.Data.Audio != "" {
				chunk, err := hex.DecodeString(event.Data.Audio)
				if err != nil {
					failures <- err
					return
				}
				select {
				case audio <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if event.IsFinal || event.Event == "task_finished" {
				return
			}
		}
	}()
	return audio, failures, nil
}
