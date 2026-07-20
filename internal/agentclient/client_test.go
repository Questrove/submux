package agentclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"submux/internal/agentproto"
)

func TestClientSignsStateRequest(t *testing.T) {
	publicKey, privateKey, _ := agentproto.GenerateDeviceKey()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := agentproto.VerifyRequest(r, agentproto.VerifyOptions{Now: time.Now().UTC(), PublicKey: publicKey}); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/agent/state":
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": agentproto.Version, "jobs": []any{}})
		case "/api/agent/platform-subscriptions/12":
			w.Header().Set("Content-Type", "text/yaml")
			w.Header().Set("X-Submux-Revision", "revision-12")
			_, _ = w.Write([]byte("mixed-port: 7890\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{ServerURL: server.URL, InstanceID: 1, PrivateKey: privateKey}
	state, err := client.GetState(context.Background())
	if err != nil || state.ProtocolVersion != agentproto.Version || len(state.Jobs) != 0 {
		t.Fatalf("state: %#v, %v", state, err)
	}
	published, err := client.FetchPlatformSubscription(context.Background(), 12)
	if err != nil || string(published.Body) != "mixed-port: 7890\n" || published.Revision != "revision-12" {
		t.Fatalf("platform subscription: %#v, %v", published, err)
	}
}
