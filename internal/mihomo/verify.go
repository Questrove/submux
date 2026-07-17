package mihomo

import (
	"context"
	"errors"
	"net"
	"time"
)

type RuntimeCheck struct {
	Client *Client
	Dialer *net.Dialer
}

func (v *RuntimeCheck) VerifyRuntime(ctx context.Context, proxyAddr string) error {
	if v.Client == nil {
		return errors.New("Mihomo API client is required")
	}
	if _, err := v.Client.Version(ctx); err != nil {
		return err
	}
	if _, err := v.Client.Configs(ctx); err != nil {
		return err
	}
	if proxyAddr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return errors.New("proxy listener must contain a port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("proxy listener must use a loopback address")
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
