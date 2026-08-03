package runtimenet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"submux/internal/runtimeapi"
)

type Connector struct {
	Endpoint          string
	RuntimeInstanceID string
	Log               io.Writer

	mu     sync.Mutex
	client *Client
}

func (connector *Connector) Preview(
	ctx context.Context,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	preview, err := client.Preview(ctx, request)
	if err == nil || !reconnectableNetworkError(err) {
		return preview, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return runtimeapi.NetworkPreview{}, errors.Join(err, reconnectErr)
	}
	return client.Preview(ctx, request)
}

func (connector *Connector) Prepare(
	ctx context.Context,
	operationID string,
	planID string,
) (PreparedNetwork, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return PreparedNetwork{}, err
	}
	prepared, err := client.Prepare(ctx, operationID, planID)
	if err == nil || !reconnectableNetworkError(err) {
		return prepared, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return PreparedNetwork{}, errors.Join(err, reconnectErr)
	}
	if resultErr := committedPayload(ctx, client, operationID, OperationPrepare, &prepared); resultErr == nil {
		return prepared, nil
	}
	return PreparedNetwork{}, err
}

func (connector *Connector) Commit(
	ctx context.Context,
	operationID string,
	ownershipID string,
) (runtimeapi.NetworkStatus, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	status, err := client.Commit(ctx, operationID, ownershipID)
	if err == nil || !reconnectableNetworkError(err) {
		return status, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return runtimeapi.NetworkStatus{}, errors.Join(err, reconnectErr)
	}
	if resultErr := committedPayload(ctx, client, operationID, OperationCommit, &status); resultErr == nil {
		client.resumeLease(operationID, ownershipID)
		return status, nil
	}
	if rejectedBeforeMutation(err) {
		return client.Commit(ctx, operationID, ownershipID)
	}
	return runtimeapi.NetworkStatus{}, err
}

func (connector *Connector) Renew(
	ctx context.Context,
	operationID string,
	ownershipID string,
) (runtimeapi.NetworkStatus, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	status, err := client.Renew(ctx, operationID, ownershipID)
	if err == nil || !reconnectableNetworkError(err) {
		return status, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return runtimeapi.NetworkStatus{}, errors.Join(err, reconnectErr)
	}
	if resultErr := committedPayload(ctx, client, operationID, OperationRenew, &status); resultErr == nil {
		return status, nil
	}
	if rejectedBeforeMutation(err) {
		return client.Renew(ctx, operationID, ownershipID)
	}
	return runtimeapi.NetworkStatus{}, err
}

func (connector *Connector) Release(
	ctx context.Context,
	operationID string,
	ownershipID string,
	reason string,
) (runtimeapi.NetworkStatus, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	status, err := client.Release(ctx, operationID, ownershipID, reason)
	if err == nil || !reconnectableNetworkError(err) {
		return status, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return runtimeapi.NetworkStatus{}, errors.Join(err, reconnectErr)
	}
	if resultErr := committedPayload(ctx, client, operationID, OperationRelease, &status); resultErr == nil {
		return status, nil
	}
	if rejectedBeforeMutation(err) {
		return client.Release(ctx, operationID, ownershipID, reason)
	}
	return runtimeapi.NetworkStatus{}, err
}

func (connector *Connector) Observe(ctx context.Context) (runtimeapi.NetworkStatus, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return unavailableNetworkStatus(), err
	}
	status, err := client.Observe(ctx)
	if err == nil || !reconnectableNetworkError(err) {
		return status, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return unavailableNetworkStatus(), errors.Join(err, reconnectErr)
	}
	return client.Observe(ctx)
}

func (connector *Connector) Result(
	ctx context.Context,
	operationID string,
	operation string,
) (CommittedResult, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return CommittedResult{}, err
	}
	result, err := client.Result(ctx, operationID, operation)
	if err == nil || !reconnectableNetworkError(err) {
		return result, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return CommittedResult{}, errors.Join(err, reconnectErr)
	}
	return client.Result(ctx, operationID, operation)
}

func (connector *Connector) StageCore(
	ctx context.Context,
	operationID string,
	stage PrivilegedCoreStage,
) (PrivilegedCoreStatus, error) {
	return connector.mutateCore(
		ctx,
		operationID,
		OperationCoreStage,
		func(client *Client) (PrivilegedCoreStatus, error) {
			return client.StageCore(ctx, operationID, stage)
		},
	)
}

func (connector *Connector) StartCore(
	ctx context.Context,
	operationID string,
	objectID string,
) (PrivilegedCoreStatus, error) {
	return connector.mutateCore(
		ctx,
		operationID,
		OperationCoreStart,
		func(client *Client) (PrivilegedCoreStatus, error) {
			return client.StartCore(ctx, operationID, objectID)
		},
	)
}

