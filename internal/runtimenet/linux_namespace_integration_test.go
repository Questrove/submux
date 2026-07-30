//go:build linux

package runtimenet

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"submux/internal/runtimeapi"
)

const (
	privilegedNamespaceTestEnv = "SUBMUX_RUN_PRIVILEGED_NETNS_TESTS"
	namespaceChildEnv          = "SUBMUX_PRIVILEGED_NETNS_CHILD"
	namespaceRuntimeUID        = 65534
)

func TestLinuxNetworkNamespaceOrdinaryTUN(t *testing.T) {
	if os.Getenv(privilegedNamespaceTestEnv) != "1" {
		t.Skip("set SUBMUX_RUN_PRIVILEGED_NETNS_TESTS=1 to run the privileged Linux namespace test")
	}
	if os.Getenv(namespaceChildEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Fatal("privileged Linux namespace test must run as root")
		}
		if _, err := exec.LookPath("unshare"); err != nil {
			t.Fatalf("find unshare: %v", err)
		}
		command := exec.Command(
			"unshare",
			"--net",
			os.Args[0],
			"-test.run=^TestLinuxNetworkNamespaceOrdinaryTUN$",
			"-test.v",
		)
		command.Env = append(os.Environ(), namespaceChildEnv+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("run isolated Linux network namespace: %v\n%s", err, output)
		}
		t.Logf("isolated Linux network namespace output:\n%s", output)
		return
	}
	runLinuxNamespaceOrdinaryTUN(t)
}

func runLinuxNamespaceOrdinaryTUN(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("isolated Linux network namespace child is not root")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Fatalf("inspect /dev/net/tun: %v", err)
	}
	runNamespaceCommand(t, "ip", "link", "set", "lo", "up")
	runNamespaceCommand(t, "ip", "link", "add", "eth0", "type", "dummy")
	runNamespaceCommand(t, "ip", "link", "set", "eth0", "up")
	runNamespaceCommand(t, "ip", "addr", "add", "192.0.2.2/24", "dev", "eth0")
	runNamespaceCommand(t, "ip", "-6", "addr", "add", "2001:db8:1::2/64", "dev", "eth0", "nodad")
	runNamespaceCommand(t, "ip", "route", "add", "default", "via", "192.0.2.1", "dev", "eth0")
	runNamespaceCommand(t, "ip", "-6", "route", "add", "default", "via", "2001:db8:1::1", "dev", "eth0")
	runNamespaceCommand(t, "ip", "route", "add", "10.0.0.0/8", "dev", "eth0")
	runNamespaceCommand(t, "ip", "link", "add", "vpn0", "type", "dummy")
	runNamespaceCommand(t, "ip", "link", "set", "vpn0", "up")
	runNamespaceCommand(t, "ip", "route", "add", "table", "10001", "10.1.0.0/16", "dev", "vpn0")

	system, err := NewLinuxSystem(namespaceRuntimeUID)
	if err != nil {
		t.Fatalf("create Linux privileged network system: %v", err)
	}
	testLinuxNamespaceConflicts(t, system)
	testLinuxNamespaceRuntimeAccountIPC(t, system)
	testLinuxNamespaceIPv6Degradation(t, system)

	stateRoot := filepath.Join(t.TempDir(), "network")
	manager, err := OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("open Linux namespace network manager: %v", err)
	}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	manager.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	manager.LeaseTTL = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- manager.Run(ctx) }()

	responder := activateNamespaceTUN(t, manager, "op_namespace_runtime")
	assertNamespaceTraffic(t)
	assertNamespaceRouteDevice(t, "203.0.113.10", linuxTUNDevice)
	assertNamespaceRouteDevice(t, "2001:db8:ffff::10", linuxTUNDevice)
	assertNamespaceRouteDevice(t, "10.1.2.3", "vpn0")

	clock.Add(int64(10 * time.Second))
	waitForNamespaceLinkState(t, system, false)
	responder.Close()
	assertNamespaceRouteDevice(t, "203.0.113.10", "eth0")
	assertNamespaceRouteDevice(t, "10.1.2.3", "eth0")
	cancel()
	if err := <-runResult; err != nil {
		t.Fatalf("stop Linux namespace lease manager: %v", err)
	}

	manager, err = OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("reopen Linux namespace network manager: %v", err)
	}
	responder = activateNamespaceTUN(t, manager, "op_namespace_helper")
	restarted, err := OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("simulate privileged network process restart: %v", err)
	}
	status, err := restarted.Recover(t.Context())
	responder.Close()
	if err != nil || status.State != runtimeapi.NetworkStateInactive || len(status.Residuals) != 0 {
		t.Fatalf("privileged process restart recovery status=%#v err=%v", status, err)
	}
	assertNamespaceRouteDevice(t, "203.0.113.10", "eth0")

	manager, err = OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("reopen Linux namespace network manager after recovery: %v", err)
	}
	responder = activateNamespaceTUN(t, manager, "op_namespace_mihomo")
	responder.Close()
	status, err = manager.FailOpen(t.Context(), ReleaseMihomoFailure)
	if err != nil || status.State != runtimeapi.NetworkStateInactive || len(status.Residuals) != 0 {
		t.Fatalf("Mihomo crash fail-open status=%#v err=%v", status, err)
	}
	assertNamespaceRouteDevice(t, "203.0.113.10", "eth0")
	assertNoNamespaceOwnedObjects(t, system)
}

