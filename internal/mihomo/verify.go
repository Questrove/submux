package mihomo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ControlProbe checks Mihomo over its Runtime-owned local control transport.
// The transport implementation is deliberately outside this package so this
// verifier cannot introduce a TCP management fallback.
type ControlProbe interface {
	CheckVersion(ctx context.Context) error
	CheckConfig(ctx context.Context) error
}

type RuntimeCheck struct {
	Control       ControlProbe
	Dialer        *net.Dialer
	ReadyTimeout  time.Duration
	RetryInterval time.Duration
}

func (v *RuntimeCheck) VerifyRuntime(ctx context.Context, proxyAddr string) error {
	if v.Control == nil {
		return errors.New("Mihomo local control probe is required")
	}
	if proxyAddr != "" {
		host, _, err := net.SplitHostPort(proxyAddr)
		if err != nil {
			return errors.New("proxy listener must contain a port")
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("proxy listener must use a loopback address")
		}
	}
	timeout := v.ReadyTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	interval := v.RetryInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		if err := v.verifyOnce(readyContext, proxyAddr); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-readyContext.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("Mihomo startup timed out after %s: %w", timeout, lastErr)
		case <-timer.C:
		}
	}
}

func (v *RuntimeCheck) verifyOnce(ctx context.Context, proxyAddr string) error {
	if err := v.Control.CheckVersion(ctx); err != nil {
		return err
	}
	if err := v.Control.CheckConfig(ctx); err != nil {
		return err
	}
	if proxyAddr == "" {
		return nil
	}
	dialer := v.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 3 * time.Second}
	}
	connection, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return err
	}
	return connection.Close()
}
