package compiler

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"submux/internal/fakeip"
	"submux/internal/store"
)

const fakeIPTemplate = `
dns:
  enable: true
  enhanced-mode: fake-ip
  fake-ip-filter-mode: blacklist
  fake-ip-filter:
    - printer.lan
    - +.template.test
proxies: []
proxy-groups: []
rules:
  - MATCH,DIRECT
`

func compileFakeIPTemplate(t *testing.T, content string, shared fakeip.Config) map[string]any {
	t.Helper()
	body, err := compileMihomo(resolvedSubscription{
		Template:     store.TemplateVersion{Content: content},
		SharedFakeIP: shared,
		Names:        map[string]string{},
		Slots:        map[string][]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSharedFakeIPFilterIsPrependedAndStablyDeduplicated(t *testing.T) {
	shared, err := fakeip.Validate(fakeip.Config{
		Mode: fakeip.ModeBlacklist, Entries: []string{"+.shared.test", "printer.lan"},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := compileFakeIPTemplate(t, fakeIPTemplate, shared)
	dns := root["dns"].(map[string]any)
	entries := dns["fake-ip-filter"].([]any)
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.(string))
	}
	want := []string{"+.shared.test", "printer.lan", "+.template.test"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("effective fake-ip-filter = %#v, want %#v", got, want)
	}
}

func TestSharedFakeIPFilterIsNoOpForRedirHostAndWhenUnset(t *testing.T) {
	shared, err := fakeip.Validate(fakeip.Config{Mode: fakeip.ModeWhitelist, Entries: []string{"+.shared.test"}})
	if err != nil {
		t.Fatal(err)
	}
	redirHost := strings.Replace(fakeIPTemplate, "enhanced-mode: fake-ip", "enhanced-mode: redir-host", 1)
	root := compileFakeIPTemplate(t, redirHost, shared)
	dns := root["dns"].(map[string]any)
	if entries := dns["fake-ip-filter"].([]any); len(entries) != 2 {
		t.Fatalf("redir-host template was modified: %#v", entries)
	}

	root = compileFakeIPTemplate(t, fakeIPTemplate, fakeip.Default())
	dns = root["dns"].(map[string]any)
	if entries := dns["fake-ip-filter"].([]any); len(entries) != 2 {
		t.Fatalf("unset shared filter changed template entries: %#v", entries)
	}
}

func TestSharedFakeIPFilterRejectsModeMismatch(t *testing.T) {
	shared, err := fakeip.Validate(fakeip.Config{Mode: fakeip.ModeWhitelist, Entries: []string{"+.shared.test"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = compileMihomo(resolvedSubscription{
		Template:     store.TemplateVersion{Content: fakeIPTemplate},
		SharedFakeIP: shared,
		Names:        map[string]string{},
		Slots:        map[string][]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("mode mismatch error = %v", err)
	}
}

func TestSharedFakeIPPreviewReportsEffectiveOrder(t *testing.T) {
	shared, err := fakeip.Validate(fakeip.Config{Mode: fakeip.ModeBlacklist, Entries: []string{"+.shared.test"}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewSharedFakeIPFilter(fakeIPTemplate, shared)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Applicable || preview.FilterMode != fakeip.ModeBlacklist ||
		len(preview.Effective) != 3 || preview.Effective[0] != "+.shared.test" {
		t.Fatalf("preview = %#v", preview)
	}
}
