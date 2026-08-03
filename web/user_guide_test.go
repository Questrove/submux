package web

import (
	"strings"
	"testing"
)

func TestUserGuideIsEmbeddedAndCoversProductLifecycle(t *testing.T) {
	content, err := FS.ReadFile("user-guide.html")
	if err != nil {
		t.Fatalf("read embedded user guide: %v", err)
	}
	page := string(content)
	for _, required := range []string{
		"submux 软件使用说明",
		"submux 控制面",
		"Submux Runtime",
		"按操作系统安装",
		"首次使用",
		"日常操作",
		"更新与备份",
		"卸载与清理",
		"常见问题",
		"data-os=\"linux\"",
		"data-os=\"windows\"",
		"data-os=\"macos\"",
		"submux-runtime source add",
		"submux-runtime network preview --mode tun",
		"TUI 的五个页面",
		"本次运行累计",
		"Ctrl+N",
		"Ctrl+X",
		"submux-runtime backup restore --confirm --wait",
		"Uninstall-SubmuxRuntime.ps1",
		"submux-runtime-uninstall --purge",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("user guide is missing %q", required)
		}
	}
}

func TestUserGuideSocialPreviewIsEmbedded(t *testing.T) {
	content, err := FS.ReadFile("user-guide-og.png")
	if err != nil {
		t.Fatalf("read embedded user guide social preview: %v", err)
	}
	if len(content) < 10_000 {
		t.Fatalf("social preview is unexpectedly small: %d bytes", len(content))
	}
}

func TestConsoleLinksToUserGuide(t *testing.T) {
	content, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read embedded console: %v", err)
	}
	if count := strings.Count(string(content), "href=\"/user-guide.html\""); count < 3 {
		t.Fatalf("console should link to the user guide before and after login, got %d links", count)
	}
}

func TestUserGuideAlignsTheContentsAndOnlyShowsARealControlPlaneLink(t *testing.T) {
	content, err := FS.ReadFile("user-guide.html")
	if err != nil {
		t.Fatalf("read embedded user guide: %v", err)
	}
	page := string(content)
	for _, required := range []string{
		"padding:42px 30px 80px;align-items:start",
		"padding:15px;margin-top:0;backdrop-filter:blur(12px)",
		"content{min-width:0;padding-top:0}",
		".step{position:relative;min-width:0;counter-increment:steps",
		"data-control-plane-link hidden",
		"仅当本页由 submux 控制面提供时",
		"仅在本页由同一控制面提供时显示",
		"fetch('/healthz'",
		"payload?.status==='ok'",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("user guide control-plane/layout contract is missing %q", required)
		}
	}
	for _, removed := range []string{
		"margin:-26px auto 0",
		"margin-top:-24px",
		"返回 submux 配置编排台",
	} {
		if strings.Contains(page, removed) {
			t.Fatalf("user guide retained obsolete layout/control-plane text %q", removed)
		}
	}
}

func TestUserGuideMatchesRuntimeTUIWorkflowAndSafetyBoundary(t *testing.T) {
	content, err := FS.ReadFile("user-guide.html")
	if err != nil {
		t.Fatalf("read embedded user guide: %v", err)
	}
	page := string(content)
	for _, required := range []string{
		"submux-runtime tui",
		"状态页会根据 Runtime Snapshot 显示六个步骤",
		"确认前不会修改 Runtime 状态",
		"首次按键只检查，再按一次才进入统一确认",
		"卸载不由正在运行的 TUI 执行",
		"先恢复直连，再卸载程序",
		"默认卸载会保留配置和状态",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("user guide TUI workflow is missing %q", required)
		}
	}
}
