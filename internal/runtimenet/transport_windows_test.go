//go:build windows

package runtimenet

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestWindowsNetworkPipeAuthenticatesCurrentProcess(t *testing.T) {
	endpoint := windowsNetworkPipe + fmt.Sprintf("-test-%x", time.Now().UnixNano())
	listener, err := Listen(endpoint, 1)
	if err != nil {
		t.Fatalf("listen on Windows Runtime network Pipe: %v", err)
	}
	defer listener.Close()

	type accepted struct {
		connection net.Conn
		err        error
	}
	result := make(chan accepted, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		result <- accepted{connection: connection, err: acceptErr}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialNetwork(ctx, endpoint)
	if err != nil {
		t.Fatalf("dial Windows Runtime network Pipe: %v", err)
	}
	defer client.Close()
	server := <-result
	if server.err != nil {
		t.Fatalf("accept Windows Runtime network Pipe: %v", server.err)
	}
	defer server.connection.Close()
	uid, err := listener.PeerUID(server.connection)
	if err != nil {
		t.Fatalf("authenticate Windows Runtime network Pipe: %v", err)
	}
	if uid != 1 {
		t.Fatalf("Windows Runtime network internal UID=%d", uid)
	}
}

func TestWindowsNetworkPipeRejectsArbitraryEndpoint(t *testing.T) {
	if _, err := Listen(`\\.\pipe\other-runtime-net`, 1); err == nil {
		t.Fatal("Windows Runtime network listener accepted arbitrary endpoint")
	}
	if _, err := dialNetwork(context.Background(), `\\server\pipe\submux-runtime-net`); err == nil {
		t.Fatal("Windows Runtime network client accepted remote endpoint")
	}
}
