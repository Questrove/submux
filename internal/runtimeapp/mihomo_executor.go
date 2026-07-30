package runtimeapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimebackup"
	"submux/internal/runtimecore"
	"submux/internal/runtimenet"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimesource"
	"submux/internal/runtimestate"
	"submux/internal/runtimeupdate"
	"submux/internal/safepath"
)

type MihomoExecutor struct {
	State           *runtimestate.Store
	Core            *runtimecore.Store
	Process         *runtimeprocess.Process
	ConfigRoot      string
	ControlEndpoint string
	ProxyPort       int
	Platform        string
	Verifier        mihomo.RuntimeVerifier
	Sources         *runtimesource.Manager
	Network         FailOpenController
	TUN             TUNController
	Updates         *runtimeupdate.Manager
	Backups         *runtimebackup.Service
	Now             func() time.Time

	lifecycleMu sync.Mutex
}

type TUNController interface {
	Prepare(context.Context, string, string) (runtimenet.PreparedNetwork, error)
	Commit(context.Context, string, string) (runtimeapi.NetworkStatus, error)
	Release(context.Context, string, string, string) (runtimeapi.NetworkStatus, error)
	Observe(context.Context) (runtimeapi.NetworkStatus, error)
}

func (e *MihomoExecutor) PreviewCandidate(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	if e == nil {
		return runtimeapi.CandidatePreview{}, errors.New("Mihomo Runtime previewer is incomplete")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.State == nil || e.Core == nil || e.Process == nil {
		return runtimeapi.CandidatePreview{}, errors.New("Mihomo Runtime previewer is incomplete")
	}
	body, contentID, sourceDigest, err := e.previewSource(peer, request)
	if err != nil {
		return runtimeapi.CandidatePreview{}, err
	}
	override, overrideDigest, err := e.previewOverride(peer, request)
	if err != nil {
		return runtimeapi.CandidatePreview{}, err
	}
	binaryPath, exactVersion, err := e.currentCore()
	if err != nil {
		return runtimeapi.CandidatePreview{}, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "A verified Mihomo core must be installed before previewing a configuration",
			Cause:   err,
		}
	}
	detailed, err := e.buildDetailedCandidate(body, override)
	if err != nil {
		return runtimeapi.CandidatePreview{}, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The imported Mihomo configuration is outside the Runtime safety policy",
			Cause:   err,
		}
	}
	listeners, err := mihomo.ProxyListeners(detailed.YAML)
	if err != nil {
		return runtimeapi.CandidatePreview{}, err
	}
	if len(listeners) == 0 {
		return runtimeapi.CandidatePreview{}, errors.New("candidate configuration has no explicit proxy listeners")
	}
	validator, err := e.configValidator(binaryPath, exactVersion)
	if err != nil {
		return runtimeapi.CandidatePreview{}, err
	}
	if err := validatePreviewCandidate(ctx, e.ConfigRoot, validator, detailed.YAML); err != nil {
		return runtimeapi.CandidatePreview{}, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Mihomo rejected the candidate configuration",
			Cause:   err,
		}
	}
	digest := sha256.Sum256(detailed.YAML)
	return runtimeapi.CandidatePreview{
		ContentID:           contentID,
		CandidateYAML:       string(detailed.YAML),
		CandidateSHA256:     hex.EncodeToString(digest[:]),
		ProxyKind:           listeners[0].Kind,
		ProxyAddresses:      listenerAddresses(listeners),
		RuntimeOwnedFields:  mihomo.ExplicitRuntimeOwnedFields(),
		FieldOrigins:        runtimeFieldOrigins(detailed.FieldOrigins),
		ReferencedResources: append([]string(nil), detailed.ReferencedResources...),
		SourceSHA256:        sourceDigest,
		OverrideSHA256:      overrideDigest,
		Validated:           true,
	}, nil
}

