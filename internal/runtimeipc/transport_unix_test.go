//go:build !windows

package runtimeipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDialDoesNotCreateRuntimeDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "missing")
	endpoint := filepath.Join(parent, "runtime.sock")
	connection, err := dialLocal(context.Background(), endpoint)
	if err == nil {
		_ = connection.Close()
		t.Fatal("dial unexpectedly succeeded")
	}
	if _, statErr := os.Stat(parent); !os.IsNotExist(statErr) {
		t.Fatalf("status client created Runtime directory: %v", statErr)
	}
}

func TestListenDoesNotRemoveActiveSocket(t *testing.T) {
	endpoint := filepath.Join(runtimeIPCTestTempDir(t), "runtime.sock")
	active, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatalf("listen on active test Socket: %v", err)
	}
	defer active.Close()

	second, err := Listen(endpoint)
	if err == nil {
		_ = second.Close()
		t.Fatal("second listener replaced an active Runtime Socket")
	}
	if _, statErr := os.Lstat(endpoint); statErr != nil {
		t.Fatalf("active Runtime Socket was removed: %v", statErr)
	}
}