func TestLinuxNetworkNamespaceRuntimeAccountClient(t *testing.T) {
	endpoint := os.Getenv("SUBMUX_PRIVILEGED_NETNS_ENDPOINT")
	if endpoint == "" {
		t.Skip("internal child for the privileged Linux namespace test")
	}
	connector := &Connector{
		Endpoint:          endpoint,
		RuntimeInstanceID: "runtime_0123456789abcdef0123456789abcdef",
	}
	preview, err := connector.Preview(t.Context(), runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeTUN,
		IPv6Policy: runtimeapi.TUNIPv6Proxy,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil {
		t.Fatalf("Runtime service account could not connect to privileged network IPC: %v", err)
	}
	defer connector.Close()
	if preview.Device != linuxTUNDevice {
		t.Fatalf("Runtime service account network preview=%#v", preview)
	}
	connector.mu.Lock()
	if connector.client == nil {
		connector.mu.Unlock()
		t.Fatal("Runtime network connector did not retain its typed client")
	}
	_ = connector.client.Close()
	connector.mu.Unlock()
	preview, err = connector.Preview(t.Context(), runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeTUN,
		IPv6Policy: runtimeapi.TUNIPv6Proxy,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil || preview.Device != linuxTUNDevice {
		t.Fatalf("Runtime network connector did not recover its session: preview=%#v err=%v", preview, err)
	}
}

func testLinuxNamespaceConflicts(t *testing.T, system *LinuxSystem) {
	t.Helper()
	runNamespaceCommand(t, "ip", "link", "add", linuxTUNDevice, "type", "dummy")
	discovery, err := system.Discover(t.Context(), runtimeapi.TUNSettings{
		IPv6Policy: runtimeapi.TUNIPv6Proxy,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil || !hasNetworkConflict(discovery.Conflicts, "tun_name") {
		t.Fatalf("same-name Linux TUN conflict discovery=%#v err=%v", discovery, err)
	}
	ownership := namespaceOwnership(t, "b")
	if _, err := system.PrepareTUN(t.Context(), ownership); err == nil {
		t.Fatal("Linux privileged network system overwrote a wrong-owner TUN device")
	}
	runNamespaceCommand(t, "ip", "link", "delete", linuxTUNDevice)

	runNamespaceCommand(t, "ip", "link", "add", "wg0", "type", "dummy")
	runNamespaceCommand(t, "ip", "link", "set", "wg0", "up")
	runNamespaceCommand(t, "ip", "route", "add", "default", "dev", "wg0", "table", "51820")
	discovery, err = system.Discover(t.Context(), runtimeapi.TUNSettings{
		IPv6Policy: runtimeapi.TUNIPv6Proxy,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil || !hasNetworkConflict(discovery.Conflicts, "full_tunnel") {
		t.Fatalf("Linux full-tunnel conflict discovery=%#v err=%v", discovery, err)
	}
	runNamespaceCommand(t, "ip", "route", "delete", "default", "dev", "wg0", "table", "51820")
	runNamespaceCommand(t, "ip", "link", "delete", "wg0")
}

func testLinuxNamespaceRuntimeAccountIPC(t *testing.T, system *LinuxSystem) {
	t.Helper()
	manager, err := OpenManager(
		filepath.Join(t.TempDir(), "network-ipc"),
		namespaceRuntimeUID,
		system,
	)
	if err != nil {
		t.Fatalf("open Linux namespace IPC manager: %v", err)
	}
	socketDirectoryID, err := randomIdentifier("submux-runtime-netns-")
	if err != nil {
		t.Fatalf("create Linux namespace IPC directory name: %v", err)
	}
	socketDirectory := filepath.Join(os.TempDir(), socketDirectoryID)
	endpoint := filepath.Join(socketDirectory, "runtime-net.sock")
	t.Cleanup(func() { _ = os.Remove(socketDirectory) })
	listener, err := Listen(endpoint, namespaceRuntimeUID)
	if err != nil {
		t.Fatalf("listen on Linux namespace privileged IPC: %v", err)
	}
	server, err := NewServer(manager)
	if err != nil {
		listener.Close()
		t.Fatalf("create Linux namespace privileged IPC server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx, listener) }()
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Errorf("stop Linux namespace privileged IPC server: %v", err)
		}
	}()

	if client, err := Dial(
		t.Context(),
		endpoint,
		"runtime_0123456789abcdef0123456789abcdef",
	); err == nil {
		client.Close()
		t.Fatal("root connected to privileged Runtime network IPC instead of the Runtime service account")
	}
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestLinuxNetworkNamespaceRuntimeAccountClient$",
		"-test.v",
	)
	command.Env = append(os.Environ(), "SUBMUX_PRIVILEGED_NETNS_ENDPOINT="+endpoint)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:         namespaceRuntimeUID,
			Gid:         namespaceRuntimeUID,
			NoSetGroups: true,
		},
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run Runtime service account IPC client: %v\n%s", err, output)
	}
}

func activateNamespaceTUN(t *testing.T, manager *Manager, operationID string) *tunResponder {
	t.Helper()
	responder, _ := activateNamespaceTUNWithSettings(
		t,
		manager,
		operationID,
		runtimeapi.TUNIPv6Proxy,
		runtimeapi.TUNDNSHijack,
	)
	return responder
}

func activateNamespaceTUNWithSettings(
	t *testing.T,
	manager *Manager,
	operationID string,
	ipv6Policy string,
	dnsPolicy string,
) (*tunResponder, runtimeapi.NetworkPreview) {
	t.Helper()
	session := openTestSession(t, manager, namespaceRuntimeUID)
	preview, err := manager.Preview(t.Context(), session.ID, runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeTUN,
		IPv6Policy: ipv6Policy,
		DNSPolicy:  dnsPolicy,
	})
	if err != nil {
		t.Fatalf("preview Linux namespace TUN: %v", err)
	}
	if len(preview.Conflicts) != 0 {
		t.Fatalf("unexpected Linux namespace TUN conflicts: %#v", preview.Conflicts)
	}
	now := manager.now()
	prepared, err := manager.Prepare(
		t.Context(),
		requestMeta(session, operationID, 1, now),
		preview.PlanID,
	)
	if err != nil {
		t.Fatalf("prepare Linux namespace TUN: %v", err)
	}
	responder, err := openTUNResponder(prepared.Device)
	if err != nil {
		t.Fatalf("attach low-privilege Linux namespace TUN data plane: %v", err)
	}
	status, err := manager.Commit(
		t.Context(),
		requestMeta(session, operationID, 2, now),
		prepared.OwnershipID,
	)
	if err != nil {
		responder.Close()
		t.Fatalf("commit Linux namespace TUN: %v", err)
	}
	if status.State != runtimeapi.NetworkStateActive || status.Device != linuxTUNDevice {
		responder.Close()
		t.Fatalf("active Linux namespace TUN status=%#v", status)
	}
	responder.Run()
	return responder, preview
}

func testLinuxNamespaceIPv6Degradation(t *testing.T, system *LinuxSystem) {
	t.Helper()
	directManager, err := OpenManager(
		filepath.Join(t.TempDir(), "network-direct"),
		namespaceRuntimeUID,
		system,
	)
	if err != nil {
		t.Fatalf("open IPv6-direct Linux namespace manager: %v", err)
	}
	responder, preview := activateNamespaceTUNWithSettings(
		t,
		directManager,
		"op_namespace_ipv6_direct",
		runtimeapi.TUNIPv6Direct,
		runtimeapi.TUNDNSHijack,
	)
	if len(preview.Warnings) == 0 || !strings.Contains(preview.Warnings[0], "IPv6") {
		responder.Close()
		t.Fatalf("IPv6 direct degradation preview warnings=%#v", preview.Warnings)
	}
	assertNamespaceRouteDevice(t, "203.0.113.10", linuxTUNDevice)
	assertNamespaceRouteDevice(t, "2001:db8:ffff::10", "eth0")
	if _, err := directManager.FailOpen(t.Context(), ReleaseOperator); err != nil {
		responder.Close()
		t.Fatalf("clean IPv6-direct Linux namespace TUN: %v", err)
	}
	responder.Close()

	if _, err := exec.LookPath("nft"); err != nil {
		t.Log("nft is unavailable; skipping real IPv6 block object check")
		return
	}
	blockManager, err := OpenManager(
		filepath.Join(t.TempDir(), "network-block"),
		namespaceRuntimeUID,
		system,
	)
	if err != nil {
		t.Fatalf("open IPv6-block Linux namespace manager: %v", err)
	}
	responder, preview = activateNamespaceTUNWithSettings(
		t,
		blockManager,
		"op_namespace_ipv6_block",
		runtimeapi.TUNIPv6Block,
		runtimeapi.TUNDNSHijack,
	)
	if len(preview.Warnings) != 0 {
		responder.Close()
		t.Fatalf("IPv6 block preview warnings=%#v", preview.Warnings)
	}
	nftables := runNamespaceCommand(t, "nft", "list", "tables")
	if !strings.Contains(nftables, "table inet smx_") {
		responder.Close()
		t.Fatalf("IPv6 block nftables object is missing: %s", nftables)
	}
	if _, err := blockManager.FailOpen(t.Context(), ReleaseOperator); err != nil {
		responder.Close()
		t.Fatalf("clean IPv6-block Linux namespace TUN: %v", err)
	}
	responder.Close()
	nftables = runNamespaceCommand(t, "nft", "list", "tables")
	if strings.Contains(nftables, "table inet smx_") {
		t.Fatalf("IPv6 block nftables object remained after cleanup: %s", nftables)
	}
}

func assertNamespaceTraffic(t *testing.T) {
	t.Helper()
	assertTCPEcho(t, "tcp4", "203.0.113.10:18080", []byte("tcp4"))
	assertUDPEcho(t, "udp4", "203.0.113.10:18081", []byte("udp4"))
	assertTCPEcho(t, "tcp6", "[2001:db8:ffff::10]:18080", []byte("tcp6"))
	assertUDPEcho(t, "udp6", "[2001:db8:ffff::10]:18081", []byte("udp6"))
	query := testDNSQuery()
	assertDNSOverUDP(t, "udp4", "203.0.113.53:53", query)
	assertDNSOverTCP(t, "tcp4", "203.0.113.53:53", query)
}

func assertTCPEcho(t *testing.T, network, address string, payload []byte) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s %s through Linux TUN: %v", network, address, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write %s through Linux TUN: %v", network, err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("read %s through Linux TUN: %v", network, err)
	}
	if string(reply) != string(payload) {
		t.Fatalf("%s Linux TUN echo=%q, want %q", network, reply, payload)
	}
}