func (e *MihomoExecutor) Execute(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if e == nil {
		return nil, errors.New("Mihomo Runtime executor is incomplete")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.State == nil || e.Core == nil || e.Process == nil || e.Verifier == nil {
		return nil, errors.New("Mihomo Runtime executor is incomplete")
	}
	switch operation.Action.Kind {
	case runtimeapi.ActionApplyImportedConfig:
		return e.applyImport(ctx, operation, report)
	case runtimeapi.ActionApplySource:
		return e.applySource(ctx, operation, report)
	case runtimeapi.ActionSwitchSource:
		return e.switchSource(ctx, operation, report)
	case runtimeapi.ActionDeleteSource:
		return e.deleteSource(ctx, operation, report)
	case runtimeapi.ActionStartProxy:
		return e.start(ctx, report)
	case runtimeapi.ActionStopProxy:
		return e.stop(ctx, operation, report)
	case runtimeapi.ActionEnableTUN:
		return e.enableNetwork(ctx, operation, runtimeapi.RunModeTUN, report)
	case runtimeapi.ActionDisableTUN:
		return e.disableNetwork(ctx, operation, runtimeapi.RunModeTUN, report)
	case runtimeapi.ActionEnableGateway:
		return e.enableNetwork(ctx, operation, runtimeapi.RunModeGateway, report)
	case runtimeapi.ActionDisableGateway:
		return e.disableNetwork(ctx, operation, runtimeapi.RunModeGateway, report)
	case runtimeapi.ActionUpdateMihomo:
		if e.Updates == nil {
			return nil, errors.New("Mihomo update manager is unavailable")
		}
		return e.Updates.Activate(ctx, operation, runtimeupdate.Reporter(report))
	case runtimeapi.ActionRollbackMihomo:
		if e.Updates == nil {
			return nil, errors.New("Mihomo update manager is unavailable")
		}
		return e.Updates.Rollback(ctx, operation, runtimeupdate.Reporter(report))
	case runtimeapi.ActionRestoreBackup:
		return e.restoreBackup(ctx, operation, report)
	case runtimeapi.ActionAddManagedResource:
		return e.addManagedResource(operation, report)
	case runtimeapi.ActionSetAdvancedOverride:
		return e.setAdvancedOverride(ctx, operation, report)
	case runtimeapi.ActionAddRemoteSource,
		runtimeapi.ActionAddImportedSource,
		runtimeapi.ActionRefreshSource:
		if e.Sources == nil {
			return nil, errors.New("Runtime source manager is unavailable")
		}
		result, err := e.Sources.Execute(ctx, operation, runtimesource.Reporter(report))
		if err == nil {
			return result, nil
		}
		return nil, exposeSourceManagerError(err)
	default:
		return nil, fmt.Errorf("unsupported Runtime action %q", operation.Action.Kind)
	}
}

func (e *MihomoExecutor) restoreBackup(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if e.Backups == nil {
		return nil, errors.New("Runtime backup restore service is unavailable")
	}
	if !operation.Action.Params.Confirm {
		return nil, errors.New("Runtime backup restore requires explicit confirmation")
	}
	if err := report("reading_backup", 5, true); err != nil {
		return nil, err
	}
	body, content, err := e.State.ConsumeImport(
		operation.Action.Params.ContentID,
		operation.CallerIdentity,
		operation.ID,
		e.now(),
	)
	if err != nil {
		return nil, err
	}
	if content.ContentType != runtimeapi.RuntimeBackupContentType {
		return nil, errors.New("uploaded content is not a Runtime backup archive")
	}
	stopResult, err := e.stop(ctx, operation, func(stage string, progress int, cancellable bool) error {
		return report(stage, 10+progress/4, cancellable)
	})
	if err != nil {
		return nil, fmt.Errorf("stop Mihomo and release Runtime network ownership before restore: %w", err)
	}
	if err := report("replacing_portable_state", 40, false); err != nil {
		return nil, err
	}
	var prepared *runtimeapi.OperationResult
	restored, err := e.Backups.Restore(
		ctx,
		body,
		operation.ID,
		func(ctx context.Context, _ runtimestate.PortableState) error {
			current, currentErr := e.State.CurrentSource()
			if errors.Is(currentErr, runtimestate.ErrSourceNotFound) {
				return clearPreparedConfiguration(e.ConfigRoot)
			}
			if currentErr != nil {
				return currentErr
			}
			raw, _, readErr := e.State.ReadSourceRevision(current)
			if readErr != nil {
				return readErr
			}
			override, _, overrideErr := e.State.AdvancedOverride()
			if overrideErr != nil {
				return overrideErr
			}
			detailed, buildErr := e.buildDetailedCandidateForNetwork(raw, override, nil)
			if buildErr != nil {
				return buildErr
			}
			prepared, buildErr = e.deployCandidate(
				ctx,
				operation.ID+"_restored",
				current.RawSHA256,
				raw,
				detailed.YAML,
				current.ID,
				func(stage string, progress int, cancellable bool) error {
					return report(stage, 55+progress*35/100, cancellable)
				},
			)
			return buildErr
		},
	)
	if err != nil {
		return nil, err
	}
	result := &runtimeapi.OperationResult{
		Verified:               true,
		RunMode:                runtimeapi.RunModeUnconfigured,
		Network:                stopResult.Network,
		BackupSHA256:           restored.BackupSHA256,
		AutomaticBackupFile:    restored.AutomaticBackupFile,
		MachineSettingsPending: restored.MachineSettingsPending,
	}
	if prepared != nil {
		result.ConfigRevision = prepared.ConfigRevision
		result.CandidateSHA256 = prepared.CandidateSHA256
		result.ProxyKind = prepared.ProxyKind
		result.ProxyAddresses = append([]string(nil), prepared.ProxyAddresses...)
		result.SourceID = prepared.SourceID
	}
	return result, nil
}

func clearPreparedConfiguration(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("Runtime configuration root must be a fixed absolute path")
	}
	root = filepath.Clean(root)
	linked, err := safepath.ContainsLinkInExistingPath(root)
	if err != nil {
		return err
	}
	if linked {
		return errors.New("Runtime configuration root must not contain symbolic or reparse links")
	}
	for _, name := range []string{"current", "previous-good", "staging", "failed"} {
		target := filepath.Join(root, name)
		if filepath.Dir(target) != root {
			return errors.New("Runtime configuration cleanup escaped its root")
		}
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Runtime prepared configuration contains an unmanaged entry")
		}
		linked, err := safepath.ContainsLink(target)
		if err != nil {
			return err
		}
		if linked {
			return errors.New("Runtime prepared configuration must not contain linked entries")
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return nil
}

func (e *MihomoExecutor) Verify(ctx context.Context) (runtimeapi.ProxyVerification, error) {
	if e == nil {
		return runtimeapi.ProxyVerification{}, errors.New("Mihomo Runtime verifier is unavailable")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	verification := runtimeapi.ProxyVerification{CheckedAt: e.now()}
	listeners, err := e.currentListeners()
	if err != nil {
		verification.Error = &runtimeapi.Fault{Code: runtimeapi.ErrorServiceUnavailable, Message: "No active Mihomo configuration is available"}
		return verification, nil
	}
	verification.Kind = listeners[0].Kind
	for _, listener := range listeners {
		verification.Addresses = append(verification.Addresses, listener.Address)
		if err := e.Verifier.VerifyRuntime(ctx, listener.Address); err != nil {
			verification.Error = &runtimeapi.Fault{Code: runtimeapi.ErrorServiceUnavailable, Message: "Mihomo explicit proxy verification failed"}
			return verification, nil
		}
	}
	verification.Available = true
	return verification, nil
}

func (e *MihomoExecutor) RestoreLastGood(ctx context.Context) error {
	if e == nil {
		return errors.New("Mihomo Runtime executor is unavailable")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	_, err := e.start(ctx, func(string, int, bool) error { return nil })
	return err
}

func (e *MihomoExecutor) ReconcileCoreVersion(context.Context) error {
	if e == nil || e.State == nil || e.Core == nil {
		return errors.New("Mihomo Runtime executor is unavailable")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	status, err := e.Core.Status()
	if err != nil {
		return err
	}
	if !status.Installed {
		return nil
	}
	if _, err := e.Core.CurrentBinaryPath(); err != nil {
		return err
	}
	_, err = e.State.ObserveMihomoCoreVersion(status.Version, e.now())
	return err
}

func (e *MihomoExecutor) FailOpen(ctx context.Context) error {
	if e == nil {
		return errors.New("Mihomo Runtime executor is unavailable")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	snapshot, stateErr := e.State.Observe("", e.now())
	if stateErr != nil {
		return stateErr
	}
	hadTakeover := snapshot.RunMode == runtimeapi.RunModeTUN ||
		snapshot.RunMode == runtimeapi.RunModeGateway
	if e.Network == nil {
		if hadTakeover {
			return errors.New("Runtime network fail-open controller is unavailable")
		}
		return nil
	}
	if e.TUN != nil {
		if status, err := e.TUN.Observe(ctx); err == nil {
			hadTakeover = status.OwnershipID != ""
		}
	}
	if err := e.Network.FailOpen(ctx); err != nil {
		return err
	}
	if !hadTakeover {
		return nil
	}
	_, err := e.prepareCurrentCandidate(
		ctx,
		fmt.Sprintf("op_failopen_%d", e.now().UnixNano()),
		nil,
		func(string, int, bool) error { return nil },
	)
	return err
}

func (e *MihomoExecutor) ExitEvents() <-chan runtimeprocess.ExitEvent {
	if e == nil || e.Process == nil {
		return nil
	}
	return e.Process.ExitEvents()
}

func (e *MihomoExecutor) applyImport(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("reading_import", 10, true); err != nil {
		return nil, err
	}
	body, content, err := e.State.ConsumeImport(
		operation.Action.Params.ContentID,
		operation.CallerIdentity,
		operation.ID,
		e.now(),
	)
	if err != nil {
		return nil, &PublicError{
			Code:    importErrorCode(err),
			Message: "The imported Mihomo configuration is unavailable or no longer usable",
			Cause:   err,
		}
	}
	if !isYAMLContentType(content.ContentType) {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The uploaded content is not a Mihomo YAML configuration",
		}
	}
	if err := report("validating_candidate", 25, true); err != nil {
		return nil, err
	}
	override, _, err := e.State.AdvancedOverride()
	if err != nil {
		return nil, err
	}
	detailed, err := e.buildDetailedCandidate(body, override)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The imported Mihomo configuration is outside the Runtime safety policy",
			Cause:   err,
		}
	}
	return e.deployCandidate(ctx, operation.ID, content.SHA256, body, detailed.YAML, "", report)
}

func (e *MihomoExecutor) applySource(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("reading_source_revision", 10, true); err != nil {
		return nil, err
	}
	record, err := e.State.CurrentSource()
	if err != nil || record.ID != operation.Action.Params.SourceID {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorNotFound,
			Message: "Only the current validated remote source can be applied",
			Cause:   err,
		}
	}
	body, _, err := e.State.ReadSourceRevision(record)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "The current remote source revision is unavailable",
			Cause:   err,
		}
	}
	if err := report("validating_candidate", 25, true); err != nil {
		return nil, err
	}
	override, _, err := e.State.AdvancedOverride()
	if err != nil {
		return nil, err
	}
	detailed, err := e.buildDetailedCandidate(body, override)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The current remote source is outside the Runtime safety policy",
			Cause:   err,
		}
	}
	return e.deployCandidate(
		ctx,
		operation.ID,
		record.RawSHA256,
		body,
		detailed.YAML,
		record.ID,
		report,
	)
}

