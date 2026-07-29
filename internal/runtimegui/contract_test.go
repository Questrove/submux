package runtimegui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebViewOnlyUsesAllowlistedTauriCommands(t *testing.T) {
	script := readGUIFile(t, "ui", "app.js")
	for _, forbidden := range []string{
		"fetch(",
		"WebSocket(",
		"XMLHttpRequest",
		`\\\\.\\pipe\\`,
		"/run/submux-runtime/",
		"/var/run/submux-runtime/",
		"/v1/",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("WebView contains direct Runtime transport %q", forbidden)
		}
	}
	for _, command := range []string{
		"runtime_handshake",
		"runtime_observe",
		"runtime_import_config",
		"runtime_preview_candidate",
		"runtime_preview_source",
		"runtime_preview_override",
		"runtime_apply_candidate",
		"runtime_add_remote_source",
		"runtime_get_advanced_override",
		"runtime_set_advanced_override",
		"runtime_add_managed_resource",
		"runtime_refresh_source",
		"runtime_apply_source",
		"runtime_start_proxy",
		"runtime_stop_proxy",
		"runtime_get_operation",
		"runtime_wait_operation",
		"runtime_cancel_operation",
		"runtime_verify_proxy",
	} {
		if !strings.Contains(script, `"`+command+`"`) {
			t.Fatalf("WebView does not invoke %q", command)
		}
	}
}

func TestGUIHasNoCloseHookThatStopsRuntime(t *testing.T) {
	script := readGUIFile(t, "ui", "app.js")
	for _, forbidden := range []string{
		"onCloseRequested",
		"close-requested",
		"beforeunload",
		"unload",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("GUI close lifecycle contains %q", forbidden)
		}
	}
}

func TestTauriCSPDisablesNetworkConnections(t *testing.T) {
	config := readGUIFile(t, "src-tauri", "tauri.conf.json")
	if !strings.Contains(config, "connect-src 'none'") {
		t.Fatal("Tauri WebView CSP does not disable direct network connections")
	}
}

func readGUIFile(t *testing.T, parts ...string) string {
	t.Helper()
	pathParts := append([]string{"..", "..", "desktop", "submux-runtime-gui"}, parts...)
	body, err := os.ReadFile(filepath.Join(pathParts...))
	if err != nil {
		t.Fatalf("read GUI file: %v", err)
	}
	return string(body)
}