func assertUDPEcho(t *testing.T, network, address string, payload []byte) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s %s through Linux TUN: %v", network, address, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write %s through Linux TUN: %v", network, err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("read %s through Linux TUN: %v", network, err)
	}
	if string(reply) != string(payload) {
		t.Fatalf("%s Linux TUN echo=%q, want %q", network, reply, payload)
	}
}

func assertDNSOverUDP(t *testing.T, network, address string, query []byte) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		t.Fatalf("dial DNS %s through Linux TUN: %v", network, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := connection.Write(query); err != nil {
		t.Fatalf("write DNS %s through Linux TUN: %v", network, err)
	}
	reply := make([]byte, len(query))
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("read DNS %s through Linux TUN: %v", network, err)
	}
	if reply[2]&0x80 == 0 {
		t.Fatalf("DNS %s Linux TUN response did not set QR: %x", network, reply)
	}
}

func assertDNSOverTCP(t *testing.T, network, address string, query []byte) {
	t.Helper()
	connection, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		t.Fatalf("dial TCP DNS %s through Linux TUN: %v", network, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	framed := make([]byte, len(query)+2)
	binary.BigEndian.PutUint16(framed[:2], uint16(len(query)))
	copy(framed[2:], query)
	if _, err := connection.Write(framed); err != nil {
		t.Fatalf("write TCP DNS %s through Linux TUN: %v", network, err)
	}
	reply := make([]byte, len(framed))
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("read TCP DNS %s through Linux TUN: %v", network, err)
	}
	if binary.BigEndian.Uint16(reply[:2]) != uint16(len(query)) || reply[4]&0x80 == 0 {
		t.Fatalf("TCP DNS %s Linux TUN response is invalid: %x", network, reply)
	}
}

