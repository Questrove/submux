package runtimetraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"submux/internal/runtimeprocess"
)

const defaultMihomoResponseLimit = 16 << 20

type MihomoReader struct {
	Endpoint string
	Timeout  time.Duration
	Dial     func(context.Context) (net.Conn, error)
}

func (r MihomoReader) ReadTraffic(ctx context.Context) (Reading, error) {
	if r.Endpoint == "" && r.Dial == nil {
		return Reading{}, errors.New("Mihomo local control endpoint is required")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if r.Dial != nil {
				return r.Dial(ctx)
			}
			return runtimeprocess.DialControl(ctx, r.Endpoint)
		},
		DisableCompression: true,
		DisableKeepAlives:  true,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://mihomo/connections", nil)
	if err != nil {
		return Reading{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Reading{}, fmt.Errorf("query Mihomo traffic counters: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return Reading{}, fmt.Errorf("Mihomo traffic endpoint returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		UploadTotal   uint64            `json:"uploadTotal"`
		DownloadTotal uint64            `json:"downloadTotal"`
		Connections   []json.RawMessage `json:"connections"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, defaultMihomoResponseLimit+1))
	if err != nil {
		return Reading{}, fmt.Errorf("read Mihomo traffic counters: %w", err)
	}
	if len(body) > defaultMihomoResponseLimit {
		return Reading{}, errors.New("Mihomo traffic response exceeds the size limit")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Reading{}, fmt.Errorf("decode Mihomo traffic counters: %w", err)
	}
	return Reading{
		UploadTotal:       payload.UploadTotal,
		DownloadTotal:     payload.DownloadTotal,
		ActiveConnections: len(payload.Connections),
	}, nil
}
