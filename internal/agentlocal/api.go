package agentlocal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"submux/internal/agent"
	"submux/internal/agentstate"
	"submux/internal/hostops"
	"submux/internal/integration"
	"submux/internal/mihomo"
)

type API struct {
	State         *agentstate.Store
	Core          hostops.CoreManager
	Daemon        *agent.Daemon
	Docker        integration.DockerDaemonManager
	DockerDesktop integration.DockerDesktopManager
	Stop          func()
}

func (a *API) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			next.ServeHTTP(w, r)
		})
	})
	router.Get("/v1/status", a.status)
	router.Get("/v1/doctor", a.doctor)
	router.Get("/v1/logs", a.logs)
	router.Post("/v1/mihomo/install", a.installCore)
	router.Post("/v1/mihomo/restart", a.restartCore)
	router.Post("/v1/mihomo/rollback", a.rollbackCore)
	router.Post("/v1/subscription/check", a.checkSubscription)
	router.Post("/v1/subscription/update", a.updateSubscription)
	router.Post("/v1/subscription/rollback", a.rollbackSubscription)
	router.Post("/v1/unenroll", a.unenroll)
	router.Get("/v1/proxy/env/{format}", a.proxyEnvironment)
	router.Get("/v1/proxy/docker/status", a.dockerStatus)
	router.Post("/v1/proxy/docker/preview", a.dockerPreview)
	router.Post("/v1/proxy/docker/enable", a.dockerEnable)
	router.Post("/v1/proxy/docker/disable", a.dockerDisable)
	router.Get("/v1/proxy/docker-desktop/status", a.dockerDesktopStatus)
	router.Post("/v1/proxy/docker-desktop/preview", a.dockerDesktopPreview)
	router.Post("/v1/proxy/docker-desktop/enable", a.dockerDesktopEnable)
	router.Post("/v1/proxy/docker-desktop/disable", a.dockerDesktopDisable)
	return router
}

func (a *API) unenroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ForceLocal bool `json:"force_local"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid unenroll request", http.StatusBadRequest)
		return
	}
	control, ok := a.Daemon.Control.(agent.SelfRevocationControl)
	if !body.ForceLocal {
		if !ok {
			http.Error(w, "control plane self-revocation is unavailable", http.StatusNotImplemented)
			return
		}
		if err := control.RevokeSelf(r.Context()); err != nil {
			http.Error(w, "control plane revocation failed; retry or explicitly force a local identity wipe", http.StatusBadGateway)
			return
		}
	}
	if err := a.State.ClearEnrollment(); err != nil {
		http.Error(w, "local enrollment could not be cleared", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "remote_revoked": !body.ForceLocal})
	if a.Stop != nil {
		go func() {
			time.Sleep(100 * time.Millisecond)
			a.Stop()
		}()
	}
}

func (a *API) recordAudit(_ context.Context, action, revision, result, summary string) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return
	}
	id := hex.EncodeToString(value)
	if err := a.State.AddLocalAudit(agentstate.LocalAudit{ID: id, RequestID: id, Action: action, Revision: revision, Result: result, Summary: summary}); err == nil && a.Daemon != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.Daemon.FlushLocalAudits(ctx)
		}()
	}
}

func (a *API) saveIntegrationObservation(kind string, status integration.DockerStatus) {
	raw, err := json.Marshal(status)
	if err != nil {
		return
	}
	_, _ = a.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.Integrations[kind] = raw
		return nil
	})
}

func (a *API) dockerDesktopStatus(w http.ResponseWriter, r *http.Request) {
	if a.DockerDesktop == nil {
		http.Error(w, "Docker Desktop integration is unavailable", http.StatusNotImplemented)
		return
	}
	status, err := a.DockerDesktop.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, status)
}

func (a *API) dockerDesktopPreview(w http.ResponseWriter, r *http.Request) {
	if a.DockerDesktop == nil {
		http.Error(w, "Docker Desktop integration is unavailable", http.StatusNotImplemented)
		return
	}
	config, ok := a.decodeDockerDesktopConfig(w, r)
	if !ok {
		return
	}
	preview, err := a.DockerDesktop.Preview(r.Context(), config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, preview)
}

func (a *API) dockerDesktopEnable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyPort            int      `json:"proxy_port"`
		NoProxy              []string `json:"no_proxy,omitempty"`
		ExpectedOriginalHash string   `json:"expected_original_hash"`
	}
	if a.DockerDesktop == nil {
		http.Error(w, "Docker Desktop integration is unavailable", http.StatusNotImplemented)
		return
	}
	if err := decodeJSON(r, &body); err != nil || body.ExpectedOriginalHash == "" {
		http.Error(w, "a confirmed preview hash and valid Docker Desktop proxy config are required", http.StatusBadRequest)
		return
	}
	if err := a.validateDockerProxyPort(body.ProxyPort); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	config := integration.DockerDesktopConfig{Enabled: true, ProxyPort: body.ProxyPort, NoProxy: body.NoProxy, BusinessAdminSettings: true}
	status, err := a.DockerDesktop.Enable(r.Context(), config, body.ExpectedOriginalHash)
	if err != nil {
		a.recordAudit(r.Context(), "integration.docker_desktop.enable", "", "failed", "Docker Desktop proxy activation failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.saveIntegrationObservation(integration.DockerDesktopType, status)
	a.recordAudit(r.Context(), "integration.docker_desktop.enable", "", "succeeded", "Docker Desktop proxy activated from a confirmed preview")
	writeJSON(w, status)
}

func (a *API) dockerDesktopDisable(w http.ResponseWriter, r *http.Request) {
	if a.DockerDesktop == nil {
		http.Error(w, "Docker Desktop integration is unavailable", http.StatusNotImplemented)
		return
	}
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	status, err := a.DockerDesktop.Disable(r.Context())
	if err != nil {
		a.recordAudit(r.Context(), "integration.docker_desktop.disable", "", "failed", "Docker Desktop proxy restore failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.saveIntegrationObservation(integration.DockerDesktopType, status)
	a.recordAudit(r.Context(), "integration.docker_desktop.disable", "", "succeeded", "Docker Desktop proxy fields restored")
	writeJSON(w, status)
}

func (a *API) decodeDockerDesktopConfig(w http.ResponseWriter, r *http.Request) (integration.DockerDesktopConfig, bool) {
	var body struct {
		ProxyPort int      `json:"proxy_port"`
		NoProxy   []string `json:"no_proxy,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid Docker Desktop proxy config", http.StatusBadRequest)
		return integration.DockerDesktopConfig{}, false
	}
	config := integration.DockerDesktopConfig{Enabled: true, ProxyPort: body.ProxyPort, NoProxy: body.NoProxy, BusinessAdminSettings: true}
	if err := config.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return integration.DockerDesktopConfig{}, false
	}
	if err := a.validateDockerProxyPort(config.ProxyPort); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return integration.DockerDesktopConfig{}, false
	}
	return config, true
}

