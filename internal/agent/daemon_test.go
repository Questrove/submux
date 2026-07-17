package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/agentclient"
	"submux/internal/agentproto"
	"submux/internal/agentstate"
	"submux/internal/hostops"
	"submux/internal/integration"
	"submux/internal/mihomo"
	"submux/internal/store"
)

type fakeControl struct {
	state          agentclient.State
	artifact       agentclient.Artifact
	heartbeats     []agentclient.Heartbeat
	jobStatuses    []string
	artifactChecks int
}

func (c *fakeControl) GetState(context.Context) (agentclient.State, error) { return c.state, nil }
func (c *fakeControl) SendHeartbeat(_ context.Context, value agentclient.Heartbeat) (agentclient.State, error) {
	c.heartbeats = append(c.heartbeats, value)
	return c.state, nil
}
func (c *fakeControl) SendJobStatus(_ context.Context, _ string, status string, _ json.RawMessage, _ string) error {
	c.jobStatuses = append(c.jobStatuses, status)
	return nil
}
func (c *fakeControl) CheckArtifact(_ context.Context, _ int64, etag string) (agentclient.Artifact, error) {
	c.artifactChecks++
	if etag != "" && etag == c.artifact.ETag {
		result := c.artifact
		result.NotModified, result.Body = true, nil
		return result, nil
	}
	result := c.artifact
	result.Body = nil
	return result, nil
}

func TestManualCheckBypassesIntervalWithoutApplyingWhenAutoUpdateIsOff(t *testing.T) {
	local := daemonStateStore(t)
	_, err := local.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.BindingID = 7
		runtime.LastCheckAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	control := &fakeControl{
		state: agentclient.State{
			Desired: store.RuntimeDesiredState{InstanceID: 1, CoreInstalled: true, CoreChannel: "stable", CoreVersion: "v1.19.28", RuntimeState: store.RuntimeStopped},
			Binding: &store.RuntimeBinding{ID: 7, InstanceID: 1, AutoUpdate: false, CheckIntervalSec: 3600},
		},
		artifact: agentclient.Artifact{Body: []byte("config"), Revision: "new", SHA256: "hash", ETag: `"new"`, ContentType: "application/yaml"},
	}
	deployer := &fakeDeployer{}
	daemon := &Daemon{
		State: local, Control: control,
		Core:     &fakeCore{status: hostops.CoreStatus{Installed: true, Version: "v1.19.28", State: "stopped"}},
		Deployer: deployer, Mihomo: fakeMihomo{},
	}
	if deployment, err := daemon.CheckSubscriptionNow(context.Background(), false); err != nil || deployment != nil {
		t.Fatalf("manual check: deployment=%#v err=%v", deployment, err)
	}
	if control.artifactChecks != 1 || deployer.calls != 0 {
		t.Fatalf("manual check did not preserve apply policy: checks=%d applies=%d", control.artifactChecks, deployer.calls)
	}
	if _, err := daemon.CheckSubscriptionNow(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if deployer.calls != 1 {
		t.Fatalf("explicit update did not apply: %d", deployer.calls)
	}
}

func TestRuntimeStreamNotificationsAreStrictlyParsed(t *testing.T) {
	session := "0123456789abcdef0123456789abcdef0123456789abcdef"
	gotSession, kind, ok := parseRuntimeStreamReason("runtime_stream|" + session + "|logs")
	if !ok || gotSession != session || kind != "logs" {
		t.Fatalf("valid stream hint was rejected: %q %q %v", gotSession, kind, ok)
	}
	for _, value := range []string{
		"runtime_stream|short|logs",
		"runtime_stream|zz23456789abcdef0123456789abcdef0123456789abcdef|logs",
		"runtime_stream|" + session + "|exec",
		"runtime_stream|" + session + "|logs|extra",
	} {
		if _, _, ok := parseRuntimeStreamReason(value); ok {
			t.Fatalf("unsafe stream hint was accepted: %q", value)
		}
	}
}

type fakeDockerManager struct {
	preview     integration.DockerPreview
	status      integration.DockerStatus
	enableCalls int
}

func (m *fakeDockerManager) Status(context.Context) (integration.DockerStatus, error) {
	return m.status, nil
}
func (m *fakeDockerManager) Preview(context.Context, integration.DockerDaemonConfig) (integration.DockerPreview, error) {
	return m.preview, nil
}
func (m *fakeDockerManager) Enable(context.Context, integration.DockerDaemonConfig, string) (integration.DockerStatus, error) {
	m.enableCalls++
	return integration.DockerStatus{State: "active"}, nil
}
func (m *fakeDockerManager) Disable(context.Context) (integration.DockerStatus, error) {
	return integration.DockerStatus{State: "disabled"}, nil
}

func TestDockerReconcileRejectsConfigurationChangedAfterPreview(t *testing.T) {
	local := daemonStateStore(t)
	_, _ = local.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.CoreStatus, runtime.ProxyPort, runtime.ProxyKind = "running", 7890, "mixed"
		return nil
	})
	docker := &fakeDockerManager{
		status:  integration.DockerStatus{State: "disabled"},
		preview: integration.DockerPreview{OriginalHash: strings.Repeat("a", 64), DesiredHash: strings.Repeat("c", 64)},
	}
	daemon := &Daemon{State: local, Docker: docker}
	config, _ := json.Marshal(integration.DockerDaemonConfig{
		Enabled: true, ProxyPort: 7890, Revision: "confirmed", ExpectedOriginalHash: strings.Repeat("b", 64),
	})
	desired := store.RuntimeDesiredState{Integrations: map[string]json.RawMessage{integration.DockerDaemonType: config}}
	if err := daemon.reconcileIntegrations(context.Background(), desired); err == nil {
		t.Fatal("Docker configuration changed after preview but was applied")
	}
	if docker.enableCalls != 0 {
		t.Fatalf("Docker enable ran despite stale preview: %d", docker.enableCalls)
	}
}

