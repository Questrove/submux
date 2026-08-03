package runtimeprocess

import (
	"net/url"
	"strings"
	"testing"
)

func TestProxyDelayPathUsesRuntimeFixedURLAndTimeout(t *testing.T) {
	path := proxyDelayPath("Tokyo/JP")
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.EscapedPath() != "/proxies/Tokyo%2FJP/delay" {
		t.Fatalf("escaped path=%q", parsed.EscapedPath())
	}
	if parsed.Query().Get("url") != ProxyDelayTestURL || parsed.Query().Get("timeout") != "5000" {
		t.Fatalf("query=%v", parsed.Query())
	}
}

func TestDecodeProxyDelayResponse(t *testing.T) {
	delay, err := decodeProxyDelayResponse(strings.NewReader(`{"delay":42}`))
	if err != nil || delay != 42 {
		t.Fatalf("delay=%d err=%v", delay, err)
	}
	for _, body := range []string{`{}`, `{"delay":-1}`, `{"delay":"42"}`} {
		if _, err := decodeProxyDelayResponse(strings.NewReader(body)); err == nil {
			t.Fatalf("invalid response accepted: %s", body)
		}
	}
}