func (a *API) dockerStatus(w http.ResponseWriter, r *http.Request) {
	if a.Docker == nil {
		http.Error(w, "Docker daemon integration is unavailable", http.StatusNotImplemented)
		return
	}
	status, err := a.Docker.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, status)
}

func (a *API) dockerPreview(w http.ResponseWriter, r *http.Request) {
	if a.Docker == nil {
		http.Error(w, "Docker daemon integration is unavailable", http.StatusNotImplemented)
		return
	}
	config, ok := a.decodeDockerConfig(w, r)
	if !ok {
		return
	}
	preview, err := a.Docker.Preview(r.Context(), config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, preview)
}

func (a *API) dockerEnable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyPort            int      `json:"proxy_port"`
		NoProxy              []string `json:"no_proxy,omitempty"`
		ExpectedOriginalHash string   `json:"expected_original_hash"`
	}
	if a.Docker == nil {
		http.Error(w, "Docker daemon integration is unavailable", http.StatusNotImplemented)
		return
	}
	if err := decodeJSON(r, &body); err != nil || body.ExpectedOriginalHash == "" {
		http.Error(w, "a confirmed preview hash and valid Docker proxy config are required", http.StatusBadRequest)
		return
	}
	if err := a.validateDockerProxyPort(body.ProxyPort); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	config := integration.DockerDaemonConfig{Enabled: true, ProxyPort: body.ProxyPort, NoProxy: body.NoProxy}
	status, err := a.Docker.Enable(r.Context(), config, body.ExpectedOriginalHash)
	if err != nil {
		a.recordAudit(r.Context(), "integration.docker_daemon.enable", "", "failed", "Docker Engine proxy activation failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.saveIntegrationObservation(integration.DockerDaemonType, status)
	a.recordAudit(r.Context(), "integration.docker_daemon.enable", "", "succeeded", "Docker Engine proxy activated from a confirmed preview")
	writeJSON(w, status)
}

func (a *API) dockerDisable(w http.ResponseWriter, r *http.Request) {
	if a.Docker == nil {
		http.Error(w, "Docker daemon integration is unavailable", http.StatusNotImplemented)
		return
	}
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	status, err := a.Docker.Disable(r.Context())
	if err != nil {
		a.recordAudit(r.Context(), "integration.docker_daemon.disable", "", "failed", "Docker Engine proxy restore failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.saveIntegrationObservation(integration.DockerDaemonType, status)
	a.recordAudit(r.Context(), "integration.docker_daemon.disable", "", "succeeded", "Docker Engine proxy fields restored")
	writeJSON(w, status)
}

func (a *API) decodeDockerConfig(w http.ResponseWriter, r *http.Request) (integration.DockerDaemonConfig, bool) {
	var body struct {
		ProxyPort int      `json:"proxy_port"`
		NoProxy   []string `json:"no_proxy,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "invalid Docker daemon proxy config", http.StatusBadRequest)
		return integration.DockerDaemonConfig{}, false
	}
	config := integration.DockerDaemonConfig{Enabled: true, ProxyPort: body.ProxyPort, NoProxy: body.NoProxy}
	if err := config.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return integration.DockerDaemonConfig{}, false
	}
	if err := a.validateDockerProxyPort(config.ProxyPort); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return integration.DockerDaemonConfig{}, false
	}
	return config, true
}

func (a *API) validateDockerProxyPort(port int) error {
	runtimeState, err := a.State.Runtime()
	if err != nil {
		return errors.New("Agent runtime state is unavailable")
	}
	if runtimeState.CoreStatus != "running" || runtimeState.ProxyPort != port || (runtimeState.ProxyKind != "mixed" && runtimeState.ProxyKind != "http") {
		return errors.New("Docker integration requires the running Agent-managed HTTP or mixed proxy listener")
	}
	return nil
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	runtimeState, err := a.State.Runtime()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	coreStatus, err := a.Core.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	identity, _ := a.State.Identity()
	runtimeState.MihomoSecret = ""
	writeJSON(w, map[string]any{
		"identity": map[string]any{"instance_id": identity.InstanceID, "server_url": identity.ServerURL, "public_key": identity.PublicKey},
		"runtime":  runtimeState, "core": coreStatus,
	})
}

func (a *API) doctor(w http.ResponseWriter, r *http.Request) {
	type check struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}
	checks := []check{}
	if identity, err := a.State.Identity(); err != nil {
		checks = append(checks, check{Name: "device_identity", Status: "failed", Message: "agent is not enrolled"})
	} else {
		checks = append(checks, check{Name: "device_identity", Status: "ok", Message: "instance is enrolled at " + identity.ServerURL})
	}
	if status, err := a.Core.Status(r.Context()); err != nil {
		checks = append(checks, check{Name: "mihomo", Status: "failed", Message: "core status unavailable"})
	} else {
		checks = append(checks, check{Name: "mihomo", Status: "ok", Message: status.State})
	}
	writeJSON(w, map[string]any{"checks": checks})
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	value, err := a.Core.Logs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, mihomo.SanitizeLogText(value))
}

func (a *API) installCore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel string `json:"channel"`
		Version string `json:"version"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Version == "" || (body.Channel != "stable" && body.Channel != "alpha") {
		http.Error(w, "exact version and stable/alpha channel are required", http.StatusBadRequest)
		return
	}
	if err := a.Daemon.InstallCoreNow(r.Context(), body.Channel, body.Version); err != nil {
		a.recordAudit(r.Context(), "mihomo.install", body.Version, "failed", "Mihomo core install or upgrade failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.recordAudit(r.Context(), "mihomo.install", body.Version, "succeeded", "exact Mihomo core version installed")
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *API) restartCore(w http.ResponseWriter, r *http.Request) {
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	if err := a.Daemon.RestartCoreNow(r.Context()); err != nil {
		a.recordAudit(r.Context(), "mihomo.restart", "", "failed", "Mihomo restart failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.recordAudit(r.Context(), "mihomo.restart", "", "succeeded", "Mihomo restarted from the local CLI")
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *API) rollbackCore(w http.ResponseWriter, r *http.Request) {
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	if err := a.Daemon.RollbackCoreNow(r.Context()); err != nil {
		a.recordAudit(r.Context(), "mihomo.rollback", "", "failed", "Mihomo core rollback failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.recordAudit(r.Context(), "mihomo.rollback", "", "succeeded", "Mihomo core rolled back to the managed previous version")
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *API) checkSubscription(w http.ResponseWriter, r *http.Request) {
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	deployment, err := a.Daemon.CheckSubscriptionNow(r.Context(), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "deployment": deployment})
}

func (a *API) updateSubscription(w http.ResponseWriter, r *http.Request) {
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	deployment, err := a.Daemon.CheckSubscriptionNow(r.Context(), true)
	if err != nil {
		a.recordAudit(r.Context(), "subscription.update", "", "failed", "bound current artifact update failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	revision := ""
	if deployment != nil {
		revision = deployment.RemoteRevision
	}
	a.recordAudit(r.Context(), "subscription.update", revision, "succeeded", "bound current artifact explicitly applied")
	writeJSON(w, map[string]any{"ok": true, "deployment": deployment})
}

func (a *API) rollbackSubscription(w http.ResponseWriter, r *http.Request) {
	if err := decodeEmpty(r); err != nil {
		http.Error(w, "request body must be empty JSON", http.StatusBadRequest)
		return
	}
	if err := a.Daemon.RollbackConfigNow(r.Context()); err != nil {
		a.recordAudit(r.Context(), "subscription.rollback", "", "failed", "last-good configuration rollback failed")
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	a.recordAudit(r.Context(), "subscription.rollback", "", "succeeded", "last-good configuration restored")
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *API) proxyEnvironment(w http.ResponseWriter, r *http.Request) {
	runtimeState, err := a.State.Runtime()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runtimeState.CoreStatus != "running" {
		http.Error(w, "Mihomo is not running", http.StatusConflict)
		return
	}
	value, err := integration.RenderProxyEnvironmentFor(chi.URLParam(r, "format"), runtimeState.ProxyPort, runtimeState.ProxyKind)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, value)
}

func decodeEmpty(r *http.Request) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	var value struct{}
	return decodeJSON(r, &value)
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
