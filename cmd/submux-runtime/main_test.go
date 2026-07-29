package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeipc"
)

type commandObserver struct{}

func (commandObserver) Observe(_ context.Context, _ runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	return runtimeapi.Snapshot{
		ProtocolVersion:   runtimeapi.ProtocolVersion,
		Revision:          11,
		Runtime:           runtimeapi.RuntimeStatus{Version: "v1.2.3", ServiceState: "running"},
		Mihomo:            runtimeapi.MihomoStatus{State: "not_installed", Recovery: "idle"},
		RunMode:           "unconfigured",
		LatestEventCursor: 12,
		ObservedAt:        time.Now().UTC(),
	}, nil
}

func TestStatusJSONUsesLocalIPC(t *testing.T) {
	endpoint := commandTestEndpoint(t)
	listener, err := runtimeipc.Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on Runtime IPC: %v", err)
	}
	authorizer, err := runtimeipc.CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := runtimeipc.NewServer(commandObserver{}, authorizer)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx, listener) }()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"status", "--endpoint", endpoint, "--json"}, &stdout, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("status exit code = %d; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var snapshot runtimeapi.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		cancel()
		t.Fatalf("decode status JSON: %v; output=%s", err, stdout.String())
	}
	if snapshot.Revision != 11 || snapshot.LatestEventCursor != 12 {
		cancel()
		t.Fatalf("status snapshot = %#v", snapshot)
	}
	if stderr.Len() != 0 {
		cancel()
		t.Fatalf("status stderr = %s", stderr.String())
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("stop Runtime IPC server: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Runtime IPC server did not stop")
	}
}

func TestStatusJSONReturnsStableUnavailableEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"status", "--endpoint", "not-a-local-endpoint", "--json"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("status exit code = %d, want 1; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var envelope runtimeapi.ErrorEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode status error JSON: %v; output=%s", err, stdout.String())
	}
	if envelope.Error.Code != runtimeapi.ErrorServiceUnavailable || !envelope.Error.Retryable {
		t.Fatalf("status error = %#v", envelope.Error)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON status stderr = %s", stderr.String())
	}
}

func TestVersionAndUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := run([]string{"--version-json"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("version exit code = %d", exitCode)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("version output is not JSON: %s", stdout.String())
	}
	stdout.Reset()
	if exitCode := run([]string{"unknown"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("unknown command exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("usage stderr = %s", stderr.String())
	}
}

func TestServeLockFileCannotBeOverridden(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"serve", "--lock-file", filepath.Join(t.TempDir(), "other.lock")}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("serve exit code = %d, want 2; stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("serve accepted lock override: %s", stderr.String())
	}
}

func commandTestEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\submux-runtime-test-%d`, time.Now().UnixNano())
	}
	return filepath.Join(t.TempDir(), "runtime.sock")
}
