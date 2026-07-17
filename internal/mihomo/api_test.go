package mihomo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientUsesOnlyObservedObjectsAndSingleProxyDelay(t *testing.T) {
	var selected, closed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"PROXY": map[string]any{"type": "Selector", "now": "A", "all": []string{"A", "B"}}, "A": map[string]any{"type": "Trojan"}}})
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/PROXY":
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			selected = body.Name
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/A/delay":
			if r.URL.Query().Get("url") != delayTestURL {
				t.Errorf("delay URL = %q", r.URL.Query().Get("url"))
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"delay": 42})
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			_ = json.NewEncoder(w).Encode(map[string]any{"connections": []map[string]any{{"id": "connection-1"}}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/connections/"):
			closed = strings.TrimPrefix(r.URL.Path, "/connections/")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "local-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Select(context.Background(), "PROXY", "B"); err != nil || selected != "B" {
		t.Fatalf("select: %q, %v", selected, err)
	}
	if err := client.Select(context.Background(), "PROXY", "unknown"); err == nil {
		t.Fatal("unobserved proxy was selected")
	}
	if delay, err := client.Delay(context.Background(), "PROXY", "A", 5*time.Second); err != nil || delay != 42 {
		t.Fatalf("delay=%d err=%v", delay, err)
	}
	if err := client.CloseConnection(context.Background(), "connection-1"); err != nil || closed != "connection-1" {
		t.Fatalf("close: %q, %v", closed, err)
	}
	if err := client.CloseConnection(context.Background(), "unknown"); err == nil {
		t.Fatal("unobserved connection was closed")
	}
}

func TestClientRejectsNonLoopbackController(t *testing.T) {
	if _, err := NewClient("http://0.0.0.0:9090", "secret", nil); err == nil {
		t.Fatal("non-loopback controller was accepted")
	}
}

func TestRuntimeProxySnapshotAndLogRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxies":
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"PROXY": map[string]any{"type": "Selector", "now": "A", "all": []string{"A"}}}})
		case "/configs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mixed-port": 7890, "secret": "must-not-leave-agent",
				"proxy-providers": map[string]any{"remote": map[string]any{"url": "https://provider.example/sub/token"}},
				"note":            "vless://embedded-credential@example.com:443",
			})
		case "/rules":
			_ = json.NewEncoder(w).Encode(map[string]any{"rules": []map[string]any{{"type": "MATCH", "payload": "DIRECT"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "local-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	var frame json.RawMessage
	if err := client.Stream(context.Background(), "proxies", func(value json.RawMessage) error { frame = append(frame[:0], value...); return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frame), `"kind":"proxies"`) || !strings.Contains(string(frame), `"PROXY"`) {
		t.Fatalf("unexpected proxy snapshot: %s", frame)
	}
	for _, kind := range []string{"configs", "rules"} {
		if err := client.Stream(context.Background(), kind, func(value json.RawMessage) error { frame = append(frame[:0], value...); return nil }); err != nil {
			t.Fatalf("%s snapshot: %v", kind, err)
		}
		if !strings.Contains(string(frame), `"kind":"`+kind+`"`) || strings.Contains(string(frame), "must-not-leave-agent") || strings.Contains(string(frame), "provider.example") || strings.Contains(string(frame), "embedded-credential") {
			t.Fatalf("unexpected %s snapshot: %s", kind, frame)
		}
	}
	redacted, err := sanitizedRuntimeFrame("logs", map[string]any{
		"payload": "GET https://provider.example/sub/token?secret=value", "authorization": "Bearer secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "provider.example") || strings.Contains(string(redacted), "Bearer secret") || !strings.Contains(string(redacted), "[redacted]") {
		t.Fatalf("runtime log was not redacted: %s", redacted)
	}
}
