package runtimeprocess

import (
	"net"
	"strings"
	"testing"
)

func TestCheckLoopbackPortsAvailable(t *testing.T) {
	ipv4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve IPv4 port: %v", err)
	}
	address := ipv4.Addr().String()
	if err := ipv4.Close(); err != nil {
		t.Fatalf("release IPv4 port: %v", err)
	}
	if err := CheckLoopbackPortsAvailable([]string{address}); err != nil {
		t.Fatalf("available loopback port rejected: %v", err)
	}

	occupied, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("occupy IPv4 port: %v", err)
	}
	defer occupied.Close()
	if err := CheckLoopbackPortsAvailable([]string{address}); err == nil ||
		!strings.Contains(err.Error(), "already in use or unavailable") {
		t.Fatalf("occupied loopback port error = %v", err)
	}
}

func TestCheckLoopbackPortsRejectsNonLoopback(t *testing.T) {
	if err := CheckLoopbackPortsAvailable([]string{"0.0.0.0:7890"}); err == nil {
		t.Fatal("port checker accepted non-loopback listener")
	}
}
