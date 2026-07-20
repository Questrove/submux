package source

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestFetchOneNetworkErrorDoesNotPersistSecretURL(t *testing.T) {
	st := newTestStore(t)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: "http://127.0.0.1:1/sub?token=super-secret"})
	src, _ := st.GetSource(id)
	err := NewFetcher(st).FetchOne(context.Background(), src)
	cache, _ := st.GetCache(id)
	if err == nil || strings.Contains(err.Error(), "super-secret") || strings.Contains(cache.LastError, "super-secret") {
		t.Fatalf("network error leaked source URL: err=%q cache=%q", err, cache.LastError)
	}
}

func TestFetchOneFallsBackToConfiguredPlatformProxyAfterNetworkFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "vless://00000000-0000-0000-0000-000000000001@node.example.com:443?encryption=none&type=tcp#HK")
	}))
	defer upstream.Close()

	originalTransport := http.DefaultTransport
	trustedTransport := originalTransport.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	trustedTransport.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = trustedTransport
	t.Cleanup(func() {
		trustedTransport.CloseIdleConnections()
		http.DefaultTransport = originalTransport
	})

	upstreamAddress := strings.TrimPrefix(upstream.URL, "https://")
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstreamConn, err := net.Dial("tcp", upstreamAddress)
		if err != nil {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		clientConn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstreamConn.Close()
			return
		}
		_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			_, _ = io.Copy(upstreamConn, clientConn)
			_ = upstreamConn.Close()
		}()
		_, _ = io.Copy(clientConn, upstreamConn)
		_ = clientConn.Close()
	}))
	defer proxy.Close()

	st := newTestStore(t)
	if err := st.SetSetting("platform_resource_proxy_mode", "http"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("platform_resource_proxy_url", proxy.URL); err != nil {
		t.Fatal(err)
	}
	id, _ := st.CreateSource(store.Source{
		Name: "A", URL: "https://127.0.0.1:1/sub?token=secret", FetchMode: store.SourceFetchProxyBackup,
	})
	src, _ := st.GetSource(id)
	if err := NewFetcher(st).FetchOne(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	cache, _ := st.GetCache(id)
	if proxyHits.Load() != 1 || cache.LastSuccessRoute != "platform_proxy" || cache.LastDirectError == "" || cache.LastProxyError != "" {
		t.Fatalf("fallback route was not recorded: hits=%d cache=%+v", proxyHits.Load(), cache)
	}
}

func TestFetchOneDoesNotUsePlatformProxyForCertificateFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "unused")
	}))
	defer upstream.Close()
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	defer proxy.Close()

	st := newTestStore(t)
	_ = st.SetSetting("platform_resource_proxy_mode", "http")
	_ = st.SetSetting("platform_resource_proxy_url", proxy.URL)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: upstream.URL, FetchMode: store.SourceFetchProxyBackup})
	src, _ := st.GetSource(id)
	if err := NewFetcher(st).FetchOne(context.Background(), src); err == nil {
		t.Fatal("certificate failure was accepted")
	}
	cache, _ := st.GetCache(id)
	if proxyHits.Load() != 0 || cache.LastDirectError == "" || cache.LastProxyError != "" {
		t.Fatalf("certificate failure unexpectedly used the proxy: hits=%d cache=%+v", proxyHits.Load(), cache)
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

func TestFetchOneClassifiesInformationNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`proxies:
  - {name: "剩余流量：12 GB", type: vless, server: info.example.com, port: 443, uuid: info}
  - {name: "套餐到期：长期有效", type: vless, server: expiry.example.com, port: 443, uuid: expiry}
  - {name: HK, type: vless, server: hk.example.com, port: 443, uuid: real}
`))
	}))
	defer srv.Close()

	st := newTestStore(t)
	id, _ := st.CreateSource(store.Source{Name: "A", URL: srv.URL})
	src, _ := st.GetSource(id)
	if err := NewFetcher(st).FetchOne(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.ListNodes()
	if len(nodes) != 3 {
		t.Fatalf("want auditable proxy and notice records, got %+v", nodes)
	}
	roles := map[string]int{}
	for _, value := range nodes {
		roles[value.Role]++
	}
	if roles["proxy"] != 1 || roles["notice"] != 2 {
		t.Fatalf("wrong role classification: %+v", roles)
	}
	cache, _ := st.GetCache(id)
	if cache.Metadata.Remaining != 12*1024*1024*1024 || cache.Metadata.Provenance["remaining"] != "node_name" {
		t.Fatalf("notice metadata not committed: %+v", cache.Metadata)
	}
	if cache.Metadata.ExpiresAt != "" || cache.Metadata.Provenance["expires_at"] != "node_name" {
		t.Fatalf("never-expiring metadata not committed: %+v", cache.Metadata)
	}
}
