package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"submux/internal/override"
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

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

type sourceDTO struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	UserAgent     string `json:"user_agent"`
	Enabled       bool   `json:"enabled"`
	SortOrder     int    `json:"sort_order"`
	LastSuccessAt string `json:"last_success_at"`
	LastError     string `json:"last_error"`
	Userinfo      string `json:"userinfo"`
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.ListSources()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]sourceDTO, 0, len(srcs))
	for _, src := range srcs {
		d := sourceDTO{
			ID: src.ID, Name: src.Name, URL: src.URL, UserAgent: src.UserAgent,
			Enabled: src.Enabled, SortOrder: src.SortOrder,
		}
		if c, err := s.store.GetCache(src.ID); err == nil {
			d.LastSuccessAt = c.LastSuccessAt
			d.LastError = c.LastError
			d.Userinfo = c.UserinfoJSON
		}
		out = append(out, d)
	}
	writeJSON(w, out)
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name, URL, UserAgent string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	if body.Name == "" || !validHTTPURL(body.URL) {
		http.Error(w, "name and valid http(s) url required", http.StatusBadRequest)
		return
	}
	id, err := s.store.CreateSource(store.Source{Name: body.Name, URL: body.URL, UserAgent: body.UserAgent})
	if err != nil {
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Name, URL, UserAgent string
		SortOrder            int
		Enabled              bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	if body.Name == "" || !validHTTPURL(body.URL) {
		http.Error(w, "name and valid http(s) url required", http.StatusBadRequest)
		return
	}
	src := store.Source{
		ID: id, Name: body.Name, URL: body.URL, UserAgent: body.UserAgent,
		SortOrder: body.SortOrder, Enabled: body.Enabled,
	}
	if err := s.store.UpdateSource(src); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteSource(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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
	ferr := s.fetcher.FetchOne(r.Context(), src)
	resp := map[string]any{"ok": ferr == nil}
	if ferr != nil {
		resp["error"] = ferr.Error()
	}
	writeJSON(w, resp)
}

func (s *Server) handleGetOverride(w http.ResponseWriter, r *http.Request) {
	c, _ := s.store.GetOverride()
	writeJSON(w, map[string]any{"content": c})
}

func (s *Server) handlePutOverride(w http.ResponseWriter, r *http.Request) {
	var body struct{ Content string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := override.Apply(map[string]any{}, body.Content); err != nil {
		http.Error(w, "invalid override yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SetOverride(body.Content); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	base, _ := s.store.GetSetting("base_url")
	token, _ := s.store.GetSetting("output_token")
	interval, _ := s.store.GetSettingInt("fetch_interval_sec", 10800)
	subURL := ""
	if base != "" {
		subURL = strings.TrimRight(base, "/") + "/sub/" + token
	}
	writeJSON(w, map[string]any{
		"base_url":           base,
		"output_token":       token,
		"sub_url":            subURL,
		"fetch_interval_sec": interval,
	})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL          string `json:"base_url"`
		FetchIntervalSec int    `json:"fetch_interval_sec"`
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
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleResetToken(w http.ResponseWriter, r *http.Request) {
	tok := randomHex(24)
	if err := s.store.SetSetting("output_token", tok); err != nil {
		http.Error(w, "reset failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"output_token": tok})
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}
