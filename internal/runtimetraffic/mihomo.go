package runtimetraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"submux/internal/runtimeapi"
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
		UploadTotal   uint64                 `json:"uploadTotal"`
		DownloadTotal uint64                 `json:"downloadTotal"`
		Connections   []mihomoConnectionWire `json:"connections"`
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
	connections := make([]runtimeapi.Connection, 0, len(payload.Connections))
	for _, wire := range payload.Connections {
		connections = append(connections, wire.connection())
	}
	sort.Slice(connections, func(left, right int) bool {
		leftUnknown := connections[left].StartedAt.IsZero()
		rightUnknown := connections[right].StartedAt.IsZero()
		if leftUnknown != rightUnknown {
			return !leftUnknown
		}
		if connections[left].StartedAt.Equal(connections[right].StartedAt) {
			return connections[left].ID < connections[right].ID
		}
		return connections[left].StartedAt.After(connections[right].StartedAt)
	})
	return Reading{
		UploadTotal:       payload.UploadTotal,
		DownloadTotal:     payload.DownloadTotal,
		ActiveConnections: len(connections),
		Connections:       connections,
	}, nil
}

type mihomoConnectionWire struct {
	ID       string             `json:"id"`
	Metadata mihomoMetadataWire `json:"metadata"`
	Upload   uint64             `json:"upload"`
	Download uint64             `json:"download"`
	Start    time.Time          `json:"start"`
	Chains   []string           `json:"chains"`
	Rule     string             `json:"rule"`
	Payload  string             `json:"rulePayload"`
}

type mihomoMetadataWire struct {
	Network         string `json:"network"`
	Type            string `json:"type"`
	SourceIP        string `json:"sourceIP"`
	SourcePort      string `json:"sourcePort"`
	DestinationIP   string `json:"destinationIP"`
	DestinationPort string `json:"destinationPort"`
	Host            string `json:"host"`
	Process         string `json:"process"`
}

func (wire mihomoConnectionWire) connection() runtimeapi.Connection {
	targetHost := wire.Metadata.Host
	if targetHost == "" {
		targetHost = wire.Metadata.DestinationIP
	}
	chain := make([]string, 0, min(len(wire.Chains), 16))
	for _, node := range wire.Chains {
		if len(chain) == 16 {
			break
		}
		chain = append(chain, boundedText(node, 256))
	}
	return runtimeapi.Connection{
		ID:            boundedText(wire.ID, 128),
		Source:        boundedText(joinConnectionAddress(wire.Metadata.SourceIP, wire.Metadata.SourcePort), 512),
		Target:        boundedText(joinConnectionAddress(targetHost, wire.Metadata.DestinationPort), 512),
		Protocol:      boundedText(wire.Metadata.Network, 32),
		Inbound:       boundedText(wire.Metadata.Type, 64),
		Process:       boundedText(wire.Metadata.Process, 256),
		Rule:          boundedText(wire.Rule, 256),
		RulePayload:   boundedText(wire.Payload, 512),
		OutboundChain: chain,
		StartedAt:     wire.Start.UTC(),
		Upload:        wire.Upload,
		Download:      wire.Download,
	}
}

func joinConnectionAddress(host string, port string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		return "未知"
	}
	if port == "" || port == "0" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func boundedText(value string, maximumRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maximumRunes {
		runes = runes[:maximumRunes]
	}
	return string(runes)
}
