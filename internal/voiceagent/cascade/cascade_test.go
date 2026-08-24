package cascade

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

type fakeTranscriber struct{ session *fakeTranscriptionSession }

func (f fakeTranscriber) Open(context.Context, voiceagent.SessionConfig) (TranscriptionSession, error) {
	return f.session, nil
}

type toolTextModel struct{ calls int }

func (m *toolTextModel) Complete(_ context.Context, history []Message, _ []voiceagent.Tool) (string, *voiceagent.ToolCall, map[string]any, error) {
	m.calls++
	if m.calls == 1 {
		return "", &voiceagent.ToolCall{ID: "tool-1", Name: "get_call_status", Arguments: json.RawMessage(`{}`)}, nil, nil
	}
	last := history[len(history)-1]
	if last.Role != "tool" || last.ToolCallID != "tool-1" {
		return "工具历史错误", nil, nil, nil
	}
	return "工具执行完成", nil, nil, nil
}

func TestCascadeContinuesAfterToolResult(t *testing.T) {
	stt := &fakeTranscriptionSession{events: make(chan Transcript, 1)}
	model := &toolTextModel{}
	provider := &Provider{ProviderName: "minimax", STT: fakeTranscriber{session: stt}, LLM: model, TTS: fakeSynthesizer{}}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{Tools: []voiceagent.Tool{{Name: "get_call_status"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stt.events <- Transcript{Text: "现在是什么状态", Final: true}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-session.Events():
			if event.Type != voiceagent.EventToolCall {
				continue
			}
			if err := session.SubmitToolResult(context.Background(), event.ToolCall.ID, map[string]any{"active": true}); err != nil {
				t.Fatal(err)
			}
			goto waitFinal
		case <-deadline:
			t.Fatal("tool call was not emitted")
		}
	}

waitFinal:
	for {
		select {
		case event := <-session.Events():
			if event.Type == voiceagent.EventOutputTranscriptFinal {
				if event.Text != "工具执行完成" {
					t.Fatalf("final = %q", event.Text)
				}
				return
			}
		case <-deadline:
			t.Fatal("tool continuation did not finish")
		}
	}
}

type streamingTextModel struct{ events chan TextStreamEvent }

func (m *streamingTextModel) Complete(context.Context, []Message, []voiceagent.Tool) (string, *voiceagent.ToolCall, map[string]any, error) {
	return "", nil, nil, nil
}
func (m *streamingTextModel) Stream(context.Context, []Message) (<-chan TextStreamEvent, error) {
	return m.events, nil
}

type recordingSynthesizer struct{ texts chan string }

func (s recordingSynthesizer) Synthesize(_ context.Context, text, _ string) (<-chan []byte, <-chan error, error) {
	s.texts <- text
	audio := make(chan []byte, 1)
	failures := make(chan error)
	audio <- []byte{1, 2}
	close(audio)
	close(failures)
	return audio, failures, nil
}

func TestCascadeSynthesizesCompletedSentenceBeforeTextStreamEnds(t *testing.T) {
	stt := &fakeTranscriptionSession{events: make(chan Transcript, 1)}
	stream := &streamingTextModel{events: make(chan TextStreamEvent, 2)}
	spoken := make(chan string, 2)
	provider := &Provider{ProviderName: "minimax", STT: fakeTranscriber{session: stt}, LLM: stream, TTS: recordingSynthesizer{texts: spoken}}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stt.events <- Transcript{Text: "介绍一下", Final: true}
	stream.events <- TextStreamEvent{Delta: "第一句。"}
	select {
	case text := <-spoken:
		if text != "第一句。" {
			t.Fatalf("first sentence = %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("first sentence was not synthesized while stream remained open")
	}
	stream.events <- TextStreamEvent{Delta: "第二句"}
	close(stream.events)
	select {
	case text := <-spoken:
		if text != "第二句" {
			t.Fatalf("second sentence = %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("final sentence was not synthesized")
	}
}

type fakeTranscriptionSession struct{ events chan Transcript }

func (f *fakeTranscriptionSession) SendAudio(context.Context, []byte) error { return nil }
func (f *fakeTranscriptionSession) Events() <-chan Transcript               { return f.events }
func (f *fakeTranscriptionSession) Close() error                            { return nil }

type fakeTextModel struct{}

func (fakeTextModel) Complete(context.Context, []Message, []voiceagent.Tool) (string, *voiceagent.ToolCall, map[string]any, error) {
	return "您好，请问有什么可以帮您？", nil, map[string]any{"total_tokens": 3}, nil
}

type fakeSynthesizer struct{}

func (fakeSynthesizer) Synthesize(context.Context, string, string) (<-chan []byte, <-chan error, error) {
	audio := make(chan []byte, 1)
	failures := make(chan error)
	audio <- []byte{1, 2, 3, 4}
	close(audio)
	close(failures)
	return audio, failures, nil
}

func TestCascadeRunsSTTThroughTextAndSpeech(t *testing.T) {
	stt := &fakeTranscriptionSession{events: make(chan Transcript, 1)}
	provider := &Provider{ProviderName: "minimax", STT: fakeTranscriber{session: stt}, LLM: fakeTextModel{}, TTS: fakeSynthesizer{}}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stt.events <- Transcript{Text: "你好", Final: true}
	deadline := time.After(time.Second)
	sawFinal := false
	sawAudio := false
	for !sawFinal || !sawAudio {
		select {
		case event := <-session.Events():
			if event.Type == voiceagent.EventOutputTranscriptFinal && event.Text != "" {
				sawFinal = true
			}
			if event.Type == voiceagent.EventAudio && len(event.Audio) == 4 {
				sawAudio = true
			}
		case <-deadline:
			t.Fatalf("events incomplete: final=%v audio=%v", sawFinal, sawAudio)
		}
	}
}

func TestCascadeStartsOpeningTurnOnlyOnce(t *testing.T) {
	stt := &fakeTranscriptionSession{events: make(chan Transcript, 1)}
	provider := &Provider{ProviderName: "minimax", STT: fakeTranscriber{session: stt}, LLM: fakeTextModel{}, TTS: fakeSynthesizer{}}
	session, err := provider.Open(context.Background(), voiceagent.SessionConfig{OpeningPrompt: "主动询问对方"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	starter := session.(voiceagent.SessionStarter)
	if err := starter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := starter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	finals := 0
	for {
		select {
		case event := <-session.Events():
			if event.Type == voiceagent.EventOutputTranscriptFinal {
				finals++
			}
			if event.Type == voiceagent.EventAudio {
				time.Sleep(20 * time.Millisecond)
				if finals != 1 {
					t.Fatalf("opening final count = %d, want 1", finals)
				}
				return
			}
		case <-deadline:
			t.Fatal("opening turn was not generated")
		}
	}
}
