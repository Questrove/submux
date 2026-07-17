package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"submux/internal/buildinfo"
	"submux/internal/compiler"
	"submux/internal/source"
	"submux/internal/store"
	"submux/web"
)

type Server struct {
	store    *store.Store
	fetcher  *source.Fetcher
	compiler *compiler.Service
	initErr  error
	nonceMu  sync.Mutex
	nonces   map[string]time.Time
	updates  *runtimeUpdateHub
	streams  *runtimeStreamHub
}

func New(st *store.Store, f *source.Fetcher) *Server {
	srv, err := NewChecked(st, f)
	if err != nil {
		return &Server{store: st, fetcher: f, compiler: compiler.New(st), initErr: err, nonces: make(map[string]time.Time), updates: newRuntimeUpdateHub(), streams: newRuntimeStreamHub()}
	}
	return srv
}

// NewChecked initializes the built-in template catalog and reports failures so
// production startup cannot silently continue without a usable template set.
func NewChecked(st *store.Store, f *source.Fetcher) (*Server, error) {
	service := compiler.New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		return nil, fmt.Errorf("initialize built-in templates: %w", err)
	}
	if f != nil {
		f.SetRebuilder(service)
	}
	return &Server{store: st, fetcher: f, compiler: service, nonces: make(map[string]time.Time), updates: newRuntimeUpdateHub(), streams: newRuntimeStreamHub()}, nil
}

func (s *Server) Handler() http.Handler {
	if s.initErr != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, s.initErr.Error(), http.StatusInternalServerError)
		})
	}
	r := chi.NewRouter()

	// 公开端点
	r.Get("/healthz", s.handleHealth)
	r.Get("/sub/{token}", s.handleSub)
	r.Get("/api/status", s.handleStatus)
	r.Post("/api/init", s.handleInit)
	r.Post("/api/login", s.handleLogin)
	r.Post("/api/logout", s.handleLogout)
	r.Post("/api/agent/enroll", s.handleAgentEnroll)

	r.Group(func(ar chi.Router) {
		ar.Use(s.requireDevice)
		ar.Get("/api/agent/state", s.handleAgentState)
		ar.Get("/api/agent/updates", s.handleAgentUpdates)
		ar.Get("/api/agent/runtime-stream/{session}", s.handleAgentRuntimeStream)
		ar.Post("/api/agent/heartbeat", s.handleAgentHeartbeat)
		ar.Post("/api/agent/local-audit", s.handleAgentLocalAudit)
		ar.Post("/api/agent/revoke-self", s.handleAgentRevokeSelf)
		ar.Post("/api/agent/jobs/{jobID}/status", s.handleAgentJobStatus)
		ar.Head("/api/agent/bindings/{id}/artifact", s.handleAgentArtifact)
		ar.Get("/api/agent/bindings/{id}/artifact", s.handleAgentArtifact)
	})

	// 受 session 保护的资源接口
	r.Group(func(pr chi.Router) {
		pr.Use(s.requireAuth)
		pr.Get("/api/sources", s.handleListSources)
		pr.Get("/api/lifecycle-events", s.handleListLifecycleEvents)
		pr.Post("/api/sources", s.handleCreateSource)
		pr.Put("/api/sources/{id}", s.handleUpdateSource)
		pr.Delete("/api/sources/{id}", s.handleDeleteSource)
		pr.Post("/api/sources/{id}/refresh", s.handleRefreshSource)
		pr.Get("/api/settings", s.handleGetSettings)
		pr.Put("/api/settings", s.handlePutSettings)
		pr.Get("/api/nodes", s.handleListNodes)
		pr.Post("/api/nodes/import", s.handleImportNodes)
		pr.Put("/api/nodes/{id}", s.handleUpdateNode)
		pr.Delete("/api/nodes/{id}", s.handleDeleteNode)
		pr.Get("/api/templates", s.handleListTemplates)
		pr.Post("/api/templates", s.handleSaveTemplate)
		pr.Put("/api/templates/{id}", s.handleSaveTemplate)
		pr.Delete("/api/templates/{id}", s.handleDeleteTemplate)
		pr.Get("/api/templates/{id}/versions", s.handleListTemplateVersions)
		pr.Post("/api/templates/{id}/versions", s.handlePublishTemplateVersion)
		pr.Get("/api/subscriptions", s.handleListOutputSubscriptions)
		pr.Post("/api/subscriptions", s.handleSaveOutputSubscription)
		pr.Put("/api/subscriptions/{id}", s.handleSaveOutputSubscription)
		pr.Delete("/api/subscriptions/{id}", s.handleDeleteOutputSubscription)
		pr.Post("/api/subscriptions/{id}/preview", s.handlePreviewOutputSubscription)
		pr.Post("/api/subscriptions/{id}/publish", s.handlePublishOutputSubscription)
		pr.Post("/api/subscriptions/{id}/reset-token", s.handleResetOutputSubscriptionToken)
		pr.Put("/api/subscriptions/{id}/enabled", s.handleSetOutputSubscriptionEnabled)
		pr.Post("/api/runtime/enrollments", s.handleCreateAgentEnrollment)
		pr.Get("/api/runtime/instances", s.handleListRuntimeInstances)
		pr.Get("/api/runtime/instances/{id}", s.handleGetRuntimeInstance)
		pr.Post("/api/runtime/instances/{id}/revoke", s.handleRevokeRuntimeInstance)
		pr.Put("/api/runtime/instances/{id}/binding", s.handlePutRuntimeBinding)
		pr.Delete("/api/runtime/instances/{id}/binding", s.handleDeleteRuntimeBinding)
		pr.Put("/api/runtime/instances/{id}/desired", s.handlePutRuntimeDesiredState)
		pr.Get("/api/runtime/instances/{id}/jobs", s.handleListRuntimeJobs)
		pr.Post("/api/runtime/instances/{id}/jobs", s.handleCreateRuntimeJob)
		pr.Get("/api/runtime/instances/{id}/audit", s.handleListRuntimeAudit)
		pr.Get("/api/runtime/instances/{id}/stream/{kind}", s.handleBrowserRuntimeStream)
	})

	// 静态控制台(兜底)
	r.Handle("/*", http.FileServer(http.FS(web.FS)))
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"status": "ok", "build": buildinfo.Current()})
}

