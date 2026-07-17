package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"submux/internal/agentclient"
	"submux/internal/agentproto"
	"submux/internal/agentstate"
	"submux/internal/hostops"
	"submux/internal/integration"
	"submux/internal/mihomo"
	"submux/internal/store"
)

var Capabilities = []string{
	"subscription.update", "mihomo.restart", "mihomo.proxy.delay",
	"mihomo.proxy.select", "mihomo.connection.close", "mihomo.runtime.observe", "diagnostics.collect",
}

func PlatformCapabilities() []string {
	result := append([]string(nil), Capabilities...)
	if runtime.GOOS == "linux" {
		result = append(result, "integration.docker_daemon")
	} else if runtime.GOOS == "windows" {
		result = append(result, "integration.docker_desktop")
	}
	return result
}

type ControlPlane interface {
	GetState(context.Context) (agentclient.State, error)
	SendHeartbeat(context.Context, agentclient.Heartbeat) (agentclient.State, error)
	SendJobStatus(context.Context, string, string, json.RawMessage, string) error
	CheckArtifact(context.Context, int64, string) (agentclient.Artifact, error)
	FetchArtifact(context.Context, int64, string) (agentclient.Artifact, error)
	WatchUpdates(context.Context, func(string)) error
}

type MihomoAPI interface {
	Delay(context.Context, string, string, time.Duration) (int, error)
	Select(context.Context, string, string) error
	CloseConnection(context.Context, string) error
	Proxies(context.Context) (map[string]mihomo.Proxy, error)
}

type RuntimeStreamControl interface {
	OpenRuntimeStream(context.Context, string, string, <-chan json.RawMessage) error
}

type LocalAuditControl interface {
	SendLocalAudit(context.Context, agentclient.LocalAudit) error
}

type SelfRevocationControl interface {
	RevokeSelf(context.Context) error
}

type MihomoStreamSource interface {
	Stream(context.Context, string, func(json.RawMessage) error) error
}

type DeploymentApplier interface {
	Apply(context.Context, string, string, []byte) (mihomo.DeploymentResult, error)
	Rollback(context.Context) (mihomo.DeploymentResult, error)
}

type Daemon struct {
	State         *agentstate.Store
	Control       ControlPlane
	Core          hostops.CoreManager
	Deployer      DeploymentApplier
	Mihomo        MihomoAPI
	Docker        integration.DockerDaemonManager
	DockerDesktop integration.DockerDesktopManager
	VerifyRuntime func(context.Context, int) error
	AgentVersion  string
	Capabilities  []string
	PollInterval  time.Duration
	StartedAt     time.Time
	Logf          func(string, ...any)
	mutationMu    sync.Mutex
	auditMu       sync.Mutex
}

func (d *Daemon) Run(ctx context.Context) error {
	if d.State == nil || d.Control == nil || d.Core == nil || d.Deployer == nil || d.Mihomo == nil {
		return errors.New("agent daemon dependencies are incomplete")
	}
	if d.PollInterval <= 0 {
		d.PollInterval = 30 * time.Second
	}
	if d.StartedAt.IsZero() {
		d.StartedAt = time.Now().UTC()
	}
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	if _, err := d.State.RecoverInterruptedJobs(); err != nil {
		return err
	}
	d.reportUnreported(ctx)
	d.FlushLocalAudits(ctx)
	trigger := make(chan bool, 1)
	notify := func(reason string) {
		urgent := reason != "poll"
		select {
		case trigger <- urgent:
		default:
			if urgent {
				select {
				case <-trigger:
				default:
				}
				select {
				case trigger <- true:
				default:
				}
			}
		}
	}
	go d.watchUpdates(ctx, notify)
	ticker := time.NewTicker(d.PollInterval)
	defer ticker.Stop()
	notify("startup")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			notify("poll")
		case ignoreCheckInterval := <-trigger:
			if err := d.syncSerialized(ctx, ignoreCheckInterval); err != nil {
				d.Logf("agent sync: %v", err)
			}
		}
	}
}