func (connector *Connector) StopCore(
	ctx context.Context,
	operationID string,
	objectID string,
) (PrivilegedCoreStatus, error) {
	return connector.mutateCore(
		ctx,
		operationID,
		OperationCoreStop,
		func(client *Client) (PrivilegedCoreStatus, error) {
			return client.StopCore(ctx, operationID, objectID)
		},
	)
}

func (connector *Connector) mutateCore(
	ctx context.Context,
	operationID string,
	operation string,
	mutate func(*Client) (PrivilegedCoreStatus, error),
) (PrivilegedCoreStatus, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return PrivilegedCoreStatus{}, err
	}
	status, err := mutate(client)
	if err == nil || !reconnectableNetworkError(err) {
		return status, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return PrivilegedCoreStatus{}, errors.Join(err, reconnectErr)
	}
	if resultErr := committedPayload(ctx, client, operationID, operation, &status); resultErr == nil {
		return status, nil
	}
	if rejectedBeforeMutation(err) {
		return mutate(client)
	}
	return PrivilegedCoreStatus{}, err
}

func (connector *Connector) ObserveCore(
	ctx context.Context,
	objectID string,
) (PrivilegedCoreStatus, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return PrivilegedCoreStatus{}, err
	}
	status, err := client.ObserveCore(ctx, objectID)
	if err == nil || !reconnectableNetworkError(err) {
		return status, err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return PrivilegedCoreStatus{}, errors.Join(err, reconnectErr)
	}
	return client.ObserveCore(ctx, objectID)
}

func (connector *Connector) FailOpen(ctx context.Context) error {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	client, err := connector.clientForLocked(ctx)
	if err != nil {
		return err
	}
	err = client.FailOpen(ctx)
	if err == nil || !reconnectableNetworkError(err) {
		return err
	}
	client, reconnectErr := connector.reconnectLocked(ctx)
	if reconnectErr != nil {
		return errors.Join(err, reconnectErr)
	}
	return client.FailOpen(ctx)
}

func (connector *Connector) Close() error {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	return connector.closeLocked()
}

func (connector *Connector) clientForLocked(ctx context.Context) (*Client, error) {
	if connector == nil {
		return nil, errors.New("privileged Runtime network connector is unavailable")
	}
	if connector.client != nil {
		return connector.client, nil
	}
	client, err := Dial(ctx, connector.Endpoint, connector.RuntimeInstanceID)
	if err != nil {
		connector.logf("ERROR privileged network connection failed: %v", err)
		return nil, err
	}
	client.Log = connector.Log
	connector.client = client
	connector.logf("INFO privileged network connection established")
	return client, nil
}

func (connector *Connector) reconnectLocked(ctx context.Context) (*Client, error) {
	closeErr := connector.closeLocked()
	client, dialErr := connector.clientForLocked(ctx)
	if dialErr != nil {
		return nil, errors.Join(closeErr, dialErr)
	}
	return client, closeErr
}

func (connector *Connector) closeLocked() error {
	if connector == nil || connector.client == nil {
		return nil
	}
	err := connector.client.Close()
	connector.client = nil
	if err != nil {
		connector.logf("ERROR privileged network connection close failed: %v", err)
	} else {
		connector.logf("INFO privileged network connection closed")
	}
	return err
}

func (connector *Connector) logf(format string, arguments ...any) {
	if connector == nil || connector.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(connector.Log, format+"\n", arguments...)
}

func committedPayload(
	ctx context.Context,
	client *Client,
	operationID string,
	operation string,
	target any,
) error {
	result, err := client.Result(ctx, operationID, operation)
	if err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	if len(result.Payload) == 0 {
		return errors.New("privileged Runtime network committed result has no payload")
	}
	if err := json.Unmarshal(result.Payload, target); err != nil {
		return errors.New("privileged Runtime network committed result is invalid")
	}
	return nil
}

func reconnectableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var outcomeUnknown *OutcomeUnknownError
	var transport *transportError
	return errors.As(err, &outcomeUnknown) ||
		errors.As(err, &transport) ||
		rejectedBeforeMutation(err)
}

func rejectedBeforeMutation(err error) bool {
	message := strings.ToLower(fmt.Sprint(err))
	return strings.Contains(message, "session is not bound to this connection") ||
		strings.Contains(message, "session is unavailable or expired") ||
		strings.Contains(message, "client is disconnected")
}

func unavailableNetworkStatus() runtimeapi.NetworkStatus {
	return runtimeapi.NetworkStatus{
		Available: false,
		Mode:      runtimeapi.RunModeExplicit,
		State:     runtimeapi.NetworkStateUnavailable,
		Fault: &runtimeapi.Fault{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "Privileged Runtime network service is unavailable",
		},
	}
}
