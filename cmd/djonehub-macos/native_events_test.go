package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNativeEventsCoalesceAndBoundSubscribers(t *testing.T) {
	e := &nativeEvents{}
	var all []chan struct{}
	for range 8 {
		ch, ok := e.subscribe()
		if !ok {
			t.Fatal("subscribe")
		}
		all = append(all, ch)
		<-ch
	}
	if _, ok := e.subscribe(); ok {
		t.Fatal("unbounded subscribers")
	}
	for range 10000 {
		e.publish()
	}
	for _, ch := range all {
		if len(ch) != 1 {
			t.Fatal("unbounded event queue")
		}
		e.unsubscribe(ch)
	}
	if e.current() != 10000 {
		t.Fatal("lost revision")
	}
	ch, ok := e.subscribe()
	if !ok || len(ch) != 1 {
		t.Fatal("reconnect missing initial refresh")
	}
	e.close()
	e.close()
	if _, ok := e.subscribe(); ok {
		t.Fatal("shutdown admitted subscriber")
	}
	select {
	case <-e.shutdown():
	default:
		t.Fatal("shutdown notification missing")
	}
	e.unsubscribe(ch)
}

func TestNativeEventsAuthenticationReconnectAndShutdown(t *testing.T) {
	e := &nativeEvents{}
	handler := nativeControlHandler("secret", func(string) (*app, bool) { t.Fatal("events must not resolve/configure hardware"); return nil, false }, e)
	server := httptest.NewServer(handler)
	defer server.Close()
	defer e.close()
	for _, tc := range []struct {
		token, origin, address string
		want                   int
	}{
		{"wrong", "", "127.0.0.1:1234", 401}, {"secret", "https://example.test", "127.0.0.1:1234", 403}, {"secret", "", "192.0.2.1:1234", 403},
	} {
		req := httptest.NewRequest("GET", "/events", nil)
		req.RemoteAddr = tc.address
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-DJOneHub-Host-ID", "host")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatal(response.Code, tc.want)
		}
	}
	dial := func() *websocket.Conn {
		t.Helper()
		headers := http.Header{"Authorization": []string{"Bearer secret"}, "X-DJOneHub-Host-Id": []string{"host"}}
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/events", headers)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	read := func(conn *websocket.Conn, want uint64) {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]json.RawMessage
		if json.Unmarshal(data, &v) != nil || len(v) != 2 || string(v["type"]) != `"changed"` {
			t.Fatal("event leaked state", string(data))
		}
		var revision uint64
		json.Unmarshal(v["revision"], &revision)
		if revision != want {
			t.Fatal(revision, want)
		}
	}
	first := dial()
	defer first.Close()
	read(first, 0)
	e.publish()
	read(first, 1)
	first.Close()
	e.publish()
	second := dial()
	defer second.Close()
	read(second, 2)
	// Closing the broker must close hijacked sockets too, not just HTTP listeners.
	e.close()
	second.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := second.ReadMessage(); err == nil {
		t.Fatal("shutdown left event socket alive")
	}
}

func TestCallEventsIgnoreStablePolls(t *testing.T) {
	a := remoteTestApp(t)
	e := &nativeEvents{}
	a.nativeEvents = e
	ch, _ := e.subscribe()
	defer e.unsubscribe(ch)
	<-ch
	active := []parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10086"}}
	a.applyCallPoll(active, time.Now())
	if len(ch) != 1 {
		t.Fatal("new call missing event")
	}
	<-ch
	before := e.current()
	for range 10 {
		a.applyCallPoll(active, time.Now())
	}
	if e.current() != before || len(ch) != 0 {
		t.Fatal("stable poll generated events")
	}
	active[0].State = "active"
	a.applyCallPoll(active, time.Now())
	if len(ch) != 1 {
		t.Fatal("answer missing event")
	}
	<-ch
	a.applyCallPoll(nil, time.Now())
	if len(ch) != 1 {
		t.Fatal("hangup missing event")
	}
}
