package runtimeapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

const DefaultQueueCapacity = 32

type StageReporter func(stage string, progress int, cancellable bool) error

type ActionExecutor interface {
	Execute(context.Context, runtimeapi.Operation, StageReporter) (*runtimeapi.OperationResult, error)
	Verify(context.Context) (runtimeapi.ProxyVerification, error)
}

type PublicError struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

func (e *PublicError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *PublicError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Coordinator struct {
	State         *runtimestate.Store
	Executor      ActionExecutor
	Version       string
	Now           func() time.Time
	QueueCapacity int

	wakeOnce sync.Once
	wake     chan struct{}
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
}

func (c *Coordinator) Observe(ctx context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	service := &Service{State: c.State, Version: c.Version, Now: c.Now}
	return service.Observe(ctx, peer)
}

func (c *Coordinator) UploadImport(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	contentType string,
	expectedSize int64,
	expectedSHA256 string,
	body []byte,
) (runtimeapi.ImportContent, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.ImportContent{}, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.ImportContent{}, errors.New("Runtime state is unavailable")
	}
	now := c.now()
	if err := c.State.GCExpiredImports(now); err != nil {
		return runtimeapi.ImportContent{}, err
	}
	return c.State.UploadImport(peer, contentType, expectedSize, expectedSHA256, body, now)
}

func (c *Coordinator) Execute(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientVersion string,
	request runtimeapi.CreateOperationRequest,
) (runtimeapi.Operation, bool, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.Operation{}, false, err
	}
	if c == nil || c.State == nil || c.Executor == nil {
		return runtimeapi.Operation{}, false, errors.New("Runtime operation service is unavailable")
	}
	if err := validateAction(request.Action); err != nil {
		return runtimeapi.Operation{}, false, err
	}
	capacity := c.QueueCapacity
	if capacity <= 0 {
		capacity = DefaultQueueCapacity
	}
	operation, duplicate, err := c.State.SubmitOperation(peer, clientVersion, request, capacity, c.now())
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	c.initializeWake()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return operation, duplicate, nil
}

func (c *Coordinator) GetOperation(ctx context.Context, id string) (runtimeapi.Operation, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.Operation{}, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.Operation{}, errors.New("Runtime state is unavailable")
	}
	return c.State.GetOperation(id)
}

func (c *Coordinator) CancelOperation(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	id string,
	request runtimeapi.CancelOperationRequest,
) (runtimeapi.Operation, bool, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.Operation{}, false, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.Operation{}, false, errors.New("Runtime state is unavailable")
	}
	operation, duplicate, err := c.State.CancelOperation(peer, id, request, c.now())
	if err != nil {
		return runtimeapi.Operation{}, false, err
	}
	if !duplicate {
		c.mu.Lock()
		cancel := c.cancels[id]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return operation, duplicate, nil
}

func (c *Coordinator) VerifyProxy(ctx context.Context) (runtimeapi.ProxyVerification, error) {
	if c == nil || c.Executor == nil {
		return runtimeapi.ProxyVerification{}, errors.New("Runtime proxy verifier is unavailable")
	}
	return c.Executor.Verify(ctx)
}

func (c *Coordinator) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Runtime operation context is required")
	}
	if c == nil || c.State == nil || c.Executor == nil {
		return errors.New("Runtime operation service is unavailable")
	}
	c.initializeWake()
	if err := c.State.RecoverOperations(c.now()); err != nil {
		return fmt.Errorf("recover Runtime operations: %w", err)
	}
	if err := c.State.GCExpiredImports(c.now()); err != nil {
		return fmt.Errorf("clean Runtime imports: %w", err)
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		processed, err := c.processNext(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.wake:
		case now := <-ticker.C:
			if err := c.State.GCExpiredImports(now); err != nil {
				return fmt.Errorf("clean Runtime imports: %w", err)
			}
		}
	}
}

func (c *Coordinator) processNext(serviceContext context.Context) (bool, error) {
	operation, found, err := c.State.BeginNextOperation(c.now())
	if err != nil || !found {
		return found, err
	}
	operationContext, cancel := context.WithCancel(serviceContext)
	c.mu.Lock()
	if c.cancels == nil {
		c.cancels = make(map[string]context.CancelFunc)
	}
	c.cancels[operation.ID] = cancel
	c.mu.Unlock()

	report := func(stage string, progress int, cancellable bool) error {
		return c.State.UpdateOperationStage(operation.ID, stage, progress, cancellable, c.now())
	}
	result, executeErr := c.Executor.Execute(operationContext, operation, report)
	cancel()
	c.mu.Lock()
	delete(c.cancels, operation.ID)
	c.mu.Unlock()

	state := runtimeapi.OperationSucceeded
	var publicError *runtimeapi.ProtocolError
	if executeErr != nil {
		state = runtimeapi.OperationFailed
		code := runtimeapi.ErrorServiceUnavailable
		message := publicExecutionMessage(operation.Action.Kind)
		retryable := false
		var exposed *PublicError
		if errors.As(executeErr, &exposed) {
			code = exposed.Code
			message = exposed.Message
			retryable = exposed.Retryable
		}
		if serviceContext.Err() != nil {
			state = runtimeapi.OperationOutcomeUnknown
			code = runtimeapi.OperationOutcomeUnknown
			message = "Runtime stopped before the operation outcome could be confirmed"
		}
		publicError = &runtimeapi.ProtocolError{Code: code, Message: message, Retryable: retryable}
	}
	if err := c.State.CompleteOperation(operation.ID, state, result, publicError, c.now()); err != nil {
		if !errors.Is(err, runtimestate.ErrOperationTerminal) {
			return true, fmt.Errorf("complete Runtime operation: %w", err)
		}
	}
	if operation.Action.Kind == runtimeapi.ActionApplyImportedConfig &&
		operation.Action.Params.ContentID != "" {
		if err := c.State.RemoveImport(operation.Action.Params.ContentID); err != nil {
			return true, fmt.Errorf("remove consumed Runtime import: %w", err)
		}
	}
	return true, nil
}

func (c *Coordinator) initializeWake() {
	c.wakeOnce.Do(func() {
		c.wake = make(chan struct{}, 1)
	})
}

func (c *Coordinator) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func validateAction(action runtimeapi.Action) error {
	switch action.Kind {
	case runtimeapi.ActionApplyImportedConfig:
		if len(action.Params.ContentID) <= len("content_") ||
			len(action.Params.ContentID) > len("content_")+64 {
			return errors.New("proxy.apply_import requires a valid content_id")
		}
	case runtimeapi.ActionStartProxy, runtimeapi.ActionStopProxy:
		if action.Params.ContentID != "" {
			return errors.New("proxy start and stop do not accept content_id")
		}
	default:
		return fmt.Errorf("Runtime action %q is not supported", action.Kind)
	}
	return nil
}

func publicExecutionMessage(kind string) string {
	switch kind {
	case runtimeapi.ActionApplyImportedConfig:
		return "Mihomo rejected or could not activate the imported configuration"
	case runtimeapi.ActionStartProxy:
		return "Mihomo could not start the explicit proxy"
	case runtimeapi.ActionStopProxy:
		return "Mihomo could not stop the explicit proxy"
	default:
		return "Runtime operation failed"
	}
}