func (e *MihomoExecutor) switchSource(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if e.Sources == nil {
		return nil, errors.New("Runtime source manager is unavailable")
	}
	if err := report("preparing_source_switch", 5, true); err != nil {
		return nil, err
	}
	current, currentErr := e.State.CurrentSource()
	if currentErr != nil && !errors.Is(currentErr, runtimestate.ErrSourceNotFound) {
		return nil, currentErr
	}
	expectedCurrentID := current.ID
	wasRunning, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	previousProcess := captureProcessConfiguration(e.Process)
	if current.ID != "" {
		previousProcess.DataDir, err = e.State.SourceRuntimeDataDir(current.ID)
		if err != nil {
			return nil, err
		}
	}
	prepared, err := e.Sources.PrepareSwitch(
		ctx,
		operation,
		func(stage string, progress int, cancellable bool) error {
			return report(stage, 5+progress*2/5, cancellable)
		},
	)
	if err != nil {
		return nil, exposeSourceManagerError(err)
	}
	raw, _, err := e.State.ReadSourceRevision(prepared.Record)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "The selected source revision is unavailable",
			Cause:   err,
		}
	}
	override, _, err := e.State.AdvancedOverride()
	if err != nil {
		return nil, err
	}
	if err := report("building_source_candidate", 50, true); err != nil {
		return nil, err
	}
	detailed, err := e.buildDetailedCandidate(raw, override)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The selected source is outside the Runtime safety policy",
			Cause:   err,
		}
	}
	binaryPath, exactVersion, err := e.currentCore()
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "A verified Mihomo core must be installed before switching sources",
			Cause:   err,
		}
	}
	targetDataDir, err := e.State.SourceRuntimeDataDir(prepared.Record.ID)
	if err != nil {
		return nil, err
	}
	validator, err := e.configValidator(binaryPath, exactVersion)
	if err != nil {
		return nil, err
	}
	validator.DataDir = targetDataDir
	targetProcess := processConfiguration{
		BinaryPath: binaryPath,
		ConfigPath: filepath.Join(e.ConfigRoot, "current", "config.yaml"),
		DataDir:    targetDataDir,
		SafePaths:  append([]string(nil), validator.SafePaths...),
	}
	service := &sourceSwitchRuntimeService{
		process:  e.Process,
		report:   report,
		target:   targetProcess,
		previous: previousProcess,
	}
	deployer := &mihomo.Deployer{
		Root: e.ConfigRoot,
		Builder: mihomo.CandidateBuilderFunc(func([]byte) ([]byte, error) {
			return append([]byte(nil), detailed.YAML...), nil
		}),
		Validator: validator,
		Service:   service,
		Verifier:  e.Verifier,
	}
	deployment, err := deployer.Apply(ctx, operation.ID, prepared.Record.RawSHA256, raw)
	if err != nil {
		previousProcess.apply(e.Process)
		if !wasRunning {
			_ = e.Process.Stop(context.Background())
		}
		return nil, &PublicError{
			Code:      runtimeapi.ErrorServiceUnavailable,
			Message:   "The selected source could not be activated; the previous source was retained",
			Retryable: true,
			Cause:     err,
		}
	}
	if !wasRunning {
		if err := report("restoring_stopped_state", 85, false); err != nil {
			rollbackErr := e.rollbackSourceDeployment(deployer, previousProcess, false)
			return nil, errors.Join(err, rollbackErr)
		}
		if stopErr := e.Process.Stop(ctx); stopErr != nil {
			rollbackErr := e.rollbackSourceDeployment(deployer, previousProcess, false)
			return nil, errors.Join(stopErr, rollbackErr)
		}
	}
	if err := report("committing_source_switch", 92, false); err != nil {
		rollbackErr := e.rollbackSourceDeployment(deployer, previousProcess, wasRunning)
		return nil, errors.Join(err, rollbackErr)
	}
	switched, err := e.State.SwitchCurrentSource(
		expectedCurrentID,
		prepared.Record.ID,
		operation.ID,
		e.now(),
	)
	if err != nil {
		rollbackErr := e.rollbackSourceDeployment(deployer, previousProcess, wasRunning)
		return nil, errors.Join(err, rollbackErr)
	}
	targetProcess.apply(e.Process)
	result := &runtimeapi.OperationResult{
		ConfigRevision:   deployment.Revision,
		CandidateSHA256:  deployment.CandidateHash,
		ProxyKind:        deployment.ProxyKind,
		ProxyAddresses:   append([]string(nil), deployment.ProxyAddresses...),
		Verified:         true,
		SourceID:         switched.ID,
		PreviousSourceID: expectedCurrentID,
		UsedCachedSource: prepared.UsedCached,
	}
	if prepared.RefreshResult != nil {
		result.RefreshResult = prepared.RefreshResult.RefreshResult
		result.RefreshRoute = prepared.RefreshResult.RefreshRoute
		result.NextRefreshAt = prepared.RefreshResult.NextRefreshAt
		result.NotModified = prepared.RefreshResult.NotModified
	} else {
		result.RefreshResult = switched.LastRefreshResult
	}
	return result, nil
}

