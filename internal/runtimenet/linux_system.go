package runtimenet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"submux/internal/runtimeapi"
)

const (
	linuxTUNDevice       = "smxtun0"
	linuxTUNIPv4         = "198.18.0.1/30"
	linuxTUNIPv6         = "fdfe:dcba:9876::1/126"
	linuxMaxBypassRoutes = 512
	linuxRouteProtocol   = "242"
	maxCommandOutput     = 4 << 20
)

type CommandRunner interface {
	Run(context.Context, []byte, string, ...string) ([]byte, error)
}

type LinuxSystem struct {
	RuntimeUID uint32
	Runner     CommandRunner
}

type execCommandRunner struct{}

type linuxRoute struct {
	Destination string          `json:"dst"`
	Gateway     string          `json:"gateway"`
	Device      string          `json:"dev"`
	Protocol    string          `json:"protocol"`
	Scope       string          `json:"scope"`
	Source      string          `json:"prefsrc"`
	Table       json.RawMessage `json:"table"`
	Type        string          `json:"type"`
	Metric      int             `json:"metric"`
}

type linuxLink struct {
	Name  string `json:"ifname"`
	Alias string `json:"ifalias"`
	Info  struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
}

type linuxRule struct {
	Priority          int             `json:"priority"`
	Destination       string          `json:"dst"`
	DestinationLength int             `json:"dstlen"`
	FWMark            string          `json:"fwmark"`
	Not               json.RawMessage `json:"not"`
	Table             json.RawMessage `json:"table"`
}

func NewLinuxSystem(runtimeUID uint32) (*LinuxSystem, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("ordinary TUN networking is only available on Linux")
	}
	if runtimeUID == 0 {
		return nil, errors.New("ordinary TUN requires a non-root Runtime service account")
	}
	return &LinuxSystem{RuntimeUID: runtimeUID, Runner: execCommandRunner{}}, nil
}

func (execCommandRunner) Run(
	ctx context.Context,
	input []byte,
	name string,
	arguments ...string,
) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	if len(input) > 0 {
		command.Stdin = bytes.NewReader(input)
	}
	var output bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, remaining: maxCommandOutput}
	command.Stderr = &limitedWriter{writer: &output, remaining: maxCommandOutput}
	err := command.Run()
	if err != nil {
		detail := strings.TrimSpace(output.String())
		if detail == "" {
			return output.Bytes(), fmt.Errorf("%s %s failed: %w", name, strings.Join(arguments, " "), err)
		}
		return output.Bytes(), fmt.Errorf(
			"%s %s failed: %w: %s",
			name,
			strings.Join(arguments, " "),
			err,
			detail,
		)
	}
	return output.Bytes(), nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *limitedWriter) Write(body []byte) (int, error) {
	if len(body) > writer.remaining {
		return 0, errors.New("privileged Runtime network command output exceeded the size limit")
	}
	count, err := writer.writer.Write(body)
	writer.remaining -= count
	return count, err
}

func (system *LinuxSystem) Discover(
	ctx context.Context,
	settings runtimeapi.TUNSettings,
) (Discovery, error) {
	if err := system.validate(); err != nil {
		return Discovery{}, err
	}
	links, err := system.links(ctx)
	if err != nil {
		return Discovery{}, err
	}
	ipv4, err := system.routes(ctx, "ipv4")
	if err != nil {
		return Discovery{}, err
	}
	ipv6, err := system.routes(ctx, "ipv6")
	if err != nil {
		return Discovery{}, err
	}
	linksByName := make(map[string]linuxLink, len(links))
	for _, link := range links {
		linksByName[link.Name] = link
	}
	routes := make([]runtimeapi.NetworkRoute, 0, len(ipv4)+len(ipv6))
	conflicts := make([]runtimeapi.NetworkConflict, 0)
	if existing, ok := linksByName[linuxTUNDevice]; ok {
		conflicts = append(conflicts, runtimeapi.NetworkConflict{
			Kind:   "tun_name",
			Owner:  existing.Alias,
			Detail: "TUN device smxtun0 already exists without matching active ownership",
		})
	}
	ipv6Available := false
	for _, familyRoutes := range [][]linuxRoute{ipv4, ipv6} {
		for _, route := range familyRoutes {
			family := "ipv4"
			if strings.Contains(route.Destination, ":") ||
				strings.Contains(route.Gateway, ":") ||
				route.Destination == "::/0" {
				family = "ipv6"
				ipv6Available = true
			}
			table := normalizeLinuxTable(route.Table)
			cidr := normalizeRouteDestination(route.Destination, family)
			if cidr == "" {
				continue
			}
			if cidr == "0.0.0.0/0" || cidr == "::/0" {
				if table != "main" || tunnelLikeLink(route.Device, linksByName[route.Device].Info.Kind) {
					conflicts = append(conflicts, runtimeapi.NetworkConflict{
						Kind:   "full_tunnel",
						Owner:  route.Device,
						Detail: fmt.Sprintf("default route in table %s is owned by %s", table, route.Device),
					})
				}
				continue
			}
			routes = append(routes, runtimeapi.NetworkRoute{
				ID:        linuxRouteID(family, cidr, route.Device, table, route.Protocol),
				Family:    family,
				CIDR:      cidr,
				Interface: route.Device,
				Table:     table,
				Source:    route.Protocol,
			})
		}
	}
	routes = uniqueNetworkRoutes(routes)
	conflicts = uniqueNetworkConflicts(conflicts)
	warnings := []string(nil)
	if ipv6Available && settings.IPv6Policy == runtimeapi.TUNIPv6Direct {
		warnings = append(warnings, "IPv6 将在普通 TUN 运行期间直连，IPv6 流量不会经过代理。")
	}
	return Discovery{
		Device:        linuxTUNDevice,
		IPv6Available: ipv6Available,
		Routes:        routes,
		Conflicts:     conflicts,
		Warnings:      warnings,
	}, nil
}

