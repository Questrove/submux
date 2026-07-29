package runtimeipc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Code            string
	Message         string
	Retryable       bool
	CurrentRevision uint64
	Cause           error
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

func (c *Client) UploadImport(
	ctx context.Context,
	contentType string,
	body []byte,
) (runtimeapi.ImportContent, error) {
	var content runtimeapi.ImportContent
	requestID, err := newRequestID()
	if err != nil {
		return content, err
	}
	digest := sha256.Sum256(body)
	request, err := c.newRequest(ctx, http.MethodPost, "/v1/imports", bytes.NewReader(body), requestID)
	if err != nil {
		return content, err
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set(HeaderContentSize, strconv.FormatInt(int64(len(body)), 10))
	request.Header.Set(HeaderContentSHA256, hex.EncodeToString(digest[:]))
	response, err := c.do(request)
	if err != nil {
		return content, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return content, decodeClientError(response)
	}
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &content); err != nil {
		return content, invalidResponseError("import response", err)
	}
	return content, nil
}

func (c *Client) GetAdvancedOverride(ctx context.Context) (runtimeapi.AdvancedOverrideDocument, error) {
	var document runtimeapi.AdvancedOverrideDocument
	requestID, err := newRequestID()
	if err != nil {
		return document, err
	}
	request, err := c.newRequest(ctx, http.MethodGet, "/v1/advanced-override", nil, requestID)
	if err != nil {
		return document, err
	}
	response, err := c.do(request)
	if err != nil {
		return document, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return document, decodeClientError(response)
	}
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &document); err != nil {
		return document, invalidResponseError("advanced override response", err)
	}
	return document, nil
}

func (c *Client) PreviewCandidate(
	ctx context.Context,
	contentID string,
) (runtimeapi.CandidatePreview, error) {
	return c.PreviewCandidateRequest(ctx, runtimeapi.PreviewCandidateRequest{ContentID: contentID})
}

func (c *Client) PreviewCandidateRequest(
	ctx context.Context,
	requestValue runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	var preview runtimeapi.CandidatePreview
	requestID, err := newRequestID()
	if err != nil {
		return preview, err
	}
	body, err := json.Marshal(requestValue)
	if err != nil {
		return preview, err
	}
	request, err := c.newRequest(
		ctx,
		http.MethodPost,
		"/v1/candidates/preview",
		bytes.NewReader(body),
		requestID,
	)
	if err != nil {
		return preview, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		return preview, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return preview, decodeClientError(response)
	}
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &preview); err != nil {
		return preview, invalidResponseError("candidate preview response", err)
	}
	return preview, nil
}

func (c *Client) Execute(
	ctx context.Context,
	requestValue runtimeapi.CreateOperationRequest,
) (runtimeapi.Operation, error) {
	if requestValue.RequestID == "" {
		requestID, err := newRequestID()
		if err != nil {
			return runtimeapi.Operation{}, err
		}
		requestValue.RequestID = requestID
	}
	body, err := json.Marshal(requestValue)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	request, err := c.newRequest(
		ctx,
		http.MethodPost,
		"/v1/operations",
		bytes.NewReader(body),
		requestValue.RequestID,
	)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return runtimeapi.Operation{}, decodeClientError(response)
	}
	var envelope runtimeapi.OperationResponse
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &envelope); err != nil {
		return runtimeapi.Operation{}, invalidResponseError("operation response", err)
	}
	return envelope.Operation, nil
}

func (c *Client) GetOperation(ctx context.Context, id string) (runtimeapi.Operation, error) {
	requestID, err := newRequestID()
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	request, err := c.newRequest(ctx, http.MethodGet, "/v1/operations/"+id, nil, requestID)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	response, err := c.do(request)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimeapi.Operation{}, decodeClientError(response)
	}
	var envelope runtimeapi.OperationResponse
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &envelope); err != nil {
		return runtimeapi.Operation{}, invalidResponseError("operation response", err)
	}
	return envelope.Operation, nil
}

