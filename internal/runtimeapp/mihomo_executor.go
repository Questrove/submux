package runtimeapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimecore"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
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
	Now             func() time.Time
}

func (e *MihomoExecutor) Execute(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if e == nil || e.State == nil || e.Core == nil || e.Process == nil || e.Verifier == nil {
		return nil, errors.New("Mihomo Runtime executor is incomplete")
	}
	switch operation.Action.Kind {
	case runtimeapi.ActionApplyImportedConfig:
		return e.applyImport(ctx, operation, report)
	case runtimeapi.ActionStartProxy:
		return e.start(ctx, report)
	case runtimeapi.ActionStopProxy:
		return e.stop(ctx, report)
	default:
		return nil, fmt.Errorf("unsupported Runtime action %q", operation.Action.Kind)
	}
}

func (e *MihomoExecutor) Verify(ctx context.Context) (runtimeapi.ProxyVerification, error) {
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
	if err := report("validating_candidate", 25, true); err != nil {
		return nil, err
	}
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
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	builder := mihomo.ExplicitCandidateBuilder{
		Port:            e.ProxyPort,
		ControlEndpoint: e.ControlEndpoint,
		Platform:        e.Platform,
	}
	candidate, err := builder.BuildCandidate(body)
	if err != nil {
		return nil, &PublicError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: "The imported Mihomo configuration is outside the Runtime safety policy",
			Cause:   err,
		}
	}
	listeners, err := mihomo.ProxyListeners(candidate)
	if err != nil {
		return nil, err
	}
	if !running {
		if err := runtimeprocess.CheckLoopbackPortsAvailable(listenerAddresses(listeners)); err != nil {
			return nil, &PublicError{
				Code:    runtimeapi.ErrorServiceUnavailable,
				Message: err.Error(),
				Cause:   err,
			}
		}
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
		Validator: runtimeprocess.ConfigValidator{
			BinaryPath:   binaryPath,
			DataDir:      e.Process.DataDir,
			ExactVersion: exactVersion,
		},
		Service:  service,
		Verifier: e.Verifier,
	}
	deployment, err := deployer.Apply(ctx, operation.ID, content.SHA256, body)
	if err != nil {
		return nil, err
	}
	if err := report("verified", 95, false); err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		ConfigRevision: deployment.Revision,
		ProxyKind:      deployment.ProxyKind,
		ProxyAddresses: append([]string(nil), deployment.ProxyAddresses...),
		Verified:       true,
	}, nil
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

func (e *MihomoExecutor) stop(ctx context.Context, report StageReporter) (*runtimeapi.OperationResult, error) {
	if err := report("stopping_proxy", 60, false); err != nil {
		return nil, err
	}
	if err := e.Process.Stop(ctx); err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{Verified: true}, nil
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
	return path, status.Version, nil
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
