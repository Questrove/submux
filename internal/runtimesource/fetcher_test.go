package runtimesource

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestFetcherUsesConditionalHeadersAndHandlesNotModified(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if requests == 1 {
			writer.Header().Set("ETag", `"revision-1"`)
			writer.Header().Set("Last-Modified", "Wed, 30 Jul 2025 08:00:00 GMT")
			_, _ = writer.Write([]byte("proxies: []\n"))
			return
		}
		if request.Header.Get("If-None-Match") != `"revision-1"` ||
			request.Header.Get("If-Modified-Since") != "Wed, 30 Jul 2025 08:00:00 GMT" {
			t.Errorf("conditional headers = %q / %q",
				request.Header.Get("If-None-Match"),
				request.Header.Get("If-Modified-Since"))
		}
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	config := mustSourceConfig(t, runtimeapi.RemoteSourceDraft{
		Name: "loopback",
		URL:  server.URL + "/config.yaml",
	})
	fetcher := &Fetcher{}
	first, err := fetcher.Fetch(context.Background(), config, ConditionalRequest{})
	if err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	if string(first.Body) != "proxies: []\n" || first.SHA256 == "" ||
		first.ETag != `"revision-1"` {
		t.Fatalf("initial result = %#v", first)
	}
	second, err := fetcher.Fetch(context.Background(), config, ConditionalRequest{
		ETag:         first.ETag,
		LastModified: first.LastModified,
	})
	if err != nil {
		t.Fatalf("conditional fetch: %v", err)
	}
	if !second.NotModified || len(second.Body) != 0 {
		t.Fatalf("conditional result = %#v", second)
	}
}

func TestFetcherDoesNotForwardCredentialsOrConditionalHeadersAcrossTargets(t *testing.T) {
	var receivedAuthorization string
	var receivedReferer string
	var receivedETag string
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAuthorization = request.Header.Get("Authorization")
		receivedReferer = request.Header.Get("Referer")
		receivedETag = request.Header.Get("If-None-Match")
		_, _ = writer.Write([]byte("proxies: []\n"))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, _, ok := request.BasicAuth(); !ok {
			t.Error("initial request did not contain configured Basic authentication")
		}
		http.Redirect(writer, request, target.URL+"/config.yaml", http.StatusFound)
	}))
	defer source.Close()

	config := mustSourceConfig(t, runtimeapi.RemoteSourceDraft{
		Name:     "credentials",
		URL:      source.URL + "/config.yaml?token=secret",
		Username: "operator",
		Password: "password",
	})
	_, err := (&Fetcher{}).Fetch(context.Background(), config, ConditionalRequest{
		ETag: `"private-revision"`,
	})
	if err != nil {
		t.Fatalf("fetch redirect: %v", err)
	}
	if receivedAuthorization != "" || receivedReferer != "" || receivedETag != "" {
		t.Fatalf("cross-target headers leaked: authorization=%q referer=%q etag=%q",
			receivedAuthorization, receivedReferer, receivedETag)
	}
}

func TestFetcherRechecksDNSBeforeDialAndBlocksRebinding(t *testing.T) {
	resolver := &sequenceResolver{
		responses: [][]netip.Addr{
			{netip.MustParseAddr("93.184.216.34")},
			{netip.MustParseAddr("169.254.169.254")},
		},
	}
	dialed := false
	config := mustSourceConfig(t, runtimeapi.RemoteSourceDraft{
		Name: "rebinding",
		URL:  "https://rebind.example/config.yaml",
	})
	_, err := (&Fetcher{
		Resolver: resolver,
		DirectDialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("must not dial")
		},
	}).Fetch(context.Background(), config, ConditionalRequest{})
	var fetchError *FetchError
	if !errors.As(err, &fetchError) || fetchError.Class != FailurePolicy {
		t.Fatalf("rebinding error = %#v / %v", fetchError, err)
	}
	if dialed || resolver.callCount() < 2 {
		t.Fatalf("dialed=%v resolver calls=%d", dialed, resolver.callCount())
	}
}

