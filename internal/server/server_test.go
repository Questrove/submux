package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/compiler"
	"submux/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func savePublishedSubscription(t *testing.T, st *store.Store, subscription store.OutputSubscription, artifact *store.SubscriptionArtifact) store.OutputSubscription {
	t.Helper()
	id, err := st.SaveOutputSubscription(subscription)
	if err != nil {
		t.Fatalf("save output subscription: %v", err)
	}
	subscription.ID = id
	if artifact != nil {
		artifact.SubscriptionID = id
		if err := st.PutSubscriptionArtifact(*artifact); err != nil {
			t.Fatalf("save artifact: %v", err)
		}
	}
	return subscription
}

func TestServerInitializationFailureIsSurfaced(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := NewChecked(st, nil); err == nil {
		t.Fatal("NewChecked accepted a closed store")
	}

	recorder := httptest.NewRecorder()
	New(st, nil).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("compatibility constructor hid initialization failure: %d", recorder.Code)
	}
}

func TestHandleSubServesFixedMihomoArtifactRegardlessOfUA(t *testing.T) {
	st := newTestStore(t)
	body := []byte("proxies:\n  - {name: fixed}\n")
	savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "desktop", Engine: compiler.EngineMihomo, Token: "mihomo-token", Enabled: true,
	}, &store.SubscriptionArtifact{Body: body, ContentType: "text/yaml; charset=utf-8", Revision: "rev-1"})

	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()
	for _, ua := range []string{"clash-verge/2.0", "v2rayN/7.0", "unknown"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sub/mihomo-token", nil)
		req.Header.Set("User-Agent", ua)
		resp := mustDo(t, http.DefaultClient, req)
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(got) != string(body) {
			t.Fatalf("UA %q changed fixed artifact: status=%d body=%q", ua, resp.StatusCode, got)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "yaml") || resp.Header.Get("X-Submux-Revision") != "rev-1" {
			t.Fatalf("wrong headers for UA %q: %v", ua, resp.Header)
		}
	}
}

func TestHandleSubServesFixedSingBoxArtifact(t *testing.T) {
	st := newTestStore(t)
	savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "server", Engine: compiler.EngineSingBox, Token: "sing-token", Enabled: true,
	}, &store.SubscriptionArtifact{Body: []byte(`{"outbounds":[]}`), ContentType: "application/json; charset=utf-8", Revision: "rev-json"})
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()

	resp := mustGet(t, http.DefaultClient, srv.URL+"/sub/sing-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), ".json") {
		t.Fatalf("wrong sing-box response: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestHandleSubAuthorizationExpiryAndPublication(t *testing.T) {
	st := newTestStore(t)
	savePublishedSubscription(t, st, store.OutputSubscription{Name: "empty", Token: "empty-token", Enabled: true}, nil)
	savePublishedSubscription(t, st, store.OutputSubscription{Name: "disabled", Token: "disabled-token", Enabled: false}, nil)
	savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "expired", Token: "expired-token", Enabled: true,
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}, nil)
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()

	cases := []struct {
		token string
		want  int
	}{
		{"wrong", http.StatusUnauthorized},
		{"disabled-token", http.StatusUnauthorized},
		{"expired-token", http.StatusGone},
		{"empty-token", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		resp := mustGet(t, http.DefaultClient, srv.URL+"/sub/"+tc.token)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("token %q: want %d, got %d", tc.token, tc.want, resp.StatusCode)
		}
	}
}

func TestHandleSubMarksLastGoodAsDegraded(t *testing.T) {
	st := newTestStore(t)
	savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "degraded", Engine: compiler.EngineMihomo, Token: "degraded-token", Enabled: true,
	}, &store.SubscriptionArtifact{
		Body: []byte("# last good\n"), ContentType: "text/yaml; charset=utf-8",
		Revision: "old", LastError: "required slot empty\nretry failed",
	})
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()

	resp := mustGet(t, http.DefaultClient, srv.URL+"/sub/degraded-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("X-Submux-Degraded"), "retry failed") {
		t.Fatalf("last-good degradation not exposed: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestHandleSubBlocksStrictLifecycleArtifact(t *testing.T) {
	st := newTestStore(t)
	savePublishedSubscription(t, st, store.OutputSubscription{
		Name: "blocked", Engine: compiler.EngineMihomo, Token: "blocked-token", Enabled: true,
	}, &store.SubscriptionArtifact{
		Body: []byte("# stale\n"), ContentType: "text/yaml; charset=utf-8",
		Revision: "old", BlockedReason: "strict upstream expired",
	})
	srv := httptest.NewServer(New(st, nil).Handler())
	defer srv.Close()

	resp := mustGet(t, http.DefaultClient, srv.URL+"/sub/blocked-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("blocked output subscription returned %d", resp.StatusCode)
	}
}
