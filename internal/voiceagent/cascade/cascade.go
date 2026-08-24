package cascade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

type Transcript struct {
	Text      string
	Final     bool
	EventType voiceagent.EventType
	Err       error
}

type TranscriptionSession interface {
	SendAudio(context.Context, []byte) error
	Events() <-chan Transcript
	Close() error
}

type Transcriber interface {
	Open(context.Context, voiceagent.SessionConfig) (TranscriptionSession, error)
}

type Message struct {
	Role       string
	Content    string
	ToolCall   *voiceagent.ToolCall
	ToolCallID string
}

type TextModel interface {
	Complete(context.Context, []Message, []voiceagent.Tool) (string, *voiceagent.ToolCall, map[string]any, error)
}

type TextStreamEvent struct {
	Delta string
	Usage map[string]any
	Err   error
}

type StreamingTextModel interface {
	Stream(context.Context, []Message) (<-chan TextStreamEvent, error)
}

type Synthesizer interface {
	Synthesize(context.Context, string, string) (<-chan []byte, <-chan error, error)
}

type Provider struct {
	ProviderName string
	STT          Transcriber
	LLM          TextModel
	TTS          Synthesizer
}

func (p *Provider) Name() string {
	if p.ProviderName == "" {
		return "cascade"
	}
	return p.ProviderName
}
func (*Provider) Mode() voiceagent.Mode { return voiceagent.ModeCascade }

func (p *Provider) Open(ctx context.Context, config voiceagent.SessionConfig) (voiceagent.Session, error) {
	if p.STT == nil || p.LLM == nil || p.TTS == nil {
		return nil, errors.New("cascade provider requires STT, LLM, and TTS")
	}
	sttConfig := config
	if config.TranscriptionModel != "" {
		sttConfig.Model = config.TranscriptionModel
	}
	stt, err := p.STT.Open(ctx, sttConfig)
	if err != nil {
		return nil, fmt.Errorf("open cascade STT: %w", err)
	}
	child, cancel := context.WithCancel(ctx)
	s := &session{provider: p.Name(), config: config, stt: stt, llm: p.LLM, tts: p.TTS, ctx: child, cancel: cancel, events: make(chan voiceagent.Event, 64)}
	if config.Instructions != "" {
		s.history = append(s.history, Message{Role: "system", Content: config.Instructions})
	}
	go s.run()
	return s, nil
}

type session struct {
	provider  string
	config    voiceagent.SessionConfig
	stt       TranscriptionSession
	llm       TextModel
	tts       Synthesizer
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan voiceagent.Event
	mu        sync.Mutex
	respondMu sync.Mutex
	startMu   sync.Mutex
	started   bool
	history   []Message
	pending   map[string]*voiceagent.ToolCall
	response  context.CancelFunc
	closeOnce sync.Once
}

func (s *session) Events() <-chan voiceagent.Event                 { return s.events }
func (s *session) SendAudio(ctx context.Context, pcm []byte) error { return s.stt.SendAudio(ctx, pcm) }

// Start asks the cascade model to produce the configured opening turn. The
// caller decides when the initial silence window has elapsed.
func (s *session) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errors.New("cascade session is closed")
	default:
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started || strings.TrimSpace(s.config.OpeningPrompt) == "" {
		return nil
	}
	s.started = true
	s.mu.Lock()
	s.history = append(s.history, Message{Role: "user", Content: s.config.OpeningPrompt})
	s.mu.Unlock()
	go s.generate()
	return nil
}

func (s *session) SubmitToolResult(ctx context.Context, callID string, output any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errors.New("cascade session is closed")
	default:
	}
	data, err := json.Marshal(output)
	if err != nil {
		return err
	}
	s.mu.Lock()
	call := s.pending[callID]
	if call == nil {
		s.mu.Unlock()
		return fmt.Errorf("cascade tool call %q is not pending", callID)
	}
	delete(s.pending, callID)
	s.history = append(s.history, Message{Role: "tool", Content: string(data), ToolCallID: callID})
	s.mu.Unlock()
	go s.generate()
	return nil
}

func (s *session) run() {
	s.emit(voiceagent.Event{Type: voiceagent.EventSessionReady})
	defer func() { s.emit(voiceagent.Event{Type: voiceagent.EventClosed}); close(s.events) }()
	for {
		select {
		case <-s.ctx.Done():
			return
		case transcript, ok := <-s.stt.Events():
			if !ok {
				return
			}
			if transcript.Err != nil {
				s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: transcript.Err})
				continue
			}
			if transcript.EventType != "" {
				if transcript.EventType == voiceagent.EventSpeechStarted {
					s.interruptResponse()
				}
				s.emit(voiceagent.Event{Type: transcript.EventType})
				continue
			}
			if !transcript.Final {
				s.emit(voiceagent.Event{Type: voiceagent.EventInputTranscriptDelta, Text: transcript.Text})
				continue
			}
			s.emit(voiceagent.Event{Type: voiceagent.EventInputTranscriptFinal, Text: transcript.Text})
			go s.respond(transcript.Text)
		}
	}
}

func (s *session) respond(input string) {
	s.mu.Lock()
	s.history = append(s.history, Message{Role: "user", Content: input})
	s.mu.Unlock()
	s.generate()
}

