package runtimenet

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"submux/internal/runtimeapi"
)

type scriptedCommandRunner struct {
	mu        sync.Mutex
	responses map[string]scriptedCommandResponse
	calls     []string
	inputs    [][]byte
}

type scriptedCommandResponse struct {
	body string
	err  error
}

func (runner *scriptedCommandRunner) Run(
	_ context.Context,
	input []byte,
	name string,
	arguments ...string,
) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	key := strings.Join(append([]string{name}, arguments...), " ")
	runner.calls = append(runner.calls, key)
	runner.inputs = append(runner.inputs, append([]byte(nil), input...))
	if response, ok := runner.responses[key]; ok {
		return []byte(response.body), response.err
	}
	if strings.Contains(key, " -j ") {
		return []byte("[]"), nil
	}
	if strings.HasPrefix(key, "nft list ") {
		return nil, errors.New("not found")
	}
	return nil, nil
}

func TestLinuxSystemDiscoveryPreservesSpecificRoutesAndFindsFullTunnelConflicts(t *testing.T) {
	runner := &scriptedCommandRunner{responses: map[string]scriptedCommandResponse{
		"ip -j -d link show": {body: `[
			{"ifname":"eth0","linkinfo":{"info_kind":"ether"}},
			{"ifname":"wg0","linkinfo":{"info_kind":"wireguard"}},
			{"ifname":"smxtun0","ifalias":"someone-else","linkinfo":{"info_kind":"tun"}}
		]`},
		"ip -j -4 route show table all": {body: `[
			{"dst":"default","dev":"eth0","table":"main","protocol":"dhcp"},
			{"dst":"192.168.50.0/24","dev":"eth0","table":"main","protocol":"kernel"},
			{"dst":"172.18.0.0/16","dev":"docker0","table":"main","protocol":"kernel"},
			{"dst":"default","dev":"wg0","table":51820,"protocol":"static"}
		]`},
		"ip -j -6 route show table all": {body: `[
			{"dst":"fe80::/64","dev":"eth0","table":"main","protocol":"kernel"}
		]`},
	}}
	system := &LinuxSystem{RuntimeUID: 1001, Runner: runner}
	discovery, err := system.Discover(t.Context(), runtimeapi.TUNSettings{
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSHijack,
	})
	if err != nil {
		t.Fatalf("discover Linux ordinary TUN network: %v", err)
	}
	if !discovery.IPv6Available || len(discovery.Routes) != 3 {
		t.Fatalf("Linux discovery=%#v", discovery)
	}
	if len(discovery.Conflicts) != 2 {
		t.Fatalf("Linux conflicts=%#v", discovery.Conflicts)
	}
	if len(discovery.Warnings) != 1 || !strings.Contains(discovery.Warnings[0], "IPv6") {
		t.Fatalf("Linux warnings=%#v", discovery.Warnings)
	}
}

func TestLinuxSystemGeneratedRulesBypassOnlySelectedSpecificRoutes(t *testing.T) {
	ownership := testLinuxOwnership(t)
	ownership.IPv6Available = true
	ownership.Settings.IPv6Policy = runtimeapi.TUNIPv6Direct
	ownership.Routes = []runtimeapi.NetworkRoute{{
		ID:     "route_lan",
		Family: "ipv4",
		CIDR:   "192.168.1.0/24",
		Bypass: true,
	}, {
		ID:     "route_captured_vpn",
		Family: "ipv4",
		CIDR:   "10.0.0.0/8",
		Bypass: false,
	}}
	system := &LinuxSystem{}
	commands := system.applyCommands(ownership)
	joined := make([]string, 0, len(commands))
	for _, command := range commands {
		joined = append(joined, strings.Join(append([]string{command.name}, command.arguments...), " "))
	}
	body := strings.Join(joined, "\n")
	if !strings.Contains(body, "to 192.168.1.0/24 lookup main") {
		t.Fatalf("selected bypass route missing from commands:\n%s", body)
	}
	if strings.Contains(body, "to 10.0.0.0/8 lookup main") {
		t.Fatalf("captured VPN route was bypassed:\n%s", body)
	}
	if strings.Contains(body, "ip -6 route add") || strings.Contains(body, "ip -6 rule add") {
		t.Fatalf("IPv6 direct policy generated IPv6 capture:\n%s", body)
	}
	if !strings.Contains(body, "not fwmark") || !strings.Contains(body, "0.0.0.0/0 dev smxtun0") {
		t.Fatalf("IPv4 capture or Mihomo bypass mark missing:\n%s", body)
	}
}

func TestLinuxSystemMaximumBypassRoutesUseDistinctPolicyPriorities(t *testing.T) {
	ownership := testLinuxOwnership(t)
	ownership.IPv6Available = false
	ownership.Settings.IPv6Policy = runtimeapi.TUNIPv6Direct
	for index := 0; index < linuxMaxBypassRoutes; index++ {
		ownership.Routes = append(ownership.Routes, runtimeapi.NetworkRoute{
			ID:     "route_" + strconv.Itoa(index),
			Family: "ipv4",
			CIDR:   "10." + strconv.Itoa(index/256) + "." + strconv.Itoa(index%256) + ".0/24",
			Bypass: true,
		})
	}
	priorities := make(map[string]struct{})
	for _, command := range (&LinuxSystem{}).applyCommands(ownership) {
		if command.name != "ip" || len(command.arguments) < 5 ||
			command.arguments[1] != "rule" || command.arguments[2] != "add" {
			continue
		}
		priority := command.arguments[4]
		if _, duplicate := priorities[priority]; duplicate {
			t.Fatalf("maximum bypass route set reused policy priority %s", priority)
		}
		priorities[priority] = struct{}{}
	}
}

