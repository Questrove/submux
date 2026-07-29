package runtimeapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimecore"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimesource"
	"submux/internal/runtimestate"
)

func TestMihomoExecutorWithOfficialBinary(t *testing.T) {
	binaryPath := os.Getenv("SUBMUX_MIHOMO_INTEGRATION_BINARY")
	exactVersion := os.Getenv("SUBMUX_MIHOMO_INTEGRATION_VERSION")
	if binaryPath == "" || exactVersion == "" {
		t.Skip("set SUBMUX_MIHOMO_INTEGRATION_BINARY and SUBMUX_MIHOMO_INTEGRATION_VERSION")
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read Mihomo integration binary: %v", err)
	}
	digest := sha256.Sum256(binary)
	root := t.TempDir()
	state, err := runtimestate.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	process := &runtimeprocess.Process{
		ConfigPath: filepath.Join(root, "config", "current", "config.yaml"),
		DataDir:    filepath.Join(root, "mihomo-data"),
	}
	core := &runtimecore.Store{
		Root:       filepath.Join(root, "core"),
		Verifier:   runtimecore.CommandVerifier{},
		Activation: process,
	}
	if err := core.Activate(context.Background(), runtimecore.Binary{
		Version:      exactVersion,
		BinaryDigest: hex.EncodeToString(digest[:]),
		Data:         binary,
	}); err != nil {
		t.Fatalf("activate Mihomo integration core: %v", err)
	}
	port := availableDualStackPort(t)
	controlEndpoint := filepath.Join(root, "mihomo.sock")
	if runtime.GOOS == "windows" {
		controlEndpoint = fmt.Sprintf(`\\.\pipe\submux-runtime-mihomo-test-%d`, time.Now().UnixNano())
	}
	control := runtimeprocess.ControlProbe{Endpoint: controlEndpoint}
	verifier := &mihomo.RuntimeCheck{
		Control:       control,
		ProxyProbe:    mihomo.LocalHTTPProxyProbe{},
		ReadyTimeout:  20 * time.Second,
		RetryInterval: 100 * time.Millisecond,
	}
	executor := &MihomoExecutor{
		State:           state,
		Core:            core,
		Process:         process,
		ConfigRoot:      filepath.Join(root, "config"),
		ControlEndpoint: controlEndpoint,
		ProxyPort:       port,
		Platform:        runtime.GOOS,
		Verifier:        verifier,
	}
	managerNow := time.Now().UTC()
	executor.Sources = &runtimesource.Manager{
		State: state,
		Fetcher: &runtimesource.Fetcher{
			MihomoAddress: fmt.Sprintf("127.0.0.1:%d", port),
		},
		Validator: executor,
		Now:       func() time.Time { return managerNow },
		Random:    func() float64 { return 0.5 },
	}
	defer process.Stop(context.Background())

	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	source := []byte(`profile:
  store-selected: true
  store-fake-ip: true
dns:
  enable: true
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  nameserver:
    - 1.1.1.1
proxies: []
rules:
  - MATCH,DIRECT
`)
	var sourceRequests int
	sourceServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sourceRequests++
		if request.Header.Get("If-None-Match") == `"integration-v1"` {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writer.Header().Set("ETag", `"integration-v1"`)
		_, _ = writer.Write(source)
	}))
	defer sourceServer.Close()
	refreshInterval := int64(runtimesource.DefaultRefreshInterval / time.Second)
	draft, err := json.Marshal(runtimeapi.RemoteSourceDraft{
		Name:                   "integration-source",
		URL:                    sourceServer.URL + "/config.yaml",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: &refreshInterval,
	})
	if err != nil {
		t.Fatalf("encode remote source draft: %v", err)
	}
	draftDigest := sha256.Sum256(draft)
	draftContent, err := state.UploadImport(
		peer,
		runtimeapi.SourceDraftContentType,
		int64(len(draft)),
		hex.EncodeToString(draftDigest[:]),
		draft,
		managerNow,
	)
	if err != nil {
		t.Fatalf("upload remote source draft: %v", err)
	}
	sourceResult, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_add",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind:   runtimeapi.ActionAddRemoteSource,
				Params: runtimeapi.ActionParams{ContentID: draftContent.ID},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || sourceResult == nil || sourceResult.SourceID == "" ||
		sourceResult.RefreshResult != "validated" {
		t.Fatalf("add validated remote source: result=%#v err=%v", sourceResult, err)
	}
	resourceBody := []byte("proxies:\n  - name: local-socks\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n")
	resourceDigest := sha256.Sum256(resourceBody)
	resourceContent, err := state.UploadImport(
		peer,
		runtimeapi.ManagedResourceContentType,
		int64(len(resourceBody)),
		hex.EncodeToString(resourceDigest[:]),
		resourceBody,
		managerNow,
	)
	if err != nil {
		t.Fatalf("upload managed resource: %v", err)
	}
	resourceResult, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_resource_add",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionAddManagedResource,
				Params: runtimeapi.ActionParams{
					ContentID:    resourceContent.ID,
					ResourceKind: runtimeapi.ResourceKindProxyProvider,
					ResourceName: "integration-provider",
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || resourceResult == nil || resourceResult.ResourceID == "" {
		t.Fatalf("add managed resource: result=%#v err=%v", resourceResult, err)
	}
	override := []byte(fmt.Sprintf(
		"proxy-providers:\n  managed:\n    type: file\n    path: resource://%s\n    health-check:\n      enable: false\n",
		resourceResult.ResourceID,
	))
	overrideDigest := sha256.Sum256(override)
	overrideContent, err := state.UploadImport(
		peer,
		"application/x-yaml",
		int64(len(override)),
		hex.EncodeToString(overrideDigest[:]),
		override,
		managerNow,
	)
	if err != nil {
		t.Fatalf("upload advanced override: %v", err)
	}
	overrideResult, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_override_set",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionSetAdvancedOverride,
				Params: runtimeapi.ActionParams{
					ContentID: overrideContent.ID,
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || overrideResult == nil || overrideResult.AdvancedOverrideSHA256 == "" {
		t.Fatalf(
			"set advanced override: result=%#v err=%v cause=%v",
			overrideResult,
			err,
			errors.Unwrap(err),
		)
	}
	sourcePreview, err := executor.PreviewCandidate(
		context.Background(),
		peer,
		runtimeapi.PreviewCandidateRequest{SourceID: sourceResult.SourceID},
	)
	if err != nil || len(sourcePreview.ReferencedResources) != 1 ||
		sourcePreview.ReferencedResources[0] != resourceResult.ResourceID ||
		len(sourcePreview.FieldOrigins) == 0 {
		t.Fatalf("preview layered remote source: preview=%#v err=%v", sourcePreview, err)
	}
	managerNow = managerNow.Add(runtimesource.ManualRefreshDebounce + time.Second)
	refreshResult, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_refresh",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionRefreshSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceResult.SourceID,
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || refreshResult == nil || !refreshResult.NotModified || sourceRequests != 2 {
		t.Fatalf("conditional remote source refresh: result=%#v requests=%d err=%v", refreshResult, sourceRequests, err)
	}
	appliedSource, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_apply",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind:   runtimeapi.ActionApplySource,
				Params: runtimeapi.ActionParams{SourceID: sourceResult.SourceID},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || appliedSource == nil || appliedSource.SourceID != sourceResult.SourceID ||
		appliedSource.Verified || len(appliedSource.ProxyAddresses) != 2 {
		t.Fatalf("apply validated remote source: result=%#v err=%v", appliedSource, err)
	}
	if running, err := process.IsRunning(context.Background()); err != nil || running {
		t.Fatalf("first applied remote source started Mihomo: running=%v err=%v", running, err)
	}

	sourceDigest := sha256.Sum256(source)
	content, err := state.UploadImport(
		peer,
		"application/x-yaml",
		int64(len(source)),
		hex.EncodeToString(sourceDigest[:]),
		source,
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("upload Mihomo integration config: %v", err)
	}
	preview, err := executor.PreviewCandidate(
		context.Background(),
		peer,
		runtimeapi.PreviewCandidateRequest{ContentID: content.ID},
	)
	if err != nil || !preview.Validated || len(preview.ProxyAddresses) != 2 {
		t.Fatalf("preview Mihomo integration config: preview=%#v err=%v", preview, err)
	}
	result, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_integration",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind:   runtimeapi.ActionApplyImportedConfig,
				Params: runtimeapi.ActionParams{ContentID: content.ID},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil {
		t.Fatalf("prepare Mihomo integration config: %v", err)
	}
	if result == nil || result.Verified || len(result.ProxyAddresses) != 2 {
		t.Fatalf("Mihomo integration result = %#v", result)
	}
	if running, err := process.IsRunning(context.Background()); err != nil || running {
		t.Fatalf("first imported config started Mihomo: running=%v err=%v", running, err)
	}
	result, err = executor.Execute(
		context.Background(),
		runtimeapi.Operation{Action: runtimeapi.Action{Kind: runtimeapi.ActionStartProxy}},
		func(string, int, bool) error { return nil },
	)
	if err != nil || result == nil || !result.Verified {
		t.Fatalf("start and verify Mihomo integration config: result=%#v err=%v", result, err)
	}
	verification, err := executor.Verify(context.Background())
	if err != nil || !verification.Available {
		t.Fatalf("verify running Mihomo = %#v err=%v", verification, err)
	}
	firstSourceDataDir := process.DataDir
	stateMarker := filepath.Join(firstSourceDataDir, "source-state.marker")
	if err := os.WriteFile(stateMarker, []byte("source-one"), 0600); err != nil {
		t.Fatalf("write source state marker: %v", err)
	}
	firstCachePath := filepath.Join(firstSourceDataDir, "cache.db")
	if info, err := os.Stat(firstCachePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("first source Mihomo cache = %#v err=%v", info, err)
	}

	var secondSourceRequests int
	secondSourceServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondSourceRequests++
		if request.Header.Get("If-None-Match") == `"integration-v2"` {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writer.Header().Set("ETag", `"integration-v2"`)
		_, _ = writer.Write(source)
	}))
	defer secondSourceServer.Close()
	secondDraft, err := json.Marshal(runtimeapi.RemoteSourceDraft{
		Type:                   runtimeapi.SourceTypeSubmuxOutput,
		Name:                   "integration-submux-output",
		URL:                    secondSourceServer.URL + "/output.yaml",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: &refreshInterval,
	})
	if err != nil {
		t.Fatalf("encode second source draft: %v", err)
	}
	secondDraftDigest := sha256.Sum256(secondDraft)
	secondDraftContent, err := state.UploadImport(
		peer,
		runtimeapi.SourceDraftContentType,
		int64(len(secondDraft)),
		hex.EncodeToString(secondDraftDigest[:]),
		secondDraft,
		managerNow,
	)
	if err != nil {
		t.Fatalf("upload second source draft: %v", err)
	}
	secondSource, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_add_second",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind:   runtimeapi.ActionAddRemoteSource,
				Params: runtimeapi.ActionParams{ContentID: secondDraftContent.ID},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || secondSource == nil || secondSource.SourceID == "" {
		t.Fatalf("add second source: result=%#v err=%v", secondSource, err)
	}
	managerNow = managerNow.Add(runtimesource.ManualRefreshDebounce + time.Second)
	switched, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_switch_second",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionSwitchSource,
				Params: runtimeapi.ActionParams{
					SourceID: secondSource.SourceID,
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || switched == nil || !switched.Verified ||
		switched.SourceID != secondSource.SourceID ||
		switched.PreviousSourceID != sourceResult.SourceID ||
		!switched.NotModified ||
		secondSourceRequests != 2 {
		t.Fatalf("switch to second source: result=%#v requests=%d err=%v", switched, secondSourceRequests, err)
	}
	secondSourceDataDir := process.DataDir
	if secondSourceDataDir == firstSourceDataDir {
		t.Fatalf("source data directory was shared: %q", secondSourceDataDir)
	}
	if _, err := os.Stat(filepath.Join(secondSourceDataDir, filepath.Base(stateMarker))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second source saw first source state: %v", err)
	}
	secondCachePath := filepath.Join(secondSourceDataDir, "cache.db")
	if info, err := os.Stat(secondCachePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("second source Mihomo cache = %#v err=%v", info, err)
	}
	if firstCachePath == secondCachePath {
		t.Fatalf("source cache path was shared: %q", firstCachePath)
	}
	current, err := state.CurrentSource()
	if err != nil || current.ID != secondSource.SourceID {
		t.Fatalf("current source after switch = %#v err=%v", current, err)
	}

	sourceServer.Close()
	managerNow = managerNow.Add(runtimesource.ManualRefreshDebounce + time.Second)
	if _, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_switch_failed_refresh",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionSwitchSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceResult.SourceID,
				},
			},
		},
		func(string, int, bool) error { return nil },
	); err == nil {
		t.Fatal("source switch used cached revision without explicit approval")
	}
	current, err = state.CurrentSource()
	if err != nil || current.ID != secondSource.SourceID || process.DataDir != secondSourceDataDir {
		t.Fatalf("failed switch changed source state: current=%#v data=%q err=%v", current, process.DataDir, err)
	}
	switched, err = executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_switch_cached",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionSwitchSource,
				Params: runtimeapi.ActionParams{
					SourceID:  sourceResult.SourceID,
					UseCached: true,
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || switched == nil || !switched.UsedCachedSource ||
		switched.SourceID != sourceResult.SourceID ||
		process.DataDir != firstSourceDataDir {
		t.Fatalf("switch with cached source: result=%#v data=%q err=%v", switched, process.DataDir, err)
	}
	if body, err := os.ReadFile(stateMarker); err != nil || string(body) != "source-one" {
		t.Fatalf("first source state was not restored: body=%q err=%v", body, err)
	}
	if _, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_delete_running_current",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceResult.SourceID,
					Confirm:  true,
				},
			},
		},
		func(string, int, bool) error { return nil },
	); err == nil {
		t.Fatal("deleted the current source while Mihomo was running")
	}
	deleted, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_delete_inactive",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: secondSource.SourceID,
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || deleted == nil || !deleted.Deleted {
		t.Fatalf("delete inactive source: result=%#v err=%v", deleted, err)
	}

	if _, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{Action: runtimeapi.Action{Kind: runtimeapi.ActionStopProxy}},
		func(string, int, bool) error { return nil },
	); err != nil {
		t.Fatalf("stop Mihomo integration process: %v", err)
	}
	deleted, err = executor.Execute(
		context.Background(),
		runtimeapi.Operation{
			ID:             "op_source_delete_stopped_current",
			CallerIdentity: peer.Key(),
			Action: runtimeapi.Action{
				Kind: runtimeapi.ActionDeleteSource,
				Params: runtimeapi.ActionParams{
					SourceID: sourceResult.SourceID,
					Confirm:  true,
				},
			},
		},
		func(string, int, bool) error { return nil },
	)
	if err != nil || deleted == nil || !deleted.Deleted {
		t.Fatalf("delete stopped current source: result=%#v err=%v", deleted, err)
	}
	defaultDataDir, err := state.RuntimeDataDir()
	if err != nil {
		t.Fatalf("read default Runtime data directory: %v", err)
	}
	if process.DataDir != defaultDataDir {
		t.Fatalf("process data directory after current source deletion = %q, want %q", process.DataDir, defaultDataDir)
	}
	if _, err := state.CurrentSource(); !errors.Is(err, runtimestate.ErrSourceNotFound) {
		t.Fatalf("current source after confirmed deletion error = %v", err)
	}
}