func (system *LinuxSystem) PrepareTUN(
	ctx context.Context,
	ownership Ownership,
) (SystemPreparation, error) {
	if err := system.validateOwnership(ownership); err != nil {
		return SystemPreparation{}, err
	}
	routeTable, priority, err := linuxOwnershipNumbers(ownership.Token)
	if err != nil {
		return SystemPreparation{}, err
	}
	existing, exists, err := system.link(ctx, ownership.Device)
	if err != nil {
		return SystemPreparation{}, err
	}
	expectedAlias := linuxOwnershipAlias(ownership.Token)
	if exists {
		if existing.Alias != expectedAlias {
			return SystemPreparation{}, errors.New("TUN device name is owned by another process")
		}
		return SystemPreparation{
			Objects:      linuxPreparedObjects(ownership),
			RoutingMark:  routeTable,
			RouteTable:   routeTable,
			RulePriority: priority,
		}, nil
	}
	if _, err := system.run(ctx, nil,
		"ip", "tuntap", "add", "dev", ownership.Device, "mode", "tun",
		"user", strconv.FormatUint(uint64(system.RuntimeUID), 10),
	); err != nil {
		return SystemPreparation{}, err
	}
	created := true
	rollback := func() {
		if created {
			_, _ = system.run(context.Background(), nil, "ip", "link", "delete", "dev", ownership.Device)
		}
	}
	if _, err := system.run(ctx, nil,
		"ip", "link", "set", "dev", ownership.Device, "alias", expectedAlias,
	); err != nil {
		rollback()
		return SystemPreparation{}, err
	}
	if _, err := system.run(ctx, nil,
		"ip", "addr", "add", linuxTUNIPv4, "dev", ownership.Device,
	); err != nil {
		rollback()
		return SystemPreparation{}, err
	}
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Proxy {
		if _, err := system.run(ctx, nil,
			"ip", "-6", "addr", "add", linuxTUNIPv6, "dev", ownership.Device,
		); err != nil {
			rollback()
			return SystemPreparation{}, err
		}
	}
	if _, err := system.run(ctx, nil, "ip", "link", "set", "dev", ownership.Device, "up"); err != nil {
		rollback()
		return SystemPreparation{}, err
	}
	created = false
	return SystemPreparation{
		Objects:      linuxPreparedObjects(ownership),
		RoutingMark:  routeTable,
		RouteTable:   routeTable,
		RulePriority: priority,
	}, nil
}

func (system *LinuxSystem) ApplyTUN(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := system.validatePreparedOwnership(ownership); err != nil {
		return nil, err
	}
	if len(ownership.Routes) > linuxMaxBypassRoutes {
		return nil, errors.New("ordinary TUN discovered too many specific routes")
	}
	link, exists, err := system.link(ctx, ownership.Device)
	if err != nil {
		return nil, err
	}
	if !exists || link.Alias != linuxOwnershipAlias(ownership.Token) {
		return nil, errors.New("ordinary TUN ownership no longer matches the system device")
	}
	if err := system.ensureRuleRangeFree(ctx, ownership); err != nil {
		return nil, err
	}
	if err := system.ensureRouteTableFree(ctx, ownership); err != nil {
		return nil, err
	}
	commands := system.applyCommands(ownership)
	applied := make([]runtimeapi.NetworkObject, 0, len(commands)+1)
	for _, command := range commands {
		if _, err := system.run(ctx, nil, command.name, command.arguments...); err != nil {
			_, _ = system.Cleanup(context.Background(), ownership)
			return applied, err
		}
		applied = append(applied, command.object)
	}
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Block {
		if _, err := system.run(ctx, []byte(linuxIPv6BlockRules(ownership.Token)), "nft", "-f", "-"); err != nil {
			_, _ = system.run(context.Background(), nil,
				"nft", "delete", "table", "inet", linuxNFTTable(ownership.Token),
			)
			_, _ = system.Cleanup(context.Background(), ownership)
			return applied, err
		}
		applied = append(applied, runtimeapi.NetworkObject{
			Kind:  "nft_table",
			ID:    "nft:" + linuxNFTTable(ownership.Token),
			Name:  linuxNFTTable(ownership.Token),
			State: runtimeapi.NetworkStateActive,
		})
	}
	return mergeNetworkObjects(applied), nil
}

