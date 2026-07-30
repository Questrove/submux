package runtimeprivileged

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimenet"
	"submux/internal/runtimeprocess"
)

type fakeCoreController struct {
	stage  runtimenet.PrivilegedCoreStage
	state  string
	starts int
	stops  int
}

func TestHashFixedRegularFileRejectsOversizedObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumFixedObjectSize + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFixedRegularFile(path); err == nil ||
		!strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized fixed object error=%v", err)
	}
}

func TestDelegateRejectsNilContext(t *testing.T) {
	delegate := &Delegate{Controller: &fakeCoreController{}, exits: make(chan runtimeprocess.ExitEvent, 1)}
	if _, err := delegate.IsRunning(nil); err == nil {
		t.Fatal("delegate accepted nil context")
	}
	if err := delegate.Stop(nil); err == nil {
		t.Fatal("delegate stop accepted nil context")
	}
}

func (controller *fakeCoreController) StageCore(
	_ context.Context,
	_ string,
	stage runtimenet.PrivilegedCoreStage,
) (runtimenet.PrivilegedCoreStatus, error) {
	controller.stage = stage
	controller.state = "staged"
	return controller.status(), nil
}

func (controller *fakeCoreController) StartCore(
	_ context.Context,
	_ string,
	_ string,
) (runtimenet.PrivilegedCoreStatus, error) {
	controller.starts++
	controller.state = "running"
	return controller.status(), nil
}

func (controller *fakeCoreController) StopCore(
	_ context.Context,
	_ string,
	_ string,
) (runtimenet.PrivilegedCoreStatus, error) {
	controller.stops++
	controller.state = "stopped"
	return controller.status(), nil
}

func (controller *fakeCoreController) ObserveCore(
	context.Context,
	string,
) (runtimenet.PrivilegedCoreStatus, error) {
	return controller.status(), nil
}

func (controller *fakeCoreController) status() runtimenet.PrivilegedCoreStatus {
	return runtimenet.PrivilegedCoreStatus{
		ObjectID:    runtimenet.PrivilegedCoreObjectMihomo,
		State:       controller.state,
		PID:         1234,
		StartedAt:   time.Now().UTC(),
		ObservedAt:  time.Now().UTC(),
		PreviewOnly: true,
	}
}

func TestDelegateStagesOnlyFixedRuntimeObjects(t *testing.T) {
	root := t.TempDir()
	corePath := filepath.Join(root, "core", "current", "mihomo")
	configPath := filepath.Join(root, "config", "current", "config.yaml")
	dataPath := filepath.Join(root, "mihomo-data", "sources", "src_abc")
	for _, directory := range []string{
		filepath.Dir(corePath),
		filepath.Dir(configPath),
		dataPath,
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(corePath, []byte("verified core"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("mixed-port: 7890\n"), 0600); err != nil {
		t.Fatal(err)
	}

	controller := &fakeCoreController{state: "stopped"}
	delegate, err := NewDelegate(controller, root)
	if err != nil {
		t.Fatal(err)
	}
	specification := runtimeprocess.Specification{
		BinaryPath: corePath,
		ConfigPath: configPath,
		DataDir:    dataPath,
	}
	if err := delegate.Start(context.Background(), specification); err != nil {
		t.Fatalf("start delegated Mihomo: %v", err)
	}
	if controller.starts != 1 ||
		controller.stage.ObjectID != runtimenet.PrivilegedCoreObjectMihomo ||
		controller.stage.DataObjectID != "src_abc" ||
		len(controller.stage.CoreSHA256) != 64 ||
		len(controller.stage.ConfigSHA256) != 64 {
		t.Fatalf("privileged stage=%#v starts=%d", controller.stage, controller.starts)
	}
	if err := delegate.Stop(context.Background()); err != nil {
		t.Fatalf("stop delegated Mihomo: %v", err)
	}
	if controller.stops != 1 {
		t.Fatalf("privileged stops=%d", controller.stops)
	}
	select {
	case event := <-delegate.ExitEvents():
		if !event.Intentional {
			t.Fatalf("delegated exit event=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("delegated stop did not emit an exit event")
	}

	specification.ConfigPath = filepath.Join(root, "other.yaml")
	if err := delegate.Start(context.Background(), specification); err == nil {
		t.Fatal("delegated Mihomo accepted an arbitrary config path")
	}
}
