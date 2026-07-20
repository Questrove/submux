package mihomo

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRuntimeCheckWaitsForMihomoReadiness(t *testing.T) {
	readyAt := time.Now().Add(60 * time.Millisecond)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(readyAt) {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"version":"v1.19.29","meta":true}`))
			return
		}
		if r.URL.Path == "/configs" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer controller.Close()
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client, err := NewClient(controller.URL, "test-secret", controller.Client())
	if err != nil {
		t.Fatal(err)
	}
	check := &RuntimeCheck{Client: client, ReadyTimeout: time.Second, RetryInterval: 10 * time.Millisecond}
	if err := check.VerifyRuntime(context.Background(), proxy.Addr().String()); err != nil {
		t.Fatalf("delayed Mihomo readiness was rejected: %v", err)
	}
}

func TestRuntimeCheckReportsBoundedStartupTimeout(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	defer controller.Close()
	client, err := NewClient(controller.URL, "test-secret", controller.Client())
	if err != nil {
		t.Fatal(err)
	}
	check := &RuntimeCheck{Client: client, ReadyTimeout: 50 * time.Millisecond, RetryInterval: 10 * time.Millisecond}
	err = check.VerifyRuntime(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "Mihomo startup timed out") {
		t.Fatalf("startup timeout = %v", err)
	}
}
