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
		"margin-top:52px;backdrop-filter:blur(12px)",
		"data-control-plane-link hidden",
		"fetch('/healthz'",
		"payload?.status==='ok'",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("user guide control-plane/layout contract is missing %q", required)
		}
	}
}