func (d *Daemon) watchUpdates(ctx context.Context, notify func(string)) {
	for ctx.Err() == nil {
		err := d.Control.WatchUpdates(ctx, func(reason string) {
			if session, kind, ok := parseRuntimeStreamReason(reason); ok {
				go d.serveRuntimeStream(ctx, session, kind)
				return
			}
			notify(reason)
		})
		if ctx.Err() != nil {
			return
		}
		d.Logf("agent update stream: %v", err)
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (d *Daemon) serveRuntimeStream(parent context.Context, session, kind string) {
	control, controlOK := d.Control.(RuntimeStreamControl)
	if !controlOK {
		d.Logf("runtime stream %s is not supported by this Agent", kind)
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	frames := make(chan json.RawMessage, 16)
	producerDone := make(chan error, 1)
	go func() {
		producerDone <- d.produceRuntimeStream(ctx, kind, func(frame json.RawMessage) error {
			copyFrame := append(json.RawMessage(nil), frame...)
			select {
			case frames <- copyFrame:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(frames)
	}()
	streamErr := control.OpenRuntimeStream(ctx, session, kind, frames)
	cancel()
	producerErr := <-producerDone
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		d.Logf("runtime stream relay ended: %v", streamErr)
	} else if producerErr != nil && !errors.Is(producerErr, context.Canceled) {
		d.Logf("Mihomo runtime stream ended: %v", producerErr)
	}
}

func (d *Daemon) produceRuntimeStream(ctx context.Context, kind string, send func(json.RawMessage) error) error {
	if kind == "docker_preview" {
		if d.Docker == nil {
			return errors.New("Docker daemon integration is unavailable")
		}
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return err
		}
		if err := validateDockerProxyRuntime(runtimeState); err != nil {
			return err
		}
		preview, err := d.Docker.Preview(ctx, integration.DockerDaemonConfig{Enabled: true, ProxyPort: runtimeState.ProxyPort})
		if err != nil {
			return err
		}
		frame, err := json.Marshal(map[string]any{"kind": kind, "data": preview})
		if err != nil {
			return err
		}
		return send(frame)
	}
	if kind == "docker_desktop_preview" {
		if d.DockerDesktop == nil {
			return errors.New("Docker Desktop integration is unavailable")
		}
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return err
		}
		if err := validateDockerProxyRuntime(runtimeState); err != nil {
			return err
		}
		preview, err := d.DockerDesktop.Preview(ctx, integration.DockerDesktopConfig{Enabled: true, ProxyPort: runtimeState.ProxyPort, BusinessAdminSettings: true})
		if err != nil {
			return err
		}
		frame, err := json.Marshal(map[string]any{"kind": kind, "data": preview})
		if err != nil {
			return err
		}
		return send(frame)
	}
	source, ok := d.Mihomo.(MihomoStreamSource)
	if !ok {
		return errors.New("Mihomo runtime streams are unavailable")
	}
	return source.Stream(ctx, kind, send)
}

func parseRuntimeStreamReason(reason string) (string, string, bool) {
	parts := strings.Split(reason, "|")
	if len(parts) != 3 || parts[0] != "runtime_stream" || len(parts[1]) != 48 {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", false
	}
	switch parts[2] {
	case "proxies", "configs", "rules", "connections", "traffic", "memory", "logs", "docker_preview", "docker_desktop_preview":
		return parts[1], parts[2], true
	default:
		return "", "", false
	}
}

// SyncOnce serializes every host mutation: desired reconciliation, artifact
// deployment, and typed one-shot jobs all run in this single call path.
func (d *Daemon) SyncOnce(ctx context.Context) error {
	return d.syncSerialized(ctx, false)
}

func (d *Daemon) syncSerialized(ctx context.Context, ignoreCheckInterval bool) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	return d.syncOnce(ctx, ignoreCheckInterval)
}

func (d *Daemon) syncOnce(ctx context.Context, ignoreCheckInterval bool) error {
	state, err := d.Control.GetState(ctx)
	if err != nil {
		return err
	}
	d.reportUnreported(ctx)
	d.FlushLocalAudits(ctx)
	var deployment *store.Deployment
	var syncError error
	if err := d.reconcileCore(ctx, state.Desired); err != nil {
		syncError = fmt.Errorf("reconcile core: %w", err)
	}
	if syncError == nil && state.Binding != nil {
		if value, err := d.checkSubscription(ctx, *state.Binding, false, ignoreCheckInterval, agentproto.ActorScheduler, randomRequestID()); err != nil {
			syncError = fmt.Errorf("check subscription: %w", err)
			deployment = value
		} else {
			deployment = value
		}
	}
	if syncError == nil {
		if err := d.reconcileRuntime(ctx, state.Desired, state.Binding != nil); err != nil {
			syncError = fmt.Errorf("reconcile runtime: %w", err)
		}
	}
	if syncError == nil {
		if err := d.reconcileIntegrations(ctx, state.Desired); err != nil {
			syncError = fmt.Errorf("reconcile integrations: %w", err)
		}
	}
	if syncError == nil {
		_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.ObservedGeneration = state.Desired.Generation
			runtime.RecentError = ""
			return nil
		})
		if err != nil {
			syncError = err
		}
	} else {
		_, _ = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.RecentError = safeOperationError(syncError)
			return nil
		})
	}
	for _, job := range state.Jobs {
		d.processJob(ctx, job.Job, state.Binding)
	}
	heartbeatErr := d.heartbeat(ctx, deployment)
	if syncError != nil {
		return syncError
	}
	return heartbeatErr
}

