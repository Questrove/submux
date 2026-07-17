package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeDockerOps struct {
	config, ownership []byte
	configExists      bool
	proxy             string
	restarts          int
	validateErr       error
}

func (f *fakeDockerOps) ReadConfig(context.Context) ([]byte, bool, error) {
	return append([]byte(nil), f.config...), f.configExists, nil
}
func (f *fakeDockerOps) WriteConfig(_ context.Context, value []byte) error {
	f.config, f.configExists = append([]byte(nil), value...), true
	var document map[string]any
	_ = strictJSON(value, &document)
	if proxies, ok := document["proxies"].(map[string]any); ok {
		f.proxy, _ = proxies["http-proxy"].(string)
	} else {
		f.proxy = ""
	}
	return nil
}
func (f *fakeDockerOps) RemoveConfig(context.Context) error {
	f.config, f.configExists, f.proxy = nil, false, ""
	return nil
}
func (f *fakeDockerOps) ReadOwnership(context.Context) ([]byte, bool, error) {
	return append([]byte(nil), f.ownership...), len(f.ownership) != 0, nil
}
func (f *fakeDockerOps) WriteOwnership(_ context.Context, value []byte) error {
	f.ownership = append([]byte(nil), value...)
	return nil
}
func (f *fakeDockerOps) RemoveOwnership(context.Context) error { f.ownership = nil; return nil }
func (f *fakeDockerOps) Validate(context.Context) error        { return f.validateErr }
func (f *fakeDockerOps) Restart(context.Context) error         { f.restarts++; return nil }
func (f *fakeDockerOps) InspectProxy(context.Context) (string, error) {
	return f.proxy, nil
}
func (f *fakeDockerOps) RestartAndVerify(context.Context) error {
	f.restarts++
	return f.validateErr
}

