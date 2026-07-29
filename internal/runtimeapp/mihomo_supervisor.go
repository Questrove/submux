package runtimeapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
)

type MihomoRecoveryTarget interface {
	ReconcileCoreVersion(context.Context) error
	RestoreLastGood(context.Context) error
	FailOpen(context.Context) error
	ExitEvents() <-chan runtimeprocess.ExitEvent
}

type FailOpenController interface {
	FailOpen(context.Context) error
}

type MihomoSupervisor struct {
	State  *runtimestate.Store
	Target MihomoRecoveryTarget
	Now    func() time.Time
	After  func(time.Duration) <-chan time.Time

	monitorStartup bool
}

func (s *MihomoSupervisor) RecoverStartup(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Mihomo recovery context is required")
	}
	if s == nil || s.State == nil || s.Target == nil {
		return errors.New("Mihomo recovery service is incomplete")
	}
	coreErr := s.Target.ReconcileCoreVersion(ctx)
	restore, err := s.State.PrepareMihomoStartup(s.now())
	if err != nil {
		return err
	}
	if coreErr != nil {
		if !restore {
			return nil
		}
		return s.State.MarkMihomoRecoveryFailure(
			"mihomo_core_unavailable",
			"Mihomo startup recovery could not use the installed core",
			false,
			s.now(),
		)
	}
	if !restore {
		return nil
	}
	if err := s.Target.FailOpen(ctx); err != nil {
		return s.State.MarkMihomoRecoveryFailure(
			"fail_open_failed",
			"Mihomo startup recovery could not confirm that network takeover was removed",
			true,
			s.now(),
		)
	}
	if err := s.Target.RestoreLastGood(ctx); err != nil {
		if stateErr := s.State.MarkMihomoRecoveryFailure(
			"startup_recovery_failed",
			"Mihomo could not restore the last known-good configuration after Runtime startup",
			false,
			s.now(),
		); stateErr != nil {
			return errors.Join(err, stateErr)
		}
		return nil
	}
	if err := s.State.MarkMihomoRestartSucceeded(s.now()); err != nil {
		return err
	}
	s.monitorStartup = true
	return nil
}

func (s *MihomoSupervisor) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Mihomo recovery context is required")
	}
	if s == nil || s.State == nil || s.Target == nil {
		return errors.New("Mihomo recovery service is incomplete")
	}
	exits := s.Target.ExitEvents()
	var retry <-chan time.Time
	var stable <-chan time.Time
	if s.monitorStartup {
		stable = s.after(runtimestate.MihomoCrashWindow)
		s.monitorStartup = false
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-exits:
			if !ok {
				return errors.New("Mihomo process exit stream closed unexpectedly")
			}
			if event.Intentional {
				retry = nil
				stable = nil
				continue
			}
			retry = nil
			stable = nil
			decision, err := s.prepareCrashRecovery(ctx)
			if err != nil {
				return err
			}
			if decision.Restart && decision.NextRestartAt != nil {
				retry = s.after(decision.NextRestartAt.Sub(s.now()))
			}
		case <-retry:
			retry = nil
			status, err := s.mihomoStatus()
			if err != nil {
				return err
			}
			if status.DesiredState != runtimeapi.MihomoDesiredRunning ||
				status.Recovery != runtimeapi.MihomoRecoveryWaiting {
				continue
			}
			if err := s.State.MarkMihomoRestarting(s.now()); err != nil {
				return err
			}
			if err := s.Target.RestoreLastGood(ctx); err != nil {
				decision, recoveryErr := s.prepareCrashRecovery(ctx)
				if recoveryErr != nil {
					return errors.Join(err, recoveryErr)
				}
				if decision.Restart && decision.NextRestartAt != nil {
					retry = s.after(decision.NextRestartAt.Sub(s.now()))
				}
				continue
			}
			if err := s.State.MarkMihomoRestartSucceeded(s.now()); err != nil {
				return err
			}
			stable = s.after(runtimestate.MihomoCrashWindow)
		case <-stable:
			stable = nil
			status, err := s.mihomoStatus()
			if err != nil {
				return err
			}
			if status.DesiredState != runtimeapi.MihomoDesiredRunning ||
				status.State != "running" ||
				status.Recovery != runtimeapi.MihomoRecoveryMonitoring {
				continue
			}
			if err := s.State.MarkMihomoStable(s.now()); err != nil {
				return fmt.Errorf("record stable Mihomo recovery: %w", err)
			}
		}
	}
}

func (s *MihomoSupervisor) mihomoStatus() (runtimeapi.MihomoStatus, error) {
	snapshot, err := s.State.Observe("", s.now())
	if err != nil {
		return runtimeapi.MihomoStatus{}, fmt.Errorf("observe Mihomo recovery state: %w", err)
	}
	return snapshot.Mihomo, nil
}

func (s *MihomoSupervisor) prepareCrashRecovery(
	ctx context.Context,
) (runtimestate.MihomoRecoveryDecision, error) {
	if err := s.Target.FailOpen(ctx); err != nil {
		stateErr := s.State.MarkMihomoRecoveryFailure(
			"fail_open_failed",
			"Mihomo exited and Runtime could not confirm that network takeover was removed",
			true,
			s.now(),
		)
		if stateErr != nil {
			return runtimestate.MihomoRecoveryDecision{}, errors.Join(err, stateErr)
		}
		return runtimestate.MihomoRecoveryDecision{}, nil
	}
	decision, err := s.State.RegisterMihomoCrash(s.now())
	if err != nil {
		return runtimestate.MihomoRecoveryDecision{}, fmt.Errorf("record Mihomo crash: %w", err)
	}
	return decision, nil
}

func (s *MihomoSupervisor) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *MihomoSupervisor) after(delay time.Duration) <-chan time.Time {
	if delay < 0 {
		delay = 0
	}
	if s != nil && s.After != nil {
		return s.After(delay)
	}
	return time.After(delay)
}
