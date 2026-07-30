//go:build windows

package runtimeprocess

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestWindowsControlPipeAcceptsRestrictedDACL(t *testing.T) {
	currentSID, err := currentProcessSID()
	if err != nil {
		t.Fatalf("read current SID: %v", err)
	}
	endpoint := controlTestEndpoint()
	listener := startControlPipe(
		t,
		endpoint,
		"D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;"+currentSID+")",
	)
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := dialControl(ctx, endpoint)
	if err != nil {
		t.Fatalf("dial restricted Mihomo control Pipe: %v", err)
	}
	_ = connection.Close()
}

func TestWindowsControlPipeRejectsBroadDACL(t *testing.T) {
	currentSID, err := currentProcessSID()
	if err != nil {
		t.Fatalf("read current SID: %v", err)
	}
	endpoint := controlTestEndpoint()
	listener := startControlPipe(
		t,
		endpoint,
		"D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;"+currentSID+")(A;;GRGW;;;WD)",
	)
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if connection, err := dialControl(ctx, endpoint); err == nil {
		_ = connection.Close()
		t.Fatal("Mihomo control Pipe accepted a broad Everyone ACE")
	}
}

func startControlPipe(t *testing.T, endpoint, descriptor string) net.Listener {
	t.Helper()
	listener, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: descriptor,
		MessageMode:        false,
	})
	if err != nil {
		t.Fatalf("listen on test Mihomo control Pipe: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
	}()
	return listener
}

func controlTestEndpoint() string {
	return `\\.\pipe\submux-runtime-mihomo-test-` +
		fmt.Sprintf("%x", time.Now().UnixNano())
}