func (d *Daemon) reconcileIntegrations(ctx context.Context, desired store.RuntimeDesiredState) error {
	raw, configured := desired.Integrations[integration.DockerDaemonType]
	if configured {
		if err := d.reconcileDockerDaemon(ctx, raw); err != nil {
			return err
		}
	}
	desktopRaw, desktopConfigured := desired.Integrations[integration.DockerDesktopType]
	if desktopConfigured {
		if err := d.reconcileDockerDesktop(ctx, desktopRaw); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) reconcileDockerDaemon(ctx context.Context, raw json.RawMessage) error {
	if d.Docker == nil {
		return errors.New("Docker daemon integration is not supported by this Agent")
	}
	var config integration.DockerDaemonConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return errors.New("desired Docker daemon integration is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("desired Docker daemon integration contains trailing data")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Enabled {
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return err
		}
		if err := validateDockerProxyRuntime(runtimeState); err != nil || config.ProxyPort != runtimeState.ProxyPort {
			return errors.New("Docker daemon integration must use the running Agent-managed HTTP or mixed proxy listener")
		}
	}
	status, err := d.Docker.Status(ctx)
	if err != nil {
		return err
	}
	if config.Enabled {
		preview, err := d.Docker.Preview(ctx, config)
		if err != nil {
			return err
		}
		if status.State != "active" && config.ExpectedOriginalHash != "" && preview.OriginalHash != config.ExpectedOriginalHash {
			return errors.New("Docker daemon configuration changed after the administrator preview")
		}
		if status.State == "active" && status.AppliedHash != preview.DesiredHash {
			return errors.New("disable the active Docker daemon integration before changing its settings")
		}
		if status.State != "active" {
			status, err = d.Docker.Enable(ctx, config, preview.OriginalHash)
			if err != nil {
				return err
			}
		}
	} else if status.State != "disabled" {
		status, err = d.Docker.Disable(ctx)
		if err != nil {
			return err
		}
	}
	statusRaw, _ := json.Marshal(status)
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.Integrations[integration.DockerDaemonType] = statusRaw
		return nil
	})
	return err
}

