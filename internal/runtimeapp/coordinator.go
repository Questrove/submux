package runtimeapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprivacy"
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

type RecoveryService interface {
	RecoverStartup(context.Context) error
	Run(context.Context) error
}

type DiagnosticsService interface {
	Preview(context.Context, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsPreview, error)
	Create(context.Context, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsResult, error)
}

type NetworkService interface {
	Preview(context.Context, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error)
	Observe(context.Context) (runtimeapi.NetworkStatus, error)
}

type MihomoUpdateService interface {
	UploadBundle(context.Context, runtimeapi.PeerIdentity, int64, string, io.Reader) (runtimeapi.MihomoUpdateBundle, error)
	Preview(context.Context, runtimeapi.PeerIdentity, runtimeapi.MihomoUpdatePreviewRequest) (runtimeapi.MihomoUpdatePlan, error)
	Status() (runtimeapi.UpdateStatus, error)
}

type ProductUpdateService interface {
	Preview(
		context.Context,
		runtimeapi.PeerIdentity,
		runtimeapi.ProductUpdatePreviewRequest,
		[]byte,
	) (runtimeapi.ProductUpdatePlan, error)
	Status(context.Context) (runtimeapi.UpdateStatus, error)
}

type BackupService interface {
	Preview(bool) (runtimeapi.BackupPreview, error)
	Export(runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error)
	Inspect([]byte, string) (runtimeapi.BackupRestorePreview, error)
}

type TrafficService interface {
	Status() runtimeapi.TrafficStatus
	History(runtimeapi.TrafficHistoryRequest) runtimeapi.TrafficHistory
	Connections(runtimeapi.ConnectionQuery) runtimeapi.ConnectionPage
}

type TrafficPolicyObserver interface {
	AppliedTrafficPolicy(context.Context) (string, error)
}

type AppliedRuleObserver interface {
	AppliedRules(context.Context, runtimeapi.RuleQuery) (runtimeapi.RuleSet, error)
}

type ProxyGroupObserver interface {
	ProxyGroups(context.Context, runtimeapi.ProxyGroupQuery) (runtimeapi.ProxyGroupList, error)
}

func (c *Coordinator) ProxyGroups(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	query runtimeapi.ProxyGroupQuery,
) (runtimeapi.ProxyGroupList, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	if peer.Key() == "" {
		return runtimeapi.ProxyGroupList{}, errors.New("Runtime proxy group observer identity is required")
	}
	observer, ok := c.Executor.(ProxyGroupObserver)
	if !ok {
		return runtimeapi.ProxyGroupList{}, errors.New("Runtime proxy group viewer is unavailable")
	}
	return observer.ProxyGroups(ctx, query)
}

func (c *Coordinator) Rules(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	query runtimeapi.RuleQuery,
) (runtimeapi.RuleSet, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.RuleSet{}, err
	}
	if peer.Key() == "" {
		return runtimeapi.RuleSet{}, errors.New("Runtime final rule observer identity is required")
	}
	observer, ok := c.Executor.(AppliedRuleObserver)
	if !ok {
		return runtimeapi.RuleSet{}, errors.New("Runtime final rule viewer is unavailable")
	}
	return observer.AppliedRules(ctx, query)
}

func (c *Coordinator) Connections(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	query runtimeapi.ConnectionQuery,
) (runtimeapi.ConnectionPage, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.ConnectionPage{}, err
	}
	if peer.Key() == "" {
		return runtimeapi.ConnectionPage{}, errors.New("Runtime connection observer identity is required")
	}
	if c == nil || c.Traffic == nil {
		return runtimeapi.ConnectionPage{}, errors.New("Runtime connection viewer is unavailable")
	}
	return c.Traffic.Connections(query), nil
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
	State          *runtimestate.Store
	Executor       ActionExecutor
	Recovery       RecoveryService
	Diagnostics    DiagnosticsService
	Network        NetworkService
	Updates        MihomoUpdateService
	ProductUpdates ProductUpdateService
	Backups        BackupService
	Traffic        TrafficService
	Version        string
	Now            func() time.Time
	QueueCapacity  int

	wakeOnce sync.Once
	wake     chan struct{}
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
}

