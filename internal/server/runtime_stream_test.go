package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRuntimeStreamSessionsAreBoundExpiringAndRemoved(t *testing.T) {
	hub := newRuntimeStreamHub()
	now := time.Now().UTC()
	id, session := hub.create(7, "logs", now)
	if id == "" || session.kind != "logs" {
		t.Fatalf("invalid session: %q %#v", id, session)
	}
	if _, err := hub.get(id, 8, now); err == nil {
		t.Fatal("stream session was accepted for another device")
	}
	if got, err := hub.get(id, 7, now); err != nil || got != session {
		t.Fatalf("stream session lookup failed: %#v %v", got, err)
	}
	hub.remove(id, session)
	if _, err := hub.get(id, 7, now); err == nil {
		t.Fatal("removed stream session remains available")
	}
	id, _ = hub.create(7, "traffic", now)
	if _, err := hub.get(id, 7, now.Add(runtimeStreamLifetime+time.Second)); err == nil {
		t.Fatal("expired stream session remains available")
	}
}

func TestRuntimeWebSocketRequiresSameOrigin(t *testing.T) {
	request := httptest.NewRequest("GET", "https://submux.example/api/runtime/instances/1/stream/logs", nil)
	request.Header.Set("Origin", "https://submux.example")
	if err := sameOriginWebSocket(nil, request); err != nil {
		t.Fatalf("same origin was rejected: %v", err)
	}
	request.Header.Set("Origin", "https://attacker.example")
	if err := sameOriginWebSocket(nil, request); err == nil {
		t.Fatal("cross-origin WebSocket was accepted")
	}
}
