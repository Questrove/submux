package runtimeapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"submux/internal/mihomo"
	"submux/internal/runtimecore"
	"submux/internal/runtimeprocess"
)

type CoreCandidateVerifier struct {
	ConfigPath string
	DataDir    string
	SafePaths  []string
}

func (v CoreCandidateVerifier) VerifyBinary(
	ctx context.Context,
	binaryPath string,
	exactVersion string,
) error {
	return v.VerifyCandidateCore(ctx, binaryPath, exactVersion)
}

func (v CoreCandidateVerifier) VerifyCandidateCore(
	ctx context.Context,
	binaryPath string,
	exactVersion string,
) error {
	if ctx == nil {
		return errors.New("candidate Mihomo core verification context is required")
	}
	info, err := os.Lstat(v.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return (runtimecore.CommandVerifier{}).VerifyBinary(ctx, binaryPath, exactVersion)
	}
	if err != nil {
		return fmt.Errorf("inspect current candidate configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("current candidate configuration is not a regular non-linked file")
	}
	return (runtimeprocess.ConfigValidator{
		BinaryPath:   binaryPath,
		DataDir:      v.DataDir,
		ExactVersion: exactVersion,
		SafePaths:    append([]string(nil), v.SafePaths...),
	}).ValidateConfig(ctx, v.ConfigPath)
}

type CoreActivation struct {
	Process    *runtimeprocess.Process
	Verifier   mihomo.RuntimeVerifier
	ConfigPath string
	Timeout    time.Duration
	Poll       time.Duration
}

func (a *CoreActivation) IsRunning(ctx context.Context) (bool, error) {
	if a == nil || a.Process == nil {
		return false, errors.New("Mihomo core activation process is unavailable")
	}
	return a.Process.IsRunning(ctx)
}

func (a *CoreActivation) Stop(ctx context.Context) error {
	if a == nil || a.Process == nil {
		return errors.New("Mihomo core activation process is unavailable")
	}
	return a.Process.Stop(ctx)
}

func (a *CoreActivation) Start(ctx context.Context) error {
	if a == nil || a.Process == nil {
		return errors.New("Mihomo core activation process is unavailable")
	}
	return a.Process.Start(ctx)
}

func (a *CoreActivation) VerifyActivation(ctx context.Context) error {
	if a == nil || a.Process == nil || a.Verifier == nil {
		return errors.New("Mihomo core activation verifier is unavailable")
	}
	body, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		return fmt.Errorf("read activated Mihomo configuration: %w", err)
	}
	listeners, err := mihomo.ProxyListeners(body)
	if err != nil {
		return err
	}
	if len(listeners) == 0 {
		return errors.New("activated Mihomo configuration has no explicit proxy listener")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	poll := a.Poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	verifyContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		allHealthy := true
		for _, listener := range listeners {
			if err := a.Verifier.VerifyRuntime(verifyContext, listener.Address); err != nil {
				lastErr = err
				allHealthy = false
				break
			}
		}
		if allHealthy {
			return nil
		}
		select {
		case <-verifyContext.Done():
			return fmt.Errorf("Mihomo immediate health verification failed: %w", errors.Join(lastErr, verifyContext.Err()))
		case <-time.After(poll):
		}
	}
}
