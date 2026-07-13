package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"submux/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFetchOneSuccess(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=3; expire=4")
		w.Write([]byte("proxies:\n  - {name: A, type: vless, server: example.com, port: 443, uuid: id}\n"))
	}))
	defer srv.Close()

	st := newTestStore(t)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: srv.URL, UserAgent: "myUA/1.0"})
	src, _ := st.GetSource(id)

	f := NewFetcher(st)
	if err := f.FetchOne(context.Background(), src); err != nil {
		t.Fatalf("FetchOne: %v", err)
	}
	if gotUA != "myUA/1.0" {
		t.Fatalf("UA not sent, got %q", gotUA)
	}
	c, _ := st.GetCache(id)
	if c.UserinfoJSON == "" || c.LastError != "" {
		t.Fatalf("cache markers wrong: %+v", c)
	}
	nodes, _ := st.ListNodes()
	if len(nodes) != 1 || nodes[0].Protocol != "vless" {
		t.Fatalf("normalized nodes not committed: %#v", nodes)
	}
}

func TestFetchOneHTTPErrorIsStored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	st := newTestStore(t)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: srv.URL})
	src, _ := st.GetSource(id)

	f := NewFetcher(st)
	err := f.FetchOne(context.Background(), src)
	if err == nil {
		t.Fatalf("expected error for 503")
	}
	c, _ := st.GetCache(id)
	if c.LastError == "" {
		t.Fatalf("last_error not recorded")
	}
}

func TestFetchOneInvalidPayloadKeepsLastGoodCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not a subscription"))
	}))
	defer srv.Close()

	st := newTestStore(t)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: srv.URL})
	st.UpsertCacheSuccess(id, "old userinfo")
	src, _ := st.GetSource(id)

	if err := NewFetcher(st).FetchOne(context.Background(), src); err == nil {
		t.Fatalf("expected invalid subscription error")
	}
	c, _ := st.GetCache(id)
	if c.UserinfoJSON != "old userinfo" || c.LastError == "" {
		t.Fatalf("last-good cache was not preserved: %+v", c)
	}
}

func TestFetchOneRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxUpstreamBytes+1)))
	}))
	defer srv.Close()

	st := newTestStore(t)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: srv.URL})
	src, _ := st.GetSource(id)
	if err := NewFetcher(st).FetchOne(context.Background(), src); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want size error, got %v", err)
	}
}
