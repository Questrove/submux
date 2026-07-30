//go:build linux

package runtimenet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

const (
	gatewayNamespaceChildEnv = "SUBMUX_GATEWAY_NETNS_CHILD"
	gatewayPeerRoleEnv       = "SUBMUX_GATEWAY_PEER_ROLE"
)

func TestLinuxNetworkNamespaceGateway(t *testing.T) {
	if os.Getenv(privilegedNamespaceTestEnv) != "1" {
		t.Skip("set SUBMUX_RUN_PRIVILEGED_NETNS_TESTS=1 to run the privileged Linux namespace test")
	}
	if os.Getenv(gatewayNamespaceChildEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Fatal("privileged Linux gateway namespace test must run as root")
		}
		command := exec.Command(
			"unshare",
			"--net",
			os.Args[0],
			"-test.run=^TestLinuxNetworkNamespaceGateway$",
			"-test.v",
		)
		command.Env = append(os.Environ(), gatewayNamespaceChildEnv+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("run isolated Linux gateway namespace: %v\n%s", err, output)
		}
		t.Logf("isolated Linux gateway namespace output:\n%s", output)
		return
	}
	runLinuxGatewayNamespace(t)
}

func TestLinuxNetworkNamespaceGatewaySingleArm(t *testing.T) {
	if os.Getenv(privilegedNamespaceTestEnv) != "1" {
		t.Skip("set SUBMUX_RUN_PRIVILEGED_NETNS_TESTS=1 to run the privileged Linux namespace test")
	}
	if os.Getenv(gatewayNamespaceChildEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Fatal("privileged Linux gateway namespace test must run as root")
		}
		command := exec.Command(
			"unshare",
			"--net",
			os.Args[0],
			"-test.run=^TestLinuxNetworkNamespaceGatewaySingleArm$",
			"-test.v",
		)
		command.Env = append(os.Environ(), gatewayNamespaceChildEnv+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("run isolated single-arm Linux gateway namespace: %v\n%s", err, output)
		}
		t.Logf("isolated single-arm Linux gateway namespace output:\n%s", output)
		return
	}
	runLinuxGatewaySingleArmNamespace(t)
}

func TestLinuxGatewayNamespacePeer(t *testing.T) {
	role := os.Getenv(gatewayPeerRoleEnv)
	if role == "" {
		t.Skip("Linux gateway namespace peer helper")
	}
	switch role {
	case "capture":
		assertGatewayCapturedTCP(t, "203.0.113.10:18080", "")
		assertGatewayCapturedUDP(t, "203.0.113.10:18081", "")
		assertGatewayQUICShapedUDP(t, "203.0.113.10:443", "")
		assertDNSOverUDP(t, "udp4", "203.0.113.53:53", testDNSQuery())
		assertDNSOverTCP(t, "tcp4", "203.0.113.53:53", testDNSQuery())
	case "new-client":
		assertGatewayCapturedTCP(t, "203.0.113.10:18080", "10.0.0.3")
		assertGatewayCapturedUDP(t, "203.0.113.10:18081", "10.0.0.3")
	case "concurrent":
		assertGatewayConcurrentTraffic(t)
	case "udp-off":
		assertGatewayCapturedTCP(t, "203.0.113.10:18080", "")
		assertGatewayDirectFailure(t, "udp", "203.0.113.10:18081")
		assertDNSOverUDP(t, "udp4", "203.0.113.53:53", testDNSQuery())
		assertDNSOverTCP(t, "tcp4", "203.0.113.53:53", testDNSQuery())
	case "udp-exception":
		assertGatewayCapturedUDP(t, "203.0.113.10:18081", "")
		assertGatewayDirectFailure(t, "udp", "203.0.113.10:443")
	case "host":
		assertGatewayCapturedTCP(t, os.Getenv("SUBMUX_GATEWAY_TARGET"), "")
	case "ipv6-direct":
		assertGatewayCapturedTCP(t, "[2001:db8:20::1]:19080", "")
	case "ipv6-block":
		assertGatewayDirectFailure(t, "tcp", "[2001:db8:20::1]:19080")
	case "dnat":
		assertGatewayCapturedTCP(t, "198.51.100.2:28080", "")
		assertGatewayCapturedUDP(t, "198.51.100.2:28081", "")
	case "wan-local":
		assertGatewayCapturedTCP(t, "198.51.100.2:29080", "")
	case "single-arm-local":
		assertGatewayCapturedTCP(t, "198.51.100.1:19080", "")
	case "echo-server":
		runGatewayEchoServer(t)
	default:
		t.Fatalf("unknown Linux gateway peer role %q", role)
	}
}

func runLinuxGatewayNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("isolated Linux gateway namespace child is not root")
	}
	for _, command := range []string{"ip", "nft", "ping", "sysctl"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("Linux gateway namespace requires %s: %v", command, err)
		}
	}
	lanNamespace := fmt.Sprintf("smxlan%d", os.Getpid())
	wanNamespace := fmt.Sprintf("smxwan%d", os.Getpid())
	runNamespaceCommand(t, "ip", "netns", "add", lanNamespace)
	t.Cleanup(func() { _, _ = exec.Command("ip", "netns", "delete", lanNamespace).CombinedOutput() })
	runNamespaceCommand(t, "ip", "netns", "add", wanNamespace)
	t.Cleanup(func() { _, _ = exec.Command("ip", "netns", "delete", wanNamespace).CombinedOutput() })
	setupGatewayDoubleArmNamespace(t, lanNamespace, wanNamespace)
	runNamespaceCommand(t, "ping", "-6", "-c", "1", "-W", "2", "2001:db8:20::1")
	runGatewayNamespaceCommand(
		t,
		lanNamespace,
		"ping", "-6", "-c", "1", "-W", "2", "2001:db8:10::1",
	)
	runGatewayNamespaceCommand(
		t,
		lanNamespace,
		"ping", "-6", "-c", "1", "-W", "2", "2001:db8:20::1",
	)

	lanServer := startGatewayEchoServer(
		t,
		lanNamespace,
		"10.0.0.2:28080",
		"10.0.0.2:28081",
	)
	defer stopGatewayPeerServer(lanServer)
	wanServer := startGatewayEchoServer(
		t,
		wanNamespace,
		"198.51.100.1:19080,[2001:db8:20::1]:19080",
		"",
	)
	defer stopGatewayPeerServer(wanServer)
	localServer := startGatewayLocalTCPServer(t, "198.51.100.2:29080")
	defer localServer()
	installGatewayUserDNAT(t)

	system, err := NewLinuxSystem(namespaceRuntimeUID)
	if err != nil {
		t.Fatalf("create Linux gateway system: %v", err)
	}
	stateRoot := filepath.Join(t.TempDir(), "gateway-network")
	manager, err := OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("open Linux gateway manager: %v", err)
	}

	defaultSettings := runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeGateway,
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	}
	responder := activateNamespaceGateway(t, manager, "op_gateway_default", defaultSettings)
	runGatewayPeer(t, lanNamespace, "capture", 0, "")
	runGatewayPeer(t, lanNamespace, "concurrent", 0, "")
	runNamespaceCommand(t, "ip", "netns", "exec", lanNamespace, "ip", "addr", "add", "10.0.0.3/24", "dev", "peer0")
	runGatewayPeer(t, lanNamespace, "new-client", 0, "")
	beforeWAN := responder.PacketCount()
	runGatewayPeer(t, wanNamespace, "wan-local", 0, "")
	runGatewayPeer(t, wanNamespace, "dnat", 0, "")
	if responder.PacketCount() != beforeWAN {
		t.Fatal("WAN local or DNAT traffic entered the gateway TUN")
	}
	beforeHost := responder.PacketCount()
	runGatewayPeer(t, "", "host", 65533, "198.51.100.1:19080")
	if responder.PacketCount() != beforeHost {
		t.Fatal("gateway host traffic was captured without explicit authorization")
	}
	runGatewayPeer(t, lanNamespace, "ipv6-direct", 0, "")
	failOpenGateway(t, manager, ReleaseOperator)
	responder.Close()
	assertGatewayUserDNATPresent(t)

	captureUDP := false
	responder = activateNamespaceGateway(t, manager, "op_gateway_udp_off", runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeGateway,
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
		CaptureUDP: &captureUDP,
	})
	runGatewayPeer(t, lanNamespace, "udp-off", 0, "")
	failOpenGateway(t, manager, ReleaseOperator)
	responder.Close()

	responder = activateNamespaceGateway(t, manager, "op_gateway_udp_exception", runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeGateway,
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
		UDPExceptions: []runtimeapi.GatewayTrafficException{{
			DestinationCIDR: "203.0.113.10/32",
			DestinationPorts: []runtimeapi.NetworkPortRange{{
				Start: 443,
			}},
		}},
	})
	runGatewayPeer(t, lanNamespace, "udp-exception", 0, "")
	failOpenGateway(t, manager, ReleaseUpdate)
	responder.Close()

	responder = activateNamespaceGateway(t, manager, "op_gateway_host_ipv6_block", runtimeapi.NetworkPreviewRequest{
		Mode:             runtimeapi.RunModeGateway,
		IPv6Policy:       runtimeapi.TUNIPv6Block,
		DNSPolicy:        runtimeapi.TUNDNSHijack,
		ProxyHostTraffic: true,
	})
	beforeHost = responder.PacketCount()
	runGatewayPeer(t, "", "host", 65533, "203.0.113.10:18080")
	if responder.PacketCount() <= beforeHost {
		t.Fatal("authorized gateway host traffic did not enter the TUN")
	}
	runGatewayPeer(t, lanNamespace, "ipv6-block", 0, "")
	status, err := manager.FailOpen(t.Context(), ReleaseMihomoFailure)
	responder.Close()
	if err != nil || status.State != runtimeapi.NetworkStateInactive {
		t.Fatalf("Mihomo gateway fail-open status=%#v err=%v", status, err)
	}
	assertGatewayUserDNATPresent(t)

	responder = activateNamespaceGateway(t, manager, "op_gateway_helper_restart", defaultSettings)
	restarted, err := OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("reopen Linux gateway manager: %v", err)
	}
	status, err = restarted.Recover(t.Context())
	responder.Close()
	if err != nil || status.State != runtimeapi.NetworkStateInactive || len(status.Residuals) != 0 {
		t.Fatalf("gateway helper restart recovery status=%#v err=%v", status, err)
	}
	assertGatewayUserDNATPresent(t)

	testGatewayLeaseExpiry(t, stateRoot, system, defaultSettings)
	assertGatewayUserDNATPresent(t)
	assertNoGatewayOwnedObjects(t, system)
}

func runLinuxGatewaySingleArmNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("isolated single-arm Linux gateway namespace child is not root")
	}
	for _, command := range []string{"ip", "nft", "sysctl"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("single-arm Linux gateway namespace requires %s: %v", command, err)
		}
	}
	lanNamespace := fmt.Sprintf("smxslan%d", os.Getpid())
	wanNamespace := fmt.Sprintf("smxswan%d", os.Getpid())
	runNamespaceCommand(t, "ip", "netns", "add", lanNamespace)
	t.Cleanup(func() { _, _ = exec.Command("ip", "netns", "delete", lanNamespace).CombinedOutput() })
	runNamespaceCommand(t, "ip", "netns", "add", wanNamespace)
	t.Cleanup(func() { _, _ = exec.Command("ip", "netns", "delete", wanNamespace).CombinedOutput() })
	setupGatewaySingleArmNamespace(t, lanNamespace, wanNamespace)

	wanServer := startGatewayEchoServer(
		t,
		wanNamespace,
		"198.51.100.1:19080",
		"",
	)
	defer stopGatewayPeerServer(wanServer)

	system, err := NewLinuxSystem(namespaceRuntimeUID)
	if err != nil {
		t.Fatalf("create single-arm Linux gateway system: %v", err)
	}
	manager, err := OpenManager(
		filepath.Join(t.TempDir(), "single-arm-gateway-network"),
		namespaceRuntimeUID,
		system,
	)
	if err != nil {
		t.Fatalf("open single-arm Linux gateway manager: %v", err)
	}
	settings := runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeGateway,
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	}
	session := openTestSession(t, manager, namespaceRuntimeUID)
	preview, err := manager.Preview(t.Context(), session.ID, settings)
	if err != nil {
		t.Fatalf("preview single-arm Linux gateway: %v", err)
	}
	var lanRoute, wanRoute *runtimeapi.NetworkRoute
	for index := range preview.Routes {
		route := &preview.Routes[index]
		switch route.CIDR {
		case "10.1.0.0/24":
			lanRoute = route
		case "198.51.100.0/24":
			wanRoute = route
		}
	}
	if lanRoute == nil ||
		lanRoute.Interface != "edge0" ||
		lanRoute.Role != runtimeapi.NetworkRouteRoleGatewayLAN ||
		lanRoute.Bypass {
		t.Fatalf("single-arm Linux gateway LAN route=%#v", lanRoute)
	}
	if wanRoute == nil ||
		wanRoute.Interface != "edge0" ||
		wanRoute.Role != runtimeapi.NetworkRouteRoleGatewayWAN ||
		!wanRoute.Bypass {
		t.Fatalf("single-arm Linux gateway WAN route=%#v", wanRoute)
	}

	responder := activateNamespaceGateway(t, manager, "op_gateway_single_arm", settings)
	runGatewayPeer(t, lanNamespace, "capture", 0, "")
	beforeDirect := responder.PacketCount()
	runGatewayPeer(t, lanNamespace, "single-arm-local", 0, "")
	if responder.PacketCount() != beforeDirect {
		t.Fatal("single-arm WAN-local traffic entered the gateway TUN")
	}
	failOpenGateway(t, manager, ReleaseOperator)
	responder.Close()
	assertNoGatewayOwnedObjects(t, system)
}

