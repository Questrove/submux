package runtimenet

import (
	"strconv"
	"strings"
	"testing"

	"submux/internal/runtimeapi"
)

func TestLinuxGatewayDiscoveryFindsDoubleAndSingleArmLANRoutes(t *testing.T) {
	runner := &scriptedCommandRunner{responses: map[string]scriptedCommandResponse{
		"sysctl -n net.ipv4.ip_forward": {body: "1\n"},
		"ip -j -d link show": {body: `[
			{"ifname":"wan0","linkinfo":{"info_kind":"ether"}},
			{"ifname":"lan0","linkinfo":{"info_kind":"ether"}}
		]`},
		"ip -j -4 route show table all": {body: `[
			{"dst":"default","gateway":"198.51.100.1","dev":"wan0","table":"main","protocol":"dhcp","metric":100},
			{"dst":"198.51.100.0/24","dev":"wan0","table":"main","protocol":"kernel","scope":"link"},
			{"dst":"10.0.0.0/24","dev":"lan0","table":"main","protocol":"kernel","scope":"link"},
			{"dst":"10.1.0.0/24","dev":"wan0","table":"main","protocol":"kernel","scope":"link"}
		]`},
		"ip -j -6 route show table all": {body: `[
			{"dst":"2001:db8:10::/64","dev":"lan0","table":"main","protocol":"kernel"}
		]`},
	}}
	system := &LinuxSystem{RuntimeUID: 1001, Runner: runner}
	discovery, err := system.DiscoverGateway(t.Context(), runtimeapi.GatewaySettings{
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
		CaptureTCP: true,
		CaptureUDP: true,
	})
	if err != nil {
		t.Fatalf("discover Linux gateway: %v", err)
	}
	roles := make(map[string]string)
	for _, route := range discovery.Routes {
		roles[route.CIDR] = route.Role
	}
	if roles["198.51.100.0/24"] != runtimeapi.NetworkRouteRoleGatewayWAN ||
		roles["10.0.0.0/24"] != runtimeapi.NetworkRouteRoleGatewayLAN ||
		roles["10.1.0.0/24"] != runtimeapi.NetworkRouteRoleGatewayLAN {
		t.Fatalf("Linux gateway roles=%#v", roles)
	}
	if len(discovery.Conflicts) != 0 || len(discovery.Warnings) != 1 {
		t.Fatalf("Linux gateway discovery=%#v", discovery)
	}
}

func TestLinuxGatewayNFTRulesUseTypedExceptionsAndOwnedMarks(t *testing.T) {
	ownership := testLinuxGatewayOwnership(t)
	uid := uint32(2001)
	ownership.GatewaySettings = &runtimeapi.GatewaySettings{
		IPv6Policy:       runtimeapi.TUNIPv6Block,
		DNSPolicy:        runtimeapi.TUNDNSHijack,
		CaptureTCP:       true,
		CaptureUDP:       true,
		ProxyHostTraffic: true,
		DNSDirectCIDRs:   []string{"10.0.0.53/32"},
		UDPExceptions: []runtimeapi.GatewayTrafficException{{
			SourceCIDR:      "10.0.0.0/24",
			DestinationCIDR: "203.0.113.0/24",
			DestinationPorts: []runtimeapi.NetworkPortRange{{
				Start: 443,
				End:   443,
			}},
		}},
		HostExceptions: []runtimeapi.GatewayTrafficException{{
			UID: &uid,
		}},
	}
	ownership.IPv6Available = true
	ownership.Routes = []runtimeapi.NetworkRoute{{
		ID:        "route_lan",
		Family:    "ipv4",
		CIDR:      "10.0.0.0/24",
		Interface: "lan0",
		Role:      runtimeapi.NetworkRouteRoleGatewayLAN,
	}, {
		ID:        "route_wan",
		Family:    "ipv4",
		CIDR:      "198.51.100.0/24",
		Interface: "wan0",
		Role:      runtimeapi.NetworkRouteRoleGatewayWAN,
		Bypass:    true,
	}}
	rules, err := linuxGatewayNFTRules(ownership, 1001)
	if err != nil {
		t.Fatalf("generate Linux gateway nftables rules: %v", err)
	}
	for _, expected := range []string{
		"iifname @lan_ifaces ip saddr @lan_v4 jump capture_lan",
		"ct status dnat return",
		"ip daddr @dns_direct_v4 udp dport 53 return",
		"ip saddr 10.0.0.0/24 ip daddr 203.0.113.0/24 udp dport { 443 } return",
		"meta skuid { 0, 1001 } return",
		"meta skuid 2001 meta l4proto tcp return",
		"ct mark set " + strconv.Itoa(ownership.CaptureMark),
		"meta nfproto ipv6 drop",
		linuxOwnershipAlias(ownership.Token),
	} {
		if !strings.Contains(rules, expected) {
			t.Fatalf("Linux gateway nftables rules missing %q:\n%s", expected, rules)
		}
	}
}

