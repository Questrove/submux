package runtimeapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	source := []byte("proxies: []\nrules:\n  - MATCH,DIRECT\n")
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
	if _, err := executor.Execute(
		context.Background(),
		runtimeapi.Operation{Action: runtimeapi.Action{Kind: runtimeapi.ActionStopProxy}},
		func(string, int, bool) error { return nil },
	); err != nil {
		t.Fatalf("stop Mihomo integration process: %v", err)
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