func testDNSQuery() []byte {
	return []byte{
		0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x01, 0x00, 0x01,
	}
}

type tunResponder struct {
	fd      int
	stopped atomic.Bool
	packets atomic.Uint64
	done    chan struct{}
}

func openTUNResponder(device string) (*tunResponder, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	request, err := unix.NewIfreq(device)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	request.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, request); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &tunResponder{fd: fd, done: make(chan struct{})}, nil
}

func (responder *tunResponder) Run() {
	go func() {
		defer close(responder.done)
		buffer := make([]byte, 65535)
		for {
			count, err := unix.Read(responder.fd, buffer)
			if err != nil {
				if responder.stopped.Load() || errors.Is(err, unix.EBADF) {
					return
				}
				continue
			}
			reply := replyTUNPacket(buffer[:count])
			if len(reply) == 0 {
				continue
			}
			responder.packets.Add(1)
			_, _ = unix.Write(responder.fd, reply)
		}
	}()
}

func (responder *tunResponder) PacketCount() uint64 {
	if responder == nil {
		return 0
	}
	return responder.packets.Load()
}

func (responder *tunResponder) Close() {
	if responder == nil || !responder.stopped.CompareAndSwap(false, true) {
		return
	}
	_ = unix.Close(responder.fd)
	select {
	case <-responder.done:
	case <-time.After(time.Second):
	}
}

