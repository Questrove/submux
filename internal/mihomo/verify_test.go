package mihomo

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type delayedControlProbe struct {
	mu      sync.Mutex
	readyAt time.Time
	err     error
}

func (p *delayedControlProbe) CheckVersion(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.readyAt) {
		return errors.New("starting")
	}
	return p.err
}

func (p *delayedControlProbe) CheckConfig(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.readyAt) {
		return errors.New("starting")
	}
	return p.err
}

func TestRuntimeCheckWaitsForLocalMihomoReadiness(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	check := &RuntimeCheck{
		Control:       &delayedControlProbe{readyAt: time.Now().Add(60 * time.Millisecond)},
		ReadyTimeout:  time.Second,
		RetryInterval: 10 * time.Millisecond,
	}
	if err := check.VerifyRuntime(context.Background(), proxy.Addr().String()); err != nil {
		t.Fatalf("delayed Mihomo readiness was rejected: %v", err)
	}
}

func TestRuntimeCheckReportsBoundedStartupTimeout(t *testing.T) {
	check := &RuntimeCheck{
		Control:       &delayedControlProbe{err: errors.New("starting")},
		ReadyTimeout:  50 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
	}
	err := check.VerifyRuntime(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "Mihomo startup timed out") {
		t.Fatalf("startup timeout = %v", err)
	}
}

func TestRuntimeCheckRejectsNonLoopbackProxyListener(t *testing.T) {
	check := &RuntimeCheck{Control: &delayedControlProbe{}}
	if err := check.VerifyRuntime(context.Background(), "192.0.2.1:7890"); err == nil {
		t.Fatal("non-loopback explicit proxy listener was accepted")
	}
}
