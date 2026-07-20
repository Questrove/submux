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
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"submux/internal/agentclient"
	"submux/internal/agentproto"
	"submux/internal/agentstate"
	"submux/internal/hostops"
	"submux/internal/mihomo"
	"submux/internal/store"
)

var Capabilities = []string{
	"subscription.manage", "mihomo.core.manage", "mihomo.restart", "mihomo.proxy.delay",
	"mihomo.proxy.select", "mihomo.connection.close", "mihomo.runtime.observe",
	"agent.runtime.observe",
	"mihomo.release.list",
	"agent.resource.proxy",
}

func PlatformCapabilities() []string {
	return append([]string(nil), Capabilities...)
}

type ControlPlane interface {
	GetState(context.Context) (agentclient.State, error)
	SendHeartbeat(context.Context, agentclient.Heartbeat) (agentclient.State, error)
	SendJobStatus(context.Context, string, string, json.RawMessage, string) error
	WatchUpdates(context.Context, func(string)) error
	FetchRuntimeSecret(context.Context, string) (agentclient.RuntimeSecret, error)
	FetchPlatformSubscription(context.Context, int64) (agentclient.PlatformSubscription, error)
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
	State            *agentstate.Store
	Control          ControlPlane
	Core             hostops.CoreManager
	Deployer         DeploymentApplier
	Mihomo           MihomoAPI
	VerifyRuntime    func(context.Context, int) error
	AgentVersion     string
	Capabilities     []string
	PollInterval     time.Duration
	SubscriptionHTTP *http.Client
	StartedAt        time.Time
	Logf             func(string, ...any)
	Logs             *RuntimeLogBuffer
	mutationMu       sync.Mutex
	auditMu          sync.Mutex
	operationMu      sync.Mutex
	operation        *store.RuntimeOperation
	progressWake     chan struct{}
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
	if d.Logs == nil {
		d.Logs = NewRuntimeLogBuffer()
	}
	baseLogf := d.Logf
	d.Logf = func(format string, args ...any) {
		d.Logs.Printf(format, args...)
		baseLogf(format, args...)
	}
	d.progressWake = make(chan struct{}, 1)
	if core, ok := d.Core.(hostops.ResourceProxyController); ok {
		core.SetProgressReporter(d.handleCoreProgress)
	}
	go d.progressHeartbeatLoop(ctx)
	if _, err := d.State.RecoverInterruptedJobs(); err != nil {
		return err
	}
	d.reportUnreported(ctx)
	d.FlushLocalAudits(ctx)
	trigger := make(chan struct{}, 1)
	notify := func(string) {
		select {
		case trigger <- struct{}{}:
		default:
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
		case <-trigger:
			if err := d.syncSerialized(ctx); err != nil {
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
		if err != nil && !errors.Is(err, io.EOF) {
			d.Logf("agent update stream: %v", err)
		}
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
	if !isExpectedStreamEnd(streamErr) {
		d.Logf("runtime stream relay ended: %v", streamErr)
	} else if !isExpectedStreamEnd(producerErr) {
		d.Logf("Mihomo runtime stream ended: %v", producerErr)
	}
}

func isExpectedStreamEnd(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"broken pipe", "connection reset by peer", "use of closed network connection", "websocket: close"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func (d *Daemon) produceRuntimeStream(ctx context.Context, kind string, send func(json.RawMessage) error) error {
	if kind == "agent_logs" {
		if d.Logs == nil {
			return errors.New("Agent runtime logs are unavailable")
		}
		return d.Logs.Stream(ctx, send)
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
	case "proxies", "configs", "rules", "connections", "traffic", "memory", "logs", "agent_logs":
		return parts[1], parts[2], true
	default:
		return "", "", false
	}
}

// SyncOnce serializes host observation and typed one-shot jobs. The Agent
// never changes Mihomo merely because the control plane stores a target state.
func (d *Daemon) SyncOnce(ctx context.Context) error {
	return d.syncSerialized(ctx)
}

func (d *Daemon) syncSerialized(ctx context.Context) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	return d.syncOnce(ctx)
}

func (d *Daemon) syncOnce(ctx context.Context) error {
	state, err := d.Control.GetState(ctx)
	if err != nil {
		return err
	}
	d.reportUnreported(ctx)
	d.FlushLocalAudits(ctx)
	observationErr := d.refreshCoreStatus(ctx)
	for _, job := range state.Jobs {
		d.processJob(ctx, job.Job)
	}
	if err := d.refreshCoreStatus(ctx); err != nil {
		observationErr = errors.Join(observationErr, err)
	}
	return errors.Join(observationErr, d.heartbeat(ctx))
}

func (d *Daemon) beginJobOperation(job agentproto.Job) {
	now := time.Now().UTC().Format(time.RFC3339)
	d.operationMu.Lock()
	d.operation = &store.RuntimeOperation{
		RequestID: job.RequestID, JobID: job.ID, Kind: job.Type,
		Phase: "accepted", Status: "running", StartedAt: now, UpdatedAt: now,
	}
	d.operationMu.Unlock()
	d.writeRuntimeLog("accepted %s job %s", job.Type, job.ID)
	d.signalProgress()
}

func (d *Daemon) configureResourceProxy(proxy agentproto.ResourceProxy) error {
	proxy = agentproto.NormalizeResourceProxy(proxy)
	if err := agentproto.ValidateResourceProxy(proxy); err != nil {
		return err
	}
	controller, supported := d.Core.(hostops.ResourceProxyController)
	if proxy.Mode == agentproto.ResourceProxyCustom && !supported {
		return errors.New("this Agent does not support a custom resource proxy")
	}
	if supported {
		proxyURL := ""
		if proxy.Mode == agentproto.ResourceProxyCustom {
			proxyURL = proxy.URL
		}
		if err := controller.SetResourceProxy(proxyURL); err != nil {
			return err
		}
	}
	_, err := d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.ResourceProxyMode, runtime.ResourceProxyURL = proxy.Mode, proxy.URL
		return nil
	})
	return err
}

func (d *Daemon) setOperationPhase(phase string, completed, total int64) {
	if phase == "" {
		return
	}
	d.operationMu.Lock()
	if d.operation == nil || d.operation.Status != "running" {
		d.operationMu.Unlock()
		return
	}
	changed := d.operation.Phase != phase
	d.operation.Phase = phase
	d.operation.BytesCompleted = completed
	d.operation.BytesTotal = total
	d.operation.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	d.operationMu.Unlock()
	if changed {
		d.writeRuntimeLog("operation entered phase %s", phase)
	}
	d.signalProgress()
}

func (d *Daemon) handleCoreProgress(progress hostops.CoreProgress) {
	d.setOperationPhase(progress.Phase, progress.BytesCompleted, progress.BytesTotal)
}

func (d *Daemon) finishOperation(operationErr error) {
	d.operationMu.Lock()
	if d.operation == nil {
		d.operationMu.Unlock()
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	d.operation.UpdatedAt, d.operation.FinishedAt = now, now
	if operationErr == nil {
		d.operation.Status, d.operation.Phase = "succeeded", "completed"
	} else {
		d.operation.Status, d.operation.Phase = "failed", "failed"
		d.operation.Error = safeOperationError(operationErr)
	}
	result, kind := d.operation.Status, d.operation.Kind
	d.operationMu.Unlock()
	d.writeRuntimeLog("%s operation %s", kind, result)
	d.signalProgress()
}

func (d *Daemon) currentOperation() *store.RuntimeOperation {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if d.operation == nil {
		return nil
	}
	copyOperation := *d.operation
	return &copyOperation
}

func (d *Daemon) signalProgress() {
	if d.progressWake == nil {
		return
	}
	select {
	case d.progressWake <- struct{}{}:
	default:
	}
}

func (d *Daemon) writeRuntimeLog(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
		return
	}
	if d.Logs != nil {
		d.Logs.Printf(format, args...)
	}
}

func (d *Daemon) progressHeartbeatLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.progressWake:
			timer := time.NewTimer(150 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			reportCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := d.heartbeat(reportCtx); err != nil && ctx.Err() == nil {
				d.Logf("report Agent operation progress: %v", err)
			}
			cancel()
		}
	}
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
	_, err := d.observeCoreStatus(ctx)
	return err
}