func TestFetcherEnforcesDecodedResponseLimit(t *testing.T) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	_, _ = gzipWriter.Write(bytes.Repeat([]byte("a"), 4096))
	_ = gzipWriter.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		_, _ = writer.Write(compressed.Bytes())
	}))
	defer server.Close()

	config := mustSourceConfig(t, runtimeapi.RemoteSourceDraft{
		Name:             "large",
		URL:              server.URL,
		MaxResponseBytes: 1024,
	})
	_, err := (&Fetcher{}).Fetch(context.Background(), config, ConditionalRequest{})
	var fetchError *FetchError
	if !errors.As(err, &fetchError) || fetchError.Result != "response_too_large" {
		t.Fatalf("large response error = %#v / %v", fetchError, err)
	}
}

func TestFetcherClassifiesRetryAfterAndDoesNotSwitchAwayFromMihomo(t *testing.T) {
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.Header().Set("Retry-After", "120")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	config := mustSourceConfig(t, runtimeapi.RemoteSourceDraft{
		Name: "retry",
		URL:  server.URL,
	})
	_, err := (&Fetcher{Now: func() time.Time { return now }}).Fetch(
		context.Background(),
		config,
		ConditionalRequest{},
	)
	var fetchError *FetchError
	if !errors.As(err, &fetchError) ||
		fetchError.Class != FailureTemporary ||
		fetchError.RetryAfter != 2*time.Minute {
		t.Fatalf("retry error = %#v / %v", fetchError, err)
	}

	config.Route = runtimeapi.SourceRouteMihomo
	_, err = (&Fetcher{MihomoAddress: "127.0.0.1:1"}).Fetch(
		context.Background(),
		config,
		ConditionalRequest{},
	)
	if !errors.As(err, &fetchError) || fetchError.Result != "network_error" {
		t.Fatalf("Mihomo route error = %#v / %v", fetchError, err)
	}
	if requests != 1 {
		t.Fatalf("origin requests = %d, want 1; Mihomo failure silently used direct route", requests)
	}
}

func TestFetcherKeepsTLSVerificationEnabledByDefault(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("proxies: []\n"))
	}))
	defer server.Close()
	config := mustSourceConfig(t, runtimeapi.RemoteSourceDraft{
		Name: "untrusted-tls",
		URL:  server.URL,
	})
	_, err := (&Fetcher{}).Fetch(context.Background(), config, ConditionalRequest{})
	var fetchError *FetchError
	if !errors.As(err, &fetchError) || fetchError.Result != "tls_error" {
		t.Fatalf("TLS verification error = %#v / %v", fetchError, err)
	}
}

func TestFetcherUsesOnlyExplicitTargetBoundTLSCompatibility(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("proxies: []\n"))
	}))
	defer server.Close()
	customCA := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	tests := []runtimeapi.RemoteSourceDraft{
		{
			Name:             "custom-ca",
			URL:              server.URL,
			AuthorizedTarget: server.URL,
			CustomCAPEM:      string(customCA),
		},
		{
			Name:             "skip-tls",
			URL:              server.URL,
			AuthorizedTarget: server.URL,
			SkipTLSVerify:    true,
		},
	}
	for _, draft := range tests {
		t.Run(draft.Name, func(t *testing.T) {
			config := mustSourceConfig(t, draft)
			result, err := (&Fetcher{}).Fetch(context.Background(), config, ConditionalRequest{})
			if err != nil || string(result.Body) != "proxies: []\n" {
				t.Fatalf("target-bound TLS compatibility fetch = %#v err=%v", result, err)
			}
		})
	}
}

type sequenceResolver struct {
	mu        sync.Mutex
	responses [][]netip.Addr
	calls     int
}

func (r *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.calls
	r.calls++
	if index >= len(r.responses) {
		index = len(r.responses) - 1
	}
	return append([]netip.Addr(nil), r.responses[index]...), nil
}

func (r *sequenceResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func mustSourceConfig(t *testing.T, draft runtimeapi.RemoteSourceDraft) SourceConfig {
	t.Helper()
	config, err := NormalizeDraft(draft)
	if err != nil {
		t.Fatalf("normalize source draft: %v", err)
	}
	return config
}