func TestProcessConfigurationRestoresAllMutableProcessFields(t *testing.T) {
	process := &runtimeprocess.Process{
		BinaryPath: "old-binary",
		ConfigPath: "old-config",
		DataDir:    "old-data",
		SafePaths:  []string{"old-safe"},
	}
	previous := captureProcessConfiguration(process)
	process.BinaryPath = "new-binary"
	process.ConfigPath = "new-config"
	process.DataDir = "new-data"
	process.SafePaths[0] = "new-safe"

	previous.apply(process)

	if process.BinaryPath != "old-binary" ||
		process.ConfigPath != "old-config" ||
		process.DataDir != "old-data" ||
		len(process.SafePaths) != 1 ||
		process.SafePaths[0] != "old-safe" {
		t.Fatalf("restored process configuration = %#v", process)
	}
	process.SafePaths[0] = "mutated"
	if previous.SafePaths[0] != "old-safe" {
		t.Fatalf("restored process SafePaths alias snapshot: %#v", previous.SafePaths)
	}
}

func TestSourceSwitchRuntimeServiceAlternatesTargetAndRollbackConfiguration(t *testing.T) {
	process := &runtimeprocess.Process{}
	target := processConfiguration{
		BinaryPath: "target-binary",
		ConfigPath: "target-config",
		DataDir:    "target-data",
		SafePaths:  []string{"target-safe"},
	}
	previous := processConfiguration{
		BinaryPath: "previous-binary",
		ConfigPath: "previous-config",
		DataDir:    "previous-data",
		SafePaths:  []string{"previous-safe"},
	}
	service := &sourceSwitchRuntimeService{
		process:  process,
		report:   func(string, int, bool) error { return nil },
		target:   target,
		previous: previous,
	}

	for index, want := range []processConfiguration{target, previous, target} {
		_ = service.ReloadOrRestart(context.Background())
		got := captureProcessConfiguration(process)
		if got.BinaryPath != want.BinaryPath ||
			got.ConfigPath != want.ConfigPath ||
			got.DataDir != want.DataDir ||
			len(got.SafePaths) != 1 ||
			got.SafePaths[0] != want.SafePaths[0] {
			t.Fatalf("activation %d process configuration = %#v, want %#v", index+1, got, want)
		}
	}
}

func availableDualStackPort(t *testing.T) int {
	t.Helper()
	for attempts := 0; attempts < 20; attempts++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find IPv4 loopback port: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		addresses := []string{
			fmt.Sprintf("127.0.0.1:%d", port),
			fmt.Sprintf("[::1]:%d", port),
		}
		if err := runtimeprocess.CheckLoopbackPortsAvailable(addresses); err == nil {
			return port
		}
	}
	t.Fatal("could not find a dual-stack loopback port")
	return 0
}
