package runtimenet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"submux/internal/runtimeapi"
)

type Controller interface {
	Preview(context.Context, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error)
	Prepare(context.Context, string, string) (PreparedTUN, error)
	Commit(context.Context, string, string) (runtimeapi.NetworkStatus, error)
	Renew(context.Context, string, string) (runtimeapi.NetworkStatus, error)
	Release(context.Context, string, string, string) (runtimeapi.NetworkStatus, error)
	Observe(context.Context) (runtimeapi.NetworkStatus, error)
	Result(context.Context, string, string) (CommittedResult, error)
	Close() error
}

type Client struct {
	endpoint          string
	runtimeInstanceID string

	mu       sync.Mutex
	http     *http.Client
	session  Session
	sequence uint64

	leaseCancel context.CancelFunc
}

type OutcomeUnknownError struct {
	OperationID string
	Operation   string
	Cause       error
}

func (err *OutcomeUnknownError) Error() string {
	return fmt.Sprintf(
		"privileged Runtime network %s outcome is unknown; reconnect and query Operation ID %s: %v",
		err.Operation,
		err.OperationID,
		err.Cause,
	)
}

func (err *OutcomeUnknownError) Unwrap() error {
	return err.Cause
}

func Dial(ctx context.Context, endpoint, runtimeInstanceID string) (*Client, error) {
	if !validOpaqueIdentifier(runtimeInstanceID, 32, 128) {
		return nil, errors.New("Runtime installation ID is invalid")
	}
	client := &Client{
		endpoint:          endpoint,
		runtimeInstanceID: runtimeInstanceID,
	}
	if err := client.connect(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *Client) Reconnect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(ctx)
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http == nil {
		return nil
	}
	c.stopLeaseLocked()
	if transport, ok := c.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	c.http = nil
	c.session = Session{}
	c.sequence = 0
	return nil
}

func (c *Client) Preview(
	ctx context.Context,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var preview runtimeapi.NetworkPreview
	err := c.postLocked(ctx, "/v1/preview", previewEnvelope{
		SessionID: c.session.ID,
		Request:   request,
	}, &preview)
	return preview, err
}

func (c *Client) Prepare(
	ctx context.Context,
	operationID string,
	planID string,
) (PreparedTUN, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, err := c.nextMetaLocked(ctx, operationID)
	if err != nil {
		return PreparedTUN{}, err
	}
	var prepared PreparedTUN
	err = c.postMutationLocked(ctx, OperationPrepare, operationID, "/v1/prepare", prepareEnvelope{
		Meta:   meta,
		PlanID: planID,
	}, &prepared)
	return prepared, err
}

func (c *Client) Commit(
	ctx context.Context,
	operationID string,
	ownershipID string,
) (runtimeapi.NetworkStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, err := c.nextMetaLocked(ctx, operationID)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	var status runtimeapi.NetworkStatus
	err = c.postMutationLocked(ctx, OperationCommit, operationID, "/v1/commit", ownershipEnvelope{
		Meta:        meta,
		OwnershipID: ownershipID,
	}, &status)
	if err == nil {
		c.startLeaseLocked(operationID, ownershipID)
	}
	return status, err
}

func (c *Client) Renew(
	ctx context.Context,
	operationID string,
	ownershipID string,
) (runtimeapi.NetworkStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, err := c.nextMetaLocked(ctx, operationID)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	var status runtimeapi.NetworkStatus
	err = c.postMutationLocked(ctx, OperationRenew, operationID, "/v1/renew", ownershipEnvelope{
		Meta:        meta,
		OwnershipID: ownershipID,
	}, &status)
	return status, err
}

func (c *Client) Release(
	ctx context.Context,
	operationID string,
	ownershipID string,
	reason string,
) (runtimeapi.NetworkStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, err := c.nextMetaLocked(ctx, operationID)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	var status runtimeapi.NetworkStatus
	err = c.postMutationLocked(ctx, OperationRelease, operationID, "/v1/release", releaseEnvelope{
		Meta:        meta,
		OwnershipID: ownershipID,
		Reason:      reason,
	}, &status)
	if err == nil {
		c.stopLeaseLocked()
	}
	return status, err
}

func (c *Client) Observe(ctx context.Context) (runtimeapi.NetworkStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var status runtimeapi.NetworkStatus
	err := c.postLocked(ctx, "/v1/observe", observeEnvelope{
		SessionID: c.session.ID,
	}, &status)
	return status, err
}

func (c *Client) Result(
	ctx context.Context,
	operationID string,
	operation string,
) (CommittedResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result CommittedResult
	err := c.postLocked(ctx, "/v1/results/query", resultEnvelope{
		SessionID:   c.session.ID,
		OperationID: operationID,
		Operation:   operation,
	}, &result)
	return result, err
}