func (s *session) generate() {
	s.respondMu.Lock()
	defer s.respondMu.Unlock()
	responseCtx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.response = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		if s.response != nil {
			s.response = nil
		}
		s.mu.Unlock()
	}()
	s.mu.Lock()
	history := append([]Message(nil), s.history...)
	s.mu.Unlock()
	if streaming, ok := s.llm.(StreamingTextModel); ok && len(s.config.Tools) == 0 {
		if err := s.generateStreaming(responseCtx, streaming, history); err != nil && !errors.Is(err, context.Canceled) {
			s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: err})
		}
		return
	}
	text, toolCall, usage, err := s.llm.Complete(responseCtx, history, s.config.Tools)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: err})
		}
		return
	}
	if toolCall != nil {
		s.mu.Lock()
		if s.pending == nil {
			s.pending = make(map[string]*voiceagent.ToolCall)
		}
		s.pending[toolCall.ID] = toolCall
		s.history = append(s.history, Message{Role: "assistant", Content: text, ToolCall: toolCall})
		s.mu.Unlock()
		s.emit(voiceagent.Event{Type: voiceagent.EventToolCall, ToolCall: toolCall})
		return
	}
	s.emit(voiceagent.Event{Type: voiceagent.EventOutputTranscriptFinal, Text: text})
	s.mu.Lock()
	s.history = append(s.history, Message{Role: "assistant", Content: text})
	s.mu.Unlock()
	audio, failures, err := s.tts.Synthesize(responseCtx, text, s.config.Voice)
	if err != nil {
		s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: err})
		return
	}
	for audio != nil || failures != nil {
		select {
		case <-responseCtx.Done():
			return
		case chunk, ok := <-audio:
			if !ok {
				audio = nil
				continue
			}
			s.emit(voiceagent.Event{Type: voiceagent.EventAudio, Audio: chunk, AudioFormat: voiceagent.TelephonePCM})
		case failure, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			if failure != nil {
				s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: failure})
			}
		}
	}
	s.emit(voiceagent.Event{Type: voiceagent.EventAudioDone})
	if usage != nil {
		s.emit(voiceagent.Event{Type: voiceagent.EventUsage, Usage: usage})
	}
}

func (s *session) generateStreaming(ctx context.Context, model StreamingTextModel, history []Message) error {
	stream, err := model.Stream(ctx, history)
	if err != nil {
		return err
	}
	sentences := make(chan string, 8)
	audioDone := make(chan error, 1)
	go func() {
		for sentence := range sentences {
			if err := s.synthesize(ctx, sentence); err != nil {
				audioDone <- err
				return
			}
		}
		audioDone <- nil
	}()
	var full, pending string
	var finalUsage map[string]any
	for event := range stream {
		if event.Err != nil {
			close(sentences)
			<-audioDone
			return event.Err
		}
		if event.Usage != nil {
			finalUsage = event.Usage
		}
		if event.Delta == "" {
			continue
		}
		full += event.Delta
		pending += event.Delta
		s.emit(voiceagent.Event{Type: voiceagent.EventOutputTranscriptDelta, Text: event.Delta})
		segments, remainder := splitCompletedSentences(pending)
		pending = remainder
		for _, segment := range segments {
			select {
			case sentences <- segment:
			case <-ctx.Done():
				close(sentences)
				<-audioDone
				return ctx.Err()
			}
		}
	}
	if strings.TrimSpace(pending) != "" {
		sentences <- strings.TrimSpace(pending)
	}
	close(sentences)
	if err := <-audioDone; err != nil {
		return err
	}
	s.emit(voiceagent.Event{Type: voiceagent.EventAudioDone})
	if finalUsage != nil {
		s.emit(voiceagent.Event{Type: voiceagent.EventUsage, Usage: finalUsage})
	}
	full = strings.TrimSpace(full)
	if full == "" {
		return errors.New("streaming text model returned an empty response")
	}
	s.emit(voiceagent.Event{Type: voiceagent.EventOutputTranscriptFinal, Text: full})
	s.mu.Lock()
	s.history = append(s.history, Message{Role: "assistant", Content: full})
	s.mu.Unlock()
	return nil
}

func splitCompletedSentences(value string) ([]string, string) {
	var completed []string
	start := 0
	for index, character := range value {
		if !strings.ContainsRune("。！？!?；;\n", character) {
			continue
		}
		end := index + len(string(character))
		if segment := strings.TrimSpace(value[start:end]); segment != "" {
			completed = append(completed, segment)
		}
		start = end
	}
	return completed, value[start:]
}

func (s *session) synthesize(ctx context.Context, text string) error {
	audio, failures, err := s.tts.Synthesize(ctx, text, s.config.Voice)
	if err != nil {
		return err
	}
	for audio != nil || failures != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-audio:
			if !ok {
				audio = nil
				continue
			}
			s.emit(voiceagent.Event{Type: voiceagent.EventAudio, Audio: chunk, AudioFormat: voiceagent.TelephonePCM})
		case failure, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			if failure != nil {
				return failure
			}
		}
	}
	return nil
}

func (s *session) interruptResponse() {
	s.mu.Lock()
	cancel := s.response
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *session) emit(event voiceagent.Event) {
	event.Provider = s.provider
	event.At = time.Now()
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}
func (s *session) Close() error {
	s.closeOnce.Do(func() { s.cancel(); _ = s.stt.Close() })
	return nil
}
