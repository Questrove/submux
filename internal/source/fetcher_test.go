package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		w.Write([]byte("proxies: []\n"))
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
	if c.Raw != "proxies: []\n" {
		t.Fatalf("raw not cached: %q", c.Raw)
	}
	if c.UserinfoJSON == "" || c.LastError != "" {
		t.Fatalf("cache markers wrong: %+v", c)
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
