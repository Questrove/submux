package mihomo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLocalHTTPProxyProbePerformsARealProxyRequest(t *testing.T) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		outbound := request.Clone(request.Context())
		outbound.RequestURI = ""
		response, err := transport.RoundTrip(outbound)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				writer.Header().Add(key, value)
			}
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
	}))
	defer proxy.Close()

	address := proxy.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (LocalHTTPProxyProbe{}).Probe(ctx, address); err != nil {
		t.Fatalf("probe local HTTP proxy: %v", err)
	}
}

func TestLocalHTTPProxyProbeRejectsNonLoopbackProxy(t *testing.T) {
	if err := (LocalHTTPProxyProbe{}).Probe(context.Background(), "192.0.2.1:7890"); err == nil {
		t.Fatal("proxy probe accepted non-loopback address")
	}
}