func (d *Daemon) reconcileDockerDesktop(ctx context.Context, raw json.RawMessage) error {
	if d.DockerDesktop == nil {
		return errors.New("Docker Desktop integration is not supported by this Agent")
	}
	var config integration.DockerDesktopConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return errors.New("desired Docker Desktop integration is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("desired Docker Desktop integration contains trailing data")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Enabled {
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return err
		}
		if err := validateDockerProxyRuntime(runtimeState); err != nil || config.ProxyPort != runtimeState.ProxyPort {
			return errors.New("Docker Desktop integration must use the running Agent-managed HTTP or mixed proxy listener")
		}
	}
	status, err := d.DockerDesktop.Status(ctx)
	if err != nil {
		return err
	}
	if config.Enabled {
		preview, err := d.DockerDesktop.Preview(ctx, config)
		if err != nil {
			return err
		}
		if status.State != "active" && config.ExpectedOriginalHash != "" && preview.OriginalHash != config.ExpectedOriginalHash {
			return errors.New("Docker Desktop settings changed after the administrator preview")
		}
		if status.State == "active" && status.AppliedHash != preview.DesiredHash {
			return errors.New("disable the active Docker Desktop integration before changing its settings")
		}
		if status.State != "active" {
			status, err = d.DockerDesktop.Enable(ctx, config, preview.OriginalHash)
			if err != nil {
				return err
			}
		}
	} else if status.State != "disabled" {
		status, err = d.DockerDesktop.Disable(ctx)
		if err != nil {
			return err
		}
	}
	statusRaw, _ := json.Marshal(status)
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.Integrations[integration.DockerDesktopType] = statusRaw
		return nil
	})
	return err
}

func validateDockerProxyRuntime(runtimeState agentstate.Runtime) error {
	if runtimeState.CoreStatus != "running" || runtimeState.ProxyPort < 1 || (runtimeState.ProxyKind != "mixed" && runtimeState.ProxyKind != "http") {
		return errors.New("a running Agent-managed HTTP or mixed proxy listener is required")
	}
	return nil
}

func (d *Daemon) CheckSubscriptionNow(ctx context.Context, apply bool) (*store.Deployment, error) {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	state, err := d.Control.GetState(ctx)
	if err != nil {
		return nil, err
	}
	if state.Binding == nil {
		return nil, errors.New("runtime instance has no binding")
	}
	if err := d.reconcileCore(ctx, state.Desired); err != nil {
		return nil, err
	}
	return d.checkSubscription(ctx, *state.Binding, apply, true, agentproto.ActorLocalCLI, randomRequestID())
}

func (d *Daemon) InstallCoreNow(ctx context.Context, channel, version string) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	operationErr := d.Core.Install(ctx, channel, version)
	return errors.Join(operationErr, d.refreshCoreStatus(ctx))
}

func (d *Daemon) RestartCoreNow(ctx context.Context) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	operationErr := d.Core.Restart(ctx)
	return errors.Join(operationErr, d.refreshCoreStatus(ctx))
}

func (d *Daemon) RollbackCoreNow(ctx context.Context) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	operationErr := d.Core.RollbackCore(ctx)
	return errors.Join(operationErr, d.refreshCoreStatus(ctx))
}

func (d *Daemon) refreshCoreStatus(ctx context.Context) error {
	status, err := d.Core.Status(ctx)
	if err != nil {
		return err
	}
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.CoreStatus, runtime.CoreVersion, runtime.PreviousCoreVersion = status.State, status.Version, status.PreviousVersion
		return nil
	})
	return err
}

func (d *Daemon) RollbackConfigNow(ctx context.Context) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	result, err := d.Deployer.Rollback(ctx)
	if err != nil {
		return err
	}
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.AppliedRevision = result.Revision
		runtime.LastGoodRevision = result.PreviousRevision
		runtime.ProxyPort, runtime.ProxyKind = result.ProxyPort, result.ProxyKind
		runtime.RejectedRevision, runtime.RecentError = "", ""
		return nil
	})
	return err
}