func (c *Client) FailOpen(ctx context.Context) error {
	status, err := c.Observe(ctx)
	if err != nil {
		return err
	}
	if status.OwnershipID == "" || status.State == runtimeapi.NetworkStateInactive {
		return nil
	}
	operationID, err := randomIdentifier("op_failopen_")
	if err != nil {
		return err
	}
	_, err = c.Release(ctx, operationID, status.OwnershipID, ReleaseMihomoFailure)
	return err
}

func (c *Client) connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(ctx)
}

func (c *Client) connectLocked(ctx context.Context) error {
	c.stopLeaseLocked()
	if c.http != nil {
		if transport, ok := c.http.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	transport := &http.Transport{
		DisableCompression:  true,
		MaxConnsPerHost:     1,
		MaxIdleConnsPerHost: 1,
		DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
			return dialNetwork(dialContext, c.endpoint)
		},
	}
	c.http = &http.Client{Transport: transport}
	c.sequence = 0
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	var session Session
	err := c.postLocked(ctx, "/v1/session", sessionEnvelope{Request: SessionRequest{
		ProtocolVersion:   ProtocolVersion,
		RuntimeInstanceID: c.runtimeInstanceID,
		ClientNonce:       hex.EncodeToString(nonce),
	}}, &session)
	if err != nil {
		transport.CloseIdleConnections()
		c.http = nil
		return err
	}
	if session.ProtocolVersion != ProtocolVersion ||
		session.ID == "" ||
		session.Epoch == "" ||
		!validHex(session.ServerNonce, 64) {
		transport.CloseIdleConnections()
		c.http = nil
		return errors.New("privileged Runtime network session response is invalid")
	}
	c.session = session
	return nil
}

func (c *Client) nextMetaLocked(ctx context.Context, operationID string) (RequestMeta, error) {
	if c.http == nil || c.session.ID == "" {
		return RequestMeta{}, errors.New("privileged Runtime network client is disconnected")
	}
	if !validOpaqueIdentifier(operationID, 3, 96) {
		return RequestMeta{}, errors.New("Runtime network Operation ID is invalid")
	}
	deadline := time.Now().UTC().Add(DefaultRequestLimit)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline.UTC()
	}
	if !deadline.After(time.Now()) {
		return RequestMeta{}, context.DeadlineExceeded
	}
	c.sequence++
	return RequestMeta{
		Epoch:           c.session.Epoch,
		SessionID:       c.session.ID,
		ConnectionNonce: c.session.ServerNonce,
		Sequence:        c.sequence,
		OperationID:     operationID,
		Deadline:        deadline,
	}, nil
}

func (c *Client) postMutationLocked(
	ctx context.Context,
	operation string,
	operationID string,
	path string,
	request any,
	response any,
) error {
	err := c.postLocked(ctx, path, request, response)
	var transportError *transportError
	if errors.As(err, &transportError) {
		return &OutcomeUnknownError{
			OperationID: operationID,
			Operation:   operation,
			Cause:       transportError,
		}
	}
	return err
}

type transportError struct {
	err error
}

func (err *transportError) Error() string {
	return err.err.Error()
}

func (err *transportError) Unwrap() error {
	return err.err
}

func (c *Client) postLocked(ctx context.Context, path string, request any, response any) error {
	if c.http == nil {
		return errors.New("privileged Runtime network client is disconnected")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://runtime-net"+path,
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		return &transportError{err: err}
	}
	defer httpResponse.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxIPCRequestBytes+1))
	if err != nil {
		return &transportError{err: err}
	}
	if len(responseBody) > maxIPCRequestBytes {
		return errors.New("privileged Runtime network response exceeds the size limit")
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		var envelope errorEnvelope
		if json.Unmarshal(responseBody, &envelope) != nil || envelope.Error == "" {
			return fmt.Errorf("privileged Runtime network service returned HTTP %d", httpResponse.StatusCode)
		}
		return errors.New(envelope.Error)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return errors.New("privileged Runtime network response is invalid")
	}
	return nil
}

func (c *Client) startLeaseLocked(operationID, ownershipID string) {
	c.stopLeaseLocked()
	ctx, cancel := context.WithCancel(context.Background())
	c.leaseCancel = cancel
	go func() {
		ticker := time.NewTicker(DefaultLeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewContext, renewCancel := context.WithTimeout(ctx, DefaultRequestLimit)
				_, err := c.Renew(renewContext, operationID, ownershipID)
				renewCancel()
				if err != nil {
					return
				}
			}
		}
	}()
}

func (c *Client) resumeLease(operationID, ownershipID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLeaseLocked(operationID, ownershipID)
}

func (c *Client) stopLeaseLocked() {
	if c.leaseCancel != nil {
		c.leaseCancel()
		c.leaseCancel = nil
	}
}