func replyTUNPacket(packet []byte) []byte {
	if len(packet) < 1 {
		return nil
	}
	switch packet[0] >> 4 {
	case 4:
		return replyIPv4Packet(packet)
	case 6:
		return replyIPv6Packet(packet)
	default:
		return nil
	}
}

func replyIPv4Packet(packet []byte) []byte {
	if len(packet) < 20 {
		return nil
	}
	headerLength := int(packet[0]&0x0f) * 4
	totalLength := int(binary.BigEndian.Uint16(packet[2:4]))
	if headerLength < 20 || totalLength < headerLength || totalLength > len(packet) {
		return nil
	}
	protocol := packet[9]
	transport := replyTransport(protocol, packet[headerLength:totalLength])
	if len(transport) == 0 {
		return nil
	}
	reply := make([]byte, headerLength+len(transport))
	copy(reply[:headerLength], packet[:headerLength])
	copy(reply[12:16], packet[16:20])
	copy(reply[16:20], packet[12:16])
	reply[8] = 64
	binary.BigEndian.PutUint16(reply[2:4], uint16(len(reply)))
	reply[10], reply[11] = 0, 0
	binary.BigEndian.PutUint16(reply[10:12], internetChecksum(reply[:headerLength]))
	copy(reply[headerLength:], transport)
	setTransportChecksumIPv4(reply[12:16], reply[16:20], protocol, reply[headerLength:])
	return reply
}