func (d *Daemon) reconcileCore(ctx context.Context, desired store.RuntimeDesiredState) error {
	status, err := d.Core.Status(ctx)
	if err != nil {
		return err
	}
	if !desired.CoreInstalled {
		if status.Installed {
			if err := d.Core.Uninstall(ctx); err != nil {
				return err
			}
		}
		_, err := d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.CoreStatus, runtime.CoreVersion, runtime.PreviousCoreVersion = "not_installed", "", ""
			return nil
		})
		return err
	}
	if !status.Installed || status.Version != desired.CoreVersion {
		if err := d.Core.Install(ctx, desired.CoreChannel, desired.CoreVersion); err != nil {
			return err
		}
		status, err = d.Core.Status(ctx)
		if err != nil {
			return err
		}
	}
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.CoreStatus, runtime.CoreVersion, runtime.PreviousCoreVersion = status.State, status.Version, status.PreviousVersion
		return nil
	})
	return err
}

func (d *Daemon) reconcileRuntime(ctx context.Context, desired store.RuntimeDesiredState, hasBinding bool) error {
	status, err := d.Core.Status(ctx)
	if err != nil {
		return err
	}
	if !desired.CoreInstalled {
		return nil
	}
	switch desired.RuntimeState {
	case store.RuntimeRunning:
		if !hasBinding {
			return errors.New("cannot start Mihomo without an Agent-compatible runtime binding")
		}
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return err
		}
		if runtimeState.AppliedRevision == "" {
			return errors.New("cannot start Mihomo without a locally verified applied configuration")
		}
		if status.State != "running" {
			if err := d.Core.Start(ctx); err != nil {
				return err
			}
		}
	case store.RuntimeStopped:
		if status.State == "running" || status.State == "failed" {
			if err := d.Core.Stop(ctx); err != nil {
				return err
			}
		}
	default:
		return errors.New("unknown desired runtime state")
	}
	status, err = d.Core.Status(ctx)
	if err == nil && desired.RuntimeState == store.RuntimeRunning && status.State == "running" && d.VerifyRuntime != nil {
		runtimeState, stateErr := d.State.Runtime()
		if stateErr != nil {
			err = stateErr
		} else {
			err = d.VerifyRuntime(ctx, runtimeState.ProxyPort)
		}
	}
	if err == nil {
		_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.CoreStatus, runtime.CoreVersion, runtime.PreviousCoreVersion = status.State, status.Version, status.PreviousVersion
			return nil
		})
	}
	return err
}

