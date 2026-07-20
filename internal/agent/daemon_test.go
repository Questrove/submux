package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/agentclient"
	"submux/internal/agentproto"
	"submux/internal/agentstate"
	"submux/internal/hostops"
	"submux/internal/mihomo"
	"submux/internal/store"
)

type fakeControl struct {
	state                 agentclient.State
	heartbeats            []agentclient.Heartbeat
	jobStatuses           []string
	jobResults            []json.RawMessage
	runtimeSecrets        map[string]agentclient.RuntimeSecret
	platformSubscriptions map[int64]agentclient.PlatformSubscription
}

func (c *fakeControl) GetState(context.Context) (agentclient.State, error) { return c.state, nil }
func (c *fakeControl) SendHeartbeat(_ context.Context, value agentclient.Heartbeat) (agentclient.State, error) {
	c.heartbeats = append(c.heartbeats, value)
	return c.state, nil
}
func (c *fakeControl) SendJobStatus(_ context.Context, _ string, status string, result json.RawMessage, _ string) error {
	c.jobStatuses = append(c.jobStatuses, status)
	c.jobResults = append(c.jobResults, append(json.RawMessage(nil), result...))
	return nil
}
func TestRuntimeStreamNotificationsAreStrictlyParsed(t *testing.T) {
	session := "0123456789abcdef0123456789abcdef0123456789abcdef"
	gotSession, kind, ok := parseRuntimeStreamReason("runtime_stream|" + session + "|logs")
	if !ok || gotSession != session || kind != "logs" {
		t.Fatalf("valid stream hint was rejected: %q %q %v", gotSession, kind, ok)
	}
	if _, kind, ok := parseRuntimeStreamReason("runtime_stream|" + session + "|agent_logs"); !ok || kind != "agent_logs" {
		t.Fatal("Agent log stream hint was rejected")
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

func TestExpectedStreamEndsAreNotReportedAsFailures(t *testing.T) {
	for _, err := range []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", io.EOF),
		errors.New("write tcp 127.0.0.1:37902->127.0.0.1:8080: write: broken pipe"),
		errors.New("read tcp: connection reset by peer"),
		errors.New("websocket: close 1000 (normal)"),
	} {
		if !isExpectedStreamEnd(err) {
			t.Fatalf("expected stream end was treated as a failure: %v", err)
		}
	}
	if err := errors.New("dial tcp 127.0.0.1:9090: connect: connection refused"); isExpectedStreamEnd(err) {
		t.Fatalf("Mihomo connection failure was hidden: %v", err)
	}
}

func TestUserAgentAdvertisesNoApplicationConfigurationCapabilities(t *testing.T) {
	for _, capability := range PlatformCapabilities() {
		if strings.HasPrefix(capability, "integration.") || capability == "subscription.update" || capability == "diagnostics.collect" {
			t.Fatalf("user Agent still advertises removed capability %q", capability)
		}
	}
	session := "0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, kind := range []string{"docker_preview", "docker_desktop_preview"} {
		if _, _, ok := parseRuntimeStreamReason("runtime_stream|" + session + "|" + kind); ok {
			t.Fatalf("removed application configuration stream %q was accepted", kind)
		}
	}
}

