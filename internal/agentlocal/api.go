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
	State  *agentstate.Store
	Core   hostops.CoreManager
	Daemon *agent.Daemon
	Stop   func()
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
	router.Post("/v1/subscription/rollback", a.rollbackSubscription)
	router.Post("/v1/unenroll", a.unenroll)
	router.Get("/v1/proxy/env/{format}", a.proxyEnvironment)
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