func (c *Coordinator) Observe(ctx context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	service := &Service{State: c.State, Version: c.Version, Now: c.Now}
	snapshot, err := service.Observe(ctx, peer)
	if err != nil {
		return runtimeapi.Snapshot{}, err
	}
	if c.Traffic != nil {
		snapshot.Traffic = c.Traffic.Status()
	}
	if observer, ok := c.Executor.(TrafficPolicyObserver); ok {
		if applied, observeErr := observer.AppliedTrafficPolicy(ctx); observeErr == nil {
			snapshot.TrafficPolicy.Applied = applied
		} else {
			snapshot.TrafficPolicy.Applied = "unknown"
		}
	}
	if c.Network == nil {
		snapshot.Network = runtimeapi.NetworkStatus{
			Available: false,
			Mode:      runtimeapi.RunModeExplicit,
			State:     runtimeapi.NetworkStateUnavailable,
		}
		return c.observeUpdates(ctx, snapshot)
	}
	network, networkErr := c.Network.Observe(ctx)
	snapshot.Network = network
	if networkErr != nil {
		snapshot.Network.Available = false
		snapshot.Network.State = runtimeapi.NetworkStateUnavailable
		if snapshot.Network.Fault == nil {
			snapshot.Network.Fault = &runtimeapi.Fault{
				Code:    runtimeapi.ErrorServiceUnavailable,
				Message: "Privileged Runtime network state is unavailable",
			}
		}
		return c.observeUpdates(ctx, snapshot)
	}
	if network.State == runtimeapi.NetworkStateActive {
		snapshot.RunMode = network.Mode
	} else if snapshot.RunMode == runtimeapi.RunModeTUN ||
		snapshot.RunMode == runtimeapi.RunModeGateway {
		snapshot.RunMode = runtimeapi.RunModeExplicit
	}
	return c.observeUpdates(ctx, snapshot)
}

func (c *Coordinator) TrafficHistory(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.TrafficHistoryRequest,
) (runtimeapi.TrafficHistory, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.TrafficHistory{}, err
	}
	if peer.Key() == "" {
		return runtimeapi.TrafficHistory{}, errors.New("Runtime traffic observer identity is required")
	}
	if c == nil || c.Traffic == nil {
		return runtimeapi.TrafficHistory{}, errors.New("Runtime traffic history is unavailable")
	}
	return c.Traffic.History(request), nil
}

