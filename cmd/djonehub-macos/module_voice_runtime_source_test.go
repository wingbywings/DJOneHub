package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type voiceRuntimeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f voiceRuntimeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestFetchVerifiedVoiceRuntimeFileChecksSHA256(t *testing.T) {
	payload := []byte("verified runtime")
	hash := sha256.Sum256(payload)
	client := &http.Client{Transport: voiceRuntimeRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(payload)),
			Header:     make(http.Header),
		}, nil
	})}
	file := upstreamVoiceFile{Name: "runtime.bin", SHA256: hex.EncodeToString(hash[:])}
	data, err := fetchVerifiedVoiceRuntimeFile(context.Background(), client, upstreamVoiceDownloadSource{Name: "test", URL: "https://example.invalid/runtime"}, file)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("verified download = %q, %v", data, err)
	}
	file.SHA256 = "0000"
	if _, err := fetchVerifiedVoiceRuntimeFile(context.Background(), client, upstreamVoiceDownloadSource{Name: "test", URL: "https://example.invalid/runtime"}, file); err == nil {
		t.Fatal("mismatched runtime hash was accepted")
	}
}

func TestWriteVerifiedVoiceRuntimeFileIsAtomicAndExecutable(t *testing.T) {
	payload := []byte("runtime")
	hash := sha256.Sum256(payload)
	file := upstreamVoiceFile{Name: "runtime.bin", Mode: 0o755, SHA256: hex.EncodeToString(hash[:])}
	if err := writeVerifiedVoiceRuntimeFile(t.TempDir(), file, []byte("tampered")); err == nil {
		t.Fatal("tampered runtime was written")
	}
	directory := t.TempDir()
	if err := writeVerifiedVoiceRuntimeFile(directory, file, payload); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	info, err := os.Stat(filepath.Join(directory, file.Name))
	if err != nil {
		t.Fatalf("stat runtime: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("runtime mode = %v", info.Mode())
	}
}

func TestVoiceProvisionRequiresConfirmation(t *testing.T) {
	instance := newDemoApp()
	request := httptest.NewRequest(http.MethodPost, "/api/voice/provision", bytes.NewBufferString(`{"confirm":false}`))
	response := httptest.NewRecorder()
	instance.voiceProvisionAPI(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestVoiceRouteCannotStartBeforeActiveCall(t *testing.T) {
	instance := newDemoApp()
	request := httptest.NewRequest(http.MethodPost, "/api/voice/start", nil)
	response := httptest.NewRecorder()
	instance.voiceStartAPI(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