func setupGatewayDoubleArmNamespace(t *testing.T, lanNamespace, wanNamespace string) {
	t.Helper()
	runNamespaceCommand(t, "ip", "link", "set", "lo", "up")
	runNamespaceCommand(t, "ip", "link", "add", "lan0", "type", "veth", "peer", "name", "peer0")
	runNamespaceCommand(t, "ip", "link", "set", "peer0", "netns", lanNamespace)
	runNamespaceCommand(t, "ip", "link", "add", "wan0", "type", "veth", "peer", "name", "peer0")
	runNamespaceCommand(t, "ip", "link", "set", "peer0", "netns", wanNamespace)
	runNamespaceCommand(t, "ip", "link", "set", "lan0", "up")
	runNamespaceCommand(t, "ip", "addr", "add", "10.0.0.1/24", "dev", "lan0")
	runNamespaceCommand(t, "ip", "-6", "addr", "add", "2001:db8:10::1/64", "dev", "lan0", "nodad")
	runNamespaceCommand(t, "ip", "link", "set", "wan0", "up")
	runNamespaceCommand(t, "ip", "addr", "add", "198.51.100.2/24", "dev", "wan0")
	runNamespaceCommand(t, "ip", "-6", "addr", "add", "2001:db8:20::2/64", "dev", "wan0", "nodad")
	runNamespaceCommand(t, "ip", "route", "add", "default", "via", "198.51.100.1", "dev", "wan0")
	runNamespaceCommand(t, "ip", "-6", "route", "add", "default", "via", "2001:db8:20::1", "dev", "wan0")
	runNamespaceCommand(t, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
	runNamespaceCommand(t, "sysctl", "-q", "-w", "net.ipv6.conf.all.forwarding=1")

	runGatewayNamespaceCommand(t, lanNamespace, "ip", "link", "set", "lo", "up")
	runGatewayNamespaceCommand(t, lanNamespace, "ip", "link", "set", "peer0", "up")
	runGatewayNamespaceCommand(t, lanNamespace, "ip", "addr", "add", "10.0.0.2/24", "dev", "peer0")
	runGatewayNamespaceCommand(
		t,
		lanNamespace,
		"ip", "-6", "addr", "add", "2001:db8:10::2/64", "dev", "peer0", "nodad",
	)
	runGatewayNamespaceCommand(t, lanNamespace, "ip", "route", "add", "default", "via", "10.0.0.1")
	runGatewayNamespaceCommand(
		t,
		lanNamespace,
		"ip", "-6", "route", "add", "default", "via", "2001:db8:10::1",
	)

	runGatewayNamespaceCommand(t, wanNamespace, "ip", "link", "set", "lo", "up")
	runGatewayNamespaceCommand(t, wanNamespace, "ip", "link", "set", "peer0", "up")
	runGatewayNamespaceCommand(t, wanNamespace, "ip", "addr", "add", "198.51.100.1/24", "dev", "peer0")
	runGatewayNamespaceCommand(
		t,
		wanNamespace,
		"ip", "-6", "addr", "add", "2001:db8:20::1/64", "dev", "peer0", "nodad",
	)
	runGatewayNamespaceCommand(
		t,
		wanNamespace,
		"ip", "route", "add", "10.0.0.0/24", "via", "198.51.100.2",
	)
	runGatewayNamespaceCommand(
		t,
		wanNamespace,
		"ip", "-6", "route", "add", "2001:db8:10::/64", "via", "2001:db8:20::2",
	)
}

func setupGatewaySingleArmNamespace(t *testing.T, lanNamespace, wanNamespace string) {
	t.Helper()
	runNamespaceCommand(t, "ip", "link", "set", "lo", "up")
	runNamespaceCommand(t, "ip", "link", "add", "edge0", "type", "bridge")
	runNamespaceCommand(t, "ip", "link", "add", "lanport", "type", "veth", "peer", "name", "peer0")
	runNamespaceCommand(t, "ip", "link", "set", "peer0", "netns", lanNamespace)
	runNamespaceCommand(t, "ip", "link", "add", "wanport", "type", "veth", "peer", "name", "peer0")
	runNamespaceCommand(t, "ip", "link", "set", "peer0", "netns", wanNamespace)
	runNamespaceCommand(t, "ip", "link", "set", "lanport", "master", "edge0")
	runNamespaceCommand(t, "ip", "link", "set", "wanport", "master", "edge0")
	runNamespaceCommand(t, "ip", "link", "set", "edge0", "up")
	runNamespaceCommand(t, "ip", "link", "set", "lanport", "up")
	runNamespaceCommand(t, "ip", "link", "set", "wanport", "up")
	runNamespaceCommand(t, "ip", "addr", "add", "10.1.0.1/24", "dev", "edge0")
	runNamespaceCommand(t, "ip", "addr", "add", "198.51.100.2/24", "dev", "edge0")
	runNamespaceCommand(t, "ip", "route", "add", "default", "via", "198.51.100.1", "dev", "edge0")
	runNamespaceCommand(t, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")

	runGatewayNamespaceCommand(t, lanNamespace, "ip", "link", "set", "lo", "up")
	runGatewayNamespaceCommand(t, lanNamespace, "ip", "link", "set", "peer0", "up")
	runGatewayNamespaceCommand(t, lanNamespace, "ip", "addr", "add", "10.1.0.2/24", "dev", "peer0")
	runGatewayNamespaceCommand(t, lanNamespace, "ip", "route", "add", "default", "via", "10.1.0.1")

	runGatewayNamespaceCommand(t, wanNamespace, "ip", "link", "set", "lo", "up")
	runGatewayNamespaceCommand(t, wanNamespace, "ip", "link", "set", "peer0", "up")
	runGatewayNamespaceCommand(t, wanNamespace, "ip", "addr", "add", "198.51.100.1/24", "dev", "peer0")
	runGatewayNamespaceCommand(
		t,
		wanNamespace,
		"ip", "route", "add", "10.1.0.0/24", "via", "198.51.100.2",
	)
}

func runGatewayNamespaceCommand(t *testing.T, namespace string, name string, arguments ...string) {
	t.Helper()
	commandArguments := append([]string{"netns", "exec", namespace, name}, arguments...)
	runNamespaceCommand(t, "ip", commandArguments...)
}

func activateNamespaceGateway(
	t *testing.T,
	manager *Manager,
	operationID string,
	request runtimeapi.NetworkPreviewRequest,
) *tunResponder {
	t.Helper()
	session := openTestSession(t, manager, namespaceRuntimeUID)
	preview, err := manager.Preview(t.Context(), session.ID, request)
	if err != nil {
		t.Fatalf("preview Linux gateway: %v", err)
	}
	if len(preview.Conflicts) > 0 {
		t.Fatalf("Linux gateway preview conflicts=%#v", preview.Conflicts)
	}
	prepared, err := manager.Prepare(
		t.Context(),
		requestMeta(session, operationID, 1, time.Now().UTC()),
		preview.PlanID,
	)
	if err != nil {
		t.Fatalf("prepare Linux gateway: %v", err)
	}
	if prepared.Mode != runtimeapi.RunModeGateway {
		t.Fatalf("prepared Linux gateway mode=%s", prepared.Mode)
	}
	responder, err := openTUNResponder(prepared.Device)
	if err != nil {
		t.Fatalf("attach low-privilege-like gateway TUN responder: %v", err)
	}
	responder.Run()
	status, err := manager.Commit(
		t.Context(),
		requestMeta(session, operationID, 2, time.Now().UTC()),
		prepared.OwnershipID,
	)
	if err != nil || status.State != runtimeapi.NetworkStateActive {
		responder.Close()
		t.Fatalf("commit Linux gateway status=%#v err=%v", status, err)
	}
	return responder
}

func failOpenGateway(t *testing.T, manager *Manager, reason string) {
	t.Helper()
	status, err := manager.FailOpen(t.Context(), reason)
	if err != nil || status.State != runtimeapi.NetworkStateInactive || len(status.Residuals) != 0 {
		t.Fatalf("release Linux gateway status=%#v err=%v", status, err)
	}
}

func testGatewayLeaseExpiry(
	t *testing.T,
	stateRoot string,
	system *LinuxSystem,
	request runtimeapi.NetworkPreviewRequest,
) {
	t.Helper()
	manager, err := OpenManager(stateRoot, namespaceRuntimeUID, system)
	if err != nil {
		t.Fatalf("open Linux gateway lease manager: %v", err)
	}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	manager.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	manager.LeaseTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runResult := make(chan error, 1)
	go func() { runResult <- manager.Run(ctx) }()
	responder := activateNamespaceGateway(t, manager, "op_gateway_lease", request)
	clock.Add(int64(2 * time.Second))
	waitForNamespaceDeviceState(t, system, linuxGatewayTUNDevice, false)
	responder.Close()
	cancel()
	if err := <-runResult; err != nil {
		t.Fatalf("stop Linux gateway lease manager: %v", err)
	}
}

func runGatewayPeer(t *testing.T, namespace, role string, uid uint32, target string) {
	t.Helper()
	arguments := []string{os.Args[0], "-test.run=^TestLinuxGatewayNamespacePeer$", "-test.v"}
	if namespace != "" {
		arguments = append([]string{"netns", "exec", namespace}, arguments...)
	}
	command := exec.Command("ip", arguments...)
	if namespace == "" {
		command = exec.Command(os.Args[0], "-test.run=^TestLinuxGatewayNamespacePeer$", "-test.v")
	}
	command.Env = append(
		os.Environ(),
		gatewayPeerRoleEnv+"="+role,
		"SUBMUX_GATEWAY_TARGET="+target,
	)
	if uid != 0 {
		command.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: uid},
		}
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run Linux gateway peer %s: %v\n%s", role, err, output)
	}
}