func (c *Coordinator) observeUpdates(ctx context.Context, snapshot runtimeapi.Snapshot) (runtimeapi.Snapshot, error) {
	if c.Updates != nil {
		status, err := c.Updates.Status()
		if err != nil {
			return runtimeapi.Snapshot{}, err
		}
		snapshot.Updates = status
	}
	if c.ProductUpdates != nil {
		status, err := c.ProductUpdates.Status(ctx)
		if err != nil {
			return runtimeapi.Snapshot{}, err
		}
		snapshot.Updates.RuntimeAvailable = status.RuntimeAvailable
		snapshot.Updates.RuntimeCurrentVersion = status.RuntimeCurrentVersion
		snapshot.Updates.RuntimePreviousVersion = status.RuntimePreviousVersion
		snapshot.Updates.RuntimeAvailableVersion = status.RuntimeAvailableVersion
		snapshot.Updates.RuntimeLastCheckedAt = status.RuntimeLastCheckedAt
		snapshot.Updates.RuntimeNextCheckAt = status.RuntimeNextCheckAt
		snapshot.Updates.RuntimePredownloadEnabled = status.RuntimePredownloadEnabled
	}
	return snapshot, nil
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

func (c *Coordinator) UploadMihomoUpdateBundle(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	expectedSize int64,
	expectedSHA256 string,
	body io.Reader,
) (runtimeapi.MihomoUpdateBundle, error) {
	if c == nil || c.Updates == nil {
		return runtimeapi.MihomoUpdateBundle{}, errors.New("Mihomo update bundle service is unavailable")
	}
	return c.Updates.UploadBundle(ctx, peer, expectedSize, expectedSHA256, body)
}

func (c *Coordinator) PreviewMihomoUpdate(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.MihomoUpdatePreviewRequest,
) (runtimeapi.MihomoUpdatePlan, error) {
	if c == nil || c.Updates == nil {
		return runtimeapi.MihomoUpdatePlan{}, errors.New("Mihomo update preview service is unavailable")
	}
	return c.Updates.Preview(ctx, peer, request)
}

func (c *Coordinator) PreviewProductUpdate(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.ProductUpdatePreviewRequest,
) (runtimeapi.ProductUpdatePlan, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	if c == nil || c.State == nil || c.ProductUpdates == nil {
		return runtimeapi.ProductUpdatePlan{}, errors.New("Runtime product update preview service is unavailable")
	}
	var offlineBundle []byte
	var err error
	if request.Source == runtimeapi.ProductUpdateSourceOfflineTUF {
		if !validContentID(request.ContentID) {
			err = errors.New("offline Runtime product update preview requires a valid content_id")
		} else {
			var content runtimeapi.ImportContent
			offlineBundle, content, err = c.State.PeekImport(request.ContentID, peer.Key(), c.now())
			if err == nil && content.ContentType != runtimeapi.ProductUpdateBundleContentType {
				err = errors.New("uploaded content is not a Runtime product update TUF bundle")
			}
		}
	} else if request.ContentID != "" {
		err = errors.New("online Runtime product update preview does not accept content_id")
	}
	var preview runtimeapi.ProductUpdatePlan
	if err == nil {
		preview, err = c.ProductUpdates.Preview(ctx, peer, request, offlineBundle)
	}
	if err == nil && request.Source == runtimeapi.ProductUpdateSourceOfflineTUF {
		if removeErr := c.State.RemoveImport(request.ContentID); removeErr != nil {
			err = fmt.Errorf("remove consumed Runtime product update import: %w", removeErr)
		}
	}
	if auditErr := c.auditRead(
		peer,
		clientType,
		clientVersion,
		requestID,
		"product.update.preview",
		request.ContentID,
		err,
	); auditErr != nil {
		return runtimeapi.ProductUpdatePlan{}, auditErr
	}
	return preview, err
}

func (c *Coordinator) PreviewBackup(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.BackupPreviewRequest,
) (runtimeapi.BackupPreview, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.BackupPreview{}, err
	}
	if c == nil || c.State == nil || c.Backups == nil {
		return runtimeapi.BackupPreview{}, errors.New("Runtime backup service is unavailable")
	}
	preview, err := c.Backups.Preview(request.IncludeSecrets)
	if auditErr := c.auditRead(
		peer,
		clientType,
		clientVersion,
		requestID,
		"backup.preview",
		"",
		err,
	); auditErr != nil {
		return runtimeapi.BackupPreview{}, auditErr
	}
	return preview, err
}

func (c *Coordinator) ExportBackup(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.BackupExportRequest,
) (runtimeapi.BackupArchive, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.BackupArchive{}, err
	}
	if c == nil || c.State == nil || c.Backups == nil {
		return runtimeapi.BackupArchive{}, errors.New("Runtime backup service is unavailable")
	}
	archive, err := c.Backups.Export(request)
	if auditErr := c.auditRead(
		peer,
		clientType,
		clientVersion,
		requestID,
		"backup.export_plaintext",
		archive.SHA256,
		err,
	); auditErr != nil {
		return runtimeapi.BackupArchive{}, auditErr
	}
	return archive, err
}

func (c *Coordinator) PreviewBackupRestore(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.BackupRestorePreviewRequest,
) (runtimeapi.BackupRestorePreview, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.BackupRestorePreview{}, err
	}
	if c == nil || c.State == nil || c.Backups == nil {
		return runtimeapi.BackupRestorePreview{}, errors.New("Runtime backup service is unavailable")
	}
	if !validContentID(request.ContentID) {
		return runtimeapi.BackupRestorePreview{}, errors.New("backup restore preview requires a valid content_id")
	}
	body, content, err := c.State.PeekImport(request.ContentID, peer.Key(), c.now())
	if err == nil && content.ContentType != runtimeapi.RuntimeBackupContentType {
		err = errors.New("uploaded content is not a Runtime backup archive")
	}
	var preview runtimeapi.BackupRestorePreview
	if err == nil {
		preview, err = c.Backups.Inspect(body, request.ContentID)
	}
	if auditErr := c.auditRead(
		peer,
		clientType,
		clientVersion,
		requestID,
		"backup.restore.preview",
		request.ContentID,
		err,
	); auditErr != nil {
		return runtimeapi.BackupRestorePreview{}, auditErr
	}
	return preview, err
}

