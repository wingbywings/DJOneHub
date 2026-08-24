package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/voiceagent"
)

type Dialect interface {
	SessionUpdate(voiceagent.SessionConfig) any
	InputAudio(string) any
	ToolResult(callID string, output any) []any
	ParseEvent([]byte) ([]voiceagent.Event, error)
}

type openingDialect interface {
	OpeningEvents(voiceagent.SessionConfig) []any
}

type Config struct {
	Name             string
	Endpoint         string
	APIKey           string
	Headers          http.Header
	InputSampleRate  int
	OutputSampleRate int
	DefaultModel     string
	Dialect          Dialect
	Dialer           *websocket.Dialer
}

type Provider struct{ config Config }

func NewProvider(config Config) (*Provider, error) {
	if strings.TrimSpace(config.Name) == "" || strings.TrimSpace(config.Endpoint) == "" || config.Dialect == nil {
		return nil, errors.New("realtime provider requires name, endpoint, and dialect")
	}
	if config.InputSampleRate == 0 {
		config.InputSampleRate = 24000
	}
	if config.OutputSampleRate == 0 {
		config.OutputSampleRate = 24000
	}
	if config.Dialer == nil {
		config.Dialer = websocket.DefaultDialer
	}
	return &Provider{config: config}, nil
}

func (p *Provider) Name() string        { return p.config.Name }
func (*Provider) Mode() voiceagent.Mode { return voiceagent.ModeRealtime }

func (p *Provider) Open(ctx context.Context, sessionConfig voiceagent.SessionConfig) (voiceagent.Session, error) {
	if strings.TrimSpace(p.config.APIKey) == "" {
		return nil, fmt.Errorf("%s API key is not configured", p.config.Name)
	}
	model := strings.TrimSpace(sessionConfig.Model)
	if model == "" {
		model = p.config.DefaultModel
	}
	endpoint, err := url.Parse(p.config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse %s realtime endpoint: %w", p.config.Name, err)
	}
	query := endpoint.Query()
	if model != "" {
		query.Set("model", model)
	}
	endpoint.RawQuery = query.Encode()
	headers := p.config.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+p.config.APIKey)
	conn, response, err := p.config.Dialer.DialContext(ctx, endpoint.String(), headers)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("connect %s realtime: HTTP %d: %w", p.config.Name, response.StatusCode, err)
		}
		return nil, fmt.Errorf("connect %s realtime: %w", p.config.Name, err)
	}
	session := &session{
		provider: p.config.Name, conn: conn, dialect: p.config.Dialect,
		inputRate: p.config.InputSampleRate, outputRate: p.config.OutputSampleRate,
		events: make(chan voiceagent.Event, 64), done: make(chan struct{}),
	}
	session.inputUpsampler = voiceagent.NewStreamingPCM16Upsampler(voiceagent.TelephoneSampleRate, p.config.InputSampleRate)
	session.outputDownsampler = voiceagent.NewStreamingPCM16Downsampler(p.config.OutputSampleRate, voiceagent.TelephoneSampleRate)
	if dialect, ok := p.config.Dialect.(openingDialect); ok {
		session.openingEvents = dialect.OpeningEvents(sessionConfig)
	}
	if err := session.writeJSON(p.config.Dialect.SessionUpdate(sessionConfig)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("configure %s realtime session: %w", p.config.Name, err)
	}
	go session.readLoop()
	return session, nil
}

type session struct {
	provider          string
	conn              *websocket.Conn
	dialect           Dialect
	inputRate         int
	outputRate        int
	events            chan voiceagent.Event
	done              chan struct{}
	writeMu           sync.Mutex
	closeOnce         sync.Once
	closingOnce       sync.Once
	startMu           sync.Mutex
	started           bool
	openingEvents     []any
	responseMu        sync.Mutex
	responseActive    bool
	callerHasSpoken   bool
	pendingResponse   any
	inputMu           sync.Mutex
	inputUpsampler    *voiceagent.StreamingPCM16Upsampler
	outputDownsampler *voiceagent.StreamingPCM16Downsampler
}

func (s *session) Events() <-chan voiceagent.Event { return s.events }

func (s *session) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("realtime session is closed")
	default:
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started || len(s.openingEvents) == 0 {
		return nil
	}
	s.responseMu.Lock()
	if s.callerHasSpoken || s.responseActive {
		s.responseMu.Unlock()
		s.started = true
		return nil
	}
	startsResponse := containsClientEvent(s.openingEvents, "response.create")
	if startsResponse {
		s.responseActive = true
	}
	s.responseMu.Unlock()
	if err := s.writeJSONBatch(s.openingEvents); err != nil {
		if startsResponse {
			s.responseMu.Lock()
			s.responseActive = false
			s.responseMu.Unlock()
		}
		return err
	}
	s.started = true
	return nil
}

func (s *session) SendAudio(ctx context.Context, pcm []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("realtime session is closed")
	default:
	}
	s.inputMu.Lock()
	var converted []byte
	if s.inputUpsampler != nil {
		converted = s.inputUpsampler.Process(pcm)
	} else {
		converted = voiceagent.ResamplePCM16(pcm, voiceagent.TelephoneSampleRate, s.inputRate)
	}
	s.inputMu.Unlock()
	if len(converted) == 0 {
		return nil
	}
	return s.writeJSON(s.dialect.InputAudio(base64.StdEncoding.EncodeToString(converted)))
}

