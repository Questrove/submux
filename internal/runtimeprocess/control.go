package runtimeprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type ControlProbe struct {
	Endpoint string
	Timeout  time.Duration
}

type ControlProxyHistory struct {
	Time  time.Time `json:"time"`
	Delay int       `json:"delay"`
}

type ControlProxy struct {
	Name    string                `json:"name"`
	Type    string                `json:"type"`
	Now     string                `json:"now"`
	All     []string              `json:"all"`
	Alive   *bool                 `json:"alive,omitempty"`
	History []ControlProxyHistory `json:"history,omitempty"`
}

type controlProxyResponse struct {
	Proxies map[string]ControlProxy `json:"proxies"`
}

func (p ControlProbe) CheckVersion(ctx context.Context) error {
	return p.check(ctx, "/version")
}

func (p ControlProbe) CheckConfig(ctx context.Context) error {
	return p.check(ctx, "/configs")
}

func (p ControlProbe) check(ctx context.Context, path string) error {
	response, err := p.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
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

func (p ControlProbe) ProxyGroups(ctx context.Context) (map[string]ControlProxy, error) {
	response, err := p.do(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("Mihomo local control endpoint returned HTTP %d", response.StatusCode)
	}
	var body controlProxyResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode Mihomo proxy groups: %w", err)
	}
	if body.Proxies == nil {
		return nil, errors.New("Mihomo proxy group response is incomplete")
	}
	return body.Proxies, nil
}

func (p ControlProbe) SelectProxy(ctx context.Context, group, node string) error {
	if group == "" || node == "" {
		return errors.New("Mihomo proxy group and node are required")
	}
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: node})
	if err != nil {
		return err
	}
	response, err := p.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read Mihomo local control response: %w", readErr)
	}
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("Mihomo local control endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (p ControlProbe) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if p.Endpoint == "" {
		return nil, errors.New("Mihomo local control endpoint is required")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return DialControl(ctx, p.Endpoint)
		},
		DisableCompression: true,
		DisableKeepAlives:  true,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, method, "http://mihomo"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query Mihomo local control endpoint: %w", err)
	}
	return response, nil
}

func DialControl(ctx context.Context, endpoint string) (net.Conn, error) {
	return dialControl(ctx, endpoint)
}
