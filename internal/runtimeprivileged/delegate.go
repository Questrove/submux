package runtimeprivileged

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimenet"
	"submux/internal/runtimeprocess"
	"submux/internal/safepath"
)

const monitorInterval = time.Second

const maximumFixedObjectSize = int64(1 << 30)

type CoreController interface {
	StageCore(context.Context, string, runtimenet.PrivilegedCoreStage) (runtimenet.PrivilegedCoreStatus, error)
	StartCore(context.Context, string, string) (runtimenet.PrivilegedCoreStatus, error)
	StopCore(context.Context, string, string) (runtimenet.PrivilegedCoreStatus, error)
	ObserveCore(context.Context, string) (runtimenet.PrivilegedCoreStatus, error)
}

type Delegate struct {
	Controller  CoreController
	RuntimeRoot string

	mu            sync.Mutex
	exits         chan runtimeprocess.ExitEvent
	runID         uint64
	startedAt     time.Time
	monitorCancel context.CancelFunc
}

func NewDelegate(controller CoreController, runtimeRoot string) (*Delegate, error) {
	if controller == nil {
		return nil, errors.New("privileged Mihomo controller is required")
	}
	root, err := filepath.Abs(runtimeRoot)
	if err != nil || root == filepath.VolumeName(root)+string(filepath.Separator) {
		return nil, errors.New("privileged Mihomo Runtime root is invalid")
	}
	linked, err := safepath.ContainsLinkInExistingPath(root)
	if err != nil {
		return nil, fmt.Errorf("inspect privileged Mihomo Runtime root: %w", err)
	}
	if linked {
		return nil, errors.New("privileged Mihomo Runtime root must not contain links")
	}
	return &Delegate{
		Controller:  controller,
		RuntimeRoot: root,
		exits:       make(chan runtimeprocess.ExitEvent, 64),
	}, nil
}

func (delegate *Delegate) IsRunning(ctx context.Context) (bool, error) {
	if delegate == nil || delegate.Controller == nil {
		return false, errors.New("privileged Mihomo delegate is unavailable")
	}
	if ctx == nil {
		return false, errors.New("privileged Mihomo context is required")
	}
	status, err := delegate.Controller.ObserveCore(ctx, runtimenet.PrivilegedCoreObjectMihomo)
	if err != nil {
		return false, err
	}
	return status.State == "running", nil
}

func (delegate *Delegate) Start(
	ctx context.Context,
	specification runtimeprocess.Specification,
) error {
	if ctx == nil {
		return errors.New("privileged Mihomo context is required")
	}
	stage, err := delegate.stageRequest(specification)
	if err != nil {
		return err
	}
	status, err := delegate.Controller.ObserveCore(ctx, runtimenet.PrivilegedCoreObjectMihomo)
	if err != nil {
		return err
	}
	if status.State == "running" {
		if status.CoreSHA256 != stage.CoreSHA256 ||
			status.ConfigSHA256 != stage.ConfigSHA256 ||
			status.DataObjectID != stage.DataObjectID {
			return errors.New("privileged Mihomo is running a different verified object")
		}
		delegate.beginMonitor(status)
		return nil
	}
	stageOperation, err := privilegedOperationID("stage")
	if err != nil {
		return err
	}
	if _, err := delegate.Controller.StageCore(ctx, stageOperation, stage); err != nil {
		return err
	}
	startOperation, err := privilegedOperationID("start")
	if err != nil {
		return err
	}
	status, err = delegate.Controller.StartCore(
		ctx,
		startOperation,
		runtimenet.PrivilegedCoreObjectMihomo,
	)
	if err != nil {
		return err
	}
	if status.State != "running" {
		return fmt.Errorf("privileged Mihomo entered unexpected state %q", status.State)
	}
	delegate.beginMonitor(status)
	return nil
}

func (delegate *Delegate) Stop(ctx context.Context) error {
	if delegate == nil || delegate.Controller == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("privileged Mihomo context is required")
	}
	running, err := delegate.IsRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		delegate.cancelMonitor()
		return nil
	}
	operationID, err := privilegedOperationID("stop")
	if err != nil {
		return err
	}
	status, err := delegate.Controller.StopCore(
		ctx,
		operationID,
		runtimenet.PrivilegedCoreObjectMihomo,
	)
	if err != nil {
		return err
	}
	if status.State == "running" {
		return errors.New("privileged Mihomo did not stop")
	}
	delegate.finishMonitor(nil, true)
	return nil
}

func (delegate *Delegate) ReloadOrRestart(
	ctx context.Context,
	specification runtimeprocess.Specification,
) error {
	if ctx == nil {
		return errors.New("privileged Mihomo context is required")
	}
	if err := delegate.Stop(ctx); err != nil {
		return err
	}
	return delegate.Start(ctx, specification)
}

func (delegate *Delegate) ExitEvents() <-chan runtimeprocess.ExitEvent {
	if delegate == nil {
		return nil
	}
	return delegate.exits
}

