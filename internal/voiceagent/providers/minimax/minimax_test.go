package minimax

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/voiceagent/cascade"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestTextModelUsesBearerAuthAndDecodesCompletion(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"您好"}}],"usage":{"total_tokens":3},"base_resp":{"status_code":0}}`))}, nil
	})}
	model := NewTextModel(TextConfig{APIKey: "secret", Client: client})
	text, call, usage, err := model.Complete(context.Background(), []cascade.Message{{Role: "user", Content: "你好"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "您好" || call != nil || usage["total_tokens"].(float64) != 3 {
		t.Fatalf("text=%q call=%#v usage=%#v", text, call, usage)
	}
}

func TestTextModelStreamsOnlyNewVisibleContent(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("accept = %q", request.Header.Get("Accept"))
		}
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"您好\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"您好！\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"total_tokens\":4}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	model := NewTextModel(TextConfig{APIKey: "secret", Client: client})
	stream, err := model.Stream(context.Background(), []cascade.Message{{Role: "user", Content: "你好"}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var usage map[string]any
	for event := range stream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		text += event.Delta
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if text != "您好！" || usage["total_tokens"].(float64) != 4 {
		t.Fatalf("text=%q usage=%#v", text, usage)
	}
}
