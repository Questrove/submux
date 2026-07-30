package productupdate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"submux/internal/owneracl"
	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
	"submux/internal/runtimeupdate"
	"submux/internal/safepath"
)

const (
	DefaultPlanTTL       = 30 * time.Minute
	DefaultCheckInterval = 24 * time.Hour
)

var productVersion = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

type Reporter func(stage string, progress int, cancellable bool) error

type ExpectedRuntimeState struct {
	MihomoRunning bool                     `json:"mihomo_running"`
	RunMode       string                   `json:"run_mode"`
	Network       runtimeapi.NetworkStatus `json:"network"`
}

type RollbackPoint struct {
	ID             string                      `json:"id"`
	Version        string                      `json:"version"`
	Database       runtimestate.DatabaseBackup `json:"database"`
	InstallerState json.RawMessage             `json:"installer_state"`
}

type InstallStatus struct {
	Available       bool
	CurrentVersion  string
	PreviousVersion string
}

type Installer interface {
	Status(context.Context) (InstallStatus, error)
	// PrepareRollback may create rollback material, but it must not stop
	// services, replace live programs, migrate the database, or otherwise
	// change the active installation. Implementations must keep the operation
	// idempotent and clean unreferenced rollback material during startup.
	PrepareRollback(
		context.Context,
		runtimeapi.ProductUpdatePlan,
		Package,
		runtimestate.DatabaseBackup,
	) (RollbackPoint, error)
	// DiscardRollback removes rollback material that never became part of a
	// durable update transaction. It must not change the active installation.
	DiscardRollback(context.Context, RollbackPoint) error
	Replace(context.Context, runtimeapi.ProductUpdatePlan, Package, RollbackPoint) error
	Verify(context.Context, runtimeapi.ProductUpdatePlan, RollbackPoint) error
	Rollback(context.Context, RollbackPoint) error
}

type SafetyController interface {
	CaptureExpectedState(context.Context) (ExpectedRuntimeState, error)
	StopForProductUpdate(context.Context, runtimeapi.Operation, Reporter) error
	VerifyProductUpdateHealth(context.Context, runtimeapi.ProductUpdatePlan, Reporter) error
	RestoreExpectedState(
		context.Context,
		runtimeapi.Operation,
		ExpectedRuntimeState,
		Reporter,
	) error
}

type Manager struct {
	Root           string
	Platform       string
	Arch           string
	CurrentVersion string
	InstallationID string
	Trust          *runtimeupdate.Verifier
	State          *runtimestate.Store
	Installer      Installer
	Now            func() time.Time
	PlanTTL        time.Duration
	CheckInterval  time.Duration

	mu sync.Mutex
}

type planRecord struct {
	Plan     runtimeapi.ProductUpdatePlan `json:"plan"`
	Target   runtimeupdate.ProductTarget  `json:"target"`
	Owner    string                       `json:"owner"`
	Consumed bool                         `json:"consumed"`
}

type checkRecord struct {
	CheckedAt        time.Time                    `json:"checked_at"`
	NextCheckAt      time.Time                    `json:"next_check_at"`
	AvailableVersion string                       `json:"available_version,omitempty"`
	Target           *runtimeupdate.ProductTarget `json:"target,omitempty"`
	Error            string                       `json:"error,omitempty"`
}

type activeRollbackRecord struct {
	Point RollbackPoint `json:"point"`
}

type transactionRecord struct {
	Operation runtimeapi.Operation         `json:"operation"`
	Plan      runtimeapi.ProductUpdatePlan `json:"plan"`
	Point     RollbackPoint                `json:"point"`
	Expected  ExpectedRuntimeState         `json:"expected"`
	Phase     string                       `json:"phase"`
}