func (e *MihomoExecutor) rollbackSourceDeployment(
	deployer *mihomo.Deployer,
	previous processConfiguration,
	wasRunning bool,
) error {
	previous.apply(e.Process)
	_, rollbackErr := deployer.Rollback(context.Background())
	if rollbackErr != nil {
		stopErr := e.Process.Stop(context.Background())
		previous.apply(e.Process)
		return errors.Join(rollbackErr, stopErr)
	}
	previous.apply(e.Process)
	if !wasRunning {
		rollbackErr = errors.Join(rollbackErr, e.Process.Stop(context.Background()))
	}
	return rollbackErr
}

func (e *MihomoExecutor) deleteSource(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	record, err := e.State.GetSource(operation.Action.Params.SourceID)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorNotFound,
			Message: "The Runtime configuration source is unavailable",
			Cause:   err,
		}
	}
	current, currentErr := e.State.CurrentSource()
	if currentErr != nil && !errors.Is(currentErr, runtimestate.ErrSourceNotFound) {
		return nil, currentErr
	}
	isCurrent := current.ID == record.ID
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	if isCurrent && running {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorBusy,
			Message: "The current source cannot be deleted while Mihomo is running",
		}
	}
	if isCurrent && !operation.Action.Params.Confirm {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Deleting the stopped current source requires explicit confirmation",
		}
	}
	var fallbackDataDir string
	if isCurrent {
		fallbackDataDir, err = e.State.RuntimeDataDir()
		if err != nil {
			return nil, err
		}
	}
	if err := report("deleting_source", 70, false); err != nil {
		return nil, err
	}
	if _, err := e.State.DeleteSource(record.ID, isCurrent, operation.ID, e.now()); err != nil {
		return nil, err
	}
	if isCurrent {
		e.Process.DataDir = fallbackDataDir
	}
	return &runtimeapi.OperationResult{
		SourceID: record.ID,
		Deleted:  true,
	}, nil
}

func (e *MihomoExecutor) deployCandidate(
	ctx context.Context,
	operationID string,
	sourceDigest string,
	source []byte,
	candidate []byte,
	sourceID string,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	binaryPath, exactVersion, err := e.currentCore()
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "A verified Mihomo core must be installed before applying a configuration",
			Cause:   err,
		}
	}
	e.Process.BinaryPath = binaryPath
	e.Process.ConfigPath = filepath.Join(e.ConfigRoot, "current", "config.yaml")
	if sourceID != "" {
		sourceDataDir, err := e.State.SourceRuntimeDataDir(sourceID)
		if err != nil {
			return nil, err
		}
		e.Process.DataDir = sourceDataDir
	}
	validator, err := e.configValidator(binaryPath, exactVersion)
	if err != nil {
		return nil, err
	}
	e.Process.SafePaths = append([]string(nil), validator.SafePaths...)
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	service := stagedRuntimeService{
		service: e.Process,
		report:  report,
	}
	deployer := &mihomo.Deployer{
		Root: e.ConfigRoot,
		Builder: mihomo.CandidateBuilderFunc(func([]byte) ([]byte, error) {
			return append([]byte(nil), candidate...), nil
		}),
		Validator: validator,
		Service:   service,
		Verifier:  e.Verifier,
	}
	var deployment mihomo.DeploymentResult
	if running {
		deployment, err = deployer.Apply(ctx, operationID, sourceDigest, source)
	} else {
		if err := report("preparing_candidate", 70, false); err != nil {
			return nil, err
		}
		deployment, err = deployer.Prepare(ctx, operationID, sourceDigest, source)
	}
	if err != nil {
		return nil, err
	}
	finalStage := "ready_to_start"
	if running {
		finalStage = "verified"
	}
	if err := report(finalStage, 95, false); err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		ConfigRevision: deployment.Revision,
		ProxyKind:      deployment.ProxyKind,
		ProxyAddresses: append([]string(nil), deployment.ProxyAddresses...),
		Verified:       running,
		SourceID:       sourceID,
	}, nil
}