func replyIPv6Packet(packet []byte) []byte {
	if len(packet) < 40 {
		return nil
	}
	payloadLength := int(binary.BigEndian.Uint16(packet[4:6]))
	if 40+payloadLength > len(packet) {
		return nil
	}
	protocol := packet[6]
	transport := replyTransport(protocol, packet[40:40+payloadLength])
	if len(transport) == 0 {
		return nil
	}
	reply := make([]byte, 40+len(transport))
	copy(reply[:40], packet[:40])
	copy(reply[8:24], packet[24:40])
	copy(reply[24:40], packet[8:24])
	reply[7] = 64
	binary.BigEndian.PutUint16(reply[4:6], uint16(len(transport)))
	copy(reply[40:], transport)
	setTransportChecksumIPv6(reply[8:24], reply[24:40], protocol, reply[40:])
	return reply
}

func replyTransport(protocol byte, segment []byte) []byte {
	switch protocol {
	case unix.IPPROTO_UDP:
		if len(segment) < 8 {
			return nil
		}
		payload := append([]byte(nil), segment[8:]...)
		if binary.BigEndian.Uint16(segment[2:4]) == 53 {
			markDNSResponse(payload, false)
		}
		reply := make([]byte, 8+len(payload))
		copy(reply[0:2], segment[2:4])
		copy(reply[2:4], segment[0:2])
		binary.BigEndian.PutUint16(reply[4:6], uint16(len(reply)))
		copy(reply[8:], payload)
		return reply
	case unix.IPPROTO_TCP:
		return replyTCP(segment)
	default:
		return nil
	}
}

func replyTCP(segment []byte) []byte {
	if len(segment) < 20 {
		return nil
	}
	headerLength := int(segment[12]>>4) * 4
	if headerLength < 20 || headerLength > len(segment) {
		return nil
	}
	sourcePort := binary.BigEndian.Uint16(segment[0:2])
	destinationPort := binary.BigEndian.Uint16(segment[2:4])
	sequence := binary.BigEndian.Uint32(segment[4:8])
	acknowledgement := binary.BigEndian.Uint32(segment[8:12])
	flags := segment[13]
	payload := append([]byte(nil), segment[headerLength:]...)
	var responseFlags byte
	var responseSequence, responseAcknowledgement uint32
	switch {
	case flags&0x02 != 0:
		responseFlags = 0x12
		responseSequence = 0x51525354
		responseAcknowledgement = sequence + 1
		payload = nil
	case len(payload) > 0:
		if destinationPort == 53 {
			markDNSResponse(payload, true)
		}
		responseFlags = 0x18
		responseSequence = acknowledgement
		responseAcknowledgement = sequence + uint32(len(segment[headerLength:]))
	case flags&0x01 != 0:
		responseFlags = 0x11
		responseSequence = acknowledgement
		responseAcknowledgement = sequence + 1
		payload = nil
	default:
		return nil
	}
	reply := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(reply[0:2], destinationPort)
	binary.BigEndian.PutUint16(reply[2:4], sourcePort)
	binary.BigEndian.PutUint32(reply[4:8], responseSequence)
	binary.BigEndian.PutUint32(reply[8:12], responseAcknowledgement)
	reply[12] = 5 << 4
	reply[13] = responseFlags
	binary.BigEndian.PutUint16(reply[14:16], 65535)
	copy(reply[20:], payload)
	return reply
}

func markDNSResponse(payload []byte, tcp bool) {
	offset := 0
	if tcp {
		if len(payload) < 2 {
			return
		}
		length := int(binary.BigEndian.Uint16(payload[:2]))
		if length > len(payload)-2 {
			return
		}
		offset = 2
	}
	if len(payload)-offset < 12 {
		return
	}
	payload[offset+2] |= 0x80
	payload[offset+3] |= 0x80
}

func setTransportChecksumIPv4(source, destination []byte, protocol byte, transport []byte) {
	checksumOffset := transportChecksumOffset(protocol)
	if checksumOffset < 0 || len(transport) < checksumOffset+2 {
		return
	}
	transport[checksumOffset], transport[checksumOffset+1] = 0, 0
	pseudo := make([]byte, 12+len(transport))
	copy(pseudo[0:4], source)
	copy(pseudo[4:8], destination)
	pseudo[9] = protocol
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(transport)))
	copy(pseudo[12:], transport)
	value := internetChecksum(pseudo)
	if value == 0 {
		value = 0xffff
	}
	binary.BigEndian.PutUint16(transport[checksumOffset:checksumOffset+2], value)
}