func TestDockerReconcileRequiresCurrentRunningHTTPProxy(t *testing.T) {
	for _, test := range []struct {
		name        string
		coreStatus  string
		proxyPort   int
		proxyKind   string
		desiredPort int
	}{
		{"stopped", "stopped", 7890, "mixed", 7890},
		{"stale-port", "running", 7890, "mixed", 7891},
		{"socks-only", "running", 7890, "socks5", 7890},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := daemonStateStore(t)
			_, _ = local.UpdateRuntime(func(runtime *agentstate.Runtime) error {
				runtime.CoreStatus, runtime.ProxyPort, runtime.ProxyKind = test.coreStatus, test.proxyPort, test.proxyKind
				return nil
			})
			docker := &fakeDockerManager{
				status:  integration.DockerStatus{State: "disabled"},
				preview: integration.DockerPreview{OriginalHash: strings.Repeat("a", 64), DesiredHash: strings.Repeat("b", 64)},
			}
			daemon := &Daemon{State: local, Docker: docker}
			config, _ := json.Marshal(integration.DockerDaemonConfig{
				Enabled: true, ProxyPort: test.desiredPort, Revision: "confirmed", ExpectedOriginalHash: strings.Repeat("a", 64),
			})
			desired := store.RuntimeDesiredState{Integrations: map[string]json.RawMessage{integration.DockerDaemonType: config}}
			if err := daemon.reconcileIntegrations(context.Background(), desired); err == nil {
				t.Fatal("unsafe Docker proxy state was accepted")
			}
			if docker.enableCalls != 0 {
				t.Fatalf("Docker enable ran for unsafe proxy state: %d", docker.enableCalls)
			}
		})
	}
}

func (c *fakeControl) FetchArtifact(context.Context, int64, string) (agentclient.Artifact, error) {
	return c.artifact, nil
}
func (c *fakeControl) WatchUpdates(ctx context.Context, _ func(string)) error {
	<-ctx.Done()
	return ctx.Err()
}

type fakeCore struct {
	status                       hostops.CoreStatus
	installs, uninstalls, starts int
	stops, restarts, validations int
}

func (c *fakeCore) Install(_ context.Context, _, version string) error {
	c.installs++
	c.status = hostops.CoreStatus{Installed: true, Version: version, PreviousVersion: c.status.Version, State: "stopped"}
	return nil
}
func (c *fakeCore) Uninstall(context.Context) error {
	c.uninstalls++
	c.status = hostops.CoreStatus{State: "not_installed"}
	return nil
}
func (c *fakeCore) RollbackCore(context.Context) error {
	c.status.Version, c.status.PreviousVersion = c.status.PreviousVersion, c.status.Version
	return nil
}
func (c *fakeCore) Status(context.Context) (hostops.CoreStatus, error) { return c.status, nil }
func (c *fakeCore) Start(context.Context) error                        { c.starts++; c.status.State = "running"; return nil }
func (c *fakeCore) Stop(context.Context) error                         { c.stops++; c.status.State = "stopped"; return nil }
func (c *fakeCore) Restart(context.Context) error {
	c.restarts++
	c.status.State = "running"
	return nil
}
func (c *fakeCore) ReloadOrRestart(context.Context) error        { return c.Restart(context.Background()) }
func (c *fakeCore) ValidateConfig(context.Context, string) error { c.validations++; return nil }
func (c *fakeCore) Logs(context.Context) (string, error)         { return "", nil }