func (d *Daemon) checkSubscription(ctx context.Context, binding store.RuntimeBinding, explicitApply, ignoreCheckInterval bool, actorType, requestID string) (*store.Deployment, error) {
	runtimeState, err := d.State.Runtime()
	if err != nil {
		return nil, err
	}
	if runtimeState.BindingID != binding.ID {
		runtimeState, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.BindingID, runtime.ArtifactETag, runtime.RemoteRevision, runtime.RejectedRevision = binding.ID, "", "", ""
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if !ignoreCheckInterval && runtimeState.LastCheckAt != "" {
		lastCheck, _ := time.Parse(time.RFC3339, runtimeState.LastCheckAt)
		if time.Since(lastCheck) < time.Duration(binding.CheckIntervalSec)*time.Second {
			return nil, nil
		}
	}
	checkETag := runtimeState.ArtifactETag
	if explicitApply {
		checkETag = ""
	}
	metadata, err := d.Control.CheckArtifact(ctx, binding.ID, checkETag)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if metadata.NotModified {
		_, err := d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error { runtime.LastCheckAt = now; return nil })
		return nil, err
	}
	runtimeState, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.RemoteRevision, runtime.ArtifactETag, runtime.LastCheckAt = metadata.Revision, metadata.ETag, now
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !explicitApply && !binding.AutoUpdate {
		return nil, nil
	}
	if !explicitApply && runtimeState.RejectedRevision == metadata.Revision {
		return nil, nil
	}
	artifact, err := d.Control.FetchArtifact(ctx, binding.ID, "")
	if err != nil {
		return nil, err
	}
	if artifact.ContentType != "application/yaml" && artifact.ContentType != "text/yaml" && artifact.ContentType != "application/x-yaml" {
		return nil, errors.New("bound artifact is not a Mihomo YAML document")
	}
	result, applyErr := d.Deployer.Apply(ctx, artifact.Revision, artifact.SHA256, artifact.Body)
	deployment := &store.Deployment{
		ActorType: actorType, RequestID: requestID,
		RemoteRevision: result.Revision, PreviousRevision: result.PreviousRevision,
		ArtifactHash: result.ArtifactHash, EffectiveHash: result.EffectiveHash,
		MihomoVersion: runtimeState.CoreVersion,
		Status:        result.Status, Validation: result.Validation, Error: result.Error,
	}
	if applyErr != nil {
		_, _ = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.RejectedRevision, runtime.RecentError = artifact.Revision, safeOperationError(applyErr)
			if result.RolledBack {
				runtime.LastGoodRevision = ""
			}
			return nil
		})
		return deployment, applyErr
	}
	selectionNotice := d.restoreProxySelections(ctx)
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.AppliedRevision, runtime.RemoteRevision, runtime.ArtifactETag = artifact.Revision, artifact.Revision, artifact.ETag
		runtime.RejectedRevision, runtime.LastUpdateAt, runtime.RecentError = "", now, ""
		runtime.LastGoodRevision, runtime.SelectionNotice = result.PreviousRevision, selectionNotice
		runtime.ProxyPort, runtime.ProxyKind = result.ProxyPort, result.ProxyKind
		return nil
	})
	return deployment, err
}

func (d *Daemon) restoreProxySelections(ctx context.Context) string {
	runtimeState, err := d.State.Runtime()
	if err != nil {
		return "配置已部署，但无法读取部署前的节点选择"
	}
	proxies, err := d.Mihomo.Proxies(ctx)
	if err != nil {
		return "配置已部署，但无法观测策略组以恢复节点选择"
	}
	groups := make([]string, 0, len(runtimeState.SelectedProxies))
	for group := range runtimeState.SelectedProxies {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	warnings := make([]string, 0)
	for _, group := range groups {
		target := runtimeState.SelectedProxies[group]
		selector, exists := proxies[group]
		if !exists || selector.Type != "Selector" || !containsString(selector.All, target) {
			warnings = append(warnings, fmt.Sprintf("策略组 %q 的原选择已不可用，使用模板默认项", group))
			continue
		}
		if selector.Now != target {
			if err := d.Mihomo.Select(ctx, group, target); err != nil {
				warnings = append(warnings, fmt.Sprintf("策略组 %q 的原选择恢复失败，使用模板默认项", group))
			}
		}
	}
	observed, err := d.Mihomo.Proxies(ctx)
	if err == nil {
		current := make(map[string]string)
		for name, proxy := range observed {
			if proxy.Type == "Selector" && proxy.Now != "" {
				current[name] = proxy.Now
			}
		}
		_, _ = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.SelectedProxies = current
			return nil
		})
	}
	notice := strings.Join(warnings, "；")
	if len(notice) > 512 {
		notice = notice[:512]
	}
	return notice
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func randomRequestID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("agent-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(value)
}