func TestLinuxSystemOrdersSpecificBypassRulesAndUsesOriginalRouteTable(t *testing.T) {
	ownership := testLinuxOwnership(t)
	ownership.IPv6Available = false
	ownership.Routes = []runtimeapi.NetworkRoute{{
		ID:     "route_broad",
		Family: "ipv4",
		CIDR:   "10.0.0.0/8",
		Table:  "main",
		Bypass: true,
	}, {
		ID:     "route_specific",
		Family: "ipv4",
		CIDR:   "10.1.0.0/16",
		Table:  "10001",
		Bypass: true,
	}}
	commands := (&LinuxSystem{}).applyCommands(ownership)
	var broad, specific linuxCommand
	for _, command := range commands {
		switch command.object.ID {
		case "bypass:route_broad":
			broad = command
		case "bypass:route_specific":
			specific = command
		}
	}
	if policyRulePriority(specific) >= policyRulePriority(broad) {
		t.Fatalf(
			"specific priority=%d broad priority=%d",
			policyRulePriority(specific),
			policyRulePriority(broad),
		)
	}
	if joined := strings.Join(specific.arguments, " "); !strings.Contains(joined, "lookup 10001") {
		t.Fatalf("specific route did not retain its source table: %s", joined)
	}
}

func TestLinuxSystemExactCleanupCommandsRetainOwnershipSelectors(t *testing.T) {
	ownership := testLinuxOwnership(t)
	ownership.IPv6Available = false
	ownership.Routes = []runtimeapi.NetworkRoute{{
		ID:     "route_lan",
		Family: "ipv4",
		CIDR:   "192.168.1.0/24",
		Table:  "main",
		Bypass: true,
	}}
	for _, command := range (&LinuxSystem{}).applyCommands(ownership) {
		deletion, ok := exactLinuxDeletion(command)
		if !ok {
			t.Fatalf("command has no exact cleanup form: %#v", command)
		}
		joined := strings.Join(deletion.arguments, " ")
		if command.object.Kind == "policy_rule" &&
			(!strings.Contains(joined, " rule delete priority ") ||
				(!strings.Contains(joined, " lookup ") && !strings.Contains(joined, " fwmark "))) {
			t.Fatalf("policy rule cleanup lost selectors: %s", joined)
		}
		if command.object.Kind == "route" &&
			(!strings.Contains(joined, " route delete table ") ||
				!strings.Contains(joined, " proto "+linuxRouteProtocol)) {
			t.Fatalf("route cleanup lost ownership protocol: %s", joined)
		}
	}
}

func TestLinuxSystemExactPolicyRuleMatchingRejectsPriorityTakeover(t *testing.T) {
	command := linuxRuleCommand(
		"-4",
		12000,
		[]string{"to", "10.1.0.0/16", "lookup", "10001"},
		"bypass:test",
		"10.1.0.0/16",
	)
	owned := linuxRule{
		Priority:          12000,
		Destination:       "10.1.0.0",
		DestinationLength: 16,
		Table:             json.RawMessage(`"10001"`),
	}
	if !hasExactPolicyRule([]linuxRule{owned}, command) {
		t.Fatal("exact policy rule was not recognized")
	}
	replacement := owned
	replacement.Table = json.RawMessage(`"main"`)
	if hasExactPolicyRule([]linuxRule{replacement}, command) {
		t.Fatal("same-priority replacement rule was accepted as owned")
	}
}

func TestLinuxSystemRejectsOccupiedDerivedRouteTable(t *testing.T) {
	ownership := testLinuxOwnership(t)
	runner := &scriptedCommandRunner{responses: map[string]scriptedCommandResponse{
		"ip -j -d link show": {
			body: `[{"ifname":"smxtun0","ifalias":"` + linuxOwnershipAlias(ownership.Token) + `"}]`,
		},
		"ip -j -4 route show table all": {
			body: `[{"dst":"10.0.0.0/8","dev":"other0","table":` + strconv.Itoa(ownership.RouteTable) + `}]`,
		},
	}}
	system := &LinuxSystem{RuntimeUID: 1001, Runner: runner}
	if _, err := system.ApplyTUN(t.Context(), ownership); err == nil ||
		!strings.Contains(err.Error(), "already owned") {
		t.Fatalf("occupied Linux route table error=%v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " route add ") || strings.Contains(call, " rule add ") {
			t.Fatalf("occupied route table was mutated: %s", call)
		}
	}
}

func TestLinuxSystemCleanupRefusesWrongDeviceOwner(t *testing.T) {
	ownership := testLinuxOwnership(t)
	runner := &scriptedCommandRunner{responses: map[string]scriptedCommandResponse{
		"ip -j -d link show": {body: `[{"ifname":"smxtun0","ifalias":"submux:not-ours"}]`},
	}}
	system := &LinuxSystem{RuntimeUID: 1001, Runner: runner}
	residuals, err := system.Cleanup(t.Context(), ownership)
	if err == nil || len(residuals) != 1 {
		t.Fatalf("wrong-owner cleanup residuals=%#v err=%v", residuals, err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " delete ") {
			t.Fatalf("wrong-owner cleanup issued a delete: %s", call)
		}
	}
}

func testLinuxOwnership(t *testing.T) Ownership {
	t.Helper()
	token := strings.Repeat("a", 64)
	table, priority, err := linuxOwnershipNumbers(token)
	if err != nil {
		t.Fatalf("derive Linux ownership numbers: %v", err)
	}
	return Ownership{
		ID:           "net_test",
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
		State: runtimeapi.NetworkStateActive,
	}
}