func (system *LinuxSystem) Cleanup(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if ownership.Mode == runtimeapi.RunModeGateway {
		return system.cleanupGateway(ctx, ownership)
	}
	if err := system.validatePreparedOwnership(ownership); err != nil {
		return nil, err
	}
	link, exists, linkErr := system.link(ctx, ownership.Device)
	if linkErr == nil && exists && link.Alias != linuxOwnershipAlias(ownership.Token) {
		return []runtimeapi.NetworkObject{{
			Kind:  "tun",
			ID:    "tun:" + ownership.Device,
			Name:  ownership.Device,
			State: runtimeapi.NetworkStateUnknown,
		}}, errors.New("ordinary TUN device ownership changed; cleanup refused")
	}
	if hasNetworkObject(ownership.Objects, "nft:"+linuxNFTTable(ownership.Token)) {
		if err := system.deleteOwnedNFT(ctx, ownership.Token); err != nil {
			return []runtimeapi.NetworkObject{{
				Kind:  "nft_table",
				ID:    "nft:" + linuxNFTTable(ownership.Token),
				Name:  linuxNFTTable(ownership.Token),
				State: runtimeapi.NetworkStateUnknown,
			}}, err
		}
	}
	commands := system.applyCommands(ownership)
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		if deletion, ok := exactLinuxDeletion(command); ok {
			_, _ = system.run(ctx, nil, deletion.name, deletion.arguments...)
		}
	}
	if exists {
		_, _ = system.run(ctx, nil, "ip", "link", "delete", "dev", ownership.Device)
	}
	return system.residuals(ctx, ownership)
}

func (system *LinuxSystem) Observe(
	ctx context.Context,
	ownership *Ownership,
) (runtimeapi.NetworkStatus, error) {
	if err := system.validate(); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if ownership == nil {
		links, err := system.links(ctx)
		if err != nil {
			return runtimeapi.NetworkStatus{}, err
		}
		status := runtimeapi.NetworkStatus{
			Mode:  runtimeapi.RunModeExplicit,
			State: runtimeapi.NetworkStateInactive,
		}
		for _, link := range links {
			if link.Name != linuxTUNDevice && link.Name != linuxGatewayTUNDevice {
				continue
			}
			status.State = runtimeapi.NetworkStateConflict
			status.Conflicts = append(status.Conflicts, runtimeapi.NetworkConflict{
				Kind:   "tun_name",
				Owner:  link.Alias,
				Detail: fmt.Sprintf("TUN device %s exists without active Runtime ownership", link.Name),
			})
		}
		return status, nil
	}
	if ownership.Mode == runtimeapi.RunModeGateway {
		return system.observeGateway(ctx, *ownership)
	}
	if err := system.validatePreparedOwnership(*ownership); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	status := runtimeapi.NetworkStatus{
		Mode:     runtimeapi.RunModeTUN,
		State:    ownership.State,
		Device:   ownership.Device,
		Settings: ownership.Settings,
		Objects:  append([]runtimeapi.NetworkObject(nil), ownership.Objects...),
		Routes:   append([]runtimeapi.NetworkRoute(nil), ownership.Routes...),
	}
	missing, err := system.missingObjects(ctx, *ownership)
	if err != nil || len(missing) > 0 {
		status.State = runtimeapi.NetworkStateUnknown
		status.Residuals = missing
	}
	return status, err
}

type linuxCommand struct {
	name      string
	arguments []string
	object    runtimeapi.NetworkObject
}