func (c *Client) CancelOperation(
	ctx context.Context,
	id string,
	requestValue runtimeapi.CancelOperationRequest,
) (runtimeapi.Operation, error) {
	if requestValue.RequestID == "" {
		requestID, err := newRequestID()
		if err != nil {
			return runtimeapi.Operation{}, err
		}
		requestValue.RequestID = requestID
	}
	body, err := json.Marshal(requestValue)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	request, err := c.newRequest(
		ctx,
		http.MethodPost,
		"/v1/operations/"+id+"/cancel",
		bytes.NewReader(body),
		requestValue.RequestID,
	)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		return runtimeapi.Operation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimeapi.Operation{}, decodeClientError(response)
	}
	var envelope runtimeapi.OperationResponse
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &envelope); err != nil {
		return runtimeapi.Operation{}, invalidResponseError("cancellation response", err)
	}
	return envelope.Operation, nil
}

func (c *Client) VerifyProxy(ctx context.Context) (runtimeapi.ProxyVerification, error) {
	requestID, err := newRequestID()
	if err != nil {
		return runtimeapi.ProxyVerification{}, err
	}
	request, err := c.newRequest(ctx, http.MethodGet, "/v1/proxy/verify", nil, requestID)
	if err != nil {
		return runtimeapi.ProxyVerification{}, err
	}
	response, err := c.do(request)
	if err != nil {
		return runtimeapi.ProxyVerification{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimeapi.ProxyVerification{}, decodeClientError(response)
	}
	var verification runtimeapi.ProxyVerification
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &verification); err != nil {
		return runtimeapi.ProxyVerification{}, invalidResponseError("proxy verification response", err)
	}
	return verification, nil
}

func (c *Client) WaitOperation(
	ctx context.Context,
	id string,
	interval time.Duration,
) (runtimeapi.Operation, error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	for {
		operation, err := c.GetOperation(ctx, id)
		if err != nil {
			return runtimeapi.Operation{}, err
		}
		if operationTerminal(operation.State) {
			return operation, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return runtimeapi.Operation{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) newRequest(
	ctx context.Context,
	method string,
	path string,
	body io.Reader,
	requestID string,
) (*http.Request, error) {
	if c == nil || c.http == nil {
		return nil, &ClientError{Code: runtimeapi.ErrorServiceUnavailable, Message: "Runtime client is not initialized"}
	}
	if ctx == nil {
		return nil, &ClientError{Code: runtimeapi.ErrorInvalidRequest, Message: "Runtime client context is required"}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://runtime"+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set(HeaderRequestID, requestID)
	request.Header.Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	request.Header.Set(HeaderClientVersion, c.version)
	return request, nil
}

func (c *Client) do(request *http.Request) (*http.Response, error) {
	response, err := c.http.Do(request)
	if err == nil {
		return response, nil
	}
	code := runtimeapi.ErrorServiceUnavailable
	message := "Submux Runtime is unavailable"
	if errors.Is(err, os.ErrPermission) {
		code = runtimeapi.ErrorUnauthorized
		message = "permission to access Submux Runtime was denied"
	}
	return nil, &ClientError{
		Code:      code,
		Message:   message,
		Retryable: code == runtimeapi.ErrorServiceUnavailable,
		Cause:     err,
	}
}

func decodeClientError(response *http.Response) error {
	var envelope runtimeapi.ErrorEnvelope
	if err := decodeStrictJSON(response.Body, MaxResponseBytes, &envelope); err != nil {
		return &ClientError{
			Code:      runtimeapi.ErrorServiceUnavailable,
			Message:   fmt.Sprintf("Runtime returned HTTP %d with an invalid error response", response.StatusCode),
			Retryable: response.StatusCode >= 500,
			Cause:     err,
		}
	}
	if envelope.Error.Code == "" {
		envelope.Error.Code = runtimeapi.ErrorServiceUnavailable
	}
	return &ClientError{
		Code:            envelope.Error.Code,
		Message:         envelope.Error.Message,
		Retryable:       envelope.Error.Retryable,
		CurrentRevision: envelope.CurrentRevision,
	}
}

func invalidResponseError(kind string, err error) error {
	return &ClientError{
		Code:      runtimeapi.ErrorServiceUnavailable,
		Message:   "Runtime returned an invalid " + kind,
		Retryable: true,
		Cause:     err,
	}
}

func operationTerminal(state string) bool {
	switch state {
	case runtimeapi.OperationSucceeded,
		runtimeapi.OperationFailed,
		runtimeapi.OperationCancelled,
		runtimeapi.OperationOutcomeUnknown:
		return true
	default:
		return false
	}
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
