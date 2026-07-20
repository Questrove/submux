package agentlocal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/agent"
	"submux/internal/agentstate"
	"submux/internal/hostops"
)

type localCore struct {
	status hostops.CoreStatus
	logs   string
}

func (c *localCore) Install(context.Context, string, string) error      { return nil }
func (c *localCore) Uninstall(context.Context) error                    { return nil }
func (c *localCore) RollbackCore(context.Context) error                 { return nil }
func (c *localCore) Status(context.Context) (hostops.CoreStatus, error) { return c.status, nil }
func (c *localCore) Start(context.Context) error                        { return nil }
func (c *localCore) Stop(context.Context) error                         { return nil }
func (c *localCore) Restart(context.Context) error                      { return nil }
func (c *localCore) ReloadOrRestart(context.Context) error              { return nil }
func (c *localCore) ValidateConfig(context.Context, string) error       { return nil }
func (c *localCore) Logs(context.Context) (string, error)               { return c.logs, nil }

func TestStatusExposesOnlyPublicLocalIdentityAndForceUnenrollStopsAgent(t *testing.T) {
	state, err := agentstate.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	identity := agentstate.Identity{ServerURL: "https://submux.test", InstanceID: 7, PublicKey: "public", PrivateKey: "private", EnrolledAt: time.Now().UTC().Format(time.RFC3339)}
	if err := state.SaveIdentity(identity); err != nil {
		t.Fatal(err)
	}
	_, _ = state.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.MihomoSecret = "must-never-be-printed"
		return nil
	})
	stopped := make(chan struct{}, 1)
	api := &API{State: state, Core: &localCore{status: hostops.CoreStatus{Installed: true, Version: "v1.0.0", State: "stopped"}, logs: "GET https://provider.example/token secret=visible Bearer credential"}, Daemon: &agent.Daemon{}, Stop: func() { stopped <- struct{}{} }}
	status := httptest.NewRecorder()
	api.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if status.Code != http.StatusOK {
		t.Fatalf("status code %d", status.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	encoded := status.Body.String()
	if encoded == "" || containsText(encoded, "private") || containsText(encoded, "must-never-be-printed") || !containsText(encoded, "public") {
		t.Fatalf("local status leaked or omitted identity data: %s", encoded)
	}
	logs := httptest.NewRecorder()
	api.Handler().ServeHTTP(logs, httptest.NewRequest(http.MethodGet, "/v1/logs", nil))
	if logs.Code != http.StatusOK || containsText(logs.Body.String(), "provider.example") || containsText(logs.Body.String(), "visible") || containsText(logs.Body.String(), "credential") {
		t.Fatalf("local logs were not redacted: %s", logs.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/unenroll", strings.NewReader(`{"force_local":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unenroll status %d: %s", response.Code, response.Body.String())
	}
	if _, err := state.Identity(); err == nil {
		t.Fatal("force unenroll retained the local identity")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("force unenroll did not stop the Agent")
	}
}

func TestLocalAPILimitsMutationBodies(t *testing.T) {
	api := &API{Daemon: &agent.Daemon{}}
	body := `{"channel":"stable","version":"` + strings.Repeat("a", 70<<10) + `"}`
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/mihomo/install", strings.NewReader(body)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized local mutation returned %d", response.Code)
	}
}

func TestLocalAPIRemovesAbandonedRoutes(t *testing.T) {
	handler := (&API{}).Handler()
	for _, path := range []string{
		"/v1/subscription/check",
		"/v1/subscription/update",
		"/v1/proxy/docker/status",
		"/v1/proxy/docker/preview",
		"/v1/proxy/docker/enable",
		"/v1/proxy/docker/disable",
		"/v1/proxy/docker-desktop/status",
		"/v1/proxy/docker-desktop/preview",
		"/v1/proxy/docker-desktop/enable",
		"/v1/proxy/docker-desktop/disable",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if response.Code != http.StatusNotFound {
			t.Fatalf("removed application configuration route %s returned %d", path, response.Code)
		}
	}
}

func containsText(value, target string) bool { return strings.Contains(value, target) }
