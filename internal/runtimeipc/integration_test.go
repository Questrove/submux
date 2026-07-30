package runtimeipc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestClientServerUsesOnlyLocalTransport(t *testing.T) {
	endpoint := testEndpoint(t)
	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on local Runtime IPC: %v", err)
	}
	if network := listener.Addr().Network(); network == "tcp" || strings.HasPrefix(network, "tcp") {
		t.Fatalf("Runtime IPC unexpectedly uses TCP: %q", network)
	}
	authorizer, err := CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := NewServer(
		observerFunc(func(_ context.Context, peer runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			if peer.Platform == "" || peer.Key() == "" {
				return runtimeapi.Snapshot{}, errors.New("missing peer")
			}
			return runtimeapi.Snapshot{
				ProtocolVersion:   runtimeapi.ProtocolVersion,
				Revision:          3,
				Runtime:           runtimeapi.RuntimeStatus{Version: "test", ServiceState: "running"},
				Mihomo:            runtimeapi.MihomoStatus{State: "not_installed", Recovery: "idle"},
				LatestEventCursor: 4,
				ObservedAt:        time.Now().UTC(),
			}, nil
		}),
		authorizer,
	)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	serverContext, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- server.Serve(serverContext, listener)
	}()

	client, err := NewClient(endpoint, "integration-test")
	if err != nil {
		cancel()
		t.Fatalf("create Runtime IPC client: %v", err)
	}
	snapshot, err := client.Observe(context.Background())
	client.CloseIdleConnections()
	if err != nil {
		cancel()
		t.Fatalf("observe Runtime over local IPC: %v", err)
	}
	if snapshot.Revision != 3 || snapshot.LatestEventCursor != 4 {
		cancel()
		t.Fatalf("snapshot = %#v", snapshot)
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

func TestClientRejectsNilContext(t *testing.T) {
	client, err := NewClient(testEndpoint(t), "test")
	if err != nil {
		t.Fatalf("create Runtime IPC client: %v", err)
	}
	defer client.CloseIdleConnections()
	_, err = client.Observe(nil)
	var clientError *ClientError
	if !errors.As(err, &clientError) || clientError.Code != runtimeapi.ErrorInvalidRequest {
		t.Fatalf("Observe(nil) error = %v", err)
	}
}

func testEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		requestID, err := newRequestID()
		if err != nil {
			t.Fatalf("create test pipe name: %v", err)
		}
		return `\\.\pipe\submux-runtime-test-` + requestID
	}
	return filepath.Join(runtimeIPCTestTempDir(t), "runtime.sock")
}

func runtimeIPCTestTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return t.TempDir()
	}
	root, err := os.MkdirTemp("/private/tmp", "submux-runtimeipc-")
	if err != nil {
		t.Fatalf("create short macOS test directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove short macOS test directory: %v", err)
		}
	})
	return root
}
