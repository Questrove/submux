package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"submux/internal/agentstate"
)

type coreAutoStartAttempt struct {
	restored bool
	retry    bool
	err      error
}

func coreAutoStartRetryDelays() []time.Duration {
	return []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
}

func (d *Daemon) restoreCoreOnStartup(ctx context.Context, retryDelays []time.Duration) error {
	if len(retryDelays) == 0 {
		return nil
	}
	var lastErr error
	for attempt, delay := range retryDelays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		d.mutationMu.Lock()
		result := d.tryRestoreCoreOnStartup(ctx)
		d.mutationMu.Unlock()
		if result.err == nil {
			if result.restored {
				d.writeRuntimeLog("automatic Mihomo startup succeeded on attempt %d/%d", attempt+1, len(retryDelays))
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = result.err
		d.writeRuntimeLog("automatic Mihomo startup attempt %d/%d failed: %s", attempt+1, len(retryDelays), safeOperationError(result.err))
		if !result.retry {
			message := "Mihomo automatic startup failed: " + safeOperationError(result.err)
			recordErr := d.recordCoreAutoStartFailure(message)
			return errors.Join(fmt.Errorf("automatic Mihomo startup failed: %w", result.err), recordErr)
		}
	}
	message := fmt.Sprintf("Mihomo automatic startup failed after %d attempts: %s", len(retryDelays), safeOperationError(lastErr))
	recordErr := d.recordCoreAutoStartFailure(message)
	return errors.Join(fmt.Errorf("automatic Mihomo startup failed after %d attempts: %w", len(retryDelays), lastErr), recordErr)
}

func (d *Daemon) tryRestoreCoreOnStartup(ctx context.Context) coreAutoStartAttempt {
	runtimeState, err := d.State.Runtime()
	if err != nil {
		return coreAutoStartAttempt{retry: true, err: err}
	}
	if !runtimeState.CoreAutoStart {
		return coreAutoStartAttempt{}
	}
	if runtimeState.AppliedRevision == "" {
		return coreAutoStartAttempt{err: errors.New("cannot automatically start Mihomo without a locally verified applied configuration")}
	}
	status, err := d.Core.Status(ctx)
	if err != nil {
		return coreAutoStartAttempt{retry: true, err: err}
	}
	if !status.Installed {
		return coreAutoStartAttempt{err: errors.New("cannot automatically start Mihomo before the core is installed")}
	}
	if status.State == "running" {
		_, _ = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.RecentError = ""
			return nil
		})
		return coreAutoStartAttempt{}
	}
	operationErr := d.startCoreAndVerify(ctx, runtimeState.ProxyPort)
	refreshErr := d.refreshCoreStatus(ctx)
	if operationErr != nil {
		return coreAutoStartAttempt{retry: true, err: errors.Join(operationErr, refreshErr)}
	}
	_, stateErr := d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.RecentError = ""
		return nil
	})
	if err := errors.Join(refreshErr, stateErr); err != nil {
		d.writeRuntimeLog("automatic Mihomo startup succeeded but state refresh failed: %s", safeOperationError(err))
	}
	return coreAutoStartAttempt{restored: true}
}

func (d *Daemon) recordCoreAutoStartFailure(message string) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	_, err := d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		if runtime.CoreAutoStart {
			runtime.RecentError = message
		}
		return nil
	})
	return err
}
