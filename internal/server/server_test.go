package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"submux/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// M4:server.New 现在需要 fetcher 参数;handleSub 路径用不到,测试传 nil。
func newTestServerHandler(st *store.Store) http.Handler { return New(st, nil).Handler() }

const upstreamClash = `proxies:
  - {name: HK, type: vless, server: 1.1.1.1, port: 443, uuid: a}
  - {name: JP, type: vless, server: 2.2.2.2, port: 443, uuid: b}
`

func TestHandleSubOK(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id, upstreamClash, "[]", "")

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()

	// clash UA → yaml,含前缀节点
	req, _ := http.NewRequest("GET", srv.URL+"/sub/secret123", nil)
	req.Header.Set("User-Agent", "clash-verge/2.0")
	resp := mustDo(t, http.DefaultClient, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "[AirA] HK") || !strings.Contains(s, "[AirA] JP") {
		t.Fatalf("output missing prefixed nodes:\n%s", s)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("content-type wrong: %q", ct)
	}
}

func TestHandleSubBadToken(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	r := mustGet(t, http.DefaultClient, srv.URL+"/sub/wrong")
	defer r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("want 401, got %d", r.StatusCode)
	}
}

func TestHandleSubNoData(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	r := mustGet(t, http.DefaultClient, srv.URL+"/sub/secret123")
	defer r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatalf("want 503, got %d", r.StatusCode)
	}
}

func TestHandleSubAppliesOverride(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id, upstreamClash, "[]", "")
	st.SetOverride("prepend-rules:\n  - DOMAIN-SUFFIX,example.com,DIRECT\n")

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/sub/secret123", nil)
	req.Header.Set("User-Agent", "clash-verge/2.0")
	resp := mustDo(t, http.DefaultClient, req)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "DOMAIN-SUFFIX,example.com,DIRECT") {
		t.Fatalf("override rule not applied:\n%s", s)
	}
	if !strings.Contains(s, "[AirA] HK") {
		t.Fatalf("merged nodes missing:\n%s", s)
	}
}

func TestHandleSubDegradesOnBadOverride(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id, upstreamClash, "[]", "")
	st.SetLastGood("clash", []byte("proxies: []\n# last-good\n"))
	st.SetOverride("\tnot: : valid")

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/sub/secret123", nil)
	req.Header.Set("User-Agent", "clash-verge/2.0")
	resp := mustDo(t, http.DefaultClient, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 (degraded), got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Submux-Degraded") == "" {
		t.Fatalf("expected degraded marker header")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "last-good") {
		t.Fatalf("did not serve last-good body:\n%s", string(body))
	}
}

func TestHandleSubBase64ForKnownGenericClient(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id,
		"proxies:\n  - {name: HK, type: vless, server: 1.1.1.1, port: 443, uuid: u}\n",
		"[]", `{"upload":1,"download":2,"total":100,"expire":1500}`)

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sub/secret123", nil)
	req.Header.Set("User-Agent", "v2rayN/7.0")
	resp := mustDo(t, http.DefaultClient, req)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("want base64/plain, got %q", ct)
	}
	if ui := resp.Header.Get("Subscription-Userinfo"); !strings.Contains(ui, "total=100") {
		t.Fatalf("userinfo header wrong: %q", ui)
	}
	if resp.Header.Get("Profile-Update-Interval") == "" {
		t.Fatalf("missing Profile-Update-Interval")
	}
}

func TestHandleSubClashForClashUA(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id, upstreamClash, "[]", "")

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/sub/secret123", nil)
	req.Header.Set("User-Agent", "clash-verge/2.0")
	resp := mustDo(t, http.DefaultClient, req)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("clash UA should get yaml, got %q", ct)
	}
}

func TestHandleSubRejectsEmptyBase64Output(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id,
		"proxies:\n  - {name: TUIC, type: tuic, server: example.com, port: 443, password: p}\n",
		"[]", "")

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/sub/secret123", nil)
	req.Header.Set("User-Agent", "v2rayN/7.0")
	resp := mustDo(t, http.DefaultClient, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

func TestHandleSubBase64DegradedContentType(t *testing.T) {
	st := newTestStore(t)
	st.SetSetting("output_token", "secret123")
	id, _ := st.CreateSource(store.Source{Name: "AirA", URL: "http://x"})
	st.UpsertCacheSuccess(id,
		"proxies:\n  - {name: VL, type: vless, server: example.com, port: 443, uuid: id}\n",
		"[]", "")
	st.SetLastGood("base64", []byte("bGFzdC1nb29k"))
	st.SetOverride("\tnot: : valid")

	srv := httptest.NewServer(newTestServerHandler(st))
	defer srv.Close()
	resp := mustGet(t, http.DefaultClient, srv.URL+"/sub/secret123")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("degraded base64 content-type wrong: %q", ct)
	}
}
