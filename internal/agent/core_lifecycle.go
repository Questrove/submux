package agent

import (
	"context"
	"errors"

	"submux/internal/agentstate"
)

func (d *Daemon) startCoreAndVerify(ctx context.Context, proxyPort int) error {
	if err := d.Core.Start(ctx); err != nil {
		return err
	}
	if d.VerifyRuntime == nil {
		return nil
	}
	d.setOperationPhase("verifying_runtime", 0, 0)
	if err := d.VerifyRuntime(ctx, proxyPort); err != nil {
		return errors.Join(err, d.Core.Stop(context.WithoutCancel(ctx)))
	}
	return nil
}

func (d *Daemon) restartCoreAndVerify(ctx context.Context, proxyPort int) error {
	if err := d.Core.Restart(ctx); err != nil {
		return err
	}
	if d.VerifyRuntime == nil {
		return nil
	}
	d.setOperationPhase("verifying_runtime", 0, 0)
	if err := d.VerifyRuntime(ctx, proxyPort); err != nil {
		return errors.Join(err, d.Core.Stop(context.WithoutCancel(ctx)))
	}
	return nil
}

func (d *Daemon) setCoreAutoStart(enabled bool) error {
	_, err := d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.CoreAutoStart = enabled
		runtime.RecentError = ""
		return nil
	})
	return err
}