func (system *LinuxSystem) applyCommands(ownership Ownership) []linuxCommand {
	commands := make([]linuxCommand, 0, len(ownership.Routes)*2+8)
	for _, family := range []string{"ipv4", "ipv6"} {
		if family == "ipv6" &&
			(!ownership.IPv6Available || ownership.Settings.IPv6Policy != runtimeapi.TUNIPv6Proxy) {
			continue
		}
		flag := "-4"
		defaultRoute := "0.0.0.0/0"
		if family == "ipv6" {
			flag = "-6"
			defaultRoute = "::/0"
		}
		offset := 0
		for _, route := range orderedBypassRoutes(ownership.Routes, family) {
			priority := ownership.RulePriority + offset
			table := route.Table
			if table == "" {
				table = "main"
			}
			commands = append(commands, linuxRuleCommand(
				flag,
				priority,
				[]string{"to", route.CIDR, "lookup", table},
				"bypass:"+route.ID,
				route.CIDR,
			))
			offset++
		}
		for _, cidr := range fixedBypassCIDRs(family) {
			priority := ownership.RulePriority + offset
			commands = append(commands, linuxRuleCommand(
				flag,
				priority,
				[]string{"to", cidr, "lookup", "main"},
				"bypass:"+family+":"+cidr,
				cidr,
			))
			offset++
		}
		markPriority := ownership.RulePriority + offset
		commands = append(commands, linuxRuleCommand(
			flag,
			markPriority,
			[]string{"fwmark", strconv.Itoa(ownership.RoutingMark), "lookup", "main"},
			"mark:"+family,
			strconv.Itoa(ownership.RoutingMark),
		))
		commands = append(commands, linuxCommand{
			name: "ip",
			arguments: []string{
				flag, "route", "add", "table", strconv.Itoa(ownership.RouteTable),
				defaultRoute, "dev", ownership.Device, "proto", linuxRouteProtocol,
			},
			object: runtimeapi.NetworkObject{
				Kind:  "route",
				ID:    "route:" + family + ":default",
				Name:  defaultRoute,
				State: runtimeapi.NetworkStateActive,
			},
		})
		commands = append(commands, linuxRuleCommand(
			flag,
			markPriority+1,
			[]string{
				"not", "fwmark", strconv.Itoa(ownership.RoutingMark),
				"lookup", strconv.Itoa(ownership.RouteTable),
			},
			"capture:"+family,
			defaultRoute,
		))
	}
	return commands
}

func linuxRuleCommand(
	family string,
	priority int,
	rule []string,
	id string,
	name string,
) linuxCommand {
	arguments := []string{family, "rule", "add", "priority", strconv.Itoa(priority)}
	arguments = append(arguments, rule...)
	return linuxCommand{
		name:      "ip",
		arguments: arguments,
		object: runtimeapi.NetworkObject{
			Kind:  "policy_rule",
			ID:    id,
			Name:  name,
			State: runtimeapi.NetworkStateActive,
		},
	}
}

func exactLinuxDeletion(command linuxCommand) (linuxCommand, bool) {
	if command.name != "ip" || len(command.arguments) < 3 {
		return linuxCommand{}, false
	}
	arguments := append([]string(nil), command.arguments...)
	switch {
	case arguments[1] == "rule" && arguments[2] == "add":
		arguments[2] = "delete"
	case arguments[1] == "route" && arguments[2] == "add":
		arguments[2] = "delete"
	default:
		return linuxCommand{}, false
	}
	return linuxCommand{name: command.name, arguments: arguments}, true
}

func policyRulePriority(command linuxCommand) int {
	if command.name != "ip" ||
		len(command.arguments) < 5 ||
		command.arguments[1] != "rule" ||
		command.arguments[2] != "add" ||
		command.arguments[3] != "priority" {
		return -1
	}
	priority, err := strconv.Atoi(command.arguments[4])
	if err != nil {
		return -1
	}
	return priority
}

func hasPolicyRuleAtPriority(rules []linuxRule, priority int) bool {
	for _, rule := range rules {
		if rule.Priority == priority {
			return true
		}
	}
	return false
}

func hasExactPolicyRule(rules []linuxRule, command linuxCommand) bool {
	priority := policyRulePriority(command)
	if priority < 0 {
		return false
	}
	expectedDestination := ""
	expectedMark := ""
	expectedTable := ""
	expectedNot := false
	for index := 5; index < len(command.arguments); {
		switch command.arguments[index] {
		case "not":
			expectedNot = true
			index++
		case "to":
			if index+1 >= len(command.arguments) {
				return false
			}
			expectedDestination = command.arguments[index+1]
			index += 2
		case "fwmark":
			if index+1 >= len(command.arguments) {
				return false
			}
			expectedMark = normalizeLinuxMark(command.arguments[index+1])
			index += 2
		case "lookup":
			if index+1 >= len(command.arguments) {
				return false
			}
			expectedTable = command.arguments[index+1]
			index += 2
		default:
			return false
		}
	}
	for _, rule := range rules {
		if rule.Priority != priority ||
			normalizeLinuxTable(rule.Table) != expectedTable ||
			(len(rule.Not) > 0) != expectedNot ||
			normalizeLinuxMark(rule.FWMark) != expectedMark ||
			normalizeLinuxRuleDestination(rule) != expectedDestination {
			continue
		}
		return true
	}
	return false
}

func normalizeLinuxMark(value string) string {
	if value == "" {
		return ""
	}
	mark, err := strconv.ParseUint(value, 0, 32)
	if err != nil {
		return value
	}
	return strconv.FormatUint(mark, 10)
}

