package server

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"submux/internal/agentproto"
	"submux/internal/store"
)

type deviceContextKey struct{}

func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instanceID, err := strconv.ParseInt(r.Header.Get(agentproto.HeaderInstance), 10, 64)
		if err != nil || instanceID <= 0 {
			http.Error(w, "invalid device identity", http.StatusUnauthorized)
			return
		}
		instance, err := s.store.GetRuntimeInstance(instanceID)
		if err != nil || instance.RevokedAt != "" {
			http.Error(w, "device is unknown or revoked", http.StatusUnauthorized)
			return
		}
		publicKey, err := agentproto.DecodePublicKey(instance.DeviceKey)
		if err != nil {
			http.Error(w, "invalid enrolled device identity", http.StatusUnauthorized)
			return
		}
		verifiedID, body, err := agentproto.VerifyRequest(r, agentproto.VerifyOptions{
			Now: time.Now().UTC(), PublicKey: publicKey,
			UseNonce: s.useDeviceNonce,
		})
		if err != nil || verifiedID != instance.ID {
			http.Error(w, "device authentication failed", http.StatusUnauthorized)
			return
		}
		r.Body = http.NoBody
		if len(body) > 0 {
			r.Body = ioNopCloser{bytes.NewReader(body)}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceContextKey{}, instance)))
	})
}

// ioNopCloser avoids importing io solely to restore a verified request body.
type ioNopCloser struct{ *bytes.Reader }

func (ioNopCloser) Close() error { return nil }

func (s *Server) useDeviceNonce(instanceID int64, nonce string, expires time.Time) bool {
	now := time.Now().UTC()
	key := strconv.FormatInt(instanceID, 10) + ":" + nonce
	s.nonceMu.Lock()
	defer s.nonceMu.Unlock()
	for existing, expiry := range s.nonces {
		if !now.Before(expiry) {
			delete(s.nonces, existing)
		}
	}
	if _, exists := s.nonces[key]; exists {
		return false
	}
	s.nonces[key] = expires
	return true
}

func deviceInstance(r *http.Request) store.RuntimeInstance {
	value, _ := r.Context().Value(deviceContextKey{}).(store.RuntimeInstance)
	return value
}