func assertGatewayCapturedTCP(t *testing.T, target, localAddress string) {
	t.Helper()
	dialer := net.Dialer{Timeout: 2 * time.Second}
	if localAddress != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(localAddress)}
	}
	connection, err := dialer.Dial("tcp", target)
	if err != nil {
		t.Fatalf("dial captured TCP %s: %v", target, err)
	}
	defer connection.Close()
	payload := []byte("submux-gateway-tcp")
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write captured TCP %s: %v", target, err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("read captured TCP %s: %v", target, err)
	}
	if string(reply) != string(payload) {
		t.Fatalf("captured TCP reply=%q", reply)
	}
}

func assertGatewayCapturedUDP(t *testing.T, target, localAddress string) {
	t.Helper()
	dialer := net.Dialer{Timeout: 2 * time.Second}
	if localAddress != "" {
		dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(localAddress)}
	}
	connection, err := dialer.Dial("udp", target)
	if err != nil {
		t.Fatalf("dial captured UDP %s: %v", target, err)
	}
	defer connection.Close()
	payload := []byte("submux-gateway-udp")
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write captured UDP %s: %v", target, err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatalf("read captured UDP %s: %v", target, err)
	}
	if string(reply) != string(payload) {
		t.Fatalf("captured UDP reply=%q", reply)
	}
}