type fakeDeployer struct {
	calls    int
	failures int
}

func (d *fakeDeployer) Apply(_ context.Context, revision, hash string, _ []byte) (mihomo.DeploymentResult, error) {
	d.calls++
	if d.calls <= d.failures {
		return mihomo.DeploymentResult{Revision: revision, ArtifactHash: hash, Status: "failed", Validation: "rejected", Error: "invalid"}, errors.New("validate config")
	}
	return mihomo.DeploymentResult{Revision: revision, ArtifactHash: hash, EffectiveHash: "effective", Status: "active", Validation: "passed", ProxyPort: 7890, ProxyKind: "mixed"}, nil
}
func (*fakeDeployer) Rollback(context.Context) (mihomo.DeploymentResult, error) {
	return mihomo.DeploymentResult{Revision: "rolled-back", PreviousRevision: "current", Status: "active"}, nil
}

type fakeMihomo struct{}

func (fakeMihomo) Delay(context.Context, string, string, time.Duration) (int, error) { return 20, nil }
func (fakeMihomo) Select(context.Context, string, string) error                      { return nil }
func (fakeMihomo) CloseConnection(context.Context, string) error                     { return nil }
func (fakeMihomo) Proxies(context.Context) (map[string]mihomo.Proxy, error) {
	return map[string]mihomo.Proxy{"select": {Name: "select", Type: "Selector", Now: "A", All: []string{"A"}}, "A": {Name: "A", Type: "VLESS"}}, nil
}

func daemonStateStore(t *testing.T) *agentstate.Store {
	t.Helper()
	value, err := agentstate.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.Close() })
	return value
}

