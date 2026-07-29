package runtimeprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type ControlProbe struct {
	Endpoint string
	Timeout  time.Duration
}

func (p ControlProbe) CheckVersion(ctx context.Context) error {
	return p.check(ctx, "/version")
}

func (p ControlProbe) CheckConfig(ctx context.Context) error {
	return p.check(ctx, "/configs")
}

func (p ControlProbe) check(ctx context.Context, path string) error {
	if p.Endpoint == "" {
		return errors.New("Mihomo local control endpoint is required")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialControl(ctx, p.Endpoint)
		},
		DisableCompression: true,
		DisableKeepAlives:  true,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://mihomo"+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("query Mihomo local control endpoint: %w", err)
	}
	defer response.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read Mihomo local control response: %w", readErr)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Mihomo local control endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}