func (d *Daemon) processJob(ctx context.Context, job agentproto.Job, binding *store.RuntimeBinding) {
	now := time.Now().UTC()
	if err := agentproto.ValidateJob(job, d.capabilities(), now); err != nil {
		record, _ := d.State.SaveUnstartedJob(job, agentproto.JobFailed, "job failed strict local validation")
		d.reportJob(ctx, record)
		return
	}
	record, execute, err := d.State.BeginJob(job)
	if err != nil {
		return
	}
	if !execute {
		if agentproto.TerminalJobStatus(record.Status) && !record.Reported {
			d.reportJob(ctx, record)
		}
		return
	}
	_ = d.Control.SendJobStatus(ctx, job.ID, agentproto.JobRunning, nil, "")
	result, runErr := d.executeJob(ctx, job, binding)
	status, safeError := agentproto.JobSucceeded, ""
	if runErr != nil {
		status, safeError = agentproto.JobFailed, safeOperationError(runErr)
		result = nil
	}
	record, err = d.State.CompleteJob(job.ID, status, result, safeError)
	if err == nil {
		d.reportJob(ctx, record)
	}
}

func (d *Daemon) executeJob(ctx context.Context, job agentproto.Job, binding *store.RuntimeBinding) (json.RawMessage, error) {
	switch job.Type {
	case agentproto.JobUpdateSubscription:
		if binding == nil {
			return nil, errors.New("runtime instance has no binding")
		}
		deployment, err := d.checkSubscription(ctx, *binding, true, true, job.ActorType, job.RequestID)
		runtimeState, _ := d.State.Runtime()
		status := "active"
		if deployment != nil {
			status = deployment.Status
		}
		return marshalResult(agentproto.UpdateSubscriptionResult{RemoteRevision: runtimeState.RemoteRevision, AppliedRevision: runtimeState.AppliedRevision, RejectedRevision: runtimeState.RejectedRevision, Status: status}), err
	case agentproto.JobRestartCore:
		if err := d.Core.Restart(ctx); err != nil {
			return nil, err
		}
		status, err := d.Core.Status(ctx)
		if err != nil {
			return nil, err
		}
		return marshalResult(agentproto.RestartCoreResult{CoreStatus: status.State}), nil
	case agentproto.JobTestProxyDelay:
		var params agentproto.TestProxyDelayParams
		_ = json.Unmarshal(job.Params, &params)
		timeout := time.Duration(params.TimeoutMS) * time.Millisecond
		delay, err := d.Mihomo.Delay(ctx, params.Group, params.Proxy, timeout)
		return marshalResult(agentproto.TestProxyDelayResult{Group: params.Group, Proxy: params.Proxy, DelayMS: delay}), err
	case agentproto.JobSelectProxy:
		var params agentproto.SelectProxyParams
		_ = json.Unmarshal(job.Params, &params)
		if err := d.Mihomo.Select(ctx, params.Group, params.Proxy); err != nil {
			return nil, err
		}
		_, _ = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
			runtime.SelectedProxies[params.Group] = params.Proxy
			return nil
		})
		return marshalResult(agentproto.SelectProxyResult{Group: params.Group, Selected: params.Proxy}), nil
	case agentproto.JobCloseConnection:
		var params agentproto.CloseConnectionParams
		_ = json.Unmarshal(job.Params, &params)
		if err := d.Mihomo.CloseConnection(ctx, params.ConnectionID); err != nil {
			return nil, err
		}
		return marshalResult(agentproto.CloseConnectionResult{ConnectionID: params.ConnectionID, Closed: true}), nil
	case agentproto.JobCollectDiagnostics:
		status, err := d.Core.Status(ctx)
		checkStatus, message := "ok", status.State
		if err != nil {
			checkStatus, message = "failed", "core status unavailable"
		}
		return marshalResult(agentproto.CollectDiagnosticsResult{Checks: []agentproto.DiagnosticCheck{{Name: "mihomo_service", Status: checkStatus, Message: message}}}), nil
	default:
		return nil, errors.New("unsupported job type")
	}
}