func (d *Daemon) observeCoreStatus(ctx context.Context) (hostops.CoreStatus, error) {
	status, err := d.Core.Status(ctx)
	if err != nil {
		return status, err
	}
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.CoreStatus, runtime.CoreVersion, runtime.PreviousCoreVersion = status.State, status.Version, status.PreviousVersion
		return nil
	})
	return status, err
}

func (d *Daemon) RollbackConfigNow(ctx context.Context) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	result, err := d.Deployer.Rollback(ctx)
	if err != nil {
		return err
	}
	activeSubscriptionID := ""
	if subscriptions, listErr := d.State.ListRuntimeSubscriptions(); listErr == nil {
		for _, subscription := range subscriptions {
			if subscription.Revision == result.Revision {
				activeSubscriptionID = subscription.ID
				break
			}
		}
	}
	_, err = d.State.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.AppliedRevision = result.Revision
		runtime.ActiveSubscriptionID = activeSubscriptionID
		runtime.LastGoodRevision = result.PreviousRevision
		runtime.ProxyPort, runtime.ProxyKind = result.ProxyPort, result.ProxyKind
		runtime.RejectedRevision, runtime.RecentError = "", ""
		return nil
	})
	return err
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

func (d *Daemon) processJob(ctx context.Context, job agentproto.Job) {
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
	d.beginJobOperation(job)
	_ = d.Control.SendJobStatus(ctx, job.ID, agentproto.JobRunning, nil, "")
	result, runErr := d.executeJob(ctx, job)
	d.finishOperation(runErr)
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

func (d *Daemon) executeJob(ctx context.Context, job agentproto.Job) (json.RawMessage, error) {
	switch job.Type {
	case agentproto.JobAddRuntimeSubscription:
		var params agentproto.AddRuntimeSubscriptionParams
		_ = json.Unmarshal(job.Params, &params)
		d.setOperationPhase("downloading_subscription", 0, 0)
		value := agentstate.RuntimeSubscription{ID: randomRequestID(), Name: strings.TrimSpace(params.Name)}
		var err error
		if params.PlatformSubscriptionID > 0 {
			value.PlatformSubscriptionID, value.Host = params.PlatformSubscriptionID, "平台订阅"
		} else {
			subscriptionURL, consumeErr := d.consumeSubscriptionURL(ctx, params.SecretRef)
			if consumeErr != nil {
				return nil, consumeErr
			}
			parsed, _ := validateAgentSubscriptionURL(subscriptionURL)
			value.URL, value.Host = subscriptionURL, parsed.Hostname()
		}
		value, err = d.fetchRuntimeSubscription(ctx, value)
		if err != nil {
			return nil, err
		}
		if _, err := d.State.SaveRuntimeSubscription(value); err != nil {
			return nil, err
		}
		runtimeState, _ := d.State.Runtime()
		return marshalResult(agentproto.RuntimeSubscriptionResult{Subscription: runtimeSubscriptionSummary(value, runtimeState.ActiveSubscriptionID), Status: "saved"}), nil
	case agentproto.JobEditRuntimeSubscription:
		var params agentproto.EditRuntimeSubscriptionParams
		_ = json.Unmarshal(job.Params, &params)
		value, err := d.State.RuntimeSubscription(params.ID)
		if err != nil {
			return nil, err
		}
		value.Name = strings.TrimSpace(params.Name)
		sourceChanged := params.SecretRef != "" || params.PlatformSubscriptionID > 0
		if sourceChanged {
			d.setOperationPhase("downloading_subscription", 0, 0)
			if params.PlatformSubscriptionID > 0 {
				value.URL, value.ETag, value.LastModified = "", "", ""
				value.PlatformSubscriptionID, value.Host = params.PlatformSubscriptionID, "平台订阅"
			} else {
				subscriptionURL, consumeErr := d.consumeSubscriptionURL(ctx, params.SecretRef)
				if consumeErr != nil {
					return nil, consumeErr
				}
				parsed, _ := validateAgentSubscriptionURL(subscriptionURL)
				value.URL, value.Host, value.ETag, value.LastModified = subscriptionURL, parsed.Hostname(), "", ""
				value.PlatformSubscriptionID = 0
			}
			value, err = d.fetchRuntimeSubscription(ctx, value)
			if err != nil {
				return nil, err
			}
		}
		value, err = d.State.SaveRuntimeSubscription(value)
		if err != nil {
			return nil, err
		}
		runtimeState, _ := d.State.Runtime()
		status := "saved"
		if sourceChanged && runtimeState.ActiveSubscriptionID == value.ID {
			if err := d.applyRuntimeSubscription(ctx, value); err != nil {
				d.saveSubscriptionFetchError(value, err)
				return nil, err
			}
			status = "active"
			runtimeState, _ = d.State.Runtime()
		}
		return marshalResult(agentproto.RuntimeSubscriptionResult{Subscription: runtimeSubscriptionSummary(value, runtimeState.ActiveSubscriptionID), Status: status}), nil
	case agentproto.JobRefreshRuntimeSubscription:
		var params agentproto.RuntimeSubscriptionIDParams
		_ = json.Unmarshal(job.Params, &params)
		value, err := d.State.RuntimeSubscription(params.ID)
		if err != nil {
			return nil, err
		}
		d.setOperationPhase("downloading_subscription", 0, 0)
		value, err = d.fetchRuntimeSubscription(ctx, value)
		if err != nil {
			d.saveSubscriptionFetchError(value, err)
			return nil, err
		}
		value, err = d.State.SaveRuntimeSubscription(value)
		if err != nil {
			return nil, err
		}
		runtimeState, _ := d.State.Runtime()
		status := "refreshed"
		if runtimeState.ActiveSubscriptionID == value.ID {
			if err := d.applyRuntimeSubscription(ctx, value); err != nil {
				d.saveSubscriptionFetchError(value, err)
				return nil, err
			}
			status = "active"
			runtimeState, _ = d.State.Runtime()
		}
		return marshalResult(agentproto.RuntimeSubscriptionResult{Subscription: runtimeSubscriptionSummary(value, runtimeState.ActiveSubscriptionID), Status: status}), nil
	case agentproto.JobActivateRuntimeSubscription:
		var params agentproto.RuntimeSubscriptionIDParams
		_ = json.Unmarshal(job.Params, &params)
		value, err := d.State.RuntimeSubscription(params.ID)
		if err != nil {
			return nil, err
		}
		if len(value.Config) == 0 {
			d.setOperationPhase("downloading_subscription", 0, 0)
			value, err = d.fetchRuntimeSubscription(ctx, value)
			if err != nil {
				d.saveSubscriptionFetchError(value, err)
				return nil, err
			}
			if value, err = d.State.SaveRuntimeSubscription(value); err != nil {
				return nil, err
			}
		}
		if err := d.applyRuntimeSubscription(ctx, value); err != nil {
			d.saveSubscriptionFetchError(value, err)
			return nil, err
		}
		value.LastError = ""
		value, _ = d.State.SaveRuntimeSubscription(value)
		return marshalResult(agentproto.RuntimeSubscriptionResult{Subscription: runtimeSubscriptionSummary(value, value.ID), Status: "active"}), nil
	case agentproto.JobDeleteRuntimeSubscription:
		var params agentproto.RuntimeSubscriptionIDParams
		_ = json.Unmarshal(job.Params, &params)
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return nil, err
		}
		if runtimeState.ActiveSubscriptionID == params.ID {
			return nil, errors.New("cannot delete the subscription currently used by Mihomo")
		}
		if err := d.State.DeleteRuntimeSubscription(params.ID); err != nil {
			return nil, err
		}
		return marshalResult(agentproto.DeleteRuntimeSubscriptionResult{ID: params.ID, Deleted: true}), nil
	case agentproto.JobConfigureResourceProxy:
		var params agentproto.ConfigureResourceProxyParams
		_ = json.Unmarshal(job.Params, &params)
		d.setOperationPhase("configuring_resource_proxy", 0, 0)
		if err := d.configureResourceProxy(params.ResourceProxy); err != nil {
			return nil, err
		}
		return marshalResult(agentproto.ConfigureResourceProxyResult{ResourceProxy: agentproto.NormalizeResourceProxy(params.ResourceProxy)}), nil
	case agentproto.JobListCoreVersions:
		var params agentproto.ListCoreVersionsParams
		_ = json.Unmarshal(job.Params, &params)
		lister, ok := d.Core.(hostops.CoreVersionLister)
		if !ok {
			return nil, errors.New("this Agent cannot list Mihomo releases")
		}
		d.setOperationPhase("listing_releases", 0, 0)
		versions, err := lister.ListCoreVersions(ctx, params.Channel, 30)
		if err != nil {
			return nil, err
		}
		return marshalResult(agentproto.ListCoreVersionsResult{Channel: params.Channel, Versions: versions}), nil
	case agentproto.JobInstallCore:
		var params agentproto.InstallCoreParams
		_ = json.Unmarshal(job.Params, &params)
		d.setOperationPhase("installing_core", 0, 0)
		if err := d.Core.Install(ctx, params.Channel, params.Version); err != nil {
			return nil, err
		}
		return d.coreOperationResult(ctx)
	case agentproto.JobUninstallCore:
		d.setOperationPhase("uninstalling_core", 0, 0)
		if err := d.Core.Uninstall(ctx); err != nil {
			return nil, err
		}
		return d.coreOperationResult(ctx)
	case agentproto.JobStartCore:
		runtimeState, err := d.State.Runtime()
		if err != nil {
			return nil, err
		}
		if runtimeState.AppliedRevision == "" {
			return nil, errors.New("cannot start Mihomo without a locally verified applied configuration")
		}
		d.setOperationPhase("starting_core", 0, 0)
		if err := d.Core.Start(ctx); err != nil {
			return nil, err
		}
		if d.VerifyRuntime != nil {
			d.setOperationPhase("verifying_runtime", 0, 0)
			if err := d.VerifyRuntime(ctx, runtimeState.ProxyPort); err != nil {
				return nil, err
			}
		}
		return d.coreOperationResult(ctx)
	case agentproto.JobStopCore:
		d.setOperationPhase("stopping_core", 0, 0)
		if err := d.Core.Stop(ctx); err != nil {
			return nil, err
		}
		return d.coreOperationResult(ctx)
	case agentproto.JobRollbackCore:
		d.setOperationPhase("rolling_back_core", 0, 0)
		if err := d.Core.RollbackCore(ctx); err != nil {
			return nil, err
		}
		return d.coreOperationResult(ctx)
	case agentproto.JobRestartCore:
		d.setOperationPhase("restarting_core", 0, 0)
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
	default:
		return nil, errors.New("unsupported job type")
	}
}