func (s *session) SubmitToolResult(ctx context.Context, callID string, output any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("realtime session is closed")
	default:
	}
	for _, event := range s.dialect.ToolResult(callID, output) {
		if clientEventType(event) == "response.create" {
			if err := s.sendOrQueueResponse(event); err != nil {
				return err
			}
			continue
		}
		if err := s.writeJSON(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) writeJSON(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(value)
}

func (s *session) writeJSONBatch(values []any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, value := range values {
		_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := s.conn.WriteJSON(value); err != nil {
			return err
		}
	}
	return nil
}

func clientEventType(value any) string {
	if event, ok := value.(map[string]any); ok {
		if eventType, ok := event["type"].(string); ok {
			return eventType
		}
	}
	return ""
}

func containsClientEvent(events []any, eventType string) bool {
	for _, event := range events {
		if clientEventType(event) == eventType {
			return true
		}
	}
	return false
}

func (s *session) sendOrQueueResponse(event any) error {
	s.responseMu.Lock()
	if s.responseActive {
		// A single model turn may contain a tool call while its audio is still
		// streaming. Coalesce response.create requests and start the follow-up
		// turn only after response.done arrives.
		if s.pendingResponse == nil {
			s.pendingResponse = event
		}
		s.responseMu.Unlock()
		return nil
	}
	s.responseActive = true
	s.responseMu.Unlock()
	if err := s.writeJSON(event); err != nil {
		s.responseMu.Lock()
		s.responseActive = false
		s.responseMu.Unlock()
		return err
	}
	return nil
}

func (s *session) observeServerEvent(raw []byte) error {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil // The dialect parser reports malformed provider events.
	}
	s.responseMu.Lock()
	switch envelope.Type {
	case "input_audio_buffer.speech_started":
		s.callerHasSpoken = true
	case "response.created":
		s.responseActive = true
	case "response.done":
		s.responseActive = false
		pending := s.pendingResponse
		s.pendingResponse = nil
		if pending != nil {
			s.responseActive = true
			s.responseMu.Unlock()
			if err := s.writeJSON(pending); err != nil {
				s.responseMu.Lock()
				s.responseActive = false
				s.responseMu.Unlock()
				return err
			}
			return nil
		}
	}
	s.responseMu.Unlock()
	return nil
}

func (s *session) readLoop() {
	defer s.finish()
	for {
		_, message, err := s.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: err})
			}
			return
		}
		if err := s.observeServerEvent(message); err != nil {
			s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: err})
			return
		}
		events, err := s.dialect.ParseEvent(message)
		if err != nil {
			s.emit(voiceagent.Event{Type: voiceagent.EventError, Err: err})
			continue
		}
		for _, event := range events {
			if len(event.Audio) > 0 {
				if s.outputDownsampler != nil {
					event.Audio = s.outputDownsampler.Process(event.Audio)
				} else {
					event.Audio = voiceagent.ResamplePCM16(event.Audio, s.outputRate, voiceagent.TelephoneSampleRate)
				}
				event.AudioFormat = voiceagent.TelephonePCM
			}
			s.emit(event)
		}
	}
}

func (s *session) emit(event voiceagent.Event) {
	event.Provider = s.provider
	event.At = time.Now()
	select {
	case s.events <- event:
	case <-s.done:
	}
}

func (s *session) finish() {
	s.closeOnce.Do(func() {
		close(s.done)
		select {
		case s.events <- voiceagent.Event{Type: voiceagent.EventClosed, Provider: s.provider, At: time.Now()}:
		default:
		}
		close(s.events)
		_ = s.conn.Close()
	})
}

func (s *session) Close() error {
	var err error
	s.closingOnce.Do(func() {
		s.writeMu.Lock()
		err = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		s.writeMu.Unlock()
		_ = s.conn.Close()
	})
	return err
}

type Envelope struct {
	Type       string          `json:"type"`
	Delta      string          `json:"delta"`
	Transcript string          `json:"transcript"`
	Error      json.RawMessage `json:"error"`
	Response   json.RawMessage `json:"response"`
	Item       struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
}

func ParseCommonEvent(raw []byte, eventNames map[string]voiceagent.EventType) ([]voiceagent.Event, error) {
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode realtime event: %w", err)
	}
	if envelope.Type == "error" {
		return []voiceagent.Event{{Type: voiceagent.EventError, Err: fmt.Errorf("provider error: %s", strings.TrimSpace(string(envelope.Error)))}}, nil
	}
	typeName, ok := eventNames[envelope.Type]
	if !ok {
		if envelope.Type == "response.output_item.done" && envelope.Item.Type == "function_call" {
			return []voiceagent.Event{{Type: voiceagent.EventToolCall, ToolCall: &voiceagent.ToolCall{
				ID: envelope.Item.CallID, Name: envelope.Item.Name, Arguments: json.RawMessage(envelope.Item.Arguments),
			}}}, nil
		}
		return nil, nil
	}
	event := voiceagent.Event{Type: typeName}
	switch typeName {
	case voiceagent.EventAudio:
		audio, err := base64.StdEncoding.DecodeString(envelope.Delta)
		if err != nil {
			return nil, fmt.Errorf("decode realtime audio: %w", err)
		}
		event.Audio = audio
	case voiceagent.EventInputTranscriptDelta, voiceagent.EventOutputTranscriptDelta:
		event.Text = envelope.Delta
	case voiceagent.EventInputTranscriptFinal, voiceagent.EventOutputTranscriptFinal:
		event.Text = envelope.Transcript
	case voiceagent.EventUsage:
		var response struct {
			Usage map[string]any `json:"usage"`
		}
		_ = json.Unmarshal(envelope.Response, &response)
		event.Usage = response.Usage
		// response.done is the most portable end-of-audio signal across
		// Realtime protocol revisions. Emit an explicit playout boundary even
		// when a provider omits its more specific response.audio.done event.
		return []voiceagent.Event{{Type: voiceagent.EventAudioDone}, event}, nil
	}
	return []voiceagent.Event{event}, nil
}