// handleSub 只提供已成功编译的固定引擎订阅产物，不根据 UA 猜测格式。
func (s *Server) handleSub(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	subscription, err := s.store.GetOutputSubscriptionByToken(token)
	if err != nil || !subscription.Enabled {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if subscription.ExpiresAt != "" {
		if expiry, parseErr := time.Parse(time.RFC3339, subscription.ExpiresAt); parseErr != nil || !time.Now().Before(expiry) {
			http.Error(w, "subscription expired", http.StatusGone)
			return
		}
	}
	artifact, err := s.store.GetSubscriptionArtifact(subscription.ID)
	if err == nil && artifact.BlockedReason != "" {
		http.Error(w, "subscription unavailable due to upstream lifecycle policy", http.StatusServiceUnavailable)
		return
	}
	if err != nil || len(artifact.Body) == 0 {
		http.Error(w, "subscription has no published artifact", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", artifact.ContentType)
	ext := ".yaml"
	if subscription.Engine == compiler.EngineSingBox {
		ext = ".json"
	}
	w.Header().Set("Content-Disposition", "attachment; filename=submux"+ext)
	w.Header().Set("X-Submux-Revision", artifact.Revision)
	if artifact.LastError != "" {
		w.Header().Set("X-Submux-Degraded", sanitizeHeader(artifact.LastError))
	} else if len(artifact.Warnings) > 0 {
		w.Header().Set("X-Submux-Degraded", "upstream lifecycle warning")
	}
	_, _ = w.Write(artifact.Body)
}

func sanitizeHeader(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
}