func (c *Coordinator) Execute(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
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
	operation, duplicate, err := c.State.SubmitOperation(peer, clientType, clientVersion, request, capacity, c.now())
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

func (c *Coordinator) Events(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	after uint64,
	limit int,
) ([]runtimeapi.Event, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if peer.Key() == "" {
		return nil, 0, errors.New("Runtime event observer identity is required")
	}
	if c == nil || c.State == nil {
		return nil, 0, errors.New("Runtime state is unavailable")
	}
	return c.State.EventsAfter(after, limit)
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

func (c *Coordinator) PreviewNetwork(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.NetworkPreview{}, err
	}
	if peer.Key() == "" {
		return runtimeapi.NetworkPreview{}, errors.New("Runtime network preview identity is required")
	}
	if c == nil || c.Network == nil {
		return runtimeapi.NetworkPreview{}, errors.New("Runtime network preview is unavailable")
	}
	return c.Network.Preview(ctx, request)
}

func (c *Coordinator) GetAdvancedOverride(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	confirm bool,
) (runtimeapi.AdvancedOverrideDocument, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.AdvancedOverrideDocument{}, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.AdvancedOverrideDocument{}, errors.New("Runtime state is unavailable")
	}
	if !confirm {
		return runtimeapi.AdvancedOverrideDocument{}, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Reading the advanced override requires an explicit confirmation",
		}
	}
	body, record, err := c.State.AdvancedOverride()
	result := "succeeded"
	var auditError *runtimeapi.ProtocolError
	if err != nil {
		result = "failed"
		auditError = &runtimeapi.ProtocolError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "The Runtime advanced override could not be read",
		}
	}
	if _, auditErr := c.State.RecordAudit(runtimeapi.AuditRecord{
		RequestID:     requestID,
		Actor:         peer.Key(),
		ClientType:    clientType,
		ClientVersion: clientVersion,
		Action:        "override.reveal",
		ObjectID:      "advanced-override",
		Stage:         "completed",
		Result:        result,
		At:            c.now(),
		Error:         auditError,
	}); auditErr != nil {
		return runtimeapi.AdvancedOverrideDocument{}, auditErr
	}
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
	clientType string,
	clientVersion string,
	id string,
	request runtimeapi.CancelOperationRequest,
) (runtimeapi.Operation, bool, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.Operation{}, false, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.Operation{}, false, errors.New("Runtime state is unavailable")
	}
	operation, duplicate, err := c.State.CancelOperation(peer, clientType, clientVersion, id, request, c.now())
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

func (c *Coordinator) RevealSourceURL(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.RevealSourceURLRequest,
) (runtimeapi.RevealSourceURLResponse, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.RevealSourceURLResponse{}, err
	}
	if c == nil || c.State == nil {
		return runtimeapi.RevealSourceURLResponse{}, errors.New("Runtime state is unavailable")
	}
	if !request.Confirm || !validSourceID(request.SourceID) {
		return runtimeapi.RevealSourceURLResponse{}, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Revealing a source URL requires an explicit confirmation and a valid source ID",
		}
	}
	record, err := c.State.GetRemoteSource(request.SourceID)
	result := "succeeded"
	var auditError *runtimeapi.ProtocolError
	if err != nil {
		result = "failed"
		auditError = &runtimeapi.ProtocolError{
			Code:    runtimeapi.ErrorNotFound,
			Message: "The requested Runtime configuration source was not found",
		}
	}
	if _, auditErr := c.State.RecordAudit(runtimeapi.AuditRecord{
		RequestID:     requestID,
		Actor:         peer.Key(),
		ClientType:    clientType,
		ClientVersion: clientVersion,
		Action:        "source.reveal_url",
		ObjectID:      request.SourceID,
		Stage:         "completed",
		Result:        result,
		At:            c.now(),
		Error:         auditError,
	}); auditErr != nil {
		return runtimeapi.RevealSourceURLResponse{}, auditErr
	}
	if err != nil {
		return runtimeapi.RevealSourceURLResponse{}, &PublicError{
			Code:    runtimeapi.ErrorNotFound,
			Message: "The requested Runtime configuration source was not found",
			Cause:   err,
		}
	}
	return runtimeapi.RevealSourceURLResponse{SourceID: record.ID, URL: record.URL}, nil
}

