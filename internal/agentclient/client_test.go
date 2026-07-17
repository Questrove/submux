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

func TestClientSignsStateAndBoundsArtifacts(t *testing.T) {
	publicKey, privateKey, _ := agentproto.GenerateDeviceKey()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := agentproto.VerifyRequest(r, agentproto.VerifyOptions{Now: time.Now().UTC(), PublicKey: publicKey}); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/agent/state":
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": agentproto.Version, "desired": map[string]any{"instance_id": 1, "generation": 1, "desired_runtime_state": "stopped"}, "jobs": []any{}})
		case "/api/agent/bindings/1/artifact":
			w.Header().Set("ETag", `"revision"`)
			w.Header().Set("X-Submux-Revision", "revision")
			_, _ = w.Write([]byte("config"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{ServerURL: server.URL, InstanceID: 1, PrivateKey: privateKey}
	state, err := client.GetState(context.Background())
	if err != nil || state.Desired.Generation != 1 {
		t.Fatalf("state: %#v, %v", state, err)
	}
	artifact, err := client.FetchArtifact(context.Background(), 1, "")
	if err != nil || string(artifact.Body) != "config" || artifact.Revision != "revision" {
		t.Fatalf("artifact: %#v, %v", artifact, err)
	}
}
