package web

import (
	"strings"
	"testing"
)

func TestNodeMetadataUsesOneDialogWithoutAlias(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="node-metadata-dialog"`,
		`id="node-detail-config"`,
		`id="node-detail-secret-toggle"`,
		`id="node-metadata-tags"`,
		`id="node-metadata-enabled"`,
		`id="node-metadata-role"`,
		`onclick="openNodeMetadata(`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("node metadata dialog is missing %q", required)
		}
	}
	for _, removed := range []string{"node.alias", "显示别名", "editNodeRole("} {
		if strings.Contains(html, removed) {
			t.Fatalf("removed alias or secondary editor remains: %q", removed)
		}
	}
}

func TestRuntimeInstanceConsoleUsesTypedControlPlaneAPIs(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`data-page="runtime"`,
		`id="page-runtime"`,
		`/api/runtime/enrollments`,
		`/api/runtime/instances/${id}`,
		`id="runtime-reconnect"`,
		`function reconnectActiveRuntime()`,
		`button.textContent='连接中…'`,
		`notify('已重新连接')`,
		`id="runtime-agent-card"`,
		`id="runtime-mihomo-card"`,
		`id="runtime-agent-resource-proxy-current"`,
		`id="runtime-mihomo-last-good-current"`,
		`function saveRuntimeSubscription(`,
		`function activateRuntimeSubscription(`,
		`id="runtime-subscription-source"`,
		`id="runtime-subscription-platform"`,
		`platform_subscription_id:platformID`,
		`availablePlatformRuntimeSubscriptions()`,
		`/secrets`,
		`function configureRuntimeResourceProxy(`,
		`function installRuntimeCore(`,
		`function createRuntimeJob(`,
		`function revokeRuntimeInstance(`,
		`完整 Mihomo YAML`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime instance console is missing %q", required)
		}
	}
	for _, forbidden := range []string{"runtime exec", "runtime shell", "arbitrary command", "function saveRuntimeBinding(", "function saveRuntimeDesired(", "desired_integrations:{}", "runtime-binding-subscription", "function applyRuntimeSubscription("} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Fatalf("runtime console exposes a generic execution concept: %q", forbidden)
		}
	}
	if strings.Contains(html, "collect_diagnostics") || strings.Contains(html, "update_subscription") || !strings.Contains(html, `class="danger runtime-danger"`) {
		t.Fatal("runtime header still exposes diagnostics or lacks the solid revoke button")
	}
	for _, numbered := range []string{`>5. 运行实例</button>`, `>6. 代理设置指南</button>`} {
		if strings.Contains(html, numbered) {
			t.Fatalf("top navigation still presents a non-sequential page as a numbered step: %q", numbered)
		}
	}
	for _, redundant := range []string{`<h3>当前运行信息</h3>`, `<h3>资源状态</h3>`, `<h3>最近部署</h3>`, `id="runtime-observation"`, `id="runtime-memory-bar"`, `id="runtime-deployment-rows"`} {
		if strings.Contains(html, redundant) {
			t.Fatalf("runtime overview still contains a redundant status panel: %q", redundant)
		}
	}
	if strings.Contains(html, `function refreshActiveRuntime()`) || strings.Contains(html, `>刷新状态</button>`) {
		t.Fatal("runtime header still exposes the old no-feedback status refresh")
	}
}

func TestNavigationStateSurvivesPageReload(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`const NAVIGATION_STATE_KEY='submux.navigation.v1'`,
		`sessionStorage.getItem(NAVIGATION_STATE_KEY)`,
		`sessionStorage.setItem(NAVIGATION_STATE_KEY`,
		`function activatePage(page,persist=true)`,
		`async function restoreNavigationState()`,
		`await loadAll();await restoreNavigationState();`,
		`async function openRuntimeInstance(id,view='overview',persist=true)`,
		`function showRuntimeView(view,persist=true)`,
		`runtimeInstanceID`,
		`RUNTIME_VIEW_NAMES.has(state.runtimeView)`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("navigation reload state is missing %q", required)
		}
	}
}