func (m *Manager) Preview(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.ProductUpdatePreviewRequest,
	offlineBundle []byte,
) (runtimeapi.ProductUpdatePlan, error) {
	if ctx == nil {
		return runtimeapi.ProductUpdatePlan{}, errors.New("Runtime product update preview context is required")
	}
	owner := peer.Key()
	if owner == "" {
		return runtimeapi.ProductUpdatePlan{}, errors.New("Runtime product update preview caller identity is required")
	}
	if err := m.validatePreviewRequest(request, offlineBundle); err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prepare(); err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	if err := m.gcPlansLocked(); err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}

	var target runtimeupdate.ProductTarget
	var packageBody []byte
	var err error
	switch request.Source {
	case runtimeapi.ProductUpdateSourceOnlineTUF:
		target, err = m.Trust.RefreshProductOnline(ctx, m.platform(), m.arch(), request.Version)
	case runtimeapi.ProductUpdateSourceOfflineTUF:
		bundlePath := filepath.Join(m.Root, "offline-preview-"+randomSuffix()+".zip")
		if err = writePrivateFile(bundlePath, offlineBundle, 0600); err != nil {
			return runtimeapi.ProductUpdatePlan{}, err
		}
		defer os.Remove(bundlePath)
		target, packageBody, err = m.Trust.RefreshProductOffline(
			ctx,
			bundlePath,
			m.platform(),
			m.arch(),
			request.Version,
		)
	default:
		return runtimeapi.ProductUpdatePlan{}, errors.New("Runtime product update source is unsupported")
	}
	if err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	schema, err := m.compatibleTarget(target)
	if err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	if !isNewerProductVersion(target.Version, m.CurrentVersion) {
		return runtimeapi.ProductUpdatePlan{}, errors.New("signed Runtime product target is not newer than the installed version")
	}
	available, err := availableDiskBytes(m.Root)
	if err != nil {
		return runtimeapi.ProductUpdatePlan{}, fmt.Errorf("inspect Runtime product update disk space: %w", err)
	}
	if available < uint64(target.RequiredFreeBytes) {
		return runtimeapi.ProductUpdatePlan{}, errors.New("Runtime product update has insufficient free disk space")
	}
	status, err := m.installStatus(ctx)
	if err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	planID, err := randomID("product_plan_")
	if err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	now := m.now()
	plan := runtimeapi.ProductUpdatePlan{
		PlanID:              planID,
		Source:              request.Source,
		Trust:               runtimeapi.ProductUpdateTrustTUF,
		Channel:             runtimeapi.ProductUpdateChannelStable,
		Version:             target.Version,
		CurrentVersion:      valueOr(status.CurrentVersion, m.CurrentVersion),
		PreviousVersion:     status.PreviousVersion,
		Platform:            target.Platform,
		Arch:                target.Arch,
		AssetName:           target.AssetName,
		AssetSize:           target.Length,
		AssetSHA256:         target.SHA256,
		ReleaseNotes:        target.ReleaseNotes,
		Components:          append([]string(nil), target.Components...),
		RequiredFreeBytes:   target.RequiredFreeBytes,
		AvailableFreeBytes:  available,
		NetworkInterruption: "更新会先撤销 TUN、路由和 DNS，使网络恢复直连；随后停止 Mihomo、Runtime 和特权网络进程。",
		Predownloaded:       request.Source == runtimeapi.ProductUpdateSourceOfflineTUF,
		Installable:         status.Available,
		Warning:             "将替换整套 Submux Runtime 产品。操作员确认前不会安装，在线检查不会预下载程序。",
		ExpiresAt:           now.Add(m.planTTL()),
		Migration: runtimeapi.ProductUpdateMigration{
			CurrentSchema:   schema,
			TargetSchema:    target.RuntimeSchemaTarget,
			Required:        schema != target.RuntimeSchemaTarget,
			Reversible:      true,
			Summary:         target.MigrationSummary,
			ProtocolMin:     target.ProtocolMin,
			ProtocolMax:     target.ProtocolMax,
			CurrentProtocol: runtimeapi.ProtocolVersion,
		},
	}
	if !status.Available {
		plan.Warning += " 当前平台安装器尚不可用，因此只能验证和查看计划，不能提交安装。"
	}
	planDir := filepath.Join(m.Root, "plans", planID)
	if err := preparePrivateDir(planDir); err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	removePlan := true
	defer func() {
		if removePlan {
			_ = os.RemoveAll(planDir)
		}
	}()
	if len(packageBody) > 0 {
		if err := m.Trust.VerifyProductTarget(target, packageBody); err != nil {
			return runtimeapi.ProductUpdatePlan{}, err
		}
		if _, err := ParsePackage(packageBody, target); err != nil {
			return runtimeapi.ProductUpdatePlan{}, err
		}
		if err := writePrivateFile(filepath.Join(planDir, "package.zip"), packageBody, 0600); err != nil {
			return runtimeapi.ProductUpdatePlan{}, err
		}
	}
	if err := writeJSONAtomic(
		filepath.Join(planDir, "plan.json"),
		planRecord{Plan: plan, Target: target, Owner: owner},
	); err != nil {
		return runtimeapi.ProductUpdatePlan{}, err
	}
	removePlan = false
	return plan, nil
}

