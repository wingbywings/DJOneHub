package voiceagent

import (
	"context"
	"encoding/json"
	"time"
)

const TelephoneSampleRate = 8000

type Mode string

const (
	ModeRealtime Mode = "realtime"
	ModeCascade  Mode = "cascade"
)

type EventType string

const (
	EventSessionReady          EventType = "session.ready"
	EventSpeechStarted         EventType = "speech.started"
	EventSpeechStopped         EventType = "speech.stopped"
	EventInputTranscriptDelta  EventType = "transcript.input.delta"
	EventInputTranscriptFinal  EventType = "transcript.input.final"
	EventOutputTranscriptDelta EventType = "transcript.output.delta"
	EventOutputTranscriptFinal EventType = "transcript.output.final"
	EventAudio                 EventType = "audio.delta"
	EventAudioDone             EventType = "audio.done"
	EventToolCall              EventType = "tool.call"
	EventUsage                 EventType = "usage"
	EventError                 EventType = "error"
	EventClosed                EventType = "session.closed"
)

type AudioFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

var TelephonePCM = AudioFormat{Encoding: "pcm_s16le", SampleRate: TelephoneSampleRate, Channels: 1}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Event struct {
	Type        EventType      `json:"type"`
	Provider    string         `json:"provider,omitempty"`
	Text        string         `json:"text,omitempty"`
	Audio       []byte         `json:"audio,omitempty"`
	AudioFormat AudioFormat    `json:"audio_format,omitempty"`
	ToolCall    *ToolCall      `json:"tool_call,omitempty"`
	Usage       map[string]any `json:"usage,omitempty"`
	Err         error          `json:"-"`
	At          time.Time      `json:"at"`
}

type SessionConfig struct {
	ID                 string
	Model              string
	TranscriptionModel string
	Voice              string
	Instructions       string
	OpeningPrompt      string
	Language           string
	Tools              []Tool
}

type Session interface {
	SendAudio(context.Context, []byte) error
	SubmitToolResult(context.Context, string, any) error
	Events() <-chan Event
	Close() error
}

// SessionStarter is implemented by providers that can proactively start a
// turn after the telephone media transport is ready.
type SessionStarter interface {
	Start(context.Context) error
}

type Provider interface {
	Name() string
	Mode() Mode
	Open(context.Context, SessionConfig) (Session, error)
}