func TestProxyGuideIsInstructionOnlyAndCoversCommonSoftware(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`data-page="proxy-guide"`, `id="page-proxy-guide"`, `function proxyGuideDefinitions(`,
		`function copyGuideCommand(`, `输入代理地址，查看常见软件的配置步骤和命令`,
		`Git`, `APT`, `DNF / YUM`, `npm / pnpm / Yarn Classic`, `pip`,
		`Docker Engine 拉取镜像`, `Docker Desktop`, `systemd 中的指定服务`, `Windows 系统代理`,
		`127.0.0.1`, `Number.isInteger(httpPort)`, `noProxyItems.every(`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("proxy guide is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`docker_preview`, `docker_desktop_preview`, `confirmRuntimeDocker`, `RUNTIME_DOCKER_PREVIEW`,
		`/v1/proxy/docker/enable`, `/v1/proxy/docker-desktop/enable`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("proxy guide retains an automatic configuration path: %q", forbidden)
		}
	}
}

func TestRuntimePanelUsesOnDemandStreamsAndExplicitProxyJobs(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`function openRuntimeStream(kind)`,
		`function startRuntimeOverviewStreams()`,
		`function showRuntimeView(view,persist=true)`,
		`/stream/${kind}`,
		`createRuntimeJob('test_proxy_delay'`,
		`createRuntimeJob('select_proxy'`,
		`createRuntimeJob('close_connection'`,
		`state.frames.length>300`,
		`agent_logs`,
		`/events`,
		`function renderRuntimeOperation()`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime observation panel is missing %q", required)
		}
	}
	if strings.Contains(html, "setInterval(()=>testRuntimeProxy") {
		t.Fatal("runtime panel performs background proxy delay tests")
	}
}

func TestRuntimeAgentLogsIgnoreReplayedFrames(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`stream_id`,
		`state.seen.has(logID)`,
		`state.seenOrder.length>600`,
		`RUNTIME_LOG_STATES=createRuntimeLogStates()`,
		`function clearRuntimeLogs(){const state=runtimeLogState();if(state)state.frames=[]`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime log resume protection is missing %q", required)
		}
	}
}

func TestRuntimePanelConfiguresAgentResourceProxyAndTracksAsyncActions(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="runtime-resource-proxy-mode"`,
		`id="runtime-resource-proxy-url"`,
		`<select id="runtime-core-version"`,
		`function loadRuntimeCoreVersions()`,
		`createRuntimeJob('list_core_versions',{channel})`,
		`job.type==='list_core_versions'&&job.params?.channel===channel`,
		`http://127.0.0.1:1080`,
		`socks5://127.0.0.1:1080`,
		`createRuntimeJob('configure_resource_proxy',{resource_proxy:{mode,url}})`,
		`createRuntimeJob('install_core',{channel,version})`,
		`runRuntimeCoreAction('start_core')`,
		`runRuntimeCoreAction('stop_core')`,
		`runRuntimeCoreAction('rollback_core')`,
		`runRuntimeCoreAction('uninstall_core')`,
		`function openRuntimeEvents()`,
		`function scheduleRuntimeEventRefresh(instanceID)`,
		`id="runtime-operation"`,
		`id="runtime-log-source-agent"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime asynchronous control is missing %q", required)
		}
	}
	for _, removed := range []string{"use_mihomo_after_install", "current_mihomo", "expected_generation", "observed_generation"} {
		if strings.Contains(html, removed) {
			t.Fatalf("Agent resource proxy retains automatic switching mode %q", removed)
		}
	}
	if strings.Contains(html, `<input id="runtime-core-version"`) {
		t.Fatal("Mihomo version remains a free-form input")
	}
}

func TestRuntimeWorkspaceUsesTaskOrientedDashboardLayout(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`class="runtime-layout"`,
		`id="runtime-instance-list"`,
		`class="runtime-stat-grid"`,
		`data-runtime-view="overview"`,
		`data-runtime-view="proxies"`,
		`data-runtime-view="connections"`,
		`data-runtime-view="logs"`,
		`data-runtime-view="config"`,
		`data-runtime-view="activity"`,
		`id="runtime-traffic-chart"`,
		`.runtime-overview-grid>div{display:flex}`,
		`id="runtime-proxy-filter"`,
		`id="runtime-connection-filter"`,
		`id="runtime-log-filter"`,
		`function renderRuntimeProxyGroups()`,
		`function renderRuntimeConnections()`,
		`font-variant-numeric:tabular-nums`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("task-oriented runtime workspace is missing %q", required)
		}
	}
	for _, removed := range []string{`id="runtime-rows"`, `id="runtime-proxy-rows"`, `id="runtime-traffic"`, `id="runtime-memory"`} {
		if strings.Contains(html, removed) {
			t.Fatalf("legacy flat runtime panel remains: %q", removed)
		}
	}
}

func TestRuntimeTrafficChartIncludesSpeedAndTimeAxes(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`.runtime-chart-axis`,
		`.runtime-chart-grid`,
		`RUNTIME_TRAFFIC_HISTORY.push({up,down,at:Date.now()})`,
		`function formatRuntimeTrafficTime(value)`,
		`function runtimeTrafficScale(value)`,
		`formatBytes(level*max)`,
		`formatRuntimeTrafficTime(values[index].at)`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime traffic chart is missing coordinate support %q", required)
		}
	}
}

func TestRuntimeProxyGroupsSeparateBuiltinsGroupsAndNodes(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`class="runtime-toolbar-actions"`,
		`.runtime-proxy-grid{grid-template-columns:repeat(auto-fit`,
		`const RUNTIME_PROXY_GROUP_TYPES=`,
		`function runtimeProxyKind(name)`,
		`function runtimeProxySelectionPath(name)`,
		`String(proxy.name||'').toUpperCase()!=='GLOBAL'`,
		`kind==='node'?runtimeLatencyHTML(delay)`,
		"kind==='node'?`<button class=\"runtime-node-test",
		`查看策略组`,
		`data-runtime-proxy-group=`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime proxy view does not separate proxy kinds: missing %q", required)
		}
	}
	if strings.Contains(html, `filter(proxy=>proxy.type==='Selector')`) {
		t.Fatal("runtime proxy view still renders Mihomo's GLOBAL selector without filtering")
	}
}

