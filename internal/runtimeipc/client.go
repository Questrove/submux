package runtimeipc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"submux/internal/runtimeapi"
)

type Client struct {
	endpoint string
	version  string
	http     *http.Client
}

type ClientError struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

func (e *ClientError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *ClientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewClient(endpoint string, clientVersion string) (*Client, error) {
	if endpoint == "" {
		return nil, errors.New("Runtime endpoint is required")
	}
	if clientVersion == "" || len(clientVersion) > 128 || hasControlCharacter(clientVersion) {
		return nil, errors.New("Runtime client version is invalid")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialContext(ctx, endpoint)
		},
		DisableCompression:  true,
		DisableKeepAlives:   false,
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{
		endpoint: endpoint,
		version:  clientVersion,
		http: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
	}, nil
}

func (c *Client) Observe(ctx context.Context) (runtimeapi.Snapshot, error) {
	if c == nil || c.http == nil {
		return runtimeapi.Snapshot{}, &ClientError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "Runtime client is not initialized",
		}
	}
	if ctx == nil {
		return runtimeapi.Snapshot{}, &ClientError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Runtime client context is required",
		}
	}
	requestID, err := newRequestID()
	if err != nil {
		return runtimeapi.Snapshot{}, &ClientError{
			Code:    runtimeapi.ErrorInternal,
			Message: "could not create Runtime request ID",
			Cause:   err,
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime/v1/snapshot", nil)
	if err != nil {
		return runtimeapi.Snapshot{}, err
	}
	request.Header.Set(HeaderRequestID, requestID)
	request.Header.Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	request.Header.Set(HeaderClientVersion, c.version)

	response, err := c.http.Do(request)
	if err != nil {
		code := runtimeapi.ErrorServiceUnavailable
		message := "Submux Runtime is unavailable"
		if errors.Is(err, os.ErrPermission) {
			code = runtimeapi.ErrorUnauthorized
			message = "permission to access Submux Runtime was denied"
		}
		return runtimeapi.Snapshot{}, &ClientError{
			Code:      code,
			Message:   message,
			Retryable: code == runtimeapi.ErrorServiceUnavailable,
			Cause:     err,
		}
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		var envelope runtimeapi.ErrorEnvelope
		if err := decodeStrictJSON(response.Body, MaxResponseBytes, &envelope); err != nil {
			return runtimeapi.Snapshot{}, &ClientError{
				Code:      runtimeapi.ErrorServiceUnavailable,
				Message:   fmt.Sprintf("Runtime returned HTTP %d with an invalid error response", response.StatusCode),
				Retryable: response.StatusCode >= 500,
				Cause:     err,
			}
		}
		if envelope.Error.Code == "" {
			envelope.Error.Code = runtimeapi.ErrorServiceUnavailable
		}
		return runtimeapi.Snapshot{}, &ClientError{
			Code:      envelope.Error.Code,
			Message:   envelope.Error.Message,
			Retryable: envelope.Error.Retryable,
		}
	}

	var snapshot runtimeapi.Snapshot
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &snapshot); err != nil {
		return runtimeapi.Snapshot{}, &ClientError{
			Code:      runtimeapi.ErrorServiceUnavailable,
			Message:   "Runtime returned an invalid snapshot",
			Retryable: true,
			Cause:     err,
		}
	}
	if snapshot.ProtocolVersion != runtimeapi.ProtocolVersion {
		return runtimeapi.Snapshot{}, &ClientError{
			Code:    runtimeapi.ErrorProtocolUnsupported,
			Message: "Runtime snapshot protocol version is not supported",
		}
	}
	return snapshot, nil
}

func (c *Client) CloseIdleConnections() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func newRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}