func (e *MihomoExecutor) ValidateSourceCandidate(
	ctx context.Context,
	body []byte,
) (runtimesource.ValidatedCandidate, error) {
	if e == nil || e.State == nil || e.Core == nil || e.Process == nil {
		return runtimesource.ValidatedCandidate{}, errors.New("Mihomo Runtime source validator is incomplete")
	}
	binaryPath, exactVersion, err := e.currentCore()
	if err != nil {
		return runtimesource.ValidatedCandidate{}, err
	}
	override, _, err := e.State.AdvancedOverride()
	if err != nil {
		return runtimesource.ValidatedCandidate{}, err
	}
	detailed, err := e.buildDetailedCandidate(body, override)
	if err != nil {
		return runtimesource.ValidatedCandidate{}, err
	}
	validator, err := e.configValidator(binaryPath, exactVersion)
	if err != nil {
		return runtimesource.ValidatedCandidate{}, err
	}
	if err := validatePreviewCandidate(ctx, e.ConfigRoot, validator, detailed.YAML); err != nil {
		return runtimesource.ValidatedCandidate{}, err
	}
	digest := sha256.Sum256(detailed.YAML)
	return runtimesource.ValidatedCandidate{
		YAML:   detailed.YAML,
		SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func (e *MihomoExecutor) previewSource(
	peer runtimeapi.PeerIdentity,
	request runtimeapi.PreviewCandidateRequest,
) ([]byte, string, string, error) {
	if (request.ContentID == "") == (request.SourceID == "") {
		return nil, "", "", &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Candidate preview requires exactly one imported configuration or remote source",
		}
	}
	if request.ContentID != "" {
		body, content, err := e.State.PeekImport(request.ContentID, peer.Key(), e.now())
		if err != nil {
			return nil, "", "", err
		}
		if !isYAMLContentType(content.ContentType) {
			return nil, "", "", &PublicError{
				Code:    runtimeapi.ErrorInvalidRequest,
				Message: "The uploaded content is not a Mihomo YAML configuration",
			}
		}
		return body, content.ID, content.SHA256, nil
	}
	record, err := e.State.GetSource(request.SourceID)
	if err != nil {
		return nil, "", "", &PublicError{
			Code:    runtimeapi.ErrorNotFound,
			Message: "The requested Runtime configuration source is unavailable",
			Cause:   err,
		}
	}
	body, _, err := e.State.ReadSourceRevision(record)
	if err != nil {
		return nil, "", "", &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "The requested Runtime configuration source revision is unavailable",
			Cause:   err,
		}
	}
	return body, "", record.RawSHA256, nil
}

func (e *MihomoExecutor) previewOverride(
	peer runtimeapi.PeerIdentity,
	request runtimeapi.PreviewCandidateRequest,
) ([]byte, string, error) {
	if request.OverrideContentID == "" {
		body, record, err := e.State.AdvancedOverride()
		return body, record.SHA256, err
	}
	body, content, err := e.State.PeekImport(request.OverrideContentID, peer.Key(), e.now())
	if err != nil {
		return nil, "", err
	}
	if !isYAMLContentType(content.ContentType) {
		return nil, "", &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The uploaded advanced override is not YAML",
		}
	}
	return body, content.SHA256, nil
}

func (e *MihomoExecutor) buildDetailedCandidate(
	source []byte,
	override []byte,
) (mihomo.DetailedCandidate, error) {
	return e.buildDetailedCandidateForNetwork(source, override, nil)
}

func (e *MihomoExecutor) buildDetailedCandidateForNetwork(
	source []byte,
	override []byte,
	tun *mihomo.TUNCandidateSettings,
) (mihomo.DetailedCandidate, error) {
	resources, err := e.State.ManagedResources()
	if err != nil {
		return mihomo.DetailedCandidate{}, err
	}
	managed := make([]mihomo.ManagedResource, 0, len(resources))
	for _, resource := range resources {
		managed = append(managed, mihomo.ManagedResource{
			ID:   resource.ID,
			Kind: resource.Kind,
			Path: resource.Path,
		})
	}
	builder := mihomo.ExplicitCandidateBuilder{
		Port:            e.ProxyPort,
		ControlEndpoint: e.ControlEndpoint,
		Platform:        e.Platform,
		TUN:             tun,
	}
	return builder.BuildDetailed(source, override, managed)
}

func (e *MihomoExecutor) addManagedResource(
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("reading_managed_resource", 10, true); err != nil {
		return nil, err
	}
	body, content, err := e.State.ConsumeImport(
		operation.Action.Params.ContentID,
		operation.CallerIdentity,
		operation.ID,
		e.now(),
	)
	if err != nil {
		return nil, &PublicError{
			Code:    importErrorCode(err),
			Message: "The uploaded managed resource is unavailable or no longer usable",
			Cause:   err,
		}
	}
	if normalizedContentType(content.ContentType) != runtimeapi.ManagedResourceContentType {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The uploaded content is not a Runtime managed resource",
		}
	}
	if err := report("validating_managed_resource", 45, true); err != nil {
		return nil, err
	}
	if err := mihomo.ValidateManagedResource(operation.Action.Params.ResourceKind, body); err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The managed resource content does not match its declared type",
			Cause:   err,
		}
	}
	if err := report("saving_managed_resource", 80, false); err != nil {
		return nil, err
	}
	record, err := e.State.CreateManagedResource(
		operation.Action.Params.ResourceName,
		operation.Action.Params.ResourceKind,
		body,
		operation.ID,
		e.now(),
	)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		ResourceID:   record.ID,
		ResourceKind: record.Kind,
	}, nil
}