func setTransportChecksumIPv6(source, destination []byte, protocol byte, transport []byte) {
	checksumOffset := transportChecksumOffset(protocol)
	if checksumOffset < 0 || len(transport) < checksumOffset+2 {
		return
	}
	transport[checksumOffset], transport[checksumOffset+1] = 0, 0
	pseudo := make([]byte, 40+len(transport))
	copy(pseudo[0:16], source)
	copy(pseudo[16:32], destination)
	binary.BigEndian.PutUint32(pseudo[32:36], uint32(len(transport)))
	pseudo[39] = protocol
	copy(pseudo[40:], transport)
	value := internetChecksum(pseudo)
	if value == 0 {
		value = 0xffff
	}
	binary.BigEndian.PutUint16(transport[checksumOffset:checksumOffset+2], value)
}

func transportChecksumOffset(protocol byte) int {
	switch protocol {
	case unix.IPPROTO_UDP:
		return 6
	case unix.IPPROTO_TCP:
		return 16
	default:
		return -1
	}
}

func internetChecksum(body []byte) uint16 {
	var sum uint32
	for len(body) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(body[:2]))
		body = body[2:]
	}
	if len(body) == 1 {
		sum += uint32(body[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func namespaceOwnership(t *testing.T, tokenCharacter string) Ownership {
	t.Helper()
	token := strings.Repeat(tokenCharacter, 64)
	table, priority, err := linuxOwnershipNumbers(token)
	if err != nil {
		t.Fatalf("derive Linux namespace ownership numbers: %v", err)
	}
	return Ownership{
		ID:           "net_namespace",
		Token:        token,
		Mode:         runtimeapi.RunModeTUN,
		Device:       linuxTUNDevice,
		RoutingMark:  table,
		RouteTable:   table,
		RulePriority: priority,
		Settings: runtimeapi.TUNSettings{
			IPv6Policy: runtimeapi.TUNIPv6Proxy,
			DNSPolicy:  runtimeapi.TUNDNSHijack,
		},
		State: runtimeapi.NetworkStatePrepared,
	}
}

func hasNetworkConflict(conflicts []runtimeapi.NetworkConflict, kind string) bool {
	for _, conflict := range conflicts {
		if conflict.Kind == kind {
			return true
		}
	}
	return false
}

func waitForNamespaceLinkState(t *testing.T, system *LinuxSystem, exists bool) {
	waitForNamespaceDeviceState(t, system, linuxTUNDevice, exists)
}

func waitForNamespaceDeviceState(
	t *testing.T,
	system *LinuxSystem,
	device string,
	exists bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, observed, err := system.link(t.Context(), device)
		if err == nil && observed == exists {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, observed, err := system.link(t.Context(), device)
	t.Fatalf("Linux namespace %s link exists=%t, want %t; err=%v", device, observed, exists, err)
}

func assertNamespaceRouteDevice(t *testing.T, destination, device string) {
	t.Helper()
	family := "-4"
	if strings.Contains(destination, ":") {
		family = "-6"
	}
	output := runNamespaceCommand(t, "ip", "-j", family, "route", "get", destination)
	if !strings.Contains(output, `"dev":"`+device+`"`) {
		t.Fatalf("route to %s does not use %s: %s", destination, device, output)
	}
}

func assertNoNamespaceOwnedObjects(t *testing.T, system *LinuxSystem) {
	t.Helper()
	_, exists, err := system.link(t.Context(), linuxTUNDevice)
	if err != nil || exists {
		t.Fatalf("Linux namespace TUN remained after cleanup: exists=%t err=%v", exists, err)
	}
	for _, family := range []string{"-4", "-6"} {
		output := runNamespaceCommand(t, "ip", "-j", family, "route", "show", "table", "all")
		if strings.Contains(output, linuxTUNDevice) {
			t.Fatalf("Linux namespace route remained after cleanup: %s", output)
		}
	}
}

func runNamespaceCommand(t *testing.T, name string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s %s: %v\n%s", name, strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