func (c *fakeControl) FetchRuntimeSecret(_ context.Context, ref string) (agentclient.RuntimeSecret, error) {
	value, ok := c.runtimeSecrets[ref]
	if !ok {
		return value, errors.New("missing runtime secret")
	}
	delete(c.runtimeSecrets, ref)
	return value, nil
}
func (c *fakeControl) FetchPlatformSubscription(_ context.Context, id int64) (agentclient.PlatformSubscription, error) {
	value, ok := c.platformSubscriptions[id]
	if !ok {
		return value, errors.New("missing platform subscription")
	}
	return value, nil
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

type proxyFakeCore struct {
	*fakeCore
	proxy       string
	listChannel string
	versions    []string
}

func (c *proxyFakeCore) SetResourceProxy(value string) error            { c.proxy = value; return nil }
func (*proxyFakeCore) SetProgressReporter(hostops.CoreProgressReporter) {}
func (c *proxyFakeCore) ListCoreVersions(_ context.Context, channel string, _ int) ([]string, error) {
	c.listChannel = channel
	return append([]string(nil), c.versions...), nil
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

func TestSyncOnlyObservesWhenThereIsNoOneShotJob(t *testing.T) {
	local := daemonStateStore(t)
	control := &fakeControl{state: agentclient.State{ProtocolVersion: 1}}
	core := &fakeCore{status: hostops.CoreStatus{Installed: true, Version: "v1.19.28", State: "running"}}
	deployer := &fakeDeployer{}
	daemon := &Daemon{State: local, Control: control, Core: core, Deployer: deployer, Mihomo: fakeMihomo{}, AgentVersion: "test"}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if core.installs != 0 || core.starts != 0 || core.stops != 0 || deployer.calls != 0 {
		t.Fatalf("sync mutated Mihomo without a job: core=%#v deploy=%d", core, deployer.calls)
	}
	runtimeState, _ := local.Runtime()
	if runtimeState.CoreVersion != "v1.19.28" || runtimeState.CoreStatus != "running" {
		t.Fatalf("unexpected local runtime: %#v", runtimeState)
	}
	if len(control.heartbeats) != 1 || control.heartbeats[0].Observation.Operation != nil {
		t.Fatalf("unexpected observation heartbeat: %#v", control.heartbeats)
	}
}

func TestConfigureResourceProxyJobRunsOnceAndReportsOperation(t *testing.T) {
	local := daemonStateStore(t)
	job := store.AgentJob{Job: agentproto.Job{
		ID: "proxy-job", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobConfigureResourceProxy,
		Params: json.RawMessage(`{"resource_proxy":{"mode":"custom","url":"socks5://127.0.0.1:1080"}}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "proxy-request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339),
	}}
	control := &fakeControl{state: agentclient.State{ProtocolVersion: 1, Jobs: []store.AgentJob{job}}}
	core := &proxyFakeCore{fakeCore: &fakeCore{status: hostops.CoreStatus{State: store.RuntimeNotInstalled}}}
	daemon := &Daemon{State: local, Control: control, Core: core, Deployer: &fakeDeployer{}, Mihomo: fakeMihomo{}, AgentVersion: "test"}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if core.proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("release proxy = %q", core.proxy)
	}
	lastHeartbeat := control.heartbeats[len(control.heartbeats)-1]
	if lastHeartbeat.Observation.ResourceProxyMode != agentproto.ResourceProxyCustom || lastHeartbeat.Observation.ResourceProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("custom proxy was not observed: %#v", control.heartbeats)
	}
	operation := lastHeartbeat.Observation.Operation
	if operation == nil || operation.JobID != job.ID || operation.Kind != agentproto.JobConfigureResourceProxy || operation.Status != "succeeded" || operation.Phase != "completed" {
		t.Fatalf("operation was not reported: %#v", operation)
	}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(control.jobStatuses) != 2 {
		t.Fatalf("duplicate proxy job was replayed: statuses=%#v", control.jobStatuses)
	}
}

func TestListCoreVersionsJobRunsOnAgentAndReturnsBoundedChoices(t *testing.T) {
	local := daemonStateStore(t)
	job := store.AgentJob{Job: agentproto.Job{
		ID: "versions-job", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobListCoreVersions,
		Params: json.RawMessage(`{"channel":"stable"}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "versions-request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339),
	}}
	control := &fakeControl{state: agentclient.State{ProtocolVersion: 1, Jobs: []store.AgentJob{job}}}
	core := &proxyFakeCore{fakeCore: &fakeCore{status: hostops.CoreStatus{State: store.RuntimeNotInstalled}}, versions: []string{"v1.19.28", "v1.19.27"}}
	daemon := &Daemon{State: local, Control: control, Core: core, Deployer: &fakeDeployer{}, Mihomo: fakeMihomo{}, AgentVersion: "test"}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if core.listChannel != "stable" {
		t.Fatalf("release channel = %q", core.listChannel)
	}
	var result agentproto.ListCoreVersionsResult
	if err := json.Unmarshal(control.jobResults[len(control.jobResults)-1], &result); err != nil {
		t.Fatal(err)
	}
	if result.Channel != "stable" || len(result.Versions) != 2 || result.Versions[0] != "v1.19.28" {
		t.Fatalf("release choices = %#v", result)
	}
	lastHeartbeat := control.heartbeats[len(control.heartbeats)-1]
	if operation := lastHeartbeat.Observation.Operation; operation == nil || operation.Kind != agentproto.JobListCoreVersions || operation.Status != "succeeded" {
		t.Fatalf("release list operation was not reported: %#v", operation)
	}
}

func TestRuntimeSubscriptionsAreFetchedStoredAndActivatedByAgent(t *testing.T) {
	config := []byte("mixed-port: 7890\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n")
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Subscription-Userinfo", "upload=100; download=200; total=1000; expire=1893456000")
		w.Header().Set("ETag", `"revision-1"`)
		_, _ = w.Write(config)
	}))
	defer provider.Close()

	local := daemonStateStore(t)
	secretRef := strings.Repeat("a", 48)
	addJob := store.AgentJob{Job: agentproto.Job{
		ID: "add-subscription", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobAddRuntimeSubscription,
		Params: json.RawMessage(`{"name":"临时测试","secret_ref":"` + secretRef + `"}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "add-subscription-request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339),
	}}
	control := &fakeControl{
		state:          agentclient.State{ProtocolVersion: 1, Jobs: []store.AgentJob{addJob}},
		runtimeSecrets: map[string]agentclient.RuntimeSecret{secretRef: {Kind: runtimeSubscriptionSecretKind, Value: provider.URL}},
	}
	core := &fakeCore{status: hostops.CoreStatus{Installed: true, Version: "v1.19.28", State: store.RuntimeRunning}}
	deployer := &fakeDeployer{}
	daemon := &Daemon{State: local, Control: control, Core: core, Deployer: deployer, Mihomo: fakeMihomo{}, AgentVersion: "test", SubscriptionHTTP: provider.Client()}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	values, err := local.ListRuntimeSubscriptions()
	if err != nil || len(values) != 1 {
		t.Fatalf("stored subscriptions = %#v, %v", values, err)
	}
	value := values[0]
	if value.URL != provider.URL || !strings.EqualFold(value.Host, "127.0.0.1") || !strings.EqualFold(string(value.Config), string(config)) || value.UsedBytes != 300 || value.TotalBytes != 1000 {
		t.Fatalf("stored subscription = %#v", value)
	}
	if deployer.calls != 0 {
		t.Fatalf("adding an inactive subscription applied it: %d", deployer.calls)
	}
	heartbeatJSON, _ := json.Marshal(control.heartbeats[len(control.heartbeats)-1])
	if bytes.Contains(heartbeatJSON, []byte(provider.URL)) || bytes.Contains(heartbeatJSON, config) {
		t.Fatalf("heartbeat exposed private subscription data: %s", heartbeatJSON)
	}
	observation := control.heartbeats[len(control.heartbeats)-1].Observation
	if len(observation.Subscriptions) != 1 || observation.Subscriptions[0].Name != "临时测试" || observation.Subscriptions[0].Active {
		t.Fatalf("safe subscription summary = %#v", observation.Subscriptions)
	}

	activateJob := store.AgentJob{Job: agentproto.Job{
		ID: "activate-subscription", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobActivateRuntimeSubscription,
		Params: json.RawMessage(`{"id":"` + value.ID + `"}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "activate-subscription-request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339),
	}}
	control.state.Jobs = []store.AgentJob{activateJob}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeState, _ := local.Runtime()
	if deployer.calls != 1 || runtimeState.ActiveSubscriptionID != value.ID || runtimeState.AppliedRevision != value.Revision {
		t.Fatalf("subscription was not activated: runtime=%#v deploys=%d", runtimeState, deployer.calls)
	}
}

func TestRuntimeSubscriptionCanUsePublishedPlatformConfiguration(t *testing.T) {
	config := []byte("mixed-port: 7890\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n")
	local := daemonStateStore(t)
	job := store.AgentJob{Job: agentproto.Job{
		ID: "add-platform-subscription", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobAddRuntimeSubscription,
		Params: json.RawMessage(`{"name":"平台配置","platform_subscription_id":12}`), Status: agentproto.JobQueued,
		ActorType: agentproto.ActorAdminSession, RequestID: "add-platform-subscription-request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339),
	}}
	control := &fakeControl{
		state:                 agentclient.State{ProtocolVersion: 1, Jobs: []store.AgentJob{job}},
		platformSubscriptions: map[int64]agentclient.PlatformSubscription{12: {Body: config, ContentType: "text/yaml", Revision: "published-revision"}},
	}
	daemon := &Daemon{State: local, Control: control, Core: &fakeCore{}, Deployer: &fakeDeployer{}, Mihomo: fakeMihomo{}, AgentVersion: "test"}
	if err := daemon.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	values, err := local.ListRuntimeSubscriptions()
	if err != nil || len(values) != 1 {
		t.Fatalf("stored platform subscriptions = %#v, %v", values, err)
	}
	value := values[0]
	if value.PlatformSubscriptionID != 12 || value.URL != "" || value.Host != "平台订阅" || !bytes.Equal(value.Config, config) {
		t.Fatalf("stored platform subscription = %#v", value)
	}
	observation := control.heartbeats[len(control.heartbeats)-1].Observation
	if len(observation.Subscriptions) != 1 || observation.Subscriptions[0].PlatformSubscriptionID != 12 {
		t.Fatalf("platform subscription summary = %#v", observation.Subscriptions)
	}
	heartbeatJSON, _ := json.Marshal(observation)
	if bytes.Contains(heartbeatJSON, config) {
		t.Fatalf("heartbeat exposed platform configuration: %s", heartbeatJSON)
	}
}

func TestRuntimeSubscriptionDefaultFetcherRejectsLocalAndPrivateTargets(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "::1", "fd00::1"} {
		if publicSubscriptionIP(netip.MustParseAddr(value)) {
			t.Fatalf("private subscription target %s was accepted", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !publicSubscriptionIP(netip.MustParseAddr(value)) {
			t.Fatalf("public subscription target %s was rejected", value)
		}
	}
}

func TestReleaseListErrorsDistinguishProxyAndGitHubFailures(t *testing.T) {
	if got := safeOperationError(errors.New("Mihomo startup timed out after 15s: connection refused")); got != "Mihomo startup timed out" {
		t.Fatalf("startup timeout error = %q", got)
	}
	if got := safeOperationError(errors.New("Mihomo release list: request failed")); got != "Mihomo official release list failed" {
		t.Fatalf("release list error = %q", got)
	}
	if got := safeOperationError(errors.New("Mihomo release list: proxyconnect tcp failed")); got != "Agent resource proxy connection failed" {
		t.Fatalf("release list proxy error = %q", got)
	}
}

func TestSubscriptionErrorsDoNotExposeURLOrLookLikeReleaseFailures(t *testing.T) {
	if got := safeOperationError(errors.New("download subscription: Get https://provider.example/private-token: context deadline exceeded")); got != "subscription download timed out" {
		t.Fatalf("subscription timeout = %q", got)
	}
	if got := safeOperationError(errors.New("runtime secret is missing or expired")); got != "subscription address expired or unavailable" {
		t.Fatalf("subscription secret error = %q", got)
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