func normalizeLinuxRuleDestination(rule linuxRule) string {
	if rule.Destination == "" {
		return ""
	}
	address, err := netip.ParseAddr(rule.Destination)
	if err != nil {
		return rule.Destination
	}
	bits := rule.DestinationLength
	if bits == 0 && !address.IsUnspecified() {
		bits = address.BitLen()
	}
	return netip.PrefixFrom(address, bits).Masked().String()
}

func (system *LinuxSystem) ensureRuleRangeFree(ctx context.Context, ownership Ownership) error {
	for family, expected := range expectedPolicyRules(system.applyCommands(ownership)) {
		existing, err := system.rulePriorities(ctx, family)
		if err != nil {
			return err
		}
		for priority := range expected {
			if _, exists := existing[priority]; exists {
				return fmt.Errorf("Linux policy rule priority %d is already owned", priority)
			}
		}
	}
	return nil
}

func (system *LinuxSystem) ensureRouteTableFree(ctx context.Context, ownership Ownership) error {
	for _, family := range []string{"-4", "-6"} {
		routes, err := system.routesInTable(ctx, family, ownership.RouteTable)
		if err != nil {
			if family == "-6" && !ownership.IPv6Available {
				continue
			}
			return err
		}
		if len(routes) > 0 {
			return fmt.Errorf("Linux route table %d is already owned", ownership.RouteTable)
		}
	}
	return nil
}

func (system *LinuxSystem) missingObjects(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	missing := make([]runtimeapi.NetworkObject, 0)
	link, exists, err := system.link(ctx, ownership.Device)
	if err != nil {
		return nil, err
	}
	if !exists || link.Alias != linuxOwnershipAlias(ownership.Token) {
		missing = append(missing, runtimeapi.NetworkObject{
			Kind:  "tun",
			ID:    "tun:" + ownership.Device,
			Name:  ownership.Device,
			State: runtimeapi.NetworkStateUnknown,
		})
	}
	if ownership.State != runtimeapi.NetworkStateActive {
		return missing, nil
	}
	for family, expected := range expectedPolicyRuleCommands(system.applyCommands(ownership)) {
		existing, ruleErr := system.policyRules(ctx, family)
		if ruleErr != nil {
			return nil, ruleErr
		}
		for _, command := range expected {
			if hasExactPolicyRule(existing, command) {
				continue
			}
			object := command.object
			object.State = runtimeapi.NetworkStateUnknown
			missing = append(missing, object)
		}
	}
	for _, family := range []string{"-4", "-6"} {
		if family == "-6" &&
			(!ownership.IPv6Available || ownership.Settings.IPv6Policy != runtimeapi.TUNIPv6Proxy) {
			continue
		}
		routes, routeErr := system.routesInTable(ctx, family, ownership.RouteTable)
		if routeErr != nil || !hasOwnedDefaultRoute(routes, ownership.Device, linuxRouteProtocol) {
			missing = append(missing, runtimeapi.NetworkObject{
				Kind:  "route_table",
				ID:    "route_table:" + family,
				Name:  strconv.Itoa(ownership.RouteTable),
				State: runtimeapi.NetworkStateUnknown,
			})
		}
	}
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Block {
		if _, nftErr := system.run(ctx, nil,
			"nft", "list", "table", "inet", linuxNFTTable(ownership.Token),
		); nftErr != nil {
			missing = append(missing, runtimeapi.NetworkObject{
				Kind:  "nft_table",
				ID:    "nft:" + linuxNFTTable(ownership.Token),
				Name:  linuxNFTTable(ownership.Token),
				State: runtimeapi.NetworkStateUnknown,
			})
		}
	}
	return mergeNetworkObjects(missing), nil
}

func (system *LinuxSystem) residuals(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	residuals := make([]runtimeapi.NetworkObject, 0)
	link, exists, linkErr := system.link(ctx, ownership.Device)
	if linkErr != nil {
		return nil, linkErr
	}
	if exists {
		state := runtimeapi.NetworkStateUnknown
		if link.Alias == linuxOwnershipAlias(ownership.Token) {
			state = ownership.State
		}
		residuals = append(residuals, runtimeapi.NetworkObject{
			Kind:  "tun",
			ID:    "tun:" + ownership.Device,
			Name:  ownership.Device,
			State: state,
		})
	}
	for _, family := range []string{"-4", "-6"} {
		routes, err := system.routesInTable(ctx, family, ownership.RouteTable)
		if err == nil && len(routes) > 0 {
			residuals = append(residuals, runtimeapi.NetworkObject{
				Kind:  "route_table",
				ID:    "route_table:" + family,
				Name:  strconv.Itoa(ownership.RouteTable),
				State: runtimeapi.NetworkStateUnknown,
			})
		}
	}
	for family, expected := range expectedPolicyRuleCommands(system.applyCommands(ownership)) {
		existing, err := system.policyRules(ctx, family)
		if err != nil {
			return nil, err
		}
		for _, command := range expected {
			if !hasPolicyRuleAtPriority(existing, policyRulePriority(command)) {
				continue
			}
			object := command.object
			object.State = runtimeapi.NetworkStateUnknown
			residuals = append(residuals, object)
		}
	}
	if hasNetworkObject(ownership.Objects, "nft:"+linuxNFTTable(ownership.Token)) {
		if _, err := system.run(ctx, nil,
			"nft", "list", "table", "inet", linuxNFTTable(ownership.Token),
		); err == nil {
			residuals = append(residuals, runtimeapi.NetworkObject{
				Kind:  "nft_table",
				ID:    "nft:" + linuxNFTTable(ownership.Token),
				Name:  linuxNFTTable(ownership.Token),
				State: runtimeapi.NetworkStateUnknown,
			})
		}
	}
	return mergeNetworkObjects(residuals), nil
}

