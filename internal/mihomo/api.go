package mihomo

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
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

const delayTestURL = "https://www.gstatic.com/generate_204"

type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

type Version struct {
	Version string `json:"version"`
	Meta    bool   `json:"meta"`
}

type Proxy struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Now     string         `json:"now,omitempty"`
	All     []string       `json:"all,omitempty"`
	History []DelayHistory `json:"history,omitempty"`
}

type DelayHistory struct {
	Time  string `json:"time"`
	Delay int    `json:"delay"`
}

type Connection struct {
	ID       string         `json:"id"`
	Upload   int64          `json:"upload"`
	Download int64          `json:"download"`
	Chains   []string       `json:"chains,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type Connections struct {
	DownloadTotal int64        `json:"downloadTotal"`
	UploadTotal   int64        `json:"uploadTotal"`
	Connections   []Connection `json:"connections"`
}

var (
	sensitiveRuntimeKey = regexp.MustCompile(`(?i)(authorization|password|passwd|token|secret|private.?key|subscription|^url$|^uri$)`)
	runtimeURL          = regexp.MustCompile(`(?i)(?:https?|vless|vmess|trojan|ss|hysteria2)://[^\s"']+`)
	runtimeAssignment   = regexp.MustCompile(`(?i)\b(authorization|password|passwd|token|secret|private[_ -]?key)\b\s*[:=]\s*[^\s,;]+`)
	runtimeBearer       = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
)

func NewClient(controller, secret string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(controller)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil {
		return nil, errors.New("Mihomo controller must be a plain loopback HTTP origin")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("Mihomo controller must use a loopback address")
	}
	if parsed.Port() == "" || secret == "" {
		return nil, errors.New("Mihomo controller port and secret are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(controller, "/"), secret: secret, http: httpClient}, nil
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var result Version
	err := c.doJSON(ctx, http.MethodGet, "/version", nil, &result)
	return result, err
}

func (c *Client) Configs(ctx context.Context) (map[string]any, error) {
	result := make(map[string]any)
	err := c.doJSON(ctx, http.MethodGet, "/configs", nil, &result)
	return result, err
}

func (c *Client) Rules(ctx context.Context) (map[string]any, error) {
	result := make(map[string]any)
	err := c.doJSON(ctx, http.MethodGet, "/rules", nil, &result)
	return result, err
}

func (c *Client) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var response struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/proxies", nil, &response); err != nil {
		return nil, err
	}
	for name, proxy := range response.Proxies {
		proxy.Name = name
		response.Proxies[name] = proxy
	}
	return response.Proxies, nil
}