func (e *MihomoExecutor) setAdvancedOverride(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("reading_advanced_override", 10, true); err != nil {
		return nil, err
	}
	body, content, err := e.State.ConsumeImport(
		operation.Action.Params.ContentID,
		operation.CallerIdentity,
		operation.ID,
		e.now(),
	)
	if err != nil {
		return nil, &PublicError{
			Code:    importErrorCode(err),
			Message: "The uploaded advanced override is unavailable or no longer usable",
			Cause:   err,
		}
	}
	if !isYAMLContentType(content.ContentType) {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The advanced override must be YAML",
		}
	}
	source := []byte("{}\n")
	if current, currentErr := e.State.CurrentSource(); currentErr == nil {
		source, _, err = e.State.ReadSourceRevision(current)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(currentErr, runtimestate.ErrSourceNotFound) {
		return nil, currentErr
	}
	if err := report("validating_advanced_override", 45, true); err != nil {
		return nil, err
	}
	detailed, err := e.buildDetailedCandidate(source, body)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The advanced override is outside the Runtime safety policy",
			Cause:   err,
		}
	}
	binaryPath, exactVersion, err := e.currentCore()
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "A verified Mihomo core must be installed before saving an advanced override",
			Cause:   err,
		}
	}
	validator, err := e.configValidator(binaryPath, exactVersion)
	if err != nil {
		return nil, err
	}
	if err := validatePreviewCandidate(ctx, e.ConfigRoot, validator, detailed.YAML); err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "Mihomo rejected the configuration produced by the advanced override",
			Cause:   err,
		}
	}
	if err := report("saving_advanced_override", 85, false); err != nil {
		return nil, err
	}
	record, err := e.State.SetAdvancedOverride(body, operation.ID, e.now())
	if err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		AdvancedOverrideSHA256: record.SHA256,
	}, nil
}

func runtimeFieldOrigins(fields []mihomo.CandidateFieldOrigin) []runtimeapi.CandidateFieldOrigin {
	result := make([]runtimeapi.CandidateFieldOrigin, 0, len(fields))
	for _, field := range fields {
		result = append(result, runtimeapi.CandidateFieldOrigin{
			Path:           field.Path,
			Origin:         field.Origin,
			Status:         field.Status,
			ReplacedOrigin: field.ReplacedOrigin,
		})
	}
	return result
}

func (e *MihomoExecutor) DueActions(now time.Time) ([]runtimeapi.Action, error) {
	if e == nil || e.Sources == nil {
		return nil, nil
	}
	return e.Sources.DueActions(now)
}

func (e *MihomoExecutor) start(ctx context.Context, report StageReporter) (*runtimeapi.OperationResult, error) {
	listeners, err := e.currentListeners()
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "No verified Mihomo configuration is available to start",
			Cause:   err,
		}
	}
	binaryPath, _, err := e.currentCore()
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorServiceUnavailable,
			Message: "A verified Mihomo core is not installed",
			Cause:   err,
		}
	}
	e.Process.BinaryPath = binaryPath
	e.Process.ConfigPath = filepath.Join(e.ConfigRoot, "current", "config.yaml")
	if current, currentErr := e.State.CurrentSource(); currentErr == nil {
		sourceDataDir, dataErr := e.State.SourceRuntimeDataDir(current.ID)
		if dataErr != nil {
			return nil, dataErr
		}
		e.Process.DataDir = sourceDataDir
	} else if !errors.Is(currentErr, runtimestate.ErrSourceNotFound) {
		return nil, currentErr
	}
	safePaths, err := e.mihomoSafePaths()
	if err != nil {
		return nil, err
	}
	e.Process.SafePaths = safePaths
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	if !running {
		if err := runtimeprocess.CheckLoopbackPortsAvailable(listenerAddresses(listeners)); err != nil {
			return nil, &PublicError{Code: runtimeapi.ErrorServiceUnavailable, Message: err.Error(), Cause: err}
		}
	}
	if err := report("starting_proxy", 60, false); err != nil {
		return nil, err
	}
	if err := e.Process.Start(ctx); err != nil {
		return nil, err
	}
	for _, listener := range listeners {
		if err := e.Verifier.VerifyRuntime(ctx, listener.Address); err != nil {
			_ = e.Process.Stop(context.Background())
			return nil, err
		}
	}
	return &runtimeapi.OperationResult{
		ProxyKind:      listeners[0].Kind,
		ProxyAddresses: listenerAddresses(listeners),
		Verified:       true,
	}, nil
}

func (e *MihomoExecutor) enableNetwork(
	ctx context.Context,
	operation runtimeapi.Operation,
	mode string,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if e.TUN == nil {
		return nil, errors.New("privileged Runtime network service is unavailable")
	}
	if mode != runtimeapi.RunModeTUN && mode != runtimeapi.RunModeGateway {
		return nil, errors.New("Runtime network mode is invalid")
	}
	if err := report("preparing_network", 10, false); err != nil {
		return nil, err
	}
	wasRunning, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	prepared, err := e.TUN.Prepare(ctx, operation.ID, operation.Action.Params.PlanID)
	if err != nil {
		return nil, err
	}
	rollback := func(cause error) (*runtimeapi.OperationResult, error) {
		rollbackErr := e.rollbackPreparedNetwork(
			prepared,
			operation.ID+"_rollback",
			wasRunning,
		)
		return nil, errors.Join(cause, rollbackErr)
	}
	if prepared.Mode != mode {
		return rollback(fmt.Errorf(
			"Runtime network plan prepared mode %s instead of %s",
			prepared.Mode,
			mode,
		))
	}
	if err := report("building_network_candidate", 25, true); err != nil {
		return rollback(err)
	}
	ipv6Policy := prepared.Settings.IPv6Policy
	hijackDNS := prepared.Settings.DNSPolicy == runtimeapi.TUNDNSHijack
	if prepared.GatewaySettings != nil {
		ipv6Policy = prepared.GatewaySettings.IPv6Policy
		hijackDNS = prepared.GatewaySettings.DNSPolicy == runtimeapi.TUNDNSHijack
	}
	deployment, err := e.prepareCurrentCandidate(ctx, operation.ID, &mihomo.TUNCandidateSettings{
		Device:      prepared.Device,
		RoutingMark: prepared.RoutingMark,
		IPv6Policy:  ipv6Policy,
		HijackDNS:   hijackDNS,
	}, report)
	if err != nil {
		return rollback(err)
	}
	if !deployment.Verified {
		started, startErr := e.start(ctx, report)
		if startErr != nil {
			return rollback(startErr)
		}
		deployment.ProxyKind = started.ProxyKind
		deployment.ProxyAddresses = append([]string(nil), started.ProxyAddresses...)
		deployment.Verified = started.Verified
	}
	if err := report("committing_network_takeover", 90, false); err != nil {
		return rollback(err)
	}
	status, err := e.TUN.Commit(ctx, operation.ID, prepared.OwnershipID)
	if err != nil {
		return rollback(err)
	}
	deployment.RunMode = mode
	deployment.Network = &status
	deployment.Verified = status.State == runtimeapi.NetworkStateActive
	return deployment, nil
}