func (m *Manager) CheckStable(ctx context.Context) (runtimeapi.OperationResult, error) {
	if ctx == nil {
		return runtimeapi.OperationResult{}, errors.New("Runtime product update check context is required")
	}
	if m == nil || m.Trust == nil || len(m.Trust.InitialRoot) == 0 || m.State == nil {
		return runtimeapi.OperationResult{}, errors.New("Runtime product update check service is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prepare(); err != nil {
		return runtimeapi.OperationResult{}, err
	}
	now := m.now()
	target, err := m.Trust.RefreshProductOnline(ctx, m.platform(), m.arch(), "")
	record := checkRecord{
		CheckedAt:   now,
		NextCheckAt: m.nextCheck(now),
	}
	if err == nil {
		_, err = m.compatibleTarget(target)
	}
	if err == nil {
		record.Target = &target
		if isNewerProductVersion(target.Version, m.CurrentVersion) {
			record.AvailableVersion = target.Version
		}
	} else {
		record.Error = publicCheckError(err)
	}
	if writeErr := writeJSONAtomic(filepath.Join(m.Root, "check.json"), record); writeErr != nil {
		return runtimeapi.OperationResult{}, writeErr
	}
	if err != nil {
		return runtimeapi.OperationResult{}, err
	}
	return runtimeapi.OperationResult{
		Verified:       true,
		RuntimeVersion: record.AvailableVersion,
		Trust:          runtimeapi.ProductUpdateTrustTUF,
	}, nil
}

func (m *Manager) Due(now time.Time) (bool, error) {
	if m == nil || m.Trust == nil {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prepare(); err != nil {
		return false, err
	}
	var record checkRecord
	if err := readJSON(filepath.Join(m.Root, "check.json"), &record); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return !now.UTC().Before(record.NextCheckAt), nil
}

func (m *Manager) Status(ctx context.Context) (runtimeapi.UpdateStatus, error) {
	if m == nil {
		return runtimeapi.UpdateStatus{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prepare(); err != nil {
		return runtimeapi.UpdateStatus{}, err
	}
	install, err := m.installStatus(ctx)
	if err != nil {
		return runtimeapi.UpdateStatus{}, err
	}
	status := runtimeapi.UpdateStatus{
		RuntimeAvailable:          install.Available && m.Trust != nil && len(m.Trust.InitialRoot) > 0,
		RuntimeCurrentVersion:     valueOr(install.CurrentVersion, m.CurrentVersion),
		RuntimePreviousVersion:    install.PreviousVersion,
		RuntimePredownloadEnabled: false,
	}
	var record checkRecord
	if err := readJSON(filepath.Join(m.Root, "check.json"), &record); err == nil {
		status.RuntimeAvailableVersion = record.AvailableVersion
		status.RuntimeLastCheckedAt = &record.CheckedAt
		status.RuntimeNextCheckAt = &record.NextCheckAt
	} else if !errors.Is(err, os.ErrNotExist) {
		return runtimeapi.UpdateStatus{}, err
	}
	return status, nil
}

func (m *Manager) Activate(
	ctx context.Context,
	operation runtimeapi.Operation,
	safety SafetyController,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	if ctx == nil || safety == nil || report == nil {
		return nil, errors.New("Runtime product update activation dependencies are incomplete")
	}
	if !operation.Action.Params.Confirm ||
		operation.Action.Params.Trust != runtimeapi.ProductUpdateTrustTUF {
		return nil, errors.New("Runtime product update requires explicit TUF confirmation")
	}
	if m == nil || m.Installer == nil || m.Trust == nil || m.State == nil {
		return nil, errors.New("Runtime product update activation service is unavailable")
	}
	if !validID(operation.ID, "op_") {
		return nil, errors.New("Runtime product update Operation ID is invalid")
	}
	m.mu.Lock()
	if err := m.prepare(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	record, packageBody, err := m.consumePlanLocked(
		operation.Action.Params.PlanID,
		operation.CallerIdentity,
	)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := report("downloading_verified_product", 10, true); err != nil {
		return nil, err
	}
	if len(packageBody) == 0 {
		packageBody, err = m.Trust.DownloadProduct(ctx, record.Target)
		if err != nil {
			return nil, err
		}
	}
	if err := m.Trust.VerifyProductTarget(record.Target, packageBody); err != nil {
		return nil, err
	}
	product, err := ParsePackage(packageBody, record.Target)
	if err != nil {
		return nil, err
	}
	if _, err := m.compatibleTarget(record.Target); err != nil {
		return nil, err
	}
	available, err := availableDiskBytes(m.Root)
	if err != nil {
		return nil, err
	}
	if available < uint64(record.Target.RequiredFreeBytes) {
		return nil, errors.New("Runtime product update no longer has sufficient free disk space")
	}
	expected, err := safety.CaptureExpectedState(ctx)
	if err != nil {
		return nil, err
	}
	if err := report("creating_program_and_database_rollback_point", 22, false); err != nil {
		return nil, err
	}
	rollbackRoot := filepath.Join(m.Root, "rollback", operation.ID)
	if err := preparePrivateDir(rollbackRoot); err != nil {
		return nil, err
	}
	database, err := m.State.BackupDatabase(filepath.Join(rollbackRoot, "runtime.db"))
	if err != nil {
		return nil, err
	}
	point, err := m.Installer.PrepareRollback(ctx, record.Plan, product, database)
	if err != nil {
		return nil, fmt.Errorf("prepare Runtime product rollback point: %w", err)
	}
	transaction := transactionRecord{
		Operation: operation,
		Plan:      record.Plan,
		Point:     point,
		Expected:  expected,
		Phase:     "prepared",
	}
	if err := m.persistTransaction(transaction); err != nil {
		discardErr := m.Installer.DiscardRollback(context.Background(), point)
		removeErr := os.RemoveAll(rollbackRoot)
		return nil, errors.Join(
			fmt.Errorf("persist Runtime product update recovery transaction: %w", err),
			discardErr,
			removeErr,
		)
	}
	rollback := func(cause error) (*runtimeapi.OperationResult, error) {
		_ = report("rolling_back_product_update", 92, false)
		rollbackErr := m.Installer.Rollback(context.Background(), point)
		var restoreErr error
		if rollbackErr == nil {
			restoreErr = safety.RestoreExpectedState(
				context.Background(),
				operation,
				expected,
				func(string, int, bool) error { return nil },
			)
		}
		outcome := errors.New("Runtime product update failed; old programs, database, and expected running state were restored")
		if rollbackErr != nil || restoreErr != nil {
			outcome = errors.New("Runtime product update failed and automatic rollback needs operator attention")
		} else if removeErr := m.removeTransaction(); removeErr != nil {
			outcome = errors.New("Runtime product update was rolled back but its recovery transaction still needs cleanup")
			restoreErr = errors.Join(restoreErr, removeErr)
		}
		return nil, errors.Join(outcome, cause, rollbackErr, restoreErr)
	}
	if err := safety.StopForProductUpdate(ctx, operation, report); err != nil {
		return rollback(err)
	}
	if err := report("replacing_runtime_product", 58, false); err != nil {
		return rollback(err)
	}
	if err := m.Installer.Replace(ctx, record.Plan, product, point); err != nil {
		return rollback(err)
	}
	if err := report("verifying_database_ipc_and_programs", 78, false); err != nil {
		return rollback(err)
	}
	if err := m.Installer.Verify(ctx, record.Plan, point); err != nil {
		return rollback(err)
	}
	if err := safety.VerifyProductUpdateHealth(ctx, record.Plan, report); err != nil {
		return rollback(err)
	}
	if err := safety.RestoreExpectedState(ctx, operation, expected, report); err != nil {
		return rollback(err)
	}
	status, err := m.Installer.Status(ctx)
	if err != nil {
		return rollback(err)
	}
	transaction.Phase = "committed"
	if err := m.persistTransaction(transaction); err != nil {
		return rollback(err)
	}
	rollbackState := "available"
	if err := m.persistActiveRollback(point); err != nil {
		rollbackState = "pending_recovery"
	} else {
		_ = m.removeTransaction()
	}
	_ = os.RemoveAll(filepath.Join(m.Root, "plans", record.Plan.PlanID))
	return &runtimeapi.OperationResult{
		Verified:               status.CurrentVersion == record.Plan.Version,
		RuntimeVersion:         status.CurrentVersion,
		PreviousRuntimeVersion: status.PreviousVersion,
		ProductRollback:        rollbackState,
		Trust:                  runtimeapi.ProductUpdateTrustTUF,
	}, nil
}

func (m *Manager) Rollback(
	ctx context.Context,
	operation runtimeapi.Operation,
	safety SafetyController,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	if ctx == nil || safety == nil || report == nil || !operation.Action.Params.Confirm {
		return nil, errors.New("Runtime product rollback requires explicit confirmation")
	}
	if m == nil || m.Installer == nil || m.State == nil {
		return nil, errors.New("Runtime product rollback service is unavailable")
	}
	m.mu.Lock()
	if err := m.prepare(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	var active activeRollbackRecord
	if err := readJSON(filepath.Join(m.Root, "active-rollback.json"), &active); err != nil {
		m.mu.Unlock()
		return nil, errors.New("no verified Runtime product rollback point is available")
	}
	m.mu.Unlock()
	expected, err := safety.CaptureExpectedState(ctx)
	if err != nil {
		return nil, err
	}
	if err := safety.StopForProductUpdate(ctx, operation, report); err != nil {
		return nil, err
	}
	if err := report("rolling_back_runtime_product", 55, false); err != nil {
		return nil, err
	}
	if err := m.Installer.Rollback(ctx, active.Point); err != nil {
		return nil, err
	}
	if err := safety.VerifyProductUpdateHealth(ctx, runtimeapi.ProductUpdatePlan{}, report); err != nil {
		return nil, err
	}
	if err := safety.RestoreExpectedState(ctx, operation, expected, report); err != nil {
		return nil, err
	}
	status, err := m.Installer.Status(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(m.Root, "active-rollback.json")); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		Verified:               status.Available,
		RuntimeVersion:         status.CurrentVersion,
		PreviousRuntimeVersion: status.PreviousVersion,
		ProductRollback:        "completed",
		Trust:                  runtimeapi.ProductUpdateTrustTUF,
	}, nil
}

func (m *Manager) RecoverIncomplete(
	ctx context.Context,
	safety SafetyController,
	report Reporter,
) error {
	if ctx == nil || safety == nil || report == nil {
		return errors.New("Runtime product update recovery dependencies are incomplete")
	}
	if m == nil || m.Installer == nil || m.State == nil {
		return nil
	}
	m.mu.Lock()
	if err := m.prepare(); err != nil {
		m.mu.Unlock()
		return err
	}
	var transaction transactionRecord
	err := readJSON(filepath.Join(m.Root, "transaction.json"), &transaction)
	m.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Runtime product update recovery transaction: %w", err)
	}
	if transaction.Phase == "committed" {
		if err := m.persistActiveRollback(transaction.Point); err != nil {
			return fmt.Errorf("recover Runtime product rollback metadata: %w", err)
		}
		return m.removeTransaction()
	}
	if transaction.Phase != "prepared" ||
		!validID(transaction.Operation.ID, "op_") ||
		transaction.Point.ID == "" {
		return errors.New("Runtime product update recovery transaction is invalid")
	}
	if err := report("recovering_incomplete_product_update", 10, false); err != nil {
		return err
	}
	if err := safety.StopForProductUpdate(ctx, transaction.Operation, report); err != nil {
		return errors.Join(
			errors.New("incomplete Runtime product update could not enter fail-open recovery"),
			err,
		)
	}
	rollbackErr := m.Installer.Rollback(ctx, transaction.Point)
	var restoreErr error
	if rollbackErr == nil {
		restoreErr = safety.RestoreExpectedState(
			ctx,
			transaction.Operation,
			transaction.Expected,
			report,
		)
	}
	if rollbackErr != nil || restoreErr != nil {
		return errors.Join(
			errors.New("incomplete Runtime product update needs operator attention"),
			rollbackErr,
			restoreErr,
		)
	}
	return m.removeTransaction()
}

func (m *Manager) validatePreviewRequest(
	request runtimeapi.ProductUpdatePreviewRequest,
	offlineBundle []byte,
) error {
	if m == nil || m.Trust == nil || len(m.Trust.InitialRoot) == 0 || m.State == nil {
		return errors.New("Runtime product update service is unavailable")
	}
	if request.Source != runtimeapi.ProductUpdateSourceOnlineTUF &&
		request.Source != runtimeapi.ProductUpdateSourceOfflineTUF {
		return errors.New("Runtime product update source must be online_tuf or offline_tuf")
	}
	if request.Source == runtimeapi.ProductUpdateSourceOnlineTUF {
		if request.ContentID != "" || len(offlineBundle) != 0 {
			return errors.New("online Runtime product update does not accept imported content")
		}
	} else if request.ContentID == "" || len(offlineBundle) == 0 ||
		len(offlineBundle) > runtimeapi.RuntimeProductUpdateMaxBytes {
		return errors.New("offline Runtime product update requires a bounded imported TUF bundle")
	}
	if request.Version != "" && !productVersion.MatchString(request.Version) {
		return errors.New("Runtime product update version must be an exact stable vX.Y.Z version")
	}
	return nil
}

func (m *Manager) compatibleTarget(target runtimeupdate.ProductTarget) (int, error) {
	if target.Channel != runtimeapi.ProductUpdateChannelStable ||
		target.Platform != m.platform() ||
		target.Arch != m.arch() {
		return 0, errors.New("signed Runtime product target is incompatible with this platform")
	}
	if runtimeapi.ProtocolVersion < target.ProtocolMin ||
		runtimeapi.ProtocolVersion > target.ProtocolMax {
		return 0, errors.New("signed Runtime product target is incompatible with this IPC protocol")
	}
	schema, err := m.State.SchemaVersion()
	if err != nil {
		return 0, err
	}
	if schema < target.RuntimeSchemaMin || schema > target.RuntimeSchemaMax {
		return 0, errors.New("signed Runtime product target cannot migrate this database schema")
	}
	return schema, nil
}

func (m *Manager) consumePlanLocked(planID, owner string) (planRecord, []byte, error) {
	if !validID(planID, "product_plan_") || owner == "" {
		return planRecord{}, nil, errors.New("Runtime product update plan is invalid")
	}
	name := filepath.Join(m.Root, "plans", planID, "plan.json")
	var record planRecord
	if err := readJSON(name, &record); err != nil {
		return planRecord{}, nil, errors.New("Runtime product update plan is unavailable")
	}
	if record.Owner != owner ||
		record.Consumed ||
		record.Plan.PlanID != planID ||
		record.Plan.Trust != runtimeapi.ProductUpdateTrustTUF ||
		!record.Plan.ExpiresAt.After(m.now()) {
		return planRecord{}, nil, errors.New("Runtime product update plan is unavailable, expired, consumed, or belongs to another caller")
	}
	record.Consumed = true
	if err := writeJSONAtomic(name, record); err != nil {
		return planRecord{}, nil, err
	}
	var packageBody []byte
	packagePath := filepath.Join(m.Root, "plans", planID, "package.zip")
	if body, err := os.ReadFile(packagePath); err == nil {
		packageBody = body
	} else if !errors.Is(err, os.ErrNotExist) {
		return planRecord{}, nil, err
	}
	return record, packageBody, nil
}

func (m *Manager) persistActiveRollback(point RollbackPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeJSONAtomic(
		filepath.Join(m.Root, "active-rollback.json"),
		activeRollbackRecord{Point: point},
	)
}

func (m *Manager) persistTransaction(record transactionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeJSONAtomic(filepath.Join(m.Root, "transaction.json"), record)
}

func (m *Manager) removeTransaction() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := os.Remove(filepath.Join(m.Root, "transaction.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (m *Manager) installStatus(ctx context.Context) (InstallStatus, error) {
	if m.Installer == nil {
		return InstallStatus{CurrentVersion: m.CurrentVersion}, nil
	}
	return m.Installer.Status(ctx)
}

func (m *Manager) prepare() error {
	if m == nil || m.Root == "" || !filepath.IsAbs(m.Root) {
		return errors.New("Runtime product update root must be fixed and absolute")
	}
	for _, name := range []string{m.Root, filepath.Join(m.Root, "plans"), filepath.Join(m.Root, "rollback")} {
		if err := preparePrivateDir(name); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) gcPlansLocked() error {
	entries, err := os.ReadDir(filepath.Join(m.Root, "plans"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			!validID(entry.Name(), "product_plan_") {
			return errors.New("Runtime product update plan root contains an unmanaged entry")
		}
		var record planRecord
		name := filepath.Join(m.Root, "plans", entry.Name(), "plan.json")
		if err := readJSON(name, &record); err != nil {
			return err
		}
		if !record.Plan.ExpiresAt.After(m.now()) {
			if err := os.RemoveAll(filepath.Join(m.Root, "plans", entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) nextCheck(now time.Time) time.Time {
	interval := m.CheckInterval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	sum := sha256.Sum256([]byte(m.InstallationID + "|" + now.UTC().Format("2006-01-02")))
	window := int64(2 * time.Hour)
	jitter := time.Duration(int64(binary.BigEndian.Uint64(sum[:8])%uint64(window)) - int64(time.Hour))
	return now.UTC().Add(interval + jitter)
}

func (m *Manager) planTTL() time.Duration {
	if m.PlanTTL > 0 {
		return m.PlanTTL
	}
	return DefaultPlanTTL
}

func (m *Manager) platform() string {
	return strings.ToLower(strings.TrimSpace(m.Platform))
}

func (m *Manager) arch() string {
	return strings.ToLower(strings.TrimSpace(m.Arch))
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func isNewerProductVersion(candidate, current string) bool {
	candidateParts := productVersion.FindStringSubmatch(candidate)
	currentParts := productVersion.FindStringSubmatch(current)
	if len(candidateParts) == 0 {
		return false
	}
	if len(currentParts) == 0 {
		return true
	}
	for index := 1; index <= 3; index++ {
		left, _ := strconv.ParseUint(candidateParts[index], 10, 64)
		right, _ := strconv.ParseUint(currentParts[index], 10, 64)
		if left != right {
			return left > right
		}
	}
	return false
}

func validID(value, prefix string) bool {
	suffix, ok := strings.CutPrefix(value, prefix)
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func randomID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}

func randomSuffix() string {
	id, err := randomID("")
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return id
}

func preparePrivateDir(name string) error {
	if err := os.MkdirAll(name, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runtime product update path must be a real directory")
	}
	linked, err := safepath.ContainsLink(name)
	if err != nil {
		return err
	}
	if linked {
		return errors.New("Runtime product update path must not contain symbolic or reparse links")
	}
	if err := owneracl.RestrictDirectory(name); err != nil {
		return err
	}
	return os.Chmod(name, 0700)
}

func writePrivateFile(name string, body []byte, mode os.FileMode) error {
	if len(body) == 0 {
		return errors.New("Runtime product update file is empty")
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(name)
		}
	}()
	if err := owneracl.RestrictFile(name); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

func writeJSONAtomic(name string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	parent := filepath.Dir(name)
	if err := preparePrivateDir(parent); err != nil {
		return err
	}
	temp, err := os.CreateTemp(parent, ".product-state-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := owneracl.RestrictFile(tempName); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceStateFile(tempName, name)
}

func readJSON(name string, target any) error {
	body, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("Runtime product update state contains trailing data")
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func publicCheckError(_ error) string {
	return "stable Runtime product update check failed"
}