func expectedPolicyRules(commands []linuxCommand) map[string]map[int]runtimeapi.NetworkObject {
	expected := make(map[string]map[int]runtimeapi.NetworkObject)
	for _, command := range commands {
		if len(command.arguments) < 5 ||
			command.name != "ip" ||
			command.arguments[1] != "rule" ||
			command.arguments[2] != "add" ||
			command.arguments[3] != "priority" {
			continue
		}
		priority, err := strconv.Atoi(command.arguments[4])
		if err != nil {
			continue
		}
		family := command.arguments[0]
		if expected[family] == nil {
			expected[family] = make(map[int]runtimeapi.NetworkObject)
		}
		expected[family][priority] = command.object
	}
	return expected
}

func expectedPolicyRuleCommands(commands []linuxCommand) map[string][]linuxCommand {
	expected := make(map[string][]linuxCommand)
	for _, command := range commands {
		if policyRulePriority(command) < 0 {
			continue
		}
		family := command.arguments[0]
		expected[family] = append(expected[family], command)
	}
	return expected
}

func (system *LinuxSystem) rulePriorities(ctx context.Context, family string) (map[int]struct{}, error) {
	rules, err := system.policyRules(ctx, family)
	if err != nil {
		return nil, err
	}
	priorities := make(map[int]struct{}, len(rules))
	for _, rule := range rules {
		priorities[rule.Priority] = struct{}{}
	}
	return priorities, nil
}

func (system *LinuxSystem) policyRules(ctx context.Context, family string) ([]linuxRule, error) {
	body, err := system.run(ctx, nil, "ip", "-j", family, "rule", "show")
	if err != nil {
		return nil, err
	}
	var rules []linuxRule
	if err := json.Unmarshal(body, &rules); err != nil {
		return nil, errors.New("Linux policy rule discovery returned invalid JSON")
	}
	return rules, nil
}

func (system *LinuxSystem) routesInTable(
	ctx context.Context,
	family string,
	table int,
) ([]linuxRoute, error) {
	body, err := system.run(ctx, nil,
		"ip", "-j", family, "route", "show", "table", "all",
	)
	if err != nil {
		return nil, err
	}
	var routes []linuxRoute
	if err := json.Unmarshal(body, &routes); err != nil {
		return nil, errors.New("Linux route table inspection returned invalid JSON")
	}
	selected := make([]linuxRoute, 0, len(routes))
	tableName := strconv.Itoa(table)
	for _, route := range routes {
		if normalizeLinuxTable(route.Table) == tableName {
			selected = append(selected, route)
		}
	}
	return selected, nil
}