func (e *MihomoExecutor) disableNetwork(
	ctx context.Context,
	operation runtimeapi.Operation,
	mode string,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if e.TUN == nil {
		return nil, errors.New("privileged Runtime network service is unavailable")
	}
	status, err := e.TUN.Observe(ctx)
	if err != nil {
		return nil, err
	}
	if status.OwnershipID == "" {
		return &runtimeapi.OperationResult{
			RunMode:  runtimeapi.RunModeExplicit,
			Network:  &status,
			Verified: status.State == runtimeapi.NetworkStateInactive,
		}, nil
	}
	if status.Mode != mode {
		return nil, fmt.Errorf("active Runtime network mode is %s, not %s", status.Mode, mode)
	}
	wasRunning, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	if err := report("releasing_network_takeover", 20, false); err != nil {
		return nil, err
	}
	status, err = e.TUN.Release(
		ctx,
		operation.ID,
		status.OwnershipID,
		runtimenet.ReleaseOperator,
	)
	if err != nil {
		return nil, err
	}
	if err := report("stopping_network_proxy", 45, false); err != nil {
		return nil, err
	}
	if err := e.Process.Stop(ctx); err != nil {
		return nil, err
	}
	deployment, err := e.prepareCurrentCandidate(ctx, operation.ID+"_explicit", nil, report)
	if err != nil {
		return nil, err
	}
	if wasRunning {
		started, startErr := e.start(ctx, report)
		if startErr != nil {
			return nil, startErr
		}
		deployment.ProxyKind = started.ProxyKind
		deployment.ProxyAddresses = append([]string(nil), started.ProxyAddresses...)
		deployment.Verified = started.Verified
	}
	deployment.RunMode = runtimeapi.RunModeExplicit
	deployment.Network = &status
	return deployment, nil
}

func (e *MihomoExecutor) stop(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	hadTakeover := false
	var networkStatus *runtimeapi.NetworkStatus
	if e.TUN != nil {
		status, observeErr := e.TUN.Observe(ctx)
		if observeErr == nil {
			hadTakeover = status.OwnershipID != ""
			networkStatus = &status
		} else {
			return nil, fmt.Errorf(
				"cannot stop Mihomo while Runtime network ownership is unavailable: %w",
				observeErr,
			)
		}
	} else {
		snapshot, stateErr := e.State.Observe("", e.now())
		if stateErr != nil {
			return nil, stateErr
		}
		if snapshot.RunMode == runtimeapi.RunModeTUN ||
			snapshot.RunMode == runtimeapi.RunModeGateway {
			return nil, errors.New("cannot stop Mihomo without the privileged Runtime network controller")
		}
	}
	if hadTakeover {
		if e.Network == nil {
			return nil, errors.New("cannot stop Mihomo without the privileged Runtime network fail-open controller")
		}
		if err := report("releasing_network_takeover", 35, false); err != nil {
			return nil, err
		}
		if err := e.Network.FailOpen(ctx); err != nil {
			return nil, err
		}
		status, err := e.TUN.Observe(ctx)
		if err != nil {
			return nil, err
		}
		networkStatus = &status
	}
	if err := report("stopping_proxy", 60, false); err != nil {
		return nil, err
	}
	if err := e.Process.Stop(ctx); err != nil {
		return nil, err
	}
	if hadTakeover {
		if _, err := e.prepareCurrentCandidate(
			ctx,
			operation.ID+"_explicit",
			nil,
			report,
		); err != nil {
			return nil, err
		}
	}
	return &runtimeapi.OperationResult{
		Verified: true,
		RunMode:  runtimeapi.RunModeExplicit,
		Network:  networkStatus,
	}, nil
}

func (e *MihomoExecutor) prepareCurrentCandidate(
	ctx context.Context,
	operationID string,
	tun *mihomo.TUNCandidateSettings,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	source, err := os.ReadFile(filepath.Join(e.ConfigRoot, "current", "source.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read current Mihomo source: %w", err)
	}
	override, _, err := e.State.AdvancedOverride()
	if err != nil {
		return nil, err
	}
	detailed, err := e.buildDetailedCandidateForNetwork(source, override, tun)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(source)
	return e.deployCandidate(
		ctx,
		operationID,
		hex.EncodeToString(digest[:]),
		source,
		detailed.YAML,
		"",
		report,
	)
}

