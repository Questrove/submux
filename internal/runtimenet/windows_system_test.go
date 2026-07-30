//go:build windows

package runtimenet

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"submux/internal/runtimeapi"
)

type fakeWindowsNetworkAPI struct {
	routes      []windowsRoute
	createError error
	deleteError error
	created     []windowsRoute
	deleted     []windowsRoute
}

func (api *fakeWindowsNetworkAPI) Routes() ([]windowsRoute, error) {
	return append([]windowsRoute(nil), api.routes...), nil
}

func (api *fakeWindowsNetworkAPI) CreateRoute(route windowsRoute) error {
	if api.createError != nil {
		return api.createError
	}
	api.created = append(api.created, route)
	api.routes = append(api.routes, route)
	return nil
}

func (api *fakeWindowsNetworkAPI) DeleteRoute(route windowsRoute) error {
	if api.deleteError != nil {
		return api.deleteError
	}
	for index, existing := range api.routes {
		if !windowsSameOwnedRoute(existing, route) {
			continue
		}
		api.deleted = append(api.deleted, route)
		api.routes = append(api.routes[:index], api.routes[index+1:]...)
		return nil
	}
	return errors.New("route not found")
}

type fakeWindowsCommandRunner struct {
	commands [][]string
	err      error
}

func (runner *fakeWindowsCommandRunner) Run(
	_ context.Context,
	_ []byte,
	name string,
	arguments ...string,
) ([]byte, error) {
	runner.commands = append(runner.commands, append([]string{name}, arguments...))
	return nil, runner.err
}

func TestWindowsDesiredRoutesCaptureDefaultsAndSelections(t *testing.T) {
	ownership := testWindowsOwnership()
	ownership.Routes = []runtimeapi.NetworkRoute{
		{CIDR: "10.0.0.0/8", Family: "ipv4", Bypass: false},
		{CIDR: "192.168.0.0/16", Family: "ipv4", Bypass: true},
		{CIDR: "2001:db8::/32", Family: "ipv6", Bypass: false},
	}
	ownership.Original["dns_servers"] = "192.168.1.53,2001:db8::53"
	routes, err := windowsDesiredRoutes(ownership)
	if err != nil {
		t.Fatalf("build Windows desired routes: %v", err)
	}
	got := make([]string, 0, len(routes))
	for _, route := range routes {
		got = append(got, route.Destination.String())
		if route.InterfaceIndex != 17 ||
			route.Protocol != windows.MIB_IPPROTO_NETMGMT ||
			route.Metric != uint32(ownership.RoutingMark) {
			t.Fatalf("Windows owned route=%#v", route)
		}
	}
	for _, expected := range []string{
		"0.0.0.0/1",
		"10.0.0.0/8",
		"128.0.0.0/1",
		"192.168.1.53/32",
		"2001:db8::/32",
		"2001:db8::53/128",
		"8000::/1",
		"::/1",
	} {
		if !slices.Contains(got, expected) {
			t.Fatalf("Windows routes=%v, missing %s", got, expected)
		}
	}
	if slices.Contains(got, "192.168.0.0/16") {
		t.Fatalf("Windows routes captured bypass route: %v", got)
	}
}

