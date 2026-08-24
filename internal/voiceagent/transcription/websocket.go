package transcription

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iniwex5/vohive/internal/voiceagent"
	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
)

type Dialect interface {
	SessionUpdate(voiceagent.SessionConfig) any
	InputAudio(string) any
	Finish() any
	ParseEvent([]byte) ([]cascade.Transcript, error)
}

type Config struct {
	Name            string
	Endpoint        string
	APIKey          string
	Headers         http.Header
	InputSampleRate int
	DefaultModel    string
	Dialect         Dialect
	Dialer          *websocket.Dialer
}

type Transcriber struct{ config Config }

func New(config Config) (*Transcriber, error) {
	if strings.TrimSpace(config.Name) == "" || strings.TrimSpace(config.Endpoint) == "" || config.Dialect == nil {
		return nil, errors.New("transcriber requires name, endpoint, and dialect")
	}
	if config.InputSampleRate == 0 {
		config.InputSampleRate = 16000
	}
	if config.Dialer == nil {
		config.Dialer = websocket.DefaultDialer
	}
	return &Transcriber{config: config}, nil
}

func (t *Transcriber) Open(ctx context.Context, config voiceagent.SessionConfig) (cascade.TranscriptionSession, error) {
	if strings.TrimSpace(t.config.APIKey) == "" {
		return nil, fmt.Errorf("%s STT API key is not configured", t.config.Name)
	}
	endpoint, err := url.Parse(t.config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse %s STT endpoint: %w", t.config.Name, err)
	}
	model := strings.TrimSpace(config.Model)
	if model == "" {
		model = t.config.DefaultModel
	}
	config.Model = model
	query := endpoint.Query()
	if model != "" {
		query.Set("model", model)
	}
	endpoint.RawQuery = query.Encode()
	headers := t.config.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+t.config.APIKey)
	conn, response, err := t.config.Dialer.DialContext(ctx, endpoint.String(), headers)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("connect %s STT: HTTP %d: %w", t.config.Name, response.StatusCode, err)
		}
		return nil, fmt.Errorf("connect %s STT: %w", t.config.Name, err)
	}
	s := &session{name: t.config.Name, conn: conn, dialect: t.config.Dialect, inputRate: t.config.InputSampleRate, events: make(chan cascade.Transcript, 64), done: make(chan struct{})}
	if err := s.writeJSON(t.config.Dialect.SessionUpdate(config)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("configure %s STT: %w", t.config.Name, err)
	}
	go s.readLoop()
	return s, nil
}

type session struct {
	name       string
	conn       *websocket.Conn
	dialect    Dialect
	inputRate  int
	events     chan cascade.Transcript
	done       chan struct{}
	writeMu    sync.Mutex
	finishOnce sync.Once
	closeOnce  sync.Once
}

func (s *session) Events() <-chan cascade.Transcript { return s.events }

func (s *session) SendAudio(ctx context.Context, pcm []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("transcription session is closed")
	default:
	}
	converted := voiceagent.ResamplePCM16(pcm, voiceagent.TelephoneSampleRate, s.inputRate)
	return s.writeJSON(s.dialect.InputAudio(base64.StdEncoding.EncodeToString(converted)))
}

func (s *session) writeJSON(value any) error {
	if value == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(value)
}

func (s *session) readLoop() {
	defer s.finish()
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.emit(cascade.Transcript{Err: fmt.Errorf("%s STT: %w", s.name, err)})
			}
			return
		}
		events, err := s.dialect.ParseEvent(raw)
		if err != nil {
			s.emit(cascade.Transcript{Err: fmt.Errorf("%s STT event: %w", s.name, err)})
			continue
		}
		for _, event := range events {
			s.emit(event)
		}
	}
}

func (s *session) emit(event cascade.Transcript) {
	select {
	case s.events <- event:
	case <-s.done:
	}
}
func (s *session) finish() {
	s.finishOnce.Do(func() { close(s.done); close(s.events); _ = s.conn.Close() })
}
func (s *session) Close() error {
	var result error
	s.closeOnce.Do(func() {
		_ = s.writeJSON(s.dialect.Finish())
		s.writeMu.Lock()
		result = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		s.writeMu.Unlock()
		_ = s.conn.Close()
	})
	return result
}