func (c *Coordinator) PreviewDiagnostics(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsPreview, error) {
	if c == nil || c.State == nil || c.Diagnostics == nil {
		return runtimeapi.DiagnosticsPreview{}, errors.New("Runtime diagnostics service is unavailable")
	}
	preview, err := c.Diagnostics.Preview(ctx, request)
	result := "succeeded"
	var auditError *runtimeapi.ProtocolError
	if err != nil {
		result = "failed"
		auditError = &runtimeapi.ProtocolError{Code: runtimeapi.ErrorInvalidRequest, Message: runtimeprivacy.RedactError(err)}
	}
	if _, auditErr := c.State.RecordAudit(runtimeapi.AuditRecord{
		RequestID:     requestID,
		Actor:         peer.Key(),
		ClientType:    clientType,
		ClientVersion: clientVersion,
		Action:        "diagnostics.preview",
		Stage:         "completed",
		Result:        result,
		At:            c.now(),
		Error:         auditError,
	}); auditErr != nil {
		return runtimeapi.DiagnosticsPreview{}, auditErr
	}
	return preview, err
}

func (c *Coordinator) CreateDiagnostics(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	request runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsResult, error) {
	if c == nil || c.State == nil || c.Diagnostics == nil {
		return runtimeapi.DiagnosticsResult{}, errors.New("Runtime diagnostics service is unavailable")
	}
	result, err := c.Diagnostics.Create(ctx, request)
	outcome := "succeeded"
	var auditError *runtimeapi.ProtocolError
	if err != nil {
		outcome = "failed"
		auditError = &runtimeapi.ProtocolError{Code: runtimeapi.ErrorServiceUnavailable, Message: runtimeprivacy.RedactError(err)}
	}
	if _, auditErr := c.State.RecordAudit(runtimeapi.AuditRecord{
		RequestID:     requestID,
		Actor:         peer.Key(),
		ClientType:    clientType,
		ClientVersion: clientVersion,
		Action:        "diagnostics.create",
		ObjectID:      result.FileName,
		Stage:         "completed",
		Result:        outcome,
		At:            c.now(),
		Error:         auditError,
	}); auditErr != nil {
		return runtimeapi.DiagnosticsResult{}, auditErr
	}
	return result, err
}