func (d *Daemon) heartbeat(ctx context.Context, deployment *store.Deployment) error {
	runtimeState, err := d.State.Runtime()
	if err != nil {
		return err
	}
	status := store.InstanceOnline
	if runtimeState.RecentError != "" {
		status = store.InstanceDegraded
	}
	uptime := int64(0)
	if !d.StartedAt.IsZero() {
		uptime = int64(time.Since(d.StartedAt).Seconds())
		if uptime < 0 {
			uptime = 0
		}
	}
	_, err = d.Control.SendHeartbeat(ctx, agentclient.Heartbeat{
		AgentVersion: d.AgentVersion, Capabilities: d.capabilities(), Status: status,
		Observation: store.RuntimeObservation{
			ObservedGeneration: runtimeState.ObservedGeneration,
			RemoteRevision:     runtimeState.RemoteRevision, AppliedRevision: runtimeState.AppliedRevision,
			RejectedRevision: runtimeState.RejectedRevision, UpdateAvailable: runtimeState.RemoteRevision != "" && runtimeState.RemoteRevision != runtimeState.AppliedRevision,
			LastCheckAt: runtimeState.LastCheckAt, LastUpdateAt: runtimeState.LastUpdateAt,
			CoreVersion: runtimeState.CoreVersion, PreviousCoreVersion: runtimeState.PreviousCoreVersion, CoreStatus: runtimeState.CoreStatus,
			AgentUptimeSeconds: uptime,
			ProxyListening:     runtimeState.CoreStatus == "running" && runtimeState.ProxyPort > 0, RecentError: runtimeState.RecentError,
			ProxyPort: runtimeState.ProxyPort, ProxyKind: runtimeState.ProxyKind, ControllerPort: runtimeState.ControllerPort,
			SelectedProxies: runtimeState.SelectedProxies, SelectionNotice: runtimeState.SelectionNotice,
			LastGoodRevision: runtimeState.LastGoodRevision,
			Integrations:     runtimeState.Integrations,
		},
		Deployment: deployment,
	})
	return err
}

func (d *Daemon) capabilities() []string {
	if len(d.Capabilities) != 0 {
		return append([]string(nil), d.Capabilities...)
	}
	return PlatformCapabilities()
}

func (d *Daemon) reportUnreported(ctx context.Context) {
	values, err := d.State.UnreportedJobs()
	if err != nil {
		return
	}
	for _, value := range values {
		d.reportJob(ctx, value)
	}
}

func (d *Daemon) FlushLocalAudits(ctx context.Context) {
	d.auditMu.Lock()
	defer d.auditMu.Unlock()
	control, ok := d.Control.(LocalAuditControl)
	if !ok {
		return
	}
	values, err := d.State.UnreportedLocalAudits()
	if err != nil {
		return
	}
	for _, value := range values {
		err := control.SendLocalAudit(ctx, agentclient.LocalAudit{
			RequestID: value.RequestID, Action: value.Action, Revision: value.Revision,
			Result: value.Result, Summary: value.Summary,
		})
		if err != nil {
			return
		}
		_ = d.State.MarkLocalAuditReported(value.ID)
	}
}

func (d *Daemon) reportJob(ctx context.Context, value agentstate.LocalJob) {
	if err := d.Control.SendJobStatus(ctx, value.Job.ID, value.Status, value.Result, value.Error); err == nil {
		_ = d.State.MarkJobReported(value.Job.ID)
	}
}

func marshalResult(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func safeOperationError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "checksum") || strings.Contains(message, "sha-256"):
		return "integrity verification failed"
	case strings.Contains(message, "validate") || strings.Contains(message, "config"):
		return "configuration validation or activation failed"
	case strings.Contains(message, "version") || strings.Contains(message, "install"):
		return "core lifecycle operation failed"
	case strings.Contains(message, "connection") || strings.Contains(message, "proxy"):
		return "Mihomo runtime operation failed"
	default:
		return "Agent operation failed"
	}
}