func (delegate *Delegate) stageRequest(
	specification runtimeprocess.Specification,
) (runtimenet.PrivilegedCoreStage, error) {
	corePath := filepath.Join(delegate.RuntimeRoot, "core", "current", "mihomo")
	configPath := filepath.Join(delegate.RuntimeRoot, "config", "current", "config.yaml")
	if !sameCleanPath(specification.BinaryPath, corePath) {
		return runtimenet.PrivilegedCoreStage{}, errors.New("privileged Mihomo core path is not the fixed Runtime object")
	}
	if !sameCleanPath(specification.ConfigPath, configPath) {
		return runtimenet.PrivilegedCoreStage{}, errors.New("privileged Mihomo config path is not the fixed Runtime object")
	}
	dataObjectID, err := delegate.dataObjectID(specification.DataDir)
	if err != nil {
		return runtimenet.PrivilegedCoreStage{}, err
	}
	coreDigest, err := hashFixedRegularFile(corePath)
	if err != nil {
		return runtimenet.PrivilegedCoreStage{}, fmt.Errorf("hash privileged Mihomo core: %w", err)
	}
	configDigest, err := hashFixedRegularFile(configPath)
	if err != nil {
		return runtimenet.PrivilegedCoreStage{}, fmt.Errorf("hash privileged Mihomo config: %w", err)
	}
	return runtimenet.PrivilegedCoreStage{
		ObjectID:     runtimenet.PrivilegedCoreObjectMihomo,
		CoreSHA256:   coreDigest,
		ConfigSHA256: configDigest,
		DataObjectID: dataObjectID,
	}, nil
}

func (delegate *Delegate) dataObjectID(path string) (string, error) {
	dataRoot := filepath.Join(delegate.RuntimeRoot, "mihomo-data")
	linked, err := safepath.ContainsLinkInExistingPath(path)
	if err != nil {
		return "", fmt.Errorf("inspect privileged Mihomo data directory: %w", err)
	}
	if linked {
		return "", errors.New("privileged Mihomo data directory must not contain links")
	}
	if sameCleanPath(path, dataRoot) {
		return "", nil
	}
	sourcesRoot := filepath.Join(dataRoot, "sources")
	relative, err := filepath.Rel(sourcesRoot, filepath.Clean(path))
	if err != nil ||
		relative == "." ||
		filepath.IsAbs(relative) ||
		strings.Contains(relative, string(filepath.Separator)) ||
		!validObjectID(relative) {
		return "", errors.New("privileged Mihomo data directory is not a fixed Runtime object")
	}
	return relative, nil
}

func (delegate *Delegate) beginMonitor(status runtimenet.PrivilegedCoreStatus) {
	delegate.mu.Lock()
	if delegate.monitorCancel != nil {
		delegate.monitorCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	delegate.monitorCancel = cancel
	delegate.runID++
	runID := delegate.runID
	startedAt := status.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	delegate.startedAt = startedAt
	delegate.mu.Unlock()
	go delegate.monitor(ctx, runID, startedAt)
}

func (delegate *Delegate) monitor(ctx context.Context, runID uint64, startedAt time.Time) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			status, err := delegate.Controller.ObserveCore(
				probeContext,
				runtimenet.PrivilegedCoreObjectMihomo,
			)
			cancel()
			if err != nil {
				failures++
				if failures < 3 {
					continue
				}
				delegate.finishRun(runID, startedAt, err, false)
				return
			}
			failures = 0
			if status.State != "running" {
				delegate.finishRun(
					runID,
					startedAt,
					fmt.Errorf("privileged Mihomo exited in state %q", status.State),
					false,
				)
				return
			}
		}
	}
}

func (delegate *Delegate) cancelMonitor() {
	delegate.mu.Lock()
	defer delegate.mu.Unlock()
	if delegate.monitorCancel != nil {
		delegate.monitorCancel()
		delegate.monitorCancel = nil
	}
}

func (delegate *Delegate) finishMonitor(err error, intentional bool) {
	delegate.mu.Lock()
	runID := delegate.runID
	startedAt := delegate.startedAt
	delegate.mu.Unlock()
	delegate.finishRun(runID, startedAt, err, intentional)
}

func (delegate *Delegate) finishRun(
	runID uint64,
	startedAt time.Time,
	err error,
	intentional bool,
) {
	delegate.mu.Lock()
	if runID == 0 || runID != delegate.runID {
		delegate.mu.Unlock()
		return
	}
	if delegate.monitorCancel != nil {
		delegate.monitorCancel()
		delegate.monitorCancel = nil
	}
	delegate.runID = 0
	delegate.startedAt = time.Time{}
	exits := delegate.exits
	delegate.mu.Unlock()
	event := runtimeprocess.ExitEvent{
		RunID:       runID,
		StartedAt:   startedAt,
		ExitedAt:    time.Now().UTC(),
		Err:         err,
		Intentional: intentional,
	}
	select {
	case exits <- event:
	default:
	}
}

func hashFixedRegularFile(path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("object must be a regular non-linked file")
	}
	if before.Size() < 0 || before.Size() > maximumFixedObjectSize {
		return "", errors.New("object exceeds the fixed object size limit")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return "", err
	}
	if linked {
		return "", errors.New("object path must not contain links")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return "", errors.New("object changed while opening")
	}
	if opened.Size() != before.Size() || opened.Size() > maximumFixedObjectSize {
		return "", errors.New("object size changed while opening")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return "", err
	}
	if written != opened.Size() {
		return "", errors.New("object changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func sameCleanPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func privilegedOperationID(kind string) (string, error) {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return "priv_" + kind + "_" + hex.EncodeToString(body), nil
}

func validObjectID(value string) bool {
	if len(value) < 3 || len(value) > 96 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' ||
			character == '-' {
			continue
		}
		return false
	}
	return true
}
