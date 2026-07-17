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
		`function saveRuntimeBinding(`,
		`function saveRuntimeDesired(`,
		`function createRuntimeJob(`,
		`function revokeRuntimeInstance(`,
		`mihomo-agent/v1`,
		`expected_original_hash:RUNTIME_DOCKER_PREVIEW.original_hash`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime instance console is missing %q", required)
		}
	}
	for _, forbidden := range []string{"runtime exec", "runtime shell", "arbitrary command"} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Fatalf("runtime console exposes a generic execution concept: %q", forbidden)
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
		`/stream/${kind}`,
		`createRuntimeJob('test_proxy_delay'`,
		`createRuntimeJob('select_proxy'`,
		`createRuntimeJob('close_connection'`,
		`RUNTIME_LOG_FRAMES.length>200`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("runtime observation panel is missing %q", required)
		}
	}
	if strings.Contains(html, "setInterval(()=>testRuntimeProxy") {
		t.Fatal("runtime panel performs background proxy delay tests")
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