func TestLocalCoreOperationsImmediatelyRefreshObservedState(t *testing.T) {
	local := daemonStateStore(t)
	core := &fakeCore{status: hostops.CoreStatus{Installed: true, Version: "v1.0.0", State: "stopped"}}
	daemon := &Daemon{State: local, Core: core}
	if err := daemon.InstallCoreNow(context.Background(), "stable", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	runtimeState, _ := local.Runtime()
	if runtimeState.CoreVersion != "v1.1.0" || runtimeState.PreviousCoreVersion != "v1.0.0" || runtimeState.CoreStatus != "stopped" {
		t.Fatalf("install status was not refreshed: %#v", runtimeState)
	}
	if err := daemon.RestartCoreNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeState, _ = local.Runtime()
	if runtimeState.CoreStatus != "running" {
		t.Fatalf("restart status was not refreshed: %#v", runtimeState)
	}
	if err := daemon.RollbackCoreNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeState, _ = local.Runtime()
	if runtimeState.CoreVersion != "v1.0.0" || runtimeState.PreviousCoreVersion != "v1.1.0" || runtimeState.CoreStatus != "running" {
		t.Fatalf("rollback status was not refreshed: %#v", runtimeState)
	}
}

func TestSyncReconcilesAndDoesNotRepeatReachedGenerationOrArtifact(t *testing.T) {
	local := daemonStateStore(t)
	control := &fakeControl{
		state:    agentclient.State{ProtocolVersion: 1, Desired: store.RuntimeDesiredState{InstanceID: 1, Generation: 2, CoreInstalled: true, CoreChannel: "stable", CoreVersion: "v1.19.28", RuntimeState: store.RuntimeStopped}, Binding: &store.RuntimeBinding{ID: 3, InstanceID: 1, RuntimeContract: "mihomo-agent/v1", AutoUpdate: true, CheckIntervalSec: 300}},
		artifact: agentclient.Artifact{Body: []byte("config"), Revision: "revision", SHA256: "hash", ETag: `"revision"`, ContentType: "application/yaml"},
	}
	core := &fakeCore{status: hostops.CoreStatus{State: "not_installed"}}
	deployer := &fakeDeployer{}
	daemon := &Daemon{State: local, Control: control, Core: core, Deployer: deployer, Mihomo: fakeMihomo{}, AgentVersion: "test"}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if core.installs != 1 || deployer.calls != 1 {
		t.Fatalf("install=%d deploy=%d", core.installs, deployer.calls)
	}
	runtimeState, _ := local.Runtime()
	if runtimeState.ObservedGeneration != 2 || runtimeState.AppliedRevision != "revision" || runtimeState.ProxyPort != 7890 || runtimeState.ProxyKind != "mixed" {
		t.Fatalf("unexpected local runtime: %#v", runtimeState)
	}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if core.installs != 1 || deployer.calls != 1 {
		t.Fatalf("reached state repeated side effects: install=%d deploy=%d", core.installs, deployer.calls)
	}
}

func TestExplicitUpdateRetriesRejectedRevisionAndDuplicateJobDoesNotReplay(t *testing.T) {
	local := daemonStateStore(t)
	job := store.AgentJob{Job: agentproto.Job{
		ID: "update", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobUpdateSubscription,
		Params: json.RawMessage(`{"retry_rejected":true}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339),
	}}
	control := &fakeControl{
		state:    agentclient.State{ProtocolVersion: 1, Desired: store.RuntimeDesiredState{InstanceID: 1, Generation: 1, CoreInstalled: true, CoreChannel: "stable", CoreVersion: "v1.19.28", RuntimeState: store.RuntimeStopped}, Binding: &store.RuntimeBinding{ID: 1, InstanceID: 1, AutoUpdate: true, CheckIntervalSec: 300}},
		artifact: agentclient.Artifact{Body: []byte("config"), Revision: "bad-revision", SHA256: "hash", ETag: `"bad-revision"`, ContentType: "application/yaml"},
	}
	core := &fakeCore{status: hostops.CoreStatus{Installed: true, Version: "v1.19.28", State: "stopped"}}
	deployer := &fakeDeployer{failures: 1}
	daemon := &Daemon{State: local, Control: control, Core: core, Deployer: deployer, Mihomo: fakeMihomo{}}
	if err := daemon.SyncOnce(context.Background()); err == nil {
		t.Fatal("automatic rejected deployment unexpectedly succeeded")
	}
	runtimeState, _ := local.Runtime()
	if runtimeState.RejectedRevision != "bad-revision" || deployer.calls != 1 {
		t.Fatalf("rejected state not persisted: %#v calls=%d", runtimeState, deployer.calls)
	}
	control.state.Jobs = []store.AgentJob{job}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeState, _ = local.Runtime()
	if runtimeState.AppliedRevision != "bad-revision" || deployer.calls != 2 {
		t.Fatalf("explicit retry did not apply: %#v calls=%d", runtimeState, deployer.calls)
	}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deployer.calls != 2 {
		t.Fatalf("duplicate job replayed deployment: %d", deployer.calls)
	}
}

type selectionMihomo struct {
	proxies map[string]mihomo.Proxy
	selects []string
}

func (m *selectionMihomo) Delay(context.Context, string, string, time.Duration) (int, error) {
	return 1, nil
}
func (m *selectionMihomo) CloseConnection(context.Context, string) error { return nil }
func (m *selectionMihomo) Proxies(context.Context) (map[string]mihomo.Proxy, error) {
	result := make(map[string]mihomo.Proxy, len(m.proxies))
	for name, proxy := range m.proxies {
		result[name] = proxy
	}
	return result, nil
}
func (m *selectionMihomo) Select(_ context.Context, group, proxy string) error {
	selector := m.proxies[group]
	selector.Now = proxy
	m.proxies[group] = selector
	m.selects = append(m.selects, group+"="+proxy)
	return nil
}

func TestDeploymentRestoresOnlyStillObservedProxySelections(t *testing.T) {
	local := daemonStateStore(t)
	_, _ = local.UpdateRuntime(func(runtime *agentstate.Runtime) error {
		runtime.SelectedProxies = map[string]string{"PROXY": "A", "removed-group": "gone"}
		return nil
	})
	client := &selectionMihomo{proxies: map[string]mihomo.Proxy{
		"PROXY": {Name: "PROXY", Type: "Selector", Now: "B", All: []string{"A", "B"}},
		"A":     {Name: "A", Type: "Trojan"}, "B": {Name: "B", Type: "Trojan"},
	}}
	daemon := &Daemon{State: local, Mihomo: client}
	notice := daemon.restoreProxySelections(context.Background())
	if len(client.selects) != 1 || client.selects[0] != "PROXY=A" {
		t.Fatalf("unexpected restored selections: %#v", client.selects)
	}
	if !strings.Contains(notice, "removed-group") {
		t.Fatalf("missing fallback notice: %q", notice)
	}
	runtimeState, _ := local.Runtime()
	if runtimeState.SelectedProxies["PROXY"] != "A" {
		t.Fatalf("observed selection was not refreshed: %#v", runtimeState.SelectedProxies)
	}
}