func (c *Coordinator) auditRead(
	peer runtimeapi.PeerIdentity,
	clientType string,
	clientVersion string,
	requestID string,
	action string,
	objectID string,
	err error,
) error {
	result := "succeeded"
	var auditError *runtimeapi.ProtocolError
	if err != nil {
		result = "failed"
		auditError = &runtimeapi.ProtocolError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: runtimeprivacy.RedactError(err),
		}
	}
	_, auditErr := c.State.RecordAudit(runtimeapi.AuditRecord{
		RequestID:     requestID,
		Actor:         peer.Key(),
		ClientType:    clientType,
		ClientVersion: clientVersion,
		Action:        action,
		ObjectID:      objectID,
		Stage:         "completed",
		Result:        result,
		At:            c.now(),
		Error:         auditError,
	})
	return auditErr
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
	if _, err := c.State.GCOperationHistory(c.now()); err != nil {
		return fmt.Errorf("clean Runtime operation history: %w", err)
	}
	var recoveryResult <-chan error
	if c.Recovery != nil {
		if err := c.Recovery.RecoverStartup(ctx); err != nil {
			return fmt.Errorf("recover Mihomo startup state: %w", err)
		}
		result := make(chan error, 1)
		recoveryResult = result
		go func() {
			result <- c.Recovery.Run(ctx)
		}()
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
		case err := <-recoveryResult:
			if err != nil {
				return fmt.Errorf("supervise Mihomo recovery: %w", err)
			}
			recoveryResult = nil
		case <-c.wake:
		case now := <-ticker.C:
			if err := c.State.GCExpiredImports(now); err != nil {
				return fmt.Errorf("clean Runtime imports: %w", err)
			}
			if _, err := c.State.GCOperationHistory(now); err != nil {
				return fmt.Errorf("clean Runtime operation history: %w", err)
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
		_, _, err := c.Execute(ctx, peer, "runtime", c.Version, runtimeapi.CreateOperationRequest{
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
	proxySelectionAction := action.Kind == runtimeapi.ActionSelectProxyNode
	if !proxySelectionAction && (action.Params.ProxyGroup != "" || action.Params.ProxyNode != "") {
		return errors.New("proxy group parameters are only accepted by a proxy selection action")
	}
	trafficPolicyAction := action.Kind == runtimeapi.ActionSetTrafficPolicy
	if !trafficPolicyAction && action.Params.TrafficPolicy != "" {
		return errors.New("traffic_policy is only accepted by a traffic policy action")
	}
	connectionAction := action.Kind == runtimeapi.ActionCloseConnection || action.Kind == runtimeapi.ActionCloseConnections
	if !connectionAction && (action.Params.ConnectionID != "" ||
		action.Params.ConnectionTarget != "" ||
		action.Params.ConnectionScope != nil ||
		action.Params.ConnectionScopeToken != "" ||
		action.Params.ConnectionCount != 0) {
		return errors.New("connection parameters are only accepted by a connection close action")
	}
	if action.Kind != runtimeapi.ActionUpdateMihomo &&
		action.Kind != runtimeapi.ActionUpdateProduct &&
		action.Params.Trust != "" {
		return errors.New("trust is only accepted by a Mihomo or Runtime product update action")
	}
	if action.Kind != runtimeapi.ActionEnableTUN &&
		action.Kind != runtimeapi.ActionEnableGateway &&
		action.Kind != runtimeapi.ActionUpdateMihomo &&
		action.Kind != runtimeapi.ActionUpdateProduct &&
		action.Params.PlanID != "" {
		return errors.New("plan_id is only accepted by a network enable, Mihomo update, or Runtime product update action")
	}
	switch action.Kind {
	case runtimeapi.ActionSelectProxyNode:
		if !validSourceID(action.Params.SourceID) || !validProxyActionName(action.Params.ProxyGroup) ||
			!validProxyActionName(action.Params.ProxyNode) || action.Params.ContentID != "" ||
			action.Params.SourceName != "" || action.Params.Route != "" || action.Params.UseCached ||
			action.Params.Confirm || action.Params.ResourceKind != "" || action.Params.ResourceName != "" ||
			action.Params.PlanID != "" || action.Params.Trust != "" || action.Params.ConnectionID != "" ||
			action.Params.ConnectionTarget != "" || action.Params.ConnectionScope != nil ||
			action.Params.ConnectionScopeToken != "" || action.Params.ConnectionCount != 0 ||
			action.Params.TrafficPolicy != "" {
			return errors.New("proxy_group.select requires only the current source, proxy group, and node")
		}
	case runtimeapi.ActionSetTrafficPolicy:
		if !validTrafficPolicy(action.Params.TrafficPolicy) ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" ||
			action.Params.Trust != "" ||
			action.Params.ConnectionID != "" ||
			action.Params.ConnectionTarget != "" ||
			action.Params.ConnectionScope != nil ||
			action.Params.ConnectionScopeToken != "" ||
			action.Params.ConnectionCount != 0 {
			return errors.New("traffic_policy.set requires only a supported traffic_policy")
		}
	case runtimeapi.ActionCloseConnection:
		if !validConnectionID(action.Params.ConnectionID) ||
			!validOptionalConnectionText(action.Params.ConnectionTarget, 512) ||
			action.Params.ConnectionScope != nil ||
			action.Params.ConnectionScopeToken != "" ||
			action.Params.ConnectionCount != 0 ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" ||
			action.Params.Trust != "" {
			return errors.New("connection.close requires only a stable connection_id and optional target")
		}
	case runtimeapi.ActionCloseConnections:
		if action.Params.ConnectionID != "" ||
			action.Params.ConnectionTarget != "" ||
			action.Params.ConnectionScope == nil ||
			!validConnectionScopeToken(action.Params.ConnectionScopeToken) ||
			action.Params.ConnectionCount <= 0 ||
			action.Params.ConnectionCount > 1_000_000 ||
			!action.Params.Confirm ||
			!validConnectionScope(*action.Params.ConnectionScope) ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" ||
			action.Params.Trust != "" {
			return errors.New("connection.close_scope requires a bounded scope, confirmed count, and explicit confirmation")
		}
	case runtimeapi.ActionApplyImportedConfig:
		if !validContentID(action.Params.ContentID) {
			return errors.New("proxy.apply_import requires a valid content_id")
		}
		if action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("proxy.apply_import accepts only content_id")
		}
	case runtimeapi.ActionStartProxy, runtimeapi.ActionStopProxy:
		if action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("proxy start and stop do not accept parameters")
		}
	case runtimeapi.ActionEnableTUN, runtimeapi.ActionEnableGateway:
		if !validPlanID(action.Params.PlanID) ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("network enable requires only a valid plan_id")
		}
	case runtimeapi.ActionDisableTUN, runtimeapi.ActionDisableGateway:
		if action.Params.PlanID != "" ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("network disable does not accept parameters")
		}
	case runtimeapi.ActionUpdateMihomo:
		if !validPlanID(action.Params.PlanID) ||
			!action.Params.Confirm ||
			(action.Params.Trust != runtimeapi.MihomoUpdateTrustTUF &&
				action.Params.Trust != runtimeapi.MihomoUpdateTrustUpstreamOnly) ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("mihomo.update requires plan_id, trust, and explicit confirmation")
		}
	case runtimeapi.ActionRollbackMihomo:
		if !action.Params.Confirm ||
			action.Params.PlanID != "" ||
			action.Params.Trust != "" ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("mihomo.rollback requires only explicit confirmation")
		}
	case runtimeapi.ActionCheckProduct:
		if action.Params != (runtimeapi.ActionParams{}) {
			return errors.New("product.check does not accept parameters")
		}
	case runtimeapi.ActionUpdateProduct:
		if !validProductPlanID(action.Params.PlanID) ||
			!action.Params.Confirm ||
			action.Params.Trust != runtimeapi.ProductUpdateTrustTUF ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("product.update requires product plan_id, TUF trust, and explicit confirmation")
		}
	case runtimeapi.ActionRollbackProduct:
		if !action.Params.Confirm ||
			action.Params.PlanID != "" ||
			action.Params.Trust != "" ||
			action.Params.ContentID != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("product.rollback requires only explicit confirmation")
		}
	case runtimeapi.ActionRestoreBackup:
		if !validContentID(action.Params.ContentID) ||
			!action.Params.Confirm ||
			action.Params.PlanID != "" ||
			action.Params.Trust != "" ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" {
			return errors.New("backup.restore requires content_id and explicit confirmation")
		}
	case runtimeapi.ActionAddRemoteSource:
		if !validContentID(action.Params.ContentID) ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("source.add_remote requires only a valid content_id")
		}
	case runtimeapi.ActionAddImportedSource:
		if !validContentID(action.Params.ContentID) ||
			!validResourceName(action.Params.SourceName) ||
			action.Params.SourceID != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("source.add_imported requires content_id and source_name")
		}
	case runtimeapi.ActionRefreshSource:
		if !validSourceID(action.Params.SourceID) ||
			action.Params.ContentID != "" ||
			action.Params.SourceName != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" ||
			(action.Params.Route != "" &&
				action.Params.Route != runtimeapi.SourceRouteDirect &&
				action.Params.Route != runtimeapi.SourceRouteMihomo) {
			return errors.New("source.refresh requires a valid source_id and optional route")
		}
	case runtimeapi.ActionApplySource:
		if !validSourceID(action.Params.SourceID) ||
			action.Params.ContentID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("source.apply requires only a valid source_id")
		}
	case runtimeapi.ActionSwitchSource:
		if !validSourceID(action.Params.SourceID) ||
			action.Params.ContentID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" ||
			(action.Params.Route != "" &&
				action.Params.Route != runtimeapi.SourceRouteDirect &&
				action.Params.Route != runtimeapi.SourceRouteMihomo) {
			return errors.New("source.switch requires source_id and optional route or use_cached")
		}
	case runtimeapi.ActionDeleteSource:
		if !validSourceID(action.Params.SourceID) ||
			action.Params.ContentID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("source.delete requires source_id and optional confirmation")
		}
	case runtimeapi.ActionAddManagedResource:
		if !validContentID(action.Params.ContentID) ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.PlanID != "" ||
			!validResourceKind(action.Params.ResourceKind) ||
			!validResourceName(action.Params.ResourceName) {
			return errors.New("resource.add requires content_id, resource_kind, and resource_name")
		}
	case runtimeapi.ActionSetAdvancedOverride:
		if !validContentID(action.Params.ContentID) ||
			action.Params.SourceID != "" ||
			action.Params.SourceName != "" ||
			action.Params.Route != "" ||
			action.Params.UseCached ||
			action.Params.Confirm ||
			action.Params.ResourceKind != "" ||
			action.Params.ResourceName != "" ||
			action.Params.PlanID != "" {
			return errors.New("override.set requires only a valid content_id")
		}
	default:
		return fmt.Errorf("Runtime action %q is not supported", action.Kind)
	}
	return nil
}

