package resourceproxy

import (
	"net/http"
	"testing"
	"time"
)

func TestValidateAcceptsSupportedModesAndRejectsUnsafeURLs(t *testing.T) {
	for _, value := range []Config{
		{},
		{Mode: ModeDirect},
		{Mode: ModeHTTP, URL: "http://127.0.0.1:1080"},
		{Mode: ModeSOCKS5, URL: "socks5://proxy.example.com:1080"},
	} {
		if err := Validate(value); err != nil {
			t.Fatalf("valid config %+v was rejected: %v", value, err)
		}
	}
	for _, value := range []Config{
		{Mode: ModeDirect, URL: "http://127.0.0.1:1080"},
		{Mode: ModeHTTP, URL: "socks5://127.0.0.1:1080"},
		{Mode: ModeHTTP, URL: "http://user:pass@127.0.0.1:1080"},
		{Mode: ModeHTTP, URL: "http://127.0.0.1:1080/path"},
		{Mode: ModeSOCKS5, URL: "socks5://127.0.0.1"},
	} {
		if err := Validate(value); err == nil {
			t.Fatalf("unsafe config %+v was accepted", value)
		}
	}
}

func TestDirectClientDoesNotInheritProcessProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:65535")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:65535")
	client, err := NewClient(Config{Mode: ModeDirect}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("direct resource client inherited a process proxy: %#v", client.Transport)
	}
}

func TestProxyClientUsesOnlyTheConfiguredProxy(t *testing.T) {
	client, err := NewClient(Config{Mode: ModeSOCKS5, URL: "socks5://127.0.0.1:1080"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	request, _ := http.NewRequest(http.MethodGet, "https://github.com/", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil || proxyURL.String() != "socks5://127.0.0.1:1080" {
		t.Fatalf("configured proxy was not used: url=%v err=%v", proxyURL, err)
	}
}