func (e *MihomoExecutor) rollbackPreparedNetwork(
	prepared runtimenet.PreparedNetwork,
	operationID string,
	wasRunning bool,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, releaseErr := e.TUN.Release(
		ctx,
		operationID,
		prepared.OwnershipID,
		runtimenet.ReleaseMihomoFailure,
	)
	if releaseErr != nil {
		return releaseErr
	}
	stopErr := e.Process.Stop(ctx)
	_, prepareErr := e.prepareCurrentCandidate(
		ctx,
		operationID+"_explicit",
		nil,
		func(string, int, bool) error { return nil },
	)
	var startErr error
	if wasRunning && prepareErr == nil {
		_, startErr = e.start(ctx, func(string, int, bool) error { return nil })
	}
	return errors.Join(stopErr, releaseErr, prepareErr, startErr)
}

func (e *MihomoExecutor) currentCore() (string, string, error) {
	status, err := e.Core.Status()
	if err != nil {
		return "", "", err
	}
	if !status.Installed || status.Version == "" {
		return "", "", errors.New("Mihomo core is not installed")
	}
	path, err := e.Core.CurrentBinaryPath()
	if err != nil {
		return "", "", err
	}
	if _, err := e.State.ObserveMihomoCoreVersion(status.Version, e.now()); err != nil {
		return "", "", err
	}
	return path, status.Version, nil
}

func (e *MihomoExecutor) configValidator(
	binaryPath string,
	exactVersion string,
) (runtimeprocess.ConfigValidator, error) {
	safePaths, err := e.mihomoSafePaths()
	if err != nil {
		return runtimeprocess.ConfigValidator{}, err
	}
	return runtimeprocess.ConfigValidator{
		BinaryPath:   binaryPath,
		DataDir:      e.Process.DataDir,
		ExactVersion: exactVersion,
		SafePaths:    safePaths,
	}, nil
}

func (e *MihomoExecutor) mihomoSafePaths() ([]string, error) {
	root, err := e.State.ManagedResourceRoot()
	if err != nil {
		return nil, err
	}
	return []string{root}, nil
}

func (e *MihomoExecutor) currentListeners() ([]mihomo.ProxyListener, error) {
	path := filepath.Join(e.ConfigRoot, "current", "config.yaml")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("current Mihomo configuration is unavailable")
	}
	config, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return mihomo.ProxyListeners(config)
}

func (e *MihomoExecutor) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

type stagedRuntimeService struct {
	service mihomo.RuntimeService
	report  StageReporter
}

type processConfiguration struct {
	BinaryPath string
	ConfigPath string
	DataDir    string
	SafePaths  []string
}

func captureProcessConfiguration(process *runtimeprocess.Process) processConfiguration {
	if process == nil {
		return processConfiguration{}
	}
	return processConfiguration{
		BinaryPath: process.BinaryPath,
		ConfigPath: process.ConfigPath,
		DataDir:    process.DataDir,
		SafePaths:  append([]string(nil), process.SafePaths...),
	}
}

func (configuration processConfiguration) apply(process *runtimeprocess.Process) {
	if process == nil {
		return
	}
	process.BinaryPath = configuration.BinaryPath
	process.ConfigPath = configuration.ConfigPath
	process.DataDir = configuration.DataDir
	process.SafePaths = append([]string(nil), configuration.SafePaths...)
}

type sourceSwitchRuntimeService struct {
	process  *runtimeprocess.Process
	report   StageReporter
	target   processConfiguration
	previous processConfiguration

	activationCalls int
}

func (s *sourceSwitchRuntimeService) ReloadOrRestart(ctx context.Context) error {
	s.activationCalls++
	stage := "activating_source"
	progress := 70
	configuration := s.target
	if s.activationCalls%2 == 0 {
		stage = "restoring_previous_source"
		progress = 82
		configuration = s.previous
	}
	if err := s.report(stage, progress, false); err != nil {
		return err
	}
	configuration.apply(s.process)
	return s.process.ReloadOrRestart(ctx)
}

func (s *sourceSwitchRuntimeService) Stop(ctx context.Context) error {
	return s.process.Stop(ctx)
}

func exposeSourceManagerError(err error) error {
	var exposed *runtimesource.ManagerError
	if !errors.As(err, &exposed) {
		return err
	}
	return &PublicError{
		Code:      exposed.Code,
		Message:   exposed.Message,
		Retryable: exposed.Retryable,
		Cause:     err,
	}
}

func (s stagedRuntimeService) ReloadOrRestart(ctx context.Context) error {
	if err := s.report("activating_candidate", 70, false); err != nil {
		return err
	}
	return s.service.ReloadOrRestart(ctx)
}

func (s stagedRuntimeService) Stop(ctx context.Context) error {
	if err := s.report("stopping_after_failed_candidate", 85, false); err != nil {
		return err
	}
	return s.service.Stop(ctx)
}

func listenerAddresses(listeners []mihomo.ProxyListener) []string {
	addresses := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		addresses = append(addresses, listener.Address)
	}
	return addresses
}

func importErrorCode(err error) string {
	switch {
	case errors.Is(err, runtimestate.ErrImportExpired):
		return runtimeapi.ErrorContentExpired
	case errors.Is(err, runtimestate.ErrImportConsumed):
		return runtimeapi.ErrorContentConsumed
	case errors.Is(err, runtimestate.ErrImportOwner):
		return runtimeapi.ErrorUnauthorized
	default:
		return runtimeapi.ErrorNotFound
	}
}

func isYAMLContentType(contentType string) bool {
	switch normalizedContentType(contentType) {
	case "application/yaml", "application/x-yaml", "text/yaml":
		return true
	default:
		return false
	}
}

func normalizedContentType(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
}

func validatePreviewCandidate(
	ctx context.Context,
	configRoot string,
	validator runtimeprocess.ConfigValidator,
	candidate []byte,
) error {
	if configRoot == "" || !filepath.IsAbs(configRoot) {
		return errors.New("Runtime configuration root is invalid")
	}
	if err := os.MkdirAll(configRoot, 0700); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(configRoot, ".preview-")
	if err != nil {
		return err
	}
	configPath := filepath.Join(directory, "config.yaml")
	defer func() {
		_ = os.Remove(configPath)
		_ = os.Remove(directory)
	}()
	file, err := os.OpenFile(configPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(candidate); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return validator.ValidateConfig(ctx, configPath)
}