func assertGatewayQUICShapedUDP(t *testing.T, target, localAddress string) {
	t.Helper()
	dialer := net.Dialer{Timeout: 2 * time.Second}
	if localAddress != "" {
		dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(localAddress)}
	}
	connection, err := dialer.Dial("udp", target)
	if err != nil {
		t.Fatalf("dial QUIC-shaped UDP %s: %v", target, err)
	}
	defer connection.Close()
	for sequence := byte(0); sequence < 3; sequence++ {
		payload := []byte{
			0xc3,
			0x00, 0x00, 0x00, 0x01,
			0x08, sequence, 1, 2, 3, 4, 5, 6,
			0x08, sequence, 8, 9, 10, 11, 12, 13,
			0x00,
		}
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := connection.Write(payload); err != nil {
			t.Fatalf("write QUIC-shaped UDP datagram: %v", err)
		}
		reply := make([]byte, len(payload))
		if _, err := io.ReadFull(connection, reply); err != nil {
			t.Fatalf("read QUIC-shaped UDP datagram: %v", err)
		}
		if string(reply) != string(payload) {
			t.Fatalf("QUIC-shaped UDP reply=%x", reply)
		}
	}
}

func assertGatewayConcurrentTraffic(t *testing.T) {
	t.Helper()
	const connectionCount = 16
	var wait sync.WaitGroup
	failures := make(chan error, connectionCount*2)
	for index := 0; index < connectionCount; index++ {
		for _, target := range []struct {
			network string
			address string
		}{
			{network: "tcp", address: "203.0.113.10:18080"},
			{network: "udp", address: "203.0.113.10:18081"},
		} {
			wait.Add(1)
			go func(sequence int, network, address string) {
				defer wait.Done()
				payload := []byte(fmt.Sprintf("submux-gateway-concurrent-%s-%d", network, sequence))
				if err := gatewayRoundTrip(network, address, payload); err != nil {
					failures <- err
				}
			}(index, target.network, target.address)
		}
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("concurrent Linux gateway traffic: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

func gatewayRoundTrip(network, target string, payload []byte) error {
	connection, err := net.DialTimeout(network, target, 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s %s: %w", network, target, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		return fmt.Errorf("write %s %s: %w", network, target, err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, reply); err != nil {
		return fmt.Errorf("read %s %s: %w", network, target, err)
	}
	if string(reply) != string(payload) {
		return fmt.Errorf("%s %s reply=%x", network, target, reply)
	}
	return nil
}

func assertGatewayDirectFailure(t *testing.T, network, target string) {
	t.Helper()
	connection, err := net.DialTimeout(network, target, 350*time.Millisecond)
	if err != nil {
		return
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(350 * time.Millisecond))
	payload := []byte("must-not-be-captured")
	if _, err := connection.Write(payload); err != nil {
		return
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, reply); err == nil {
		t.Fatalf("%s %s unexpectedly returned through the gateway TUN", network, target)
	}
}

type gatewayPeerServer struct {
	command *exec.Cmd
}

func startGatewayEchoServer(
	t *testing.T,
	namespace string,
	tcpAddresses string,
	udpAddresses string,
) *gatewayPeerServer {
	t.Helper()
	readyFile := filepath.Join(t.TempDir(), "ready")
	arguments := []string{
		"netns", "exec", namespace,
		os.Args[0],
		"-test.run=^TestLinuxGatewayNamespacePeer$",
		"-test.v",
	}
	command := exec.Command("ip", arguments...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	command.Env = append(
		os.Environ(),
		gatewayPeerRoleEnv+"=echo-server",
		"SUBMUX_GATEWAY_TCP_ADDRESSES="+tcpAddresses,
		"SUBMUX_GATEWAY_UDP_ADDRESSES="+udpAddresses,
		"SUBMUX_GATEWAY_READY_FILE="+readyFile,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start Linux gateway echo server: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			return &gatewayPeerServer{command: command}
		}
		if command.ProcessState != nil && command.ProcessState.Exited() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	t.Fatalf("Linux gateway echo server did not become ready: %s", output.String())
	return nil
}

func stopGatewayPeerServer(server *gatewayPeerServer) {
	if server == nil || server.command == nil || server.command.Process == nil {
		return
	}
	_ = server.command.Process.Kill()
	_ = server.command.Wait()
}

func runGatewayEchoServer(t *testing.T) {
	t.Helper()
	var closers []func()
	for _, address := range splitGatewayAddresses(os.Getenv("SUBMUX_GATEWAY_TCP_ADDRESSES")) {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("listen gateway TCP echo %s: %v", address, err)
		}
		closers = append(closers, func() { _ = listener.Close() })
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				go gatewayEchoTCP(connection)
			}
		}()
	}
	for _, address := range splitGatewayAddresses(os.Getenv("SUBMUX_GATEWAY_UDP_ADDRESSES")) {
		connection, err := net.ListenPacket("udp", address)
		if err != nil {
			t.Fatalf("listen gateway UDP echo %s: %v", address, err)
		}
		closers = append(closers, func() { _ = connection.Close() })
		go gatewayEchoUDP(connection)
	}
	defer func() {
		for _, closeServer := range closers {
			closeServer()
		}
	}()
	if err := os.WriteFile(os.Getenv("SUBMUX_GATEWAY_READY_FILE"), []byte("ready"), 0600); err != nil {
		t.Fatalf("write gateway server readiness: %v", err)
	}
	select {}
}

func splitGatewayAddresses(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func gatewayEchoTCP(connection net.Conn) {
	defer connection.Close()
	buffer := make([]byte, 4096)
	for {
		count, err := connection.Read(buffer)
		if count > 0 {
			_, _ = connection.Write(buffer[:count])
		}
		if err != nil {
			return
		}
	}
}

func gatewayEchoUDP(connection net.PacketConn) {
	buffer := make([]byte, 65535)
	for {
		count, address, err := connection.ReadFrom(buffer)
		if err != nil {
			return
		}
		_, _ = connection.WriteTo(buffer[:count], address)
	}
}

func startGatewayLocalTCPServer(t *testing.T, address string) func() {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen gateway WAN-local TCP: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go gatewayEchoTCP(connection)
		}
	}()
	return func() {
		_ = listener.Close()
		<-done
	}
}