func (c *Client) Delay(ctx context.Context, group, proxy string, timeout time.Duration) (int, error) {
	proxies, err := c.Proxies(ctx)
	if err != nil {
		return 0, err
	}
	selector, ok := proxies[group]
	if !ok || selector.Type != "Selector" {
		return 0, errors.New("target is not a currently observed select group")
	}
	if _, ok := proxies[proxy]; !ok || !contains(selector.All, proxy) {
		return 0, errors.New("proxy is not a currently observed member of the select group")
	}
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 5 * time.Second
	}
	query := url.Values{"url": {delayTestURL}, "timeout": {strconv.FormatInt(timeout.Milliseconds(), 10)}, "expected": {"200-299"}}
	var result struct {
		Delay int `json:"delay"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/proxies/"+url.PathEscape(proxy)+"/delay?"+query.Encode(), nil, &result); err != nil {
		return 0, err
	}
	return result.Delay, nil
}

func (c *Client) Select(ctx context.Context, group, proxy string) error {
	proxies, err := c.Proxies(ctx)
	if err != nil {
		return err
	}
	value, ok := proxies[group]
	if !ok || value.Type != "Selector" {
		return errors.New("target is not a currently observed select group")
	}
	if !contains(value.All, proxy) {
		return errors.New("proxy is not a currently observed member of the select group")
	}
	return c.doJSON(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": proxy}, nil)
}

func (c *Client) Connections(ctx context.Context) (Connections, error) {
	var result Connections
	err := c.doJSON(ctx, http.MethodGet, "/connections", nil, &result)
	return result, err
}

func (c *Client) CloseConnection(ctx context.Context, id string) error {
	connections, err := c.Connections(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, connection := range connections.Connections {
		if connection.ID == id {
			found = true
			break
		}
	}
	if !found {
		return errors.New("connection is not currently observed")
	}
	return c.doJSON(ctx, http.MethodDelete, "/connections/"+url.PathEscape(id), nil, nil)
}

// Stream exposes only the fixed Mihomo observation endpoints needed by the
// on-demand control-plane relay. No stream is opened until a browser asks for
// one, and every frame is validated, size-limited and redacted locally.
func (c *Client) Stream(ctx context.Context, kind string, send func(json.RawMessage) error) error {
	if send == nil {
		return errors.New("runtime stream consumer is required")
	}
	if kind == "proxies" || kind == "configs" || kind == "rules" {
		var value any
		var err error
		switch kind {
		case "proxies":
			value, err = c.Proxies(ctx)
		case "configs":
			value, err = c.Configs(ctx)
		case "rules":
			value, err = c.Rules(ctx)
		}
		if err != nil {
			return err
		}
		frame, err := sanitizedRuntimeFrame(kind, value)
		if err != nil {
			return err
		}
		return send(frame)
	}
	path := ""
	switch kind {
	case "connections":
		path = "/connections"
	case "traffic":
		path = "/traffic"
	case "memory":
		path = "/memory"
	case "logs":
		path = "/logs?level=info"
	default:
		return errors.New("unsupported Mihomo runtime stream")
	}
	streamURL, err := url.Parse(c.baseURL + path)
	if err != nil {
		return err
	}
	streamURL.Scheme = "ws"
	config, err := websocket.NewConfig(streamURL.String(), c.baseURL)
	if err != nil {
		return err
	}
	config.Header.Set("Authorization", "Bearer "+c.secret)
	connection, err := websocket.DialConfig(config)
	if err != nil {
		return err
	}
	defer connection.Close()
	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()
	windowStart, windowFrames, windowBytes := time.Now(), 0, 0
	for {
		var raw string
		if err := websocket.Message.Receive(connection, &raw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if len(raw) == 0 || len(raw) > 256<<10 || !json.Valid([]byte(raw)) {
			return errors.New("Mihomo runtime stream returned an invalid or oversized frame")
		}
		if time.Since(windowStart) >= time.Second {
			windowStart, windowFrames, windowBytes = time.Now(), 0, 0
		}
		windowFrames++
		windowBytes += len(raw)
		if windowFrames > 20 || windowBytes > 1<<20 {
			return errors.New("Mihomo runtime stream exceeded local rate limits")
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return errors.New("Mihomo runtime stream returned invalid JSON")
		}
		frame, err := sanitizedRuntimeFrame(kind, value)
		if err != nil {
			return err
		}
		if err := send(frame); err != nil {
			return err
		}
	}
}

func sanitizedRuntimeFrame(kind string, value any) (json.RawMessage, error) {
	value = sanitizeRuntimeValue(kind, "", value)
	raw, err := json.Marshal(map[string]any{"kind": kind, "data": value})
	if err != nil {
		return nil, errors.New("could not encode a Mihomo runtime frame")
	}
	return raw, nil
}

func sanitizeRuntimeValue(kind, key string, value any) any {
	if sensitiveRuntimeKey.MatchString(key) {
		return "[redacted]"
	}
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		for childKey, childValue := range current {
			result[childKey] = sanitizeRuntimeValue(kind, childKey, childValue)
		}
		return result
	case map[string]Proxy:
		result := make(map[string]any, len(current))
		for childKey, childValue := range current {
			result[childKey] = sanitizeRuntimeValue(kind, childKey, map[string]any{
				"name": childValue.Name, "type": childValue.Type, "now": childValue.Now, "all": childValue.All, "history": childValue.History,
			})
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, child := range current {
			result[index] = sanitizeRuntimeValue(kind, key, child)
		}
		return result
	case string:
		if kind == "logs" || kind == "configs" {
			return SanitizeLogText(current)
		}
		return current
	default:
		return value
	}
}

// SanitizeLogText removes credentials and URL-shaped configuration values
// before Agent-owned diagnostics leave the local Mihomo boundary.
func SanitizeLogText(value string) string {
	value = runtimeURL.ReplaceAllString(value, "[redacted-url]")
	value = runtimeBearer.ReplaceAllString(value, "Bearer [redacted]")
	return runtimeAssignment.ReplaceAllString(value, "$1=[redacted]")
}

func (c *Client) doJSON(ctx context.Context, method, path string, input, output any) error {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.secret)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("Mihomo API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
