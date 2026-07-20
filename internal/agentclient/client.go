package agentclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"submux/internal/agentproto"
	"submux/internal/store"
)

type Client struct {
	ServerURL  string
	InstanceID int64
	PrivateKey ed25519.PrivateKey
	HTTP       *http.Client
}

var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

type EnrollmentRequest struct {
	Code         string   `json:"code"`
	PublicKey    string   `json:"public_key"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	AgentVersion string   `json:"agent_version"`
	Capabilities []string `json:"capabilities"`
}

type EnrollmentResponse struct {
	InstanceID      int64  `json:"instance_id"`
	ProtocolVersion int    `json:"protocol_version"`
	RequestID       string `json:"request_id"`
}

type State struct {
	ProtocolVersion int              `json:"protocol_version"`
	Jobs            []store.AgentJob `json:"jobs"`
}

type Heartbeat struct {
	AgentVersion string                   `json:"agent_version"`
	Capabilities []string                 `json:"capabilities"`
	Status       string                   `json:"status"`
	Observation  store.RuntimeObservation `json:"observation"`
}

type LocalAudit struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Revision  string `json:"revision,omitempty"`
	Result    string `json:"result"`
	Summary   string `json:"summary,omitempty"`
}

type RuntimeSecret struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type PlatformSubscription struct {
	Body        []byte
	ContentType string
	Revision    string
}

func Enroll(ctx context.Context, httpClient *http.Client, serverURL string, request EnrollmentRequest) (EnrollmentResponse, error) {
	serverURL = strings.TrimRight(serverURL, "/")
	if err := validateServerURL(serverURL); err != nil {
		return EnrollmentResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/api/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return EnrollmentResponse{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return EnrollmentResponse{}, responseError(response)
	}
	var result EnrollmentResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return result, err
	}
	if result.InstanceID <= 0 || result.ProtocolVersion != agentproto.Version {
		return result, errors.New("control plane returned an incompatible device identity")
	}
	return result, nil
}

func (c *Client) GetState(ctx context.Context) (State, error) {
	var result State
	if err := c.doJSON(ctx, http.MethodGet, "/api/agent/state", nil, &result); err != nil {
		return result, err
	}
	if result.ProtocolVersion != agentproto.Version {
		return result, fmt.Errorf("unsupported control protocol version %d", result.ProtocolVersion)
	}
	return result, nil
}

func (c *Client) SendHeartbeat(ctx context.Context, value Heartbeat) (State, error) {
	var result State
	if err := c.doJSON(ctx, http.MethodPost, "/api/agent/heartbeat", value, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) SendJobStatus(ctx context.Context, id, status string, result json.RawMessage, safeError string) error {
	body := struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  string          `json:"error,omitempty"`
	}{Status: status, Result: result, Error: safeError}
	return c.doJSON(ctx, http.MethodPost, "/api/agent/jobs/"+url.PathEscape(id)+"/status", body, nil)
}

func (c *Client) SendLocalAudit(ctx context.Context, value LocalAudit) error {
	return c.doJSON(ctx, http.MethodPost, "/api/agent/local-audit", value, nil)
}

func (c *Client) RevokeSelf(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/api/agent/revoke-self", struct{}{}, nil)
}

func (c *Client) FetchRuntimeSecret(ctx context.Context, ref string) (RuntimeSecret, error) {
	var result RuntimeSecret
	if err := c.doJSON(ctx, http.MethodPost, "/api/agent/secrets/"+url.PathEscape(ref), struct{}{}, &result); err != nil {
		return result, err
	}
	if result.Kind == "" || result.Value == "" {
		return result, errors.New("control plane returned an empty runtime secret")
	}
	return result, nil
}

func (c *Client) FetchPlatformSubscription(ctx context.Context, id int64) (PlatformSubscription, error) {
	var result PlatformSubscription
	if id <= 0 {
		return result, errors.New("platform subscription id is invalid")
	}
	request, err := c.signedRequest(ctx, http.MethodGet, "/api/agent/platform-subscriptions/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return result, err
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, responseError(response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (10<<20)+1))
	if err != nil {
		return result, err
	}
	if len(body) == 0 || len(body) > 10<<20 {
		return result, errors.New("platform subscription config is empty or exceeds 10 MiB")
	}
	result.Body = body
	result.ContentType = response.Header.Get("Content-Type")
	result.Revision = response.Header.Get("X-Submux-Revision")
	return result, nil
}

// WatchUpdates is only a latency hint. Callers must retain periodic state and
// state polling because a WebSocket notification may be lost.
func (c *Client) WatchUpdates(ctx context.Context, notify func(reason string)) error {
	server, err := url.Parse(strings.TrimRight(c.ServerURL, "/") + "/api/agent/updates")
	if err != nil {
		return err
	}
	switch server.Scheme {
	case "https":
		server.Scheme = "wss"
	case "http":
		server.Scheme = "ws"
	default:
		return errors.New("control plane URL must use http or https")
	}
	config, err := websocket.NewConfig(server.String(), strings.TrimRight(c.ServerURL, "/"))
	if err != nil {
		return err
	}
	httpRequest, _ := http.NewRequest(http.MethodGet, server.String(), nil)
	if err := agentproto.SignRequest(httpRequest, c.InstanceID, c.PrivateKey, nil, time.Now().UTC()); err != nil {
		return err
	}
	config.Header = httpRequest.Header
	connection, err := websocket.DialConfig(config)
	if err != nil {
		return err
	}
	defer connection.Close()
	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()
	for {
		var message struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		}
		if err := websocket.JSON.Receive(connection, &message); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if message.Type == "check_state" && notify != nil {
			notify(message.Reason)
		}
	}
}

// OpenRuntimeStream attaches an on-demand, device-authenticated outbound
// stream to a browser-created relay session. Frames are never persisted by
// the control plane.
func (c *Client) OpenRuntimeStream(ctx context.Context, session, kind string, frames <-chan json.RawMessage) error {
	if session == "" || (kind != "proxies" && kind != "configs" && kind != "rules" && kind != "connections" && kind != "traffic" && kind != "memory" && kind != "logs" && kind != "agent_logs") {
		return errors.New("invalid runtime stream session")
	}
	server, err := url.Parse(strings.TrimRight(c.ServerURL, "/") + "/api/agent/runtime-stream/" + url.PathEscape(session))
	if err != nil {
		return err
	}
	switch server.Scheme {
	case "https":
		server.Scheme = "wss"
	case "http":
		server.Scheme = "ws"
	default:
		return errors.New("control plane URL must use http or https")
	}
	config, err := websocket.NewConfig(server.String(), strings.TrimRight(c.ServerURL, "/"))
	if err != nil {
		return err
	}
	httpRequest, _ := http.NewRequest(http.MethodGet, server.String(), nil)
	if err := agentproto.SignRequest(httpRequest, c.InstanceID, c.PrivateKey, nil, time.Now().UTC()); err != nil {
		return err
	}
	config.Header = httpRequest.Header
	connection, err := websocket.DialConfig(config)
	if err != nil {
		return err
	}
	defer connection.Close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			if len(frame) == 0 || len(frame) > 256<<10 {
				return errors.New("runtime stream frame exceeds protocol limits")
			}
			if err := websocket.Message.Send(connection, string(frame)); err != nil {
				return err
			}
		}
	}
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
	request, err := c.signedRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
}

func (c *Client) signedRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	if err := validateServerURL(c.ServerURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.ServerURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if err := agentproto.SignRequest(request, c.InstanceID, c.PrivateKey, body, time.Now().UTC()); err != nil {
		return nil, err
	}
	return request, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

func validateServerURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return errors.New("control plane URL must be an absolute http(s) URL without user info")
	}
	return nil
}

func responseError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("control plane returned %d: %s", response.StatusCode, message)
}
