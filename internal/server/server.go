package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"submux/internal/merge"
	"submux/internal/output"
	"submux/internal/override"
	"submux/internal/parse"
	"submux/internal/source"
	"submux/internal/store"
	"submux/web"
)

type Server struct {
	store   *store.Store
	fetcher *source.Fetcher
}

func New(st *store.Store, f *source.Fetcher) *Server {
	return &Server{store: st, fetcher: f}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	// 公开端点
	r.Get("/sub/{token}", s.handleSub)
	r.Get("/api/status", s.handleStatus)
	r.Post("/api/init", s.handleInit)
	r.Post("/api/login", s.handleLogin)
	r.Post("/api/logout", s.handleLogout)

	// 受 session 保护的资源接口
	r.Group(func(pr chi.Router) {
		pr.Use(s.requireAuth)
		pr.Get("/api/sources", s.handleListSources)
		pr.Post("/api/sources", s.handleCreateSource)
		pr.Put("/api/sources/{id}", s.handleUpdateSource)
		pr.Delete("/api/sources/{id}", s.handleDeleteSource)
		pr.Post("/api/sources/{id}/refresh", s.handleRefreshSource)
		pr.Get("/api/override", s.handleGetOverride)
		pr.Put("/api/override", s.handlePutOverride)
		pr.Get("/api/settings", s.handleGetSettings)
		pr.Put("/api/settings", s.handlePutSettings)
		pr.Post("/api/settings/reset-token", s.handleResetToken)
	})

	// 静态控制台(兜底)
	r.Handle("/*", http.FileServer(http.FS(web.FS)))
	return r
}

// handleSub 输出聚合订阅:校验 token → 读各源缓存 → 解析 → 合并 → 覆盖 → 按 UA 渲染。
func (s *Server) handleSub(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	want, _ := s.store.GetSetting("output_token")
	if want == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	srcs, err := s.store.ListEnabledSources()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	var sources []merge.SourceNodes
	var infos []source.Userinfo
	for _, src := range srcs {
		c, err := s.store.GetCache(src.ID)
		if err != nil {
			continue // 该源还没有缓存
		}
		nodes, err := parse.ParseClash(c.Raw)
		if err != nil || len(nodes) == 0 {
			continue
		}
		sources = append(sources, merge.SourceNodes{SourceName: src.Name, Nodes: nodes})
		if c.UserinfoJSON != "" {
			var u source.Userinfo
			if json.Unmarshal([]byte(c.UserinfoJSON), &u) == nil {
				infos = append(infos, u)
			}
		}
	}
	if len(sources) == 0 {
		http.Error(w, "no upstream data", http.StatusServiceUnavailable)
		return
	}

	cfg := merge.Merge(sources)
	defFmt, _ := s.store.GetSetting("default_format")
	if defFmt == "" {
		defFmt = "clash"
	}
	adapter := output.SelectByUA(r.UserAgent(), defFmt)

	overrideYAML, _ := s.store.GetOverride()
	final, err := override.Apply(cfg, overrideYAML)
	if err != nil {
		s.serveLastGoodOr(w, adapter.Format(), "override apply failed: "+err.Error())
		return
	}
	body, ct, err := adapter.Render(final)
	if err != nil {
		s.serveLastGoodOr(w, adapter.Format(), "render failed: "+err.Error())
		return
	}
	_ = s.store.SetLastGood(adapter.Format(), body)

	interval, _ := s.store.GetSettingInt("output_update_interval_hours", 24)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", "attachment; filename=submux")
	w.Header().Set("Subscription-Userinfo", source.AggregateUserinfo(infos).Header())
	w.Header().Set("Profile-Update-Interval", strconv.Itoa(interval))
	_, _ = w.Write(body)
}

// serveLastGoodOr 在覆盖/渲染出错时回退到上次成功输出;没有则 503。
func (s *Server) serveLastGoodOr(w http.ResponseWriter, format, reason string) {
	reason = strings.ReplaceAll(strings.ReplaceAll(reason, "\n", " "), "\r", " ")
	if lg, err := s.store.GetLastGood(format); err == nil && len(lg) > 0 {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=submux")
		w.Header().Set("X-Submux-Degraded", reason)
		_, _ = w.Write(lg)
		return
	}
	http.Error(w, "config error and no last-good: "+reason, http.StatusServiceUnavailable)
}