func TestLinuxGatewayNFTRulesRejectInvalidTypedExceptions(t *testing.T) {
	for _, test := range []struct {
		name      string
		exception runtimeapi.GatewayTrafficException
	}{
		{
			name: "nft injection",
			exception: runtimeapi.GatewayTrafficException{
				DestinationCIDR: "203.0.113.0/24 counter accept",
			},
		},
		{
			name: "reversed port range",
			exception: runtimeapi.GatewayTrafficException{
				DestinationPorts: []runtimeapi.NetworkPortRange{{
					Start: 8443,
					End:   443,
				}},
			},
		},
		{
			name:      "empty exception",
			exception: runtimeapi.GatewayTrafficException{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ownership := testLinuxGatewayOwnership(t)
			ownership.GatewaySettings.UDPExceptions = []runtimeapi.GatewayTrafficException{
				test.exception,
			}
			if _, err := linuxGatewayNFTRules(ownership, 1001); err == nil {
				t.Fatal("Linux gateway accepted an invalid typed exception")
			}
		})
	}
}

func TestLinuxGatewayCommandsKeepMihomoAndCaptureMarksSeparate(t *testing.T) {
	ownership := testLinuxGatewayOwnership(t)
	commands := (&LinuxSystem{}).gatewayCommands(ownership)
	body := make([]string, 0, len(commands))
	for _, command := range commands {
		body = append(body, strings.Join(command.arguments, " "))
	}
	joined := strings.Join(body, "\n")
	if ownership.RoutingMark == ownership.CaptureMark ||
		!strings.Contains(joined, "fwmark "+strconv.Itoa(ownership.RoutingMark)+" lookup main") ||
		!strings.Contains(
			joined,
			"fwmark "+strconv.Itoa(ownership.CaptureMark)+" lookup "+strconv.Itoa(ownership.RouteTable),
		) ||
		!strings.Contains(joined, "proto "+linuxRouteProtocol) {
		t.Fatalf("Linux gateway commands:\n%s", joined)
	}
}

func testLinuxGatewayOwnership(t *testing.T) Ownership {
	t.Helper()
	token := strings.Repeat("b", 64)
	table, routingMark, captureMark, priority, err := linuxGatewayOwnershipNumbers(token)
	if err != nil {
		t.Fatalf("derive Linux gateway ownership numbers: %v", err)
	}
	return Ownership{
		ID:           "net_gateway_test",
		Token:        token,
		Mode:         runtimeapi.RunModeGateway,
		Device:       linuxGatewayTUNDevice,
		RoutingMark:  routingMark,
		CaptureMark:  captureMark,
		RouteTable:   table,
		RulePriority: priority,
		GatewaySettings: &runtimeapi.GatewaySettings{
			IPv6Policy: runtimeapi.TUNIPv6Direct,
			DNSPolicy:  runtimeapi.TUNDNSHijack,
			CaptureTCP: true,
			CaptureUDP: true,
		},
		State: runtimeapi.NetworkStateActive,
	}
}
