package runtimeapp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

type CandidatePreviewer interface {
	PreviewCandidate(context.Context, runtimeapi.PeerIdentity, runtimeapi.PreviewCandidateRequest) (runtimeapi.CandidatePreview, error)
}

type ScheduledActionProvider interface {
	DueActions(time.Time) ([]runtimeapi.Action, error)
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

func (e *PublicError) ProtocolCode() string {
	return e.Code
}

func (e *PublicError) ProtocolMessage() string {
	return e.Message
}

func (e *PublicError) ProtocolRetryable() bool {
	return e.Retryable
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

func (c *Coordinator) RuntimeVersion() string {
	if c == nil {
		return ""
	}
	return c.Version
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

func (c *Coordinator) PreviewCandidate(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.CandidatePreview{}, err
	}
	if c == nil {
		return runtimeapi.CandidatePreview{}, errors.New("Runtime candidate preview is unavailable")
	}
	previewer, ok := c.Executor.(CandidatePreviewer)
	if !ok {
		return runtimeapi.CandidatePreview{}, errors.New("Runtime candidate preview is unavailable")
	}
	return previewer.PreviewCandidate(ctx, peer, request)
}

func (c *Coordinator) GetAdvancedOverride(
	ctx context.Context,
	_ runtimeapi.PeerIdentity,
) (runtimeapi.AdvancedOverrideDocument, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.AdvancedOverrideDocument{}, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.AdvancedOverrideDocument{}, errors.New("Runtime state is unavailable")
	}
	body, record, err := c.State.AdvancedOverride()
	if err != nil {
		return runtimeapi.AdvancedOverrideDocument{}, err
	}
	return runtimeapi.AdvancedOverrideDocument{
		YAML:   string(body),
		SHA256: record.SHA256,
	}, nil
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
		scheduled, err := c.scheduleDueActions(ctx)
		if err != nil {
			return err
		}
		if scheduled {
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

func (c *Coordinator) scheduleDueActions(ctx context.Context) (bool, error) {
	scheduler, ok := c.Executor.(ScheduledActionProvider)
	if !ok {
		return false, nil
	}
	now := c.now()
	actions, err := scheduler.DueActions(now)
	if err != nil {
		return false, fmt.Errorf("inspect scheduled Runtime actions: %w", err)
	}
	if len(actions) == 0 {
		return false, nil
	}
	snapshot, err := c.State.Observe(c.Version, now)
	if err != nil {
		return false, err
	}
	peer := runtimeapi.PeerIdentity{Platform: "runtime"}
	for index, action := range actions {
		if err := validateAction(action); err != nil {
			return false, err
		}
		_, _, err := c.Execute(ctx, peer, c.Version, runtimeapi.CreateOperationRequest{
			RequestID:  fmt.Sprintf("scheduled-%d-%d", now.UnixNano(), index),
			IfRevision: snapshot.Revision,
			Action:     action,
		})
		if err == nil {
			return true, nil
		}
		if errors.Is(err, runtimestate.ErrRevisionConflict) || errors.Is(err, runtimestate.ErrBusy) {
			return false, nil
		}
		return false, err
	}
	return false, nil
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
	if (operation.Action.Kind == runtimeapi.ActionApplyImportedConfig ||
		operation.Action.Kind == runtimeapi.ActionAddRemoteSource ||
		operation.Action.Kind == runtimeapi.ActionAddManagedResource ||
		operation.Action.Kind == runtimeapi.ActionSetAdvancedOverride) &&
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
		if !validContentID(action.Params.ContentID) {
			return errors.New("proxy.apply_import requires a valid content_id")
		}
		if action.Params.SourceID != "" ||
			action.Params.Route != "" ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("proxy.apply_import accepts only content_id")
		}
	case runtimeapi.ActionStartProxy, runtimeapi.ActionStopProxy:
		if action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.Route != "" ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("proxy start and stop do not accept parameters")
		}
	case runtimeapi.ActionAddRemoteSource:
		if !validContentID(action.Params.ContentID) ||
			action.Params.SourceID != "" ||
			action.Params.Route != "" ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("source.add_remote requires only a valid content_id")
		}
	case runtimeapi.ActionRefreshSource:
		if !validSourceID(action.Params.SourceID) ||
			action.Params.ContentID != "" ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			(action.Params.Route != "" &&
				action.Params.Route != runtimeapi.SourceRouteDirect &&
				action.Params.Route != runtimeapi.SourceRouteMihomo) {
			return errors.New("source.refresh requires a valid source_id and optional route")
		}
	case runtimeapi.ActionApplySource:
		if !validSourceID(action.Params.SourceID) ||
			action.Params.ContentID != "" ||
			action.Params.Route != "" ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("source.apply requires only a valid source_id")
		}
	case runtimeapi.ActionAddManagedResource:
		if !validContentID(action.Params.ContentID) ||
			action.Params.SourceID != "" ||
			action.Params.Route != "" ||
			!validResourceKind(action.Params.ResourceKind) ||
			!validResourceName(action.Params.ResourceName) {
			return errors.New("resource.add requires content_id, resource_kind, and resource_name")
		}
	case runtimeapi.ActionSetAdvancedOverride:
		if !validContentID(action.Params.ContentID) ||
			action.Params.SourceID != "" ||
			action.Params.Route != "" ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("override.set requires only a valid content_id")
		}
	default:
		return fmt.Errorf("Runtime action %q is not supported", action.Kind)
	}
	return nil
}

func validSourceID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "src_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validContentID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "content_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validResourceKind(kind string) bool {
	switch kind {
	case runtimeapi.ResourceKindProxyProvider,
		runtimeapi.ResourceKindRuleProvider,
		runtimeapi.ResourceKindCertificate,
		runtimeapi.ResourceKindPrivateKey:
		return true
	default:
		return false
	}
}

func validResourceName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func publicExecutionMessage(kind string) string {
	switch kind {
	case runtimeapi.ActionApplyImportedConfig:
		return "Mihomo rejected or could not activate the imported configuration"
	case runtimeapi.ActionStartProxy:
		return "Mihomo could not start the explicit proxy"
	case runtimeapi.ActionStopProxy:
		return "Mihomo could not stop the explicit proxy"
	case runtimeapi.ActionAddRemoteSource:
		return "Runtime could not add the remote configuration source"
	case runtimeapi.ActionRefreshSource:
		return "Runtime could not refresh the remote configuration source"
	case runtimeapi.ActionApplySource:
		return "Runtime could not apply the current remote configuration source"
	case runtimeapi.ActionAddManagedResource:
		return "Runtime could not import the managed resource"
	case runtimeapi.ActionSetAdvancedOverride:
		return "Runtime could not save the advanced override"
	default:
		return "Runtime operation failed"
	}
}
