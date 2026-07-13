package server

import (
	"encoding/json"
	"net/http"
	"time"

	"submux/internal/auth"
)

const sessionCookie = "submux_session"
const sessionTTL = 24 * time.Hour

func (s *Server) secret() []byte {
	v, _ := s.store.GetSetting("session_secret")
	return []byte(v)
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return auth.ValidateSession(s.secret(), c.Value)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    auth.IssueSession(s.secret(), sessionTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	h, _ := s.store.GetSetting("admin_pw_hash")
	writeJSON(w, map[string]any{"initialized": h != "", "authed": s.authed(r)})
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	h, _ := s.store.GetSetting("admin_pw_hash")
	if h != "" {
		http.Error(w, "already initialized", http.StatusConflict)
		return
	}
	var body struct{ Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Password) < 6 {
		http.Error(w, "password too short (min 6)", http.StatusBadRequest)
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		http.Error(w, "hash error", http.StatusInternalServerError)
		return
	}
	s.store.SetSetting("admin_pw_hash", hash)
	s.store.SetSetting("session_secret", randomHex(32))
	s.setSession(w)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	hash, _ := s.store.GetSetting("admin_pw_hash")
	if hash == "" || !auth.CheckPassword(hash, body.Password) {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	s.setSession(w)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}