func TestRuntimeCopyDescribesFeaturesWithoutProtocolCommentary(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`查看 Agent、Mihomo、流量、连接和任务状态。`,
		`查看 Mihomo 当前加载的配置。`,
		`查看 Mihomo 当前加载的规则。`,
		`供当前 Agent 读取版本和下载 Mihomo 核心，其他请求不使用这里的设置。`,
		`},'任务已提交');`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime feature copy is missing %q", required)
		}
	}
	for _, removed := range []string{
		`一次性操作已提交，页面会持续显示进度和结果`,
		`应用下载代理`,
		`只读展示，不反向写入模板。`,
		`敏感键在 Agent 侧移除。`,
		`每个按钮只执行一次，完成后以 Agent 上报状态为准。`,
		`不保存期望状态`,
	} {
		if strings.Contains(html, removed) {
			t.Fatalf("runtime page retains implementation-oriented copy %q", removed)
		}
	}
}

func TestRuntimeEnrollmentOffersSafeOneCommandBootstrap(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="runtime-enrollment-server"`,
		`id="runtime-enrollment-version"`,
		`id="runtime-bootstrap-command"`,
		`function runtimeBootstrapCommand()`,
		`function copyRuntimeBootstrapCommand()`,
		`scripts/bootstrap-agent.sh`,
		`--require-bundle`,
		`overflow-wrap:anywhere`,
		`word-break:break-all`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime one-command enrollment is missing %q", required)
		}
	}
	if strings.Contains(html, `id="runtime-enrollment-code" style=`) {
		t.Fatal("pairing code retains an unbounded inline layout")
	}
}

func TestNodeNameOpensDetailsAndSensitiveConfigIsMasked(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`class="node-link"`,
		`function maskNodeConfig(`,
		`function toggleNodeConfigSecrets(`,
		`password|passwd|uuid|token|auth|secret`,
		`NODE_CONFIG_SECRETS_VISIBLE=false`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("node detail behavior is missing %q", required)
		}
	}
}

func TestInformationNodeClassificationIsRenderedWithTags(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	if !strings.Contains(html, `<th>分类 / 标签</th>`) {
		t.Fatal("node classification and tags column has an ambiguous heading")
	}
	if !strings.Contains(html, `function nodeTagsHTML(node)`) || !strings.Contains(html, `<td>${nodeTagsHTML(node)}</td>`) {
		t.Fatal("information node classification is not rendered in the tags column")
	}
	if strings.Contains(html, `${esc(node.protocol)}</span> ${node.role==='notice'`) {
		t.Fatal("information node classification remains in the protocol column")
	}
	if !strings.Contains(html, `[classification,tags].filter(Boolean).join(' ')||'<span class="muted">—</span>'`) {
		t.Fatal("empty user tags still render a placeholder beside the information-node classification")
	}
}

func TestManualImportUsesBuiltinGroupWithoutSourcePicker(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`自动保存到内置“自建节点”分组`,
		`{content:$('#import-content').value}`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("simplified manual import is missing %q", required)
		}
	}
	for _, removed := range []string{`id="source-kind"`, `id="import-source"`, "renderSourceOptions"} {
		if strings.Contains(html, removed) {
			t.Fatalf("manual source setup remains in UI: %q", removed)
		}
	}
}

func TestTemplateActionButtonsDoNotCollapseOrWrap(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		"grid-template-columns:minmax(0,1fr) max-content",
		".template-item .actions button{min-width:72px;white-space:nowrap",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("stable template action layout is missing %q", required)
		}
	}
}

func TestTemplateNameOpensCurrentVersionEditor(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	if !strings.Contains(html, `<h4><button class="node-link" type="button" onclick="selectTemplate(${template.id})">${esc(template.name)}</button></h4>`) {
		t.Fatal("template name does not open its current version editor")
	}
}

func TestTemplateEditorDoesNotExposeLegacyRuntimeContract(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, removed := range []string{`id="template-runtime-contract"`, `运行契约（可空）`, `version?.runtime_contract`, `runtime_contract:$('#template-runtime-contract')`} {
		if strings.Contains(html, removed) {
			t.Fatalf("template editor still exposes legacy Agent binding metadata: %q", removed)
		}
	}
}

func TestTemplateEditorInfersStandardNodeGroupsWithoutSlotJSON(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`Mihomo 模板使用 <code>PROXY</code> 作为主代理组`,
		`content:$('#template-content').value`,
		`function slotLabel(key)`,
		`primary:'主代理节点',media:'流媒体节点'`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("standard node group UI is missing %q", required)
		}
	}
	for _, removed := range []string{`节点插槽 JSON`, `id="template-slots"`, `slots:JSON.parse`, `个模板插槽`} {
		if strings.Contains(html, removed) {
			t.Fatalf("template slot implementation detail remains visible: %q", removed)
		}
	}
}

func TestRetiredTemplatesAreHiddenFromCatalogAndNewSubscriptions(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		"TEMPLATES.filter(template=>template.status!=='retired').map(template=>",
		"TEMPLATES.filter(template=>template.status!=='retired'&&template.current_version_id)",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("retired template compatibility records remain selectable: missing %q", required)
		}
	}
}

func TestNodeDetectionIsNotExposed(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, removed := range []string{
		`triggerHealthCheck`,
		`setting-health-enabled`,
		`nodeHealthHTML`,
		`稳定率`,
		`端点可达`,
	} {
		if strings.Contains(html, removed) {
			t.Fatalf("node detection UI remains: %q", removed)
		}
	}
}

func TestRuleProfilesUseFullCatalogAndOrderedSelections(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`data-page="rules"`, `id="page-rules"`, `id="rule-profile-list"`,
		`id="rule-selected-list"`, `id="rule-catalog-list"`, `id="rule-custom-list"`,
		`/api/rule-catalog`, `/api/rule-profiles`, `function moveRuleSelection(`,
		`function addCustomRule(`, `function renderRuleCatalog(`, `rule_profile_id:`,
		`id="subscription-rule-profile"`, `流媒体代理`, `MetaCubeX 规则目录`,
		`/api/rule-catalog/refresh`, `/catalog-version`, `更新规则版本`,
		`AVAILABLE_RULE_CATALOG`, `profile.catalog_commit`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("rule profile UI is missing %q", required)
		}
	}
	if strings.Contains(html, "把全部规则同时写入") {
		t.Fatal("rule UI exposes an all-rules output mode")
	}
}

func TestPlatformAndAgentResourceProxiesHaveSeparateScopes(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="setting-platform-proxy-mode"`, `id="setting-platform-proxy-url"`,
		`/api/settings/platform-resource-proxy/test`, `platform_resource_proxy:{mode,url}`,
		`id="source-fetch-mode"`, `direct_then_platform_proxy`, `/refresh-via-platform-proxy`,
		`id="runtime-resource-proxy-mode"`, `id="runtime-resource-proxy-url"`,
		`configure_resource_proxy`, `resource_proxy:{mode,url}`,
		`供当前 Agent 读取版本和下载 Mihomo 核心`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("separate resource proxy UI is missing %q", required)
		}
	}
	for _, removed := range []string{`id="runtime-download-proxy-mode"`, `configure_download_proxy`, `download_proxy:{`} {
		if strings.Contains(html, removed) {
			t.Fatalf("legacy Mihomo download proxy UI remains: %q", removed)
		}
	}
}