func installGatewayUserDNAT(t *testing.T) {
	t.Helper()
	script := `add table ip user_nat
add chain ip user_nat prerouting { type nat hook prerouting priority dstnat; policy accept; }
add rule ip user_nat prerouting iifname "wan0" tcp dport 28080 dnat to 10.0.0.2:28080 comment "submux-user-tcp"
add rule ip user_nat prerouting iifname "wan0" udp dport 28081 dnat to 10.0.0.2:28081 comment "submux-user-udp"
`
	command := exec.Command("nft", "-f", "-")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install user DNAT rules: %v\n%s", err, output)
	}
	assertGatewayUserDNATPresent(t)
}

func assertGatewayUserDNATPresent(t *testing.T) {
	t.Helper()
	output, err := exec.Command("nft", "list", "table", "ip", "user_nat").CombinedOutput()
	if err != nil {
		t.Fatalf("list user DNAT table: %v\n%s", err, output)
	}
	body := string(output)
	for _, expected := range []string{"submux-user-tcp", "submux-user-udp", "dnat"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("user DNAT table lost %q:\n%s", expected, body)
		}
	}
}

func assertNoGatewayOwnedObjects(t *testing.T, system *LinuxSystem) {
	t.Helper()
	links, err := system.links(t.Context())
	if err != nil {
		t.Fatalf("list links after Linux gateway cleanup: %v", err)
	}
	for _, link := range links {
		if link.Name == linuxGatewayTUNDevice {
			t.Fatalf("Linux gateway TUN remained after cleanup: %#v", link)
		}
	}
	rules, err := system.policyRules(t.Context(), "-4")
	if err != nil {
		t.Fatalf("list policy rules after Linux gateway cleanup: %v", err)
	}
	for _, rule := range rules {
		if rule.Priority >= 18000 && rule.Priority < 24002 {
			t.Fatalf("Linux gateway policy rule remained after cleanup: %#v", rule)
		}
	}
	output, err := exec.Command("nft", "list", "tables").CombinedOutput()
	if err != nil {
		t.Fatalf("list nftables tables after gateway cleanup: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, " smx_") {
			t.Fatalf("Runtime-owned nftables table remained after gateway cleanup: %s", line)
		}
	}
}