func (d *Daemon) coreOperationResult(ctx context.Context) (json.RawMessage, error) {
	status, err := d.observeCoreStatus(ctx)
	if err != nil {
		return nil, err
	}
	return marshalResult(agentproto.CoreOperationResult{
		CoreStatus: status.State, CoreVersion: status.Version, PreviousCoreVersion: status.PreviousVersion,
	}), nil
}

func (d *Daemon) heartbeat(ctx context.Context) error {
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
	subscriptions, activeSubscriptionID, err := d.runtimeSubscriptionSummaries()
	if err != nil {
		return err
	}
	_, err = d.Control.SendHeartbeat(ctx, agentclient.Heartbeat{
		AgentVersion: d.AgentVersion, Capabilities: d.capabilities(), Status: status,
		Observation: store.RuntimeObservation{
			RemoteRevision: runtimeState.RemoteRevision, AppliedRevision: runtimeState.AppliedRevision,
			RejectedRevision: runtimeState.RejectedRevision, UpdateAvailable: runtimeState.RemoteRevision != "" && runtimeState.RemoteRevision != runtimeState.AppliedRevision,
			LastUpdateAt: runtimeState.LastUpdateAt,
			CoreVersion:  runtimeState.CoreVersion, PreviousCoreVersion: runtimeState.PreviousCoreVersion, CoreStatus: runtimeState.CoreStatus,
			AgentUptimeSeconds: uptime,
			ProxyListening:     runtimeState.CoreStatus == "running" && runtimeState.ProxyPort > 0, RecentError: runtimeState.RecentError,
			ProxyPort: runtimeState.ProxyPort, ProxyKind: runtimeState.ProxyKind, ControllerPort: runtimeState.ControllerPort,
			ResourceProxyMode: runtimeState.ResourceProxyMode, ResourceProxyURL: runtimeState.ResourceProxyURL, Operation: d.currentOperation(),
			SelectedProxies: runtimeState.SelectedProxies, SelectionNotice: runtimeState.SelectionNotice,
			LastGoodRevision:     runtimeState.LastGoodRevision,
			ActiveSubscriptionID: activeSubscriptionID, Subscriptions: subscriptions,
		},
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
	case strings.Contains(message, "mihomo startup timed out"):
		return "Mihomo startup timed out"
	case strings.Contains(message, "runtime secret"):
		return "subscription address expired or unavailable"
	case strings.Contains(message, "subscription") && (strings.Contains(message, "deadline exceeded") || strings.Contains(message, "client.timeout") || strings.Contains(message, "timeout")):
		return "subscription download timed out"
	case strings.Contains(message, "subscription") && !strings.Contains(message, "config"):
		return "subscription download failed"
	case strings.Contains(message, "socks5") || strings.Contains(message, "proxyconnect") || strings.Contains(message, "resource proxy"):
		return "Agent resource proxy connection failed"
	case strings.Contains(message, "release list"):
		return "Mihomo official release list failed"
	case strings.Contains(message, "deadline exceeded") || strings.Contains(message, "client.timeout") || strings.Contains(message, "timeout"):
		return "Mihomo release download timed out"
	case strings.Contains(message, "checksum") || strings.Contains(message, "sha-256"):
		return "integrity verification failed"
	case strings.Contains(message, "official release") || strings.Contains(message, "official asset"):
		return "Mihomo official release download failed"
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