func (system *LinuxSystem) deleteOwnedNFT(ctx context.Context, token string) error {
	body, err := system.run(ctx, nil, "nft", "-j", "list", "tables")
	if err != nil {
		return err
	}
	var listing struct {
		NFTables []struct {
			Table *struct {
				Family  string `json:"family"`
				Name    string `json:"name"`
				Comment string `json:"comment"`
			} `json:"table,omitempty"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return errors.New("Linux nftables table discovery returned invalid JSON")
	}
	expectedName := linuxNFTTable(token)
	expectedComment := linuxOwnershipAlias(token)
	for _, object := range listing.NFTables {
		if object.Table == nil ||
			object.Table.Family != "inet" ||
			object.Table.Name != expectedName {
			continue
		}
		if object.Table.Comment != expectedComment {
			return errors.New("Runtime nftables table ownership changed; cleanup refused")
		}
		_, err = system.run(ctx, nil,
			"nft", "delete", "table", "inet", expectedName,
		)
		return err
	}
	return nil
}

func (system *LinuxSystem) routes(ctx context.Context, family string) ([]linuxRoute, error) {
	flag := "-4"
	if family == "ipv6" {
		flag = "-6"
	}
	body, err := system.run(ctx, nil, "ip", "-j", flag, "route", "show", "table", "all")
	if err != nil {
		if family == "ipv6" {
			return nil, nil
		}
		return nil, err
	}
	var routes []linuxRoute
	if err := json.Unmarshal(body, &routes); err != nil {
		return nil, errors.New("Linux route discovery returned invalid JSON")
	}
	for index := range routes {
		if family == "ipv6" && routes[index].Destination == "default" {
			routes[index].Destination = "::/0"
		}
	}
	return routes, nil
}

func (system *LinuxSystem) links(ctx context.Context) ([]linuxLink, error) {
	body, err := system.run(ctx, nil, "ip", "-j", "-d", "link", "show")
	if err != nil {
		return nil, err
	}
	var links []linuxLink
	if err := json.Unmarshal(body, &links); err != nil {
		return nil, errors.New("Linux link discovery returned invalid JSON")
	}
	return links, nil
}

func (system *LinuxSystem) link(
	ctx context.Context,
	name string,
) (linuxLink, bool, error) {
	links, err := system.links(ctx)
	if err != nil {
		return linuxLink{}, false, err
	}
	for _, link := range links {
		if link.Name == name {
			return link, true, nil
		}
	}
	return linuxLink{}, false, nil
}

func (system *LinuxSystem) run(
	ctx context.Context,
	input []byte,
	name string,
	arguments ...string,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("privileged Runtime network command context is required")
	}
	return system.Runner.Run(ctx, input, name, arguments...)
}

func (system *LinuxSystem) validate() error {
	if system == nil || system.Runner == nil {
		return errors.New("Linux privileged Runtime network system is unavailable")
	}
	if system.RuntimeUID == 0 {
		return errors.New("ordinary TUN requires a non-root Runtime service account")
	}
	return nil
}

func (system *LinuxSystem) validateOwnership(ownership Ownership) error {
	if err := system.validate(); err != nil {
		return err
	}
	if ownership.Device != linuxTUNDevice ||
		ownership.Mode != runtimeapi.RunModeTUN ||
		!validHex(ownership.Token, 64) ||
		!validOpaqueIdentifier(ownership.ID, 3, 96) {
		return errors.New("ordinary TUN ownership is invalid")
	}
	return nil
}

func (system *LinuxSystem) validatePreparedOwnership(ownership Ownership) error {
	if err := system.validateOwnership(ownership); err != nil {
		return err
	}
	table, priority, err := linuxOwnershipNumbers(ownership.Token)
	if err != nil {
		return err
	}
	if ownership.RoutingMark != table ||
		ownership.RouteTable != table ||
		ownership.RulePriority != priority {
		return errors.New("ordinary TUN ownership numbers do not match its token")
	}
	return nil
}

func linuxOwnershipNumbers(token string) (int, int, error) {
	body, err := hex.DecodeString(token)
	if err != nil || len(body) != 32 {
		return 0, 0, errors.New("ordinary TUN ownership token is invalid")
	}
	table := 30000 + int(binary.BigEndian.Uint16(body[:2]))%20000
	priority := 10000 + int(binary.BigEndian.Uint16(body[2:4]))%8000
	return table, priority, nil
}

func linuxOwnershipAlias(token string) string {
	return "submux:" + token
}

func linuxNFTTable(token string) string {
	return "smx_" + token[:16]
}

func linuxIPv6BlockRules(token string) string {
	table := linuxNFTTable(token)
	comment := linuxOwnershipAlias(token)
	return fmt.Sprintf(
		"add table inet %s { comment %q; }\n"+
			"add chain inet %s output { type filter hook output priority -150; policy accept; }\n"+
			"add rule inet %s output ip6 daddr ::1 accept\n"+
			"add rule inet %s output ip6 daddr fe80::/10 accept\n"+
			"add rule inet %s output ip6 daddr ff00::/8 accept\n"+
			"add rule inet %s output meta nfproto ipv6 counter drop comment \"%s\"\n",
		table, comment, table, table, table, table, table, comment,
	)
}

func linuxPreparedObjects(ownership Ownership) []runtimeapi.NetworkObject {
	objects := []runtimeapi.NetworkObject{{
		Kind:  "tun",
		ID:    "tun:" + ownership.Device,
		Name:  ownership.Device,
		State: runtimeapi.NetworkStatePrepared,
	}, {
		Kind:  "address",
		ID:    "address:ipv4",
		Name:  linuxTUNIPv4,
		State: runtimeapi.NetworkStatePrepared,
	}}
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Proxy {
		objects = append(objects, runtimeapi.NetworkObject{
			Kind:  "address",
			ID:    "address:ipv6",
			Name:  linuxTUNIPv6,
			State: runtimeapi.NetworkStatePrepared,
		})
	}
	return objects
}

func normalizeLinuxTable(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "main"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch text {
		case "", "254":
			return "main"
		case "255":
			return "local"
		case "253":
			return "default"
		default:
			return text
		}
	}
	var number int
	if json.Unmarshal(raw, &number) == nil {
		switch number {
		case 254:
			return "main"
		case 255:
			return "local"
		case 253:
			return "default"
		default:
			return strconv.Itoa(number)
		}
	}
	return "unknown"
}

func normalizeRouteDestination(destination, family string) string {
	if destination == "" || destination == "default" {
		if family == "ipv6" {
			return "::/0"
		}
		return "0.0.0.0/0"
	}
	prefix, err := netip.ParsePrefix(destination)
	if err != nil {
		address, addressErr := netip.ParseAddr(destination)
		if addressErr != nil {
			return ""
		}
		prefix = netip.PrefixFrom(address, address.BitLen())
	}
	return prefix.Masked().String()
}

func linuxRouteID(family, cidr, device, table, protocol string) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{family, cidr, device, table, protocol},
		"\x00",
	)))
	return "route_" + hex.EncodeToString(sum[:12])
}

func tunnelLikeLink(name, kind string) bool {
	name = strings.ToLower(name)
	kind = strings.ToLower(kind)
	if kind == "tun" || kind == "tap" || kind == "wireguard" || kind == "xfrm" {
		return true
	}
	for _, prefix := range []string{"tun", "tap", "wg", "ppp", "vpn", "tailscale", "zt"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func fixedBypassCIDRs(family string) []string {
	if family == "ipv6" {
		return []string{"::1/128", "fe80::/10", "ff00::/8"}
	}
	return []string{
		"127.0.0.0/8",
		"169.254.0.0/16",
		"224.0.0.0/4",
		"255.255.255.255/32",
	}
}

func alwaysBypassRoute(cidr string) bool {
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, fixed := range fixedBypassCIDRs(family) {
			if cidr == fixed {
				return true
			}
		}
	}
	return false
}

func orderedBypassRoutes(routes []runtimeapi.NetworkRoute, family string) []runtimeapi.NetworkRoute {
	ordered := make([]runtimeapi.NetworkRoute, 0, len(routes))
	for _, route := range routes {
		if route.Family == family && route.Bypass && !alwaysBypassRoute(route.CIDR) {
			ordered = append(ordered, route)
		}
	}
	sort.Slice(ordered, func(left, right int) bool {
		leftPrefix, leftErr := netip.ParsePrefix(ordered[left].CIDR)
		rightPrefix, rightErr := netip.ParsePrefix(ordered[right].CIDR)
		if leftErr == nil && rightErr == nil && leftPrefix.Bits() != rightPrefix.Bits() {
			return leftPrefix.Bits() > rightPrefix.Bits()
		}
		if ordered[left].CIDR != ordered[right].CIDR {
			return ordered[left].CIDR < ordered[right].CIDR
		}
		if ordered[left].Table != ordered[right].Table {
			return ordered[left].Table < ordered[right].Table
		}
		return ordered[left].ID < ordered[right].ID
	})
	return ordered
}

func hasOwnedDefaultRoute(routes []linuxRoute, device, protocol string) bool {
	for _, route := range routes {
		if route.Device == device &&
			route.Protocol == protocol &&
			(route.Destination == "" ||
				route.Destination == "default" ||
				route.Destination == "0.0.0.0/0" ||
				route.Destination == "::/0") {
			return true
		}
	}
	return false
}

func hasNetworkObject(objects []runtimeapi.NetworkObject, id string) bool {
	for _, object := range objects {
		if object.ID == id {
			return true
		}
	}
	return false
}

func uniqueNetworkRoutes(routes []runtimeapi.NetworkRoute) []runtimeapi.NetworkRoute {
	byID := make(map[string]runtimeapi.NetworkRoute, len(routes))
	for _, route := range routes {
		byID[route.ID] = route
	}
	result := make([]runtimeapi.NetworkRoute, 0, len(byID))
	for _, route := range byID {
		result = append(result, route)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Family != result[right].Family {
			return result[left].Family < result[right].Family
		}
		if result[left].CIDR != result[right].CIDR {
			return result[left].CIDR < result[right].CIDR
		}
		return result[left].ID < result[right].ID
	})
	return result
}

func uniqueNetworkConflicts(conflicts []runtimeapi.NetworkConflict) []runtimeapi.NetworkConflict {
	seen := make(map[string]struct{}, len(conflicts))
	result := make([]runtimeapi.NetworkConflict, 0, len(conflicts))
	for _, conflict := range conflicts {
		key := conflict.Kind + "\x00" + conflict.Owner + "\x00" + conflict.Detail
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, conflict)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Kind != result[right].Kind {
			return result[left].Kind < result[right].Kind
		}
		return result[left].Owner < result[right].Owner
	})
	return result
}
