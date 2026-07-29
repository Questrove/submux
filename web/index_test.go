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

func TestSubscriptionTemplateAndVersionUseSeparateSelectors(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="subscription-template"`,
		`id="subscription-template-version"`,
		`onchange="subscriptionTemplateVersionChanged()"`,
		`function renderSubscriptionTemplateVersionOptions(`,
		`version.id===template?.current_version_id?'（最新）'`,
		`renderSubscriptionTemplateVersionOptions(subscription.template_version_id)`,
		`template_version_id:versionID`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("separate subscription template/version picker is missing %q", required)
		}
	}
	if strings.Contains(html, `template_version_id:Number($('#subscription-template').value)`) {
		t.Fatal("subscription save still treats the template selector as a template version")
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
			t.Fatalf("template editor still exposes legacy remote-runtime metadata: %q", removed)
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
		"TEMPLATES.filter(template=>template.status!=='retired'&&template.current_version_id&&VERSIONS.has(template.current_version_id))",
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

func TestPlatformResourceProxyHasExplicitScope(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="setting-platform-proxy-mode"`, `id="setting-platform-proxy-url"`,
		`/api/settings/platform-resource-proxy/test`, `platform_resource_proxy:{mode,url}`,
		`id="source-fetch-mode"`, `direct_then_platform_proxy`, `/refresh-via-platform-proxy`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("platform resource proxy UI is missing %q", required)
		}
	}
}

func TestRemoteRuntimeControlSurfaceIsAbsent(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, removed := range []string{
		`data-page="runtime"`, `id="page-runtime"`, `/api/runtime/`, `/api/agent/`,
		`runtime-enrollment`, `runtime-instance`, `RUNTIME_`, `bootstrap-agent`,
		`install-gateway-agent`, `Agent 资源代理`, `一次性配对码`,
	} {
		if strings.Contains(html, removed) {
			t.Fatalf("remote runtime control surface remains: %q", removed)
		}
	}
}

func TestSharedFakeIPFilterHasOneEditorAndTemplatePreview(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`id="setting-fake-ip-mode"`,
		`id="setting-fake-ip-entries"`,
		`id="setting-fake-ip-template"`,
		`id="setting-fake-ip-preview"`,
		`/api/settings/shared-fake-ip-filter/preview?template_version_id=${version}`,
		`shared_fake_ip_filter:{mode:$('#setting-fake-ip-mode').value,entries}`,
		`共享条目排在模板专用条目前并按首次出现去重`,
		`redir-host 模板不使用这份列表`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("shared fake-IP editor is missing %q", required)
		}
	}
}

func TestConsoleUsesOnePageSnapshotRead(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(content)
	for _, required := range []string{
		`/api/console-snapshot`,
		`function applyConsoleSnapshot(snapshot)`,
		`for(const version of value.versions||[])VERSIONS.set(version.id,version)`,
		`async function reloadConsoleSnapshot()`,
		`await reloadConsoleSnapshot()`,
		`数据关联异常`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("console snapshot integration is missing %q", required)
		}
	}
	for _, removed := range []string{
		`api('GET','/api/settings')`,
		`api('GET','/api/sources')`,
		`api('GET','/api/nodes')`,
		`api('GET','/api/lifecycle-events')`,
		`api('GET','/api/templates')`,
		`api('GET','/api/rule-profiles')`,
		`api('GET','/api/subscriptions')`,
		`loadVersions`,
		`reloadSourcesAndNodes`,
		`reloadBuildState`,
	} {
		if strings.Contains(html, removed) {
			t.Fatalf("console still uses removed management read path %q", removed)
		}
	}
}