func TestWindowsDiscoverAndPrepareRequireFixedAdapterIdentity(t *testing.T) {
	api := &fakeWindowsNetworkAPI{routes: []windowsRoute{
		{
			InterfaceIndex: 4,
			Destination:    netip.MustParsePrefix("0.0.0.0/0"),
			NextHop:        netip.MustParseAddr("192.0.2.1"),
			Metric:         10,
			Protocol:       windows.MIB_IPPROTO_LOCAL,
		},
		{
			InterfaceIndex: 4,
			Destination:    netip.MustParsePrefix("10.0.0.0/8"),
			NextHop:        netip.MustParseAddr("192.0.2.1"),
			Metric:         10,
			Protocol:       windows.MIB_IPPROTO_LOCAL,
		},
	}}
	system := testWindowsSystem(api, &fakeWindowsCommandRunner{})
	preview, err := system.Discover(context.Background(), runtimeapi.TUNSettings{
		IPv6Policy: runtimeapi.TUNIPv6Proxy,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil {
		t.Fatalf("discover Windows TUN: %v", err)
	}
	if !preview.PreviewOnly ||
		preview.Device != windowsTUNDevice ||
		preview.Original["interface_index"] != "17" ||
		preview.Original["interface_guid"] != "{11111111-1111-1111-1111-111111111111}" ||
		preview.Original["dns_servers"] != "192.168.1.53,2001:db8::53" ||
		len(preview.Conflicts) != 0 ||
		len(preview.Routes) != 1 {
		t.Fatalf("Windows TUN discovery=%#v", preview)
	}

	ownership := testWindowsOwnership()
	prepared, err := system.PrepareTUN(context.Background(), ownership)
	if err != nil {
		t.Fatalf("prepare Windows TUN: %v", err)
	}
	if prepared.RoutingMark != ownership.RoutingMark ||
		len(prepared.Objects) != 1 ||
		prepared.Objects[0].Kind != "wintun_adapter" {
		t.Fatalf("Windows TUN preparation=%#v", prepared)
	}

	ownership.Original["interface_guid"] = "{22222222-2222-2222-2222-222222222222}"
	if _, err := system.PrepareTUN(context.Background(), ownership); err == nil ||
		!strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("prepare changed Windows adapter error=%v", err)
	}
}

func TestWindowsDiscoverReportsMissingAndForeignFullTunnel(t *testing.T) {
	api := &fakeWindowsNetworkAPI{routes: []windowsRoute{{
		InterfaceIndex: 9,
		Destination:    netip.MustParsePrefix("0.0.0.0/1"),
		NextHop:        netip.IPv4Unspecified(),
		Metric:         1,
		Protocol:       windows.MIB_IPPROTO_NETMGMT,
	}}}
	system := &WindowsSystem{
		API:    api,
		Runner: &fakeWindowsCommandRunner{},
		Interfaces: func() ([]windowsInterface, error) {
			return []windowsInterface{{Index: 9, Name: "OtherVPN", Type: 131}}, nil
		},
		DNSServers: func(uint32) ([]netip.Addr, error) {
			return nil, nil
		},
	}
	preview, err := system.Discover(context.Background(), runtimeapi.TUNSettings{
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil {
		t.Fatalf("discover conflicting Windows TUN: %v", err)
	}
	kinds := make([]string, 0, len(preview.Conflicts))
	for _, conflict := range preview.Conflicts {
		kinds = append(kinds, conflict.Kind)
	}
	if !slices.Contains(kinds, "tun_device_missing") ||
		!slices.Contains(kinds, "full_tunnel") {
		t.Fatalf("Windows TUN conflicts=%#v", preview.Conflicts)
	}
}

func TestWindowsApplyAndCleanupOwnOnlyExactRoutes(t *testing.T) {
	ownership := testWindowsOwnership()
	api := &fakeWindowsNetworkAPI{
		routes: []windowsRoute{{
			InterfaceIndex: 4,
			Destination:    netip.MustParsePrefix("192.168.1.0/24"),
			NextHop:        netip.MustParseAddr("192.168.1.1"),
			Metric:         5,
			Protocol:       windows.MIB_IPPROTO_LOCAL,
		}},
	}
	runner := &fakeWindowsCommandRunner{}
	system := testWindowsSystem(api, runner)
	objects, err := system.ApplyTUN(context.Background(), ownership)
	if err != nil {
		t.Fatalf("apply Windows TUN: %v", err)
	}
	if len(objects) != 4 {
		t.Fatalf("Windows TUN objects=%#v", objects)
	}
	if len(api.created) != 4 {
		t.Fatalf("created Windows routes=%#v", api.created)
	}
	ownership.Objects = append(ownership.Objects, objects...)
	residuals, err := system.Cleanup(context.Background(), ownership)
	if err != nil {
		t.Fatalf("cleanup Windows TUN: %v", err)
	}
	if len(residuals) != 0 || len(api.deleted) != len(api.created) {
		t.Fatalf("cleanup residuals=%#v deleted=%#v", residuals, api.deleted)
	}
	if len(api.routes) != 1 ||
		api.routes[0].Destination.String() != "192.168.1.0/24" ||
		api.routes[0].Protocol != windows.MIB_IPPROTO_LOCAL {
		t.Fatalf("cleanup changed user route=%#v", api.routes)
	}
}

func TestWindowsIPv6BlockUsesTokenOwnedFixedFirewallRule(t *testing.T) {
	ownership := testWindowsOwnership()
	ownership.Settings.IPv6Policy = runtimeapi.TUNIPv6Block
	api := &fakeWindowsNetworkAPI{}
	runner := &fakeWindowsCommandRunner{}
	system := testWindowsSystem(api, runner)
	objects, err := system.ApplyTUN(context.Background(), ownership)
	if err != nil {
		t.Fatalf("apply Windows IPv6 block: %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("Windows firewall commands=%#v", runner.commands)
	}
	add := strings.Join(runner.commands[0], " ")
	if !strings.Contains(add, "name=SubmuxRuntime-"+ownership.Token[:16]) ||
		!strings.Contains(add, "remoteip=::/0") ||
		!strings.Contains(add, "action=block") {
		t.Fatalf("Windows firewall add=%q", add)
	}
	ownership.Objects = append(ownership.Objects, objects...)
	if _, err := system.Cleanup(context.Background(), ownership); err != nil {
		t.Fatalf("cleanup Windows IPv6 block: %v", err)
	}
	if len(runner.commands) != 2 ||
		!strings.Contains(strings.Join(runner.commands[1], " "), "delete rule") {
		t.Fatalf("Windows firewall cleanup=%#v", runner.commands)
	}
}

func TestWindowsRouteRowRoundTrip(t *testing.T) {
	for _, destination := range []string{"198.51.100.0/24", "2001:db8::/32"} {
		prefix := netip.MustParsePrefix(destination)
		nextHop := netip.IPv4Unspecified()
		if prefix.Addr().Is6() {
			nextHop = netip.IPv6Unspecified()
		}
		expected := windowsRoute{
			InterfaceIndex: 17,
			Destination:    prefix,
			NextHop:        nextHop,
			Metric:         23456,
			Protocol:       windows.MIB_IPPROTO_NETMGMT,
		}
		row, err := windowsRouteRow(expected)
		if err != nil {
			t.Fatalf("encode Windows route %s: %v", destination, err)
		}
		actual, err := windowsRouteFromRow(&row)
		if err != nil {
			t.Fatalf("decode Windows route %s: %v", destination, err)
		}
		if !windowsSameOwnedRoute(actual, expected) {
			t.Fatalf("Windows route round trip got=%#v want=%#v", actual, expected)
		}
	}
}

func TestWindowsFullTunnelConflictDetection(t *testing.T) {
	if !windowsFullTunnelRoute(
		windowsRoute{Destination: netip.MustParsePrefix("0.0.0.0/1")},
		windowsInterface{},
	) {
		t.Fatal("Windows split default route was not treated as a conflict")
	}
	if !windowsFullTunnelRoute(
		windowsRoute{Destination: netip.MustParsePrefix("::/0")},
		windowsInterface{Type: 131},
	) {
		t.Fatal("Windows tunnel default route was not treated as a conflict")
	}
	if windowsFullTunnelRoute(
		windowsRoute{Destination: netip.MustParsePrefix("0.0.0.0/0")},
		windowsInterface{Type: 6},
	) {
		t.Fatal("Windows Ethernet default route was treated as a conflict")
	}
}

func testWindowsOwnership() Ownership {
	token := strings.Repeat("a", 64)
	metric, err := windowsOwnershipMetric(token)
	if err != nil {
		panic(err)
	}
	return Ownership{
		ID:            "net_windows_test",
		Token:         token,
		Mode:          runtimeapi.RunModeTUN,
		Device:        windowsTUNDevice,
		IPv6Available: true,
		RoutingMark:   int(metric),
		Settings: runtimeapi.TUNSettings{
			IPv6Policy: runtimeapi.TUNIPv6Proxy,
			DNSPolicy:  runtimeapi.TUNDNSHijack,
		},
		Original: map[string]string{
			"interface_index": "17",
			"interface_guid":  "{11111111-1111-1111-1111-111111111111}",
		},
	}
}

func testWindowsSystem(
	api windowsNetworkAPI,
	runner CommandRunner,
) *WindowsSystem {
	return &WindowsSystem{
		API:    api,
		Runner: runner,
		Interfaces: func() ([]windowsInterface, error) {
			return []windowsInterface{{
				Index: 17,
				Name:  windowsTUNDevice,
				GUID:  "{11111111-1111-1111-1111-111111111111}",
			}}, nil
		},
		DNSServers: func(uint32) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("192.168.1.53"),
				netip.MustParseAddr("2001:db8::53"),
			}, nil
		},
	}
}