func TestDockerDaemonLifecyclePreservesExistingFieldsAndProxy(t *testing.T) {
	ops := &fakeDockerOps{configExists: true, config: []byte(`{"log-driver":"journald","proxies":{"http-proxy":"http://old:1","no-proxy":"old"}}`)}
	manager := &DockerManager{Ops: ops}
	config := DockerDaemonConfig{Enabled: true, ProxyPort: 7890}
	preview, err := manager.Preview(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Before == preview.After || !preview.RestartRequired {
		t.Fatalf("preview did not describe a change: %#v", preview)
	}
	status, err := manager.Enable(context.Background(), config, preview.OriginalHash)
	if err != nil || status.State != "active" {
		t.Fatalf("enable: status=%#v err=%v", status, err)
	}
	var active map[string]any
	if err := strictJSON(ops.config, &active); err != nil || active["log-driver"] != "journald" {
		t.Fatalf("unrelated Docker settings were not preserved: %s err=%v", ops.config, err)
	}
	status, err = manager.Disable(context.Background())
	if err != nil || status.State != "disabled" {
		t.Fatalf("disable: status=%#v err=%v", status, err)
	}
	var restored map[string]any
	if err := strictJSON(ops.config, &restored); err != nil {
		t.Fatal(err)
	}
	proxies := restored["proxies"].(map[string]any)
	if proxies["http-proxy"] != "http://old:1" || restored["log-driver"] != "journald" {
		t.Fatalf("original values were not restored: %s", ops.config)
	}
}

func TestDockerDaemonRefusesStalePreviewAndExternalModification(t *testing.T) {
	ops := &fakeDockerOps{configExists: true, config: []byte(`{"features":{"containerd-snapshotter":true}}`)}
	manager := &DockerManager{Ops: ops}
	config := DockerDaemonConfig{Enabled: true, ProxyPort: 7890}
	preview, _ := manager.Preview(context.Background(), config)
	ops.config = []byte(`{"debug":true}`)
	if _, err := manager.Enable(context.Background(), config, preview.OriginalHash); err == nil {
		t.Fatal("stale preview was accepted")
	}
	preview, _ = manager.Preview(context.Background(), config)
	if _, err := manager.Enable(context.Background(), config, preview.OriginalHash); err != nil {
		t.Fatal(err)
	}
	ops.config = append(append([]byte(nil), ops.config...), ' ')
	status, err := manager.Disable(context.Background())
	if err == nil || status.State != "conflict" {
		t.Fatalf("external modification was not protected: status=%#v err=%v", status, err)
	}
}

func TestDockerDaemonRestoresOnValidationFailure(t *testing.T) {
	original := []byte(`{"debug":true}`)
	ops := &fakeDockerOps{configExists: true, config: append([]byte(nil), original...), validateErr: errors.New("bad config")}
	manager := &DockerManager{Ops: ops}
	config := DockerDaemonConfig{Enabled: true, ProxyPort: 7890}
	preview, _ := manager.Preview(context.Background(), config)
	if _, err := manager.Enable(context.Background(), config, preview.OriginalHash); err == nil {
		t.Fatal("validation failure was accepted")
	}
	if string(ops.config) != string(original) || len(ops.ownership) != 0 || ops.restarts != 1 {
		t.Fatalf("failed activation was not restored: config=%s state=%s restarts=%d", ops.config, ops.ownership, ops.restarts)
	}
}

func TestDockerIntegrationRejectsUnboundedOriginalProxyBackup(t *testing.T) {
	largeValue, _ := json.Marshal(map[string]string{"note": strings.Repeat("x", maxSavedProxyValue+1)})
	for name, desktop := range map[string]bool{"daemon": false, "desktop": true} {
		t.Run(name, func(t *testing.T) {
			key := "proxies"
			if desktop {
				key = "containersProxy"
			}
			original := []byte(`{"` + key + `":` + string(largeValue) + `}`)
			ops := &fakeDockerOps{configExists: true, config: append([]byte(nil), original...)}
			if desktop {
				manager := &DesktopManager{Ops: ops}
				config := DockerDesktopConfig{Enabled: true, ProxyPort: 7890, BusinessAdminSettings: true}
				preview, _ := manager.Preview(context.Background(), config)
				if _, err := manager.Enable(context.Background(), config, preview.OriginalHash); err == nil {
					t.Fatal("oversized Desktop recovery value was accepted")
				}
			} else {
				manager := &DockerManager{Ops: ops}
				config := DockerDaemonConfig{Enabled: true, ProxyPort: 7890}
				preview, _ := manager.Preview(context.Background(), config)
				if _, err := manager.Enable(context.Background(), config, preview.OriginalHash); err == nil {
					t.Fatal("oversized daemon recovery value was accepted")
				}
			}
			if string(ops.config) != string(original) || len(ops.ownership) != 0 || ops.restarts != 0 {
				t.Fatal("oversized recovery value caused a system mutation")
			}
		})
	}
}

func TestDockerDesktopLifecycleIsSeparateAndRestoresContainersProxy(t *testing.T) {
	ops := &fakeDockerOps{configExists: true, config: []byte(`{"configurationFileVersion":2,"analyticsEnabled":{"value":false},"containersProxy":{"locked":false,"mode":"system"}}`)}
	manager := &DesktopManager{Ops: ops}
	config := DockerDesktopConfig{Enabled: true, ProxyPort: 7890, BusinessAdminSettings: true}
	preview, err := manager.Preview(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.After, `"containersProxy"`) || strings.Contains(preview.After, `"proxies"`) {
		t.Fatalf("Docker Desktop adapter reused the Linux daemon schema: %s", preview.After)
	}
	if _, err := manager.Enable(context.Background(), config, preview.OriginalHash); err != nil {
		t.Fatal(err)
	}
	var active map[string]any
	if err := strictJSON(ops.config, &active); err != nil || active["analyticsEnabled"] == nil {
		t.Fatalf("Docker Desktop unrelated settings were lost: %s", ops.config)
	}
	if _, err := manager.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if err := strictJSON(ops.config, &restored); err != nil {
		t.Fatal(err)
	}
	original := restored["containersProxy"].(map[string]any)
	if original["mode"] != "system" || original["locked"] != false {
		t.Fatalf("original Docker Desktop containersProxy was not restored: %s", ops.config)
	}
}

func TestDockerDesktopRequiresExplicitBusinessPrerequisite(t *testing.T) {
	manager := &DesktopManager{Ops: &fakeDockerOps{}}
	if _, err := manager.Preview(context.Background(), DockerDesktopConfig{Enabled: true, ProxyPort: 7890}); err == nil {
		t.Fatal("Docker Desktop integration ignored its Business settings prerequisite")
	}
}

func TestInterruptedDockerDaemonApplyResumesFromBothDurablePhases(t *testing.T) {
	original := []byte(`{"debug":true}`)
	config := DockerDaemonConfig{Enabled: true, ProxyPort: 7890}
	desired, originalProxy, originalSet, err := mergeDockerDaemon(original, config)
	if err != nil {
		t.Fatal(err)
	}
	ownership, _ := json.Marshal(dockerOwnership{
		Phase: "applying", OriginalHash: hashDocument(original), AppliedHash: hashDocument(desired), OriginalExisted: true,
		OriginalProxySet: originalSet, OriginalProxies: originalProxy, ProxyURL: proxyURL(config.ProxyPort), UpdatedAt: "now",
	})
	for _, candidateWritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-candidate-write", true: "after-candidate-write"}[candidateWritten], func(t *testing.T) {
			ops := &fakeDockerOps{configExists: true, config: append([]byte(nil), original...), ownership: append([]byte(nil), ownership...)}
			if candidateWritten {
				if err := ops.WriteConfig(context.Background(), desired); err != nil {
					t.Fatal(err)
				}
			}
			manager := &DockerManager{Ops: ops}
			status, err := manager.Enable(context.Background(), config, hashDocument(original))
			if err != nil || status.State != "active" || ops.restarts == 0 {
				t.Fatalf("resume: status=%#v restarts=%d err=%v", status, ops.restarts, err)
			}
			if persisted, exists, err := manager.readOwnership(context.Background()); err != nil || !exists || persisted.Phase != "active" {
				t.Fatalf("resumed ownership=%#v exists=%v err=%v", persisted, exists, err)
			}
		})
	}
}