func validConnectionID(id string) bool {
	return id != "" && len(id) <= 128 && validOptionalConnectionText(id, 128) && strings.TrimSpace(id) == id
}

func validTrafficPolicy(selection string) bool {
	switch selection {
	case runtimeapi.TrafficPolicyFollowSource,
		runtimeapi.TrafficPolicyRule,
		runtimeapi.TrafficPolicyGlobal,
		runtimeapi.TrafficPolicyDirect:
		return true
	default:
		return false
	}
}

func validConnectionScope(scope runtimeapi.ConnectionQuery) bool {
	return scope.Page == 0 && scope.PageSize == 0 &&
		validOptionalConnectionText(scope.Target, 256) &&
		validOptionalConnectionText(scope.Process, 256) &&
		validOptionalConnectionText(scope.Rule, 256) &&
		validOptionalConnectionText(scope.Node, 256)
}

func validConnectionScopeToken(token string) bool {
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == sha256.Size
}

func validOptionalConnectionText(value string, maximum int) bool {
	if len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
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

func validPlanID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "plan_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validProductPlanID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "product_plan_")
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
	case runtimeapi.ActionSetTrafficPolicy:
		return "Runtime could not save the traffic policy"
	case runtimeapi.ActionCloseConnection:
		return "Runtime could not close the selected Mihomo connection"
	case runtimeapi.ActionCloseConnections:
		return "Runtime could not close the confirmed Mihomo connection scope"
	case runtimeapi.ActionApplyImportedConfig:
		return "Mihomo rejected or could not activate the imported configuration"
	case runtimeapi.ActionStartProxy:
		return "Mihomo could not start the explicit proxy"
	case runtimeapi.ActionStopProxy:
		return "Mihomo could not stop the explicit proxy"
	case runtimeapi.ActionEnableTUN:
		return "Runtime could not enable ordinary TUN networking"
	case runtimeapi.ActionDisableTUN:
		return "Runtime could not disable ordinary TUN networking"
	case runtimeapi.ActionEnableGateway:
		return "Runtime could not enable Linux gateway networking"
	case runtimeapi.ActionDisableGateway:
		return "Runtime could not disable Linux gateway networking"
	case runtimeapi.ActionUpdateMihomo:
		return "Runtime could not install the confirmed Mihomo core update"
	case runtimeapi.ActionRollbackMihomo:
		return "Runtime could not roll back the Mihomo core"
	case runtimeapi.ActionCheckProduct:
		return "Runtime could not check the stable product update channel"
	case runtimeapi.ActionUpdateProduct:
		return "Runtime could not install the confirmed product update"
	case runtimeapi.ActionRollbackProduct:
		return "Runtime could not roll back the product update"
	case runtimeapi.ActionRestoreBackup:
		return "Runtime could not restore the confirmed portable backup"
	case runtimeapi.ActionAddRemoteSource:
		return "Runtime could not add the remote configuration source"
	case runtimeapi.ActionAddImportedSource:
		return "Runtime could not add the imported configuration source"
	case runtimeapi.ActionRefreshSource:
		return "Runtime could not refresh the remote configuration source"
	case runtimeapi.ActionApplySource:
		return "Runtime could not apply the current configuration source"
	case runtimeapi.ActionSwitchSource:
		return "Runtime could not switch the current configuration source"
	case runtimeapi.ActionDeleteSource:
		return "Runtime could not delete the configuration source"
	case runtimeapi.ActionAddManagedResource:
		return "Runtime could not import the managed resource"
	case runtimeapi.ActionSetAdvancedOverride:
		return "Runtime could not save the advanced override"
	default:
		return "Runtime operation failed"
	}
}
