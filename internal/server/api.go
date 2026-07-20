package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"submux/internal/lifecycle"
	"submux/internal/resourceproxy"
	"submux/internal/store"
)

const (
	minFetchIntervalSec = 60
	maxFetchIntervalSec = 7 * 24 * 60 * 60
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

type sourceDTO struct {
	ID               int64             `json:"id"`
	Kind             string            `json:"kind"`
	Builtin          bool              `json:"builtin,omitempty"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	Tags             []string          `json:"tags,omitempty"`
	URL              string            `json:"url,omitempty"`
	UserAgent        string            `json:"user_agent,omitempty"`
	Enabled          bool              `json:"enabled"`
	SortOrder        int               `json:"sort_order"`
	LifecyclePolicy  string            `json:"lifecycle_policy,omitempty"`
	WarnBeforeDays   int               `json:"warn_before_days,omitempty"`
	TrustNodeNotices bool              `json:"trust_node_notices,omitempty"`
	FetchMode        string            `json:"fetch_mode,omitempty"`
	NodeCount        int               `json:"node_count"`
	NoticeCount      int               `json:"notice_count,omitempty"`
	LastSuccessAt    string            `json:"last_success_at,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
	LastSuccessRoute string            `json:"last_success_route,omitempty"`
	LastDirectError  string            `json:"last_direct_error,omitempty"`
	LastProxyError   string            `json:"last_proxy_error,omitempty"`
	Userinfo         string            `json:"userinfo,omitempty"`
	Lifecycle        *lifecycle.Status `json:"lifecycle,omitempty"`
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.ListSources()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	nodes, _ := s.store.ListNodes()
	counts := make(map[int64]int)
	noticeCounts := make(map[int64]int)
	for _, value := range nodes {
		if value.Role == "notice" {
			noticeCounts[value.SourceID]++
		} else {
			counts[value.SourceID]++
		}
	}
	out := make([]sourceDTO, 0, len(srcs))
	for _, src := range srcs {
		d := sourceDTO{
			ID: src.ID, Kind: src.Kind, Builtin: src.Builtin, Name: src.Name, Description: src.Description,
			Tags: src.Tags, URL: src.URL, UserAgent: src.UserAgent,
			Enabled: src.Enabled, SortOrder: src.SortOrder, NodeCount: counts[src.ID], NoticeCount: noticeCounts[src.ID],
			LifecyclePolicy: src.LifecyclePolicy, WarnBeforeDays: src.WarnBeforeDays, TrustNodeNotices: src.TrustNodeNotices,
			FetchMode: src.FetchMode,
		}
		if src.Kind == store.SourceKindSubscription {
			c, err := s.store.GetCache(src.ID)
			if err != nil {
				status := lifecycle.Evaluate(src, store.Cache{}, time.Now())
				d.Lifecycle = &status
				out = append(out, d)
				continue
			}
			d.LastSuccessAt = c.LastSuccessAt
			d.LastError = c.LastError
			d.LastSuccessRoute = c.LastSuccessRoute
			d.LastDirectError = c.LastDirectError
			d.LastProxyError = c.LastProxyError
			d.Userinfo = c.UserinfoJSON
			status := lifecycle.Evaluate(src, c, time.Now())
			d.Lifecycle = &status
		}
		out = append(out, d)
	}
	writeJSON(w, out)
}

func (s *Server) handleListLifecycleEvents(w http.ResponseWriter, _ *http.Request) {
	values, err := s.store.ListLifecycleEvents(100)
	if err != nil {
		http.Error(w, "list lifecycle events failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, values)
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind             string   `json:"kind"`
		Name             string   `json:"name"`
		Description      string   `json:"description"`
		Tags             []string `json:"tags"`
		URL              string   `json:"url"`
		UserAgent        string   `json:"user_agent"`
		LifecyclePolicy  string   `json:"lifecycle_policy"`
		WarnBeforeDays   int      `json:"warn_before_days"`
		TrustNodeNotices bool     `json:"trust_node_notices"`
		FetchMode        string   `json:"fetch_mode"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	body.Kind = strings.TrimSpace(body.Kind)
	if body.Kind == "" {
		body.Kind = store.SourceKindSubscription
	}
	if body.Name == "" || !validSource(body.Kind, body.URL) {
		http.Error(w, "name and a valid source kind/url are required", http.StatusBadRequest)
		return
	}
	if body.Kind == store.SourceKindSubscription {
		if body.LifecyclePolicy == "" {
			body.LifecyclePolicy = store.LifecycleContinuity
		}
		if body.WarnBeforeDays == 0 {
			body.WarnBeforeDays = 7
		}
		if body.LifecyclePolicy != store.LifecycleContinuity && body.LifecyclePolicy != store.LifecycleStrict {
			http.Error(w, "lifecycle_policy must be continuity or strict", http.StatusBadRequest)
			return
		}
		if body.WarnBeforeDays < 1 || body.WarnBeforeDays > 365 {
			http.Error(w, "warn_before_days must be between 1 and 365", http.StatusBadRequest)
			return
		}
		if body.FetchMode == "" {
			body.FetchMode = store.SourceFetchDirectOnly
		}
		if !validSourceFetchMode(body.FetchMode, body.URL) {
			http.Error(w, "fetch_mode must be direct_only, or direct_then_platform_proxy for an HTTPS source", http.StatusBadRequest)
			return
		}
	}
	id, err := s.store.CreateSource(store.Source{
		Kind: body.Kind, Name: body.Name, Description: strings.TrimSpace(body.Description),
		Tags: store.NormalizeStringSet(body.Tags), URL: body.URL, UserAgent: strings.TrimSpace(body.UserAgent),
		LifecyclePolicy: body.LifecyclePolicy, WarnBeforeDays: body.WarnBeforeDays, TrustNodeNotices: body.TrustNodeNotices,
		FetchMode: body.FetchMode,
	})
	if err != nil {
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	response := map[string]any{"id": id}
	if body.Kind == store.SourceKindSubscription {
		response["refresh_ok"] = false
		if s.fetcher == nil {
			response["refresh_error"] = "fetcher unavailable"
		} else if src, getErr := s.store.GetSource(id); getErr != nil {
			response["refresh_error"] = "created source could not be loaded"
		} else if refreshErr := s.fetcher.FetchOne(r.Context(), src); refreshErr != nil {
			// Keep the source so the administrator can inspect the recorded error,
			// correct the upstream settings, and retry without re-entering it.
			response["refresh_error"] = refreshErr.Error()
		} else {
			response["refresh_ok"] = true
		}
	}
	writeJSON(w, response)
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Kind             string   `json:"kind"`
		Name             string   `json:"name"`
		Description      string   `json:"description"`
		Tags             []string `json:"tags"`
		URL              string   `json:"url"`
		UserAgent        string   `json:"user_agent"`
		SortOrder        int      `json:"sort_order"`
		Enabled          bool     `json:"enabled"`
		LifecyclePolicy  string   `json:"lifecycle_policy"`
		WarnBeforeDays   int      `json:"warn_before_days"`
		TrustNodeNotices *bool    `json:"trust_node_notices"`
		FetchMode        string   `json:"fetch_mode"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	old, err := s.store.GetSource(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if old.Builtin {
		http.Error(w, "built-in node group cannot be modified", http.StatusConflict)
		return
	}
	if body.Kind == "" {
		body.Kind = old.Kind
	}
	if body.Kind != old.Kind {
		http.Error(w, "source kind cannot be changed", http.StatusConflict)
		return
	}
	if body.Name == "" || !validSource(body.Kind, body.URL) {
		http.Error(w, "name and a valid source kind/url are required", http.StatusBadRequest)
		return
	}
	src := store.Source{
		ID: id, Kind: body.Kind, Name: body.Name, Description: strings.TrimSpace(body.Description),
		Tags: store.NormalizeStringSet(body.Tags), URL: body.URL, UserAgent: strings.TrimSpace(body.UserAgent),
		SortOrder: body.SortOrder, Enabled: body.Enabled,
		LifecyclePolicy: body.LifecyclePolicy, WarnBeforeDays: body.WarnBeforeDays,
		FetchMode: body.FetchMode,
	}
	if body.TrustNodeNotices != nil {
		src.TrustNodeNotices = *body.TrustNodeNotices
	} else {
		src.TrustNodeNotices = old.TrustNodeNotices
	}
	if src.LifecyclePolicy == "" {
		src.LifecyclePolicy = old.LifecyclePolicy
	}
	if src.WarnBeforeDays == 0 {
		src.WarnBeforeDays = old.WarnBeforeDays
	}
	if src.FetchMode == "" {
		src.FetchMode = old.FetchMode
	}
	if src.Kind == store.SourceKindSubscription && !validSourceFetchMode(src.FetchMode, src.URL) {
		http.Error(w, "fetch_mode must be direct_only, or direct_then_platform_proxy for an HTTPS source", http.StatusBadRequest)
		return
	}
	if src.Kind == store.SourceKindSubscription && src.LifecyclePolicy != store.LifecycleContinuity && src.LifecyclePolicy != store.LifecycleStrict {
		http.Error(w, "lifecycle_policy must be continuity or strict", http.StatusBadRequest)
		return
	}
	if src.Kind == store.SourceKindSubscription && (src.WarnBeforeDays < 1 || src.WarnBeforeDays > 365) {
		http.Error(w, "warn_before_days must be between 1 and 365", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateSource(src); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.compiler.RebuildAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sourceValue, err := s.store.GetSource(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if sourceValue.Builtin {
		http.Error(w, "built-in node group cannot be deleted", http.StatusConflict)
		return
	}
	subscriptions, _ := s.store.ListOutputSubscriptions()
	nodes, _ := s.store.ListNodes()
	nodeSource := make(map[int64]int64, len(nodes))
	for _, node := range nodes {
		nodeSource[node.ID] = node.SourceID
	}
	for _, subscription := range subscriptions {
		for _, binding := range subscription.Bindings {
			for _, nodeID := range binding.NodeIDs {
				if nodeSource[nodeID] == id {
					http.Error(w, "source node is used by output subscription "+strconv.FormatInt(subscription.ID, 10), http.StatusConflict)
					return
				}
			}
		}
	}
	if err := s.store.DeleteSource(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.compiler.RebuildAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleRefreshSource(w http.ResponseWriter, r *http.Request) {
	if s.fetcher == nil {
		http.Error(w, "fetcher unavailable", http.StatusServiceUnavailable)
		return
	}
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	src, err := s.store.GetSource(id)
	if err != nil {
		http.Error(w, "no such source", http.StatusNotFound)
		return
	}
	if src.Kind != store.SourceKindSubscription {
		http.Error(w, "manual sources cannot be refreshed", http.StatusBadRequest)
		return
	}
	ferr := s.fetcher.FetchOne(r.Context(), src)
	resp := map[string]any{"ok": ferr == nil}
	if ferr != nil {
		resp["error"] = ferr.Error()
	}
	writeJSON(w, resp)
}

func (s *Server) handleRefreshSourceViaPlatformProxy(w http.ResponseWriter, r *http.Request) {
	if s.fetcher == nil {
		http.Error(w, "fetcher unavailable", http.StatusServiceUnavailable)
		return
	}
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	src, err := s.store.GetSource(id)
	if err != nil {
		http.Error(w, "no such source", http.StatusNotFound)
		return
	}
	fetchErr := s.fetcher.FetchOneViaPlatformProxy(r.Context(), src)
	result := map[string]any{"ok": fetchErr == nil}
	if fetchErr != nil {
		result["error"] = fetchErr.Error()
	}
	writeJSON(w, result)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	base, _ := s.store.GetSetting("base_url")
	interval, _ := s.store.GetSettingInt("fetch_interval_sec", 10800)
	proxy, err := resourceproxy.Load(s.store)
	if err != nil {
		proxy = resourceproxy.Config{Mode: resourceproxy.ModeDirect}
	}
	writeJSON(w, map[string]any{
		"base_url":                base,
		"fetch_interval_sec":      interval,
		"platform_resource_proxy": proxy,
	})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL               string               `json:"base_url"`
		FetchIntervalSec      int                  `json:"fetch_interval_sec"`
		PlatformResourceProxy resourceproxy.Config `json:"platform_resource_proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.BaseURL = strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if body.BaseURL != "" && !validHTTPURL(body.BaseURL) {
		http.Error(w, "base_url must be an absolute http(s) url", http.StatusBadRequest)
		return
	}
	if body.FetchIntervalSec != 0 && (body.FetchIntervalSec < minFetchIntervalSec || body.FetchIntervalSec > maxFetchIntervalSec) {
		http.Error(w, "fetch_interval_sec must be between 60 and 604800", http.StatusBadRequest)
		return
	}
	if err := resourceproxy.Validate(body.PlatformResourceProxy); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SetSetting("base_url", body.BaseURL); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	if body.FetchIntervalSec > 0 {
		if err := s.store.SetSetting("fetch_interval_sec", strconv.Itoa(body.FetchIntervalSec)); err != nil {
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		if s.fetcher != nil {
			s.fetcher.NotifyIntervalChanged()
		}
	}
	if err := resourceproxy.Save(s.store, body.PlatformResourceProxy); err != nil {
		http.Error(w, "save platform resource proxy failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleTestPlatformResourceProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlatformResourceProxy resourceproxy.Config `json:"platform_resource_proxy"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	config := body.PlatformResourceProxy
	client, err := resourceproxy.NewClient(config, 15*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/rate_limit", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "submux-platform-resource-proxy-test")
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "GitHub connection failed"})
		return
	}
	resp.Body.Close()
	writeJSON(w, map[string]any{"ok": resp.StatusCode >= 200 && resp.StatusCode < 400, "status": resp.StatusCode, "mode": config.Mode})
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func validSourceFetchMode(mode, rawURL string) bool {
	if mode == "" || mode == store.SourceFetchDirectOnly {
		return true
	}
	u, err := url.Parse(rawURL)
	return mode == store.SourceFetchProxyBackup && err == nil && u.Scheme == "https"
}

func validSource(kind, rawURL string) bool {
	switch kind {
	case store.SourceKindSubscription:
		return validHTTPURL(rawURL)
	case store.SourceKindManual:
		return rawURL == ""
	default:
		return false
	}
}
