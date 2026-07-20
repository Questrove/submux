package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const runtimeSecretSubscriptionURL = "subscription_url"

type runtimeSecret struct {
	InstanceID int64
	Kind       string
	Value      string
	ExpiresAt  time.Time
}

type runtimeSecretVault struct {
	mu     sync.Mutex
	values map[string]runtimeSecret
}

func newRuntimeSecretVault() *runtimeSecretVault {
	return &runtimeSecretVault{values: make(map[string]runtimeSecret)}
}

func (v *runtimeSecretVault) put(instanceID int64, kind, value string, now time.Time) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for ref, secret := range v.values {
		if !now.Before(secret.ExpiresAt) {
			delete(v.values, ref)
		}
	}
	if len(v.values) >= 256 {
		return "", errors.New("too many pending runtime secrets")
	}
	ref := randomHex(24)
	v.values[ref] = runtimeSecret{InstanceID: instanceID, Kind: kind, Value: value, ExpiresAt: now.Add(10 * time.Minute)}
	return ref, nil
}

func (v *runtimeSecretVault) consume(instanceID int64, ref string, now time.Time) (runtimeSecret, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	secret, ok := v.values[ref]
	if !ok || secret.InstanceID != instanceID || !now.Before(secret.ExpiresAt) {
		if ok && !now.Before(secret.ExpiresAt) {
			delete(v.values, ref)
		}
		return runtimeSecret{}, errors.New("runtime secret is missing or expired")
	}
	delete(v.values, ref)
	return secret, nil
}

func validateRuntimeSubscriptionURL(value string) error {
	if len(value) == 0 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("subscription URL is required and must not exceed 4096 characters")
	}
	parsed, err := url.Parse(value)
	host := ""
	if parsed != nil {
		host = parsed.Hostname()
	}
	if err != nil || parsed.Scheme != "https" || host == "" || len(host) > 253 || strings.ContainsAny(host, "\r\n\x00") || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("subscription URL must be an HTTPS URL without credentials or a fragment")
	}
	return nil
}

func (s *Server) handleCreateRuntimeSecret(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	instanceID, err := idParam(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	instance, err := s.store.GetRuntimeInstance(instanceID)
	if err != nil || instance.RevokedAt != "" {
		http.Error(w, "runtime instance does not exist or is revoked", http.StatusNotFound)
		return
	}
	var body struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Kind != runtimeSecretSubscriptionURL || validateRuntimeSubscriptionURL(body.Value) != nil {
		http.Error(w, "invalid runtime secret", http.StatusBadRequest)
		return
	}
	ref, err := s.secrets.put(instanceID, body.Kind, body.Value, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ref": ref, "expires_in": 600})
}

func (s *Server) handleAgentRuntimeSecret(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	instance := deviceInstance(r)
	secret, err := s.secrets.consume(instance.ID, chi.URLParam(r, "ref"), time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusGone)
		return
	}
	writeJSON(w, map[string]string{"kind": secret.Kind, "value": secret.Value})
}