func TestInterruptedDockerDesktopApplyResumesFromBothDurablePhases(t *testing.T) {
	original := []byte(`{"configurationFileVersion":2,"analyticsEnabled":{"value":false}}`)
	config := DockerDesktopConfig{Enabled: true, ProxyPort: 7890, BusinessAdminSettings: true}
	desired, originalProxy, originalSet, err := mergeDockerDesktop(original, config)
	if err != nil {
		t.Fatal(err)
	}
	ownership, _ := json.Marshal(dockerOwnership{
		Phase: "applying", OriginalHash: hashDocument(original), AppliedHash: hashDocument(desired), OriginalExisted: true,
		OriginalProxySet: originalSet, OriginalProxies: originalProxy, ProxyURL: proxyURL(config.ProxyPort), UpdatedAt: "now",
	})
	for _, candidateWritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-candidate-write", true: "after-candidate-write"}[candidateWritten], func(t *testing.T) {
			ops := &fakeDockerOps{configExists: true, config: append([]byte(nil), original...), ownership: append([]byte(nil), ownership...)}
			if candidateWritten {
				if err := ops.WriteConfig(context.Background(), desired); err != nil {
					t.Fatal(err)
				}
			}
			manager := &DesktopManager{Ops: ops}
			status, err := manager.Enable(context.Background(), config, hashDocument(original))
			if err != nil || status.State != "active" || ops.restarts == 0 {
				t.Fatalf("resume: status=%#v restarts=%d err=%v", status, ops.restarts, err)
			}
			if persisted, exists, err := manager.readOwnership(context.Background()); err != nil || !exists || persisted.Phase != "active" {
				t.Fatalf("resumed ownership=%#v exists=%v err=%v", persisted, exists, err)
			}
		})
	}
}
