package runtimenet

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"submux/internal/runtimeapi"
)

const (
	linuxGatewayTUNDevice   = "smxgw0"
	linuxGatewayTUNIPv4     = "198.18.0.5/30"
	linuxGatewayNFTPriority = -50
)

func (system *LinuxSystem) DiscoverGateway(
	ctx context.Context,
	settings runtimeapi.GatewaySettings,
) (Discovery, error) {
	if err := system.validate(); err != nil {
		return Discovery{}, err
	}
	if err := validateGatewaySettings(settings); err != nil {
		return Discovery{}, err
	}
	links, err := system.links(ctx)
	if err != nil {
		return Discovery{}, err
	}
	routes, err := system.routes(ctx, "ipv4")
	if err != nil {
		return Discovery{}, err
	}
	ipv6Routes, err := system.routes(ctx, "ipv6")
	if err != nil {
		return Discovery{}, err
	}
	linksByName := make(map[string]linuxLink, len(links))
	for _, link := range links {
		linksByName[link.Name] = link
	}
	conflicts := make([]runtimeapi.NetworkConflict, 0)
	if existing, ok := linksByName[linuxGatewayTUNDevice]; ok {
		conflicts = append(conflicts, runtimeapi.NetworkConflict{
			Kind:   "tun_name",
			Owner:  existing.Alias,
			Detail: "TUN device smxgw0 already exists without matching active ownership",
		})
	}
	forwarding, err := system.run(ctx, nil, "sysctl", "-n", "net.ipv4.ip_forward")
	if err != nil {
		return Discovery{}, err
	}
	if strings.TrimSpace(string(forwarding)) != "1" {
		conflicts = append(conflicts, runtimeapi.NetworkConflict{
			Kind:   "ip_forward",
			Owner:  "net.ipv4.ip_forward",
			Detail: "Linux IPv4 forwarding is disabled",
		})
	}
	defaults := make([]linuxRoute, 0)
	for _, route := range routes {
		if normalizeLinuxTable(route.Table) != "main" {
			continue
		}
		cidr := normalizeRouteDestination(route.Destination, "ipv4")
		if cidr == "0.0.0.0/0" {
			defaults = append(defaults, route)
		}
	}
	sort.Slice(defaults, func(left, right int) bool {
		if defaults[left].Metric != defaults[right].Metric {
			return defaults[left].Metric < defaults[right].Metric
		}
		return defaults[left].Device < defaults[right].Device
	})
	var wan linuxRoute
	if len(defaults) == 0 {
		conflicts = append(conflicts, runtimeapi.NetworkConflict{
			Kind:   "gateway_wan",
			Owner:  "main",
			Detail: "Linux gateway could not discover a main-table WAN default route",
		})
	} else {
		wan = defaults[0]
		if !validNFTInterface(wan.Device) {
			return Discovery{}, errors.New("Linux gateway WAN interface name is not safe for nftables")
		}
		if tunnelLikeLink(wan.Device, linksByName[wan.Device].Info.Kind) {
			conflicts = append(conflicts, runtimeapi.NetworkConflict{
				Kind:   "full_tunnel",
				Owner:  wan.Device,
				Detail: "Linux gateway WAN default route is owned by another tunnel",
			})
		}
		for _, candidate := range defaults[1:] {
			if candidate.Device == wan.Device && candidate.Gateway == wan.Gateway {
				continue
			}
			conflicts = append(conflicts, runtimeapi.NetworkConflict{
				Kind:   "gateway_wan",
				Owner:  candidate.Device,
				Detail: "Linux gateway found multiple main-table WAN default routes",
			})
		}
	}
	previewRoutes := make([]runtimeapi.NetworkRoute, 0, len(routes)+1)
	if wan.Device != "" {
		previewRoutes = append(previewRoutes, runtimeapi.NetworkRoute{
			ID:        linuxRouteID("ipv4", "0.0.0.0/0", wan.Device, "main", wan.Protocol),
			Family:    "ipv4",
			CIDR:      "0.0.0.0/0",
			Interface: wan.Device,
			Table:     "main",
			Source:    wan.Protocol,
			Role:      runtimeapi.NetworkRouteRoleGatewayWAN,
			Bypass:    true,
		})
	}
	wanGateway, _ := netip.ParseAddr(wan.Gateway)
	lanCount := 0
	for _, route := range routes {
		table := normalizeLinuxTable(route.Table)
		cidr := normalizeRouteDestination(route.Destination, "ipv4")
		if cidr == "" || cidr == "0.0.0.0/0" || route.Device == "" {
			continue
		}
		prefix, prefixErr := netip.ParsePrefix(cidr)
		if prefixErr != nil || !prefix.Addr().Is4() {
			continue
		}
		if !validNFTInterface(route.Device) {
			return Discovery{}, errors.New("Linux gateway discovered an interface name that is not safe for nftables")
		}
		role := runtimeapi.NetworkRouteRoleDirect
		if table == "main" &&
			route.Device != "lo" &&
			(route.Gateway == "" && (route.Scope == "" || route.Scope == "link")) {
			role = runtimeapi.NetworkRouteRoleGatewayLAN
			if route.Device == wan.Device && wanGateway.IsValid() && prefix.Contains(wanGateway) {
				role = runtimeapi.NetworkRouteRoleGatewayWAN
			} else {
				lanCount++
			}
		}
		previewRoutes = append(previewRoutes, runtimeapi.NetworkRoute{
			ID:        linuxRouteID("ipv4", cidr, route.Device, table, route.Protocol),
			Family:    "ipv4",
			CIDR:      cidr,
			Interface: route.Device,
			Table:     table,
			Source:    route.Protocol,
			Role:      role,
			Bypass:    role != runtimeapi.NetworkRouteRoleGatewayLAN,
		})
	}
	if lanCount == 0 {
		conflicts = append(conflicts, runtimeapi.NetworkConflict{
			Kind:   "gateway_lan",
			Owner:  "route_discovery",
			Detail: "Linux gateway did not discover a directly connected LAN route",
		})
	}
	warnings := []string(nil)
	if len(ipv6Routes) > 0 && settings.IPv6Policy == runtimeapi.TUNIPv6Direct {
		warnings = append(
			warnings,
			"Linux 网关第一版不代理 IPv6；内网设备的 IPv6 将直连。",
		)
	}
	return Discovery{
		Device:        linuxGatewayTUNDevice,
		IPv6Available: len(ipv6Routes) > 0,
		Routes:        uniqueNetworkRoutes(previewRoutes),
		Conflicts:     uniqueNetworkConflicts(conflicts),
		Warnings:      warnings,
		Original: map[string]string{
			"wan_interface": wan.Device,
			"wan_gateway":   wan.Gateway,
		},
	}, nil
}

func (system *LinuxSystem) PrepareGateway(
	ctx context.Context,
	ownership Ownership,
) (SystemPreparation, error) {
	if err := system.validateGatewayOwnership(ownership); err != nil {
		return SystemPreparation{}, err
	}
	routeTable, routingMark, captureMark, priority, err := linuxGatewayOwnershipNumbers(ownership.Token)
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
			return SystemPreparation{}, errors.New("gateway TUN device name is owned by another process")
		}
		return SystemPreparation{
			Objects:      linuxGatewayPreparedObjects(ownership),
			RoutingMark:  routingMark,
			CaptureMark:  captureMark,
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
	rollback := func() {
		_, _ = system.run(context.Background(), nil, "ip", "link", "delete", "dev", ownership.Device)
	}
	if _, err := system.run(ctx, nil,
		"ip", "link", "set", "dev", ownership.Device, "alias", expectedAlias,
	); err != nil {
		rollback()
		return SystemPreparation{}, err
	}
	if _, err := system.run(ctx, nil,
		"ip", "addr", "add", linuxGatewayTUNIPv4, "dev", ownership.Device,
	); err != nil {
		rollback()
		return SystemPreparation{}, err
	}
	if _, err := system.run(ctx, nil, "ip", "link", "set", "dev", ownership.Device, "up"); err != nil {
		rollback()
		return SystemPreparation{}, err
	}
	return SystemPreparation{
		Objects:      linuxGatewayPreparedObjects(ownership),
		RoutingMark:  routingMark,
		CaptureMark:  captureMark,
		RouteTable:   routeTable,
		RulePriority: priority,
	}, nil
}

func (system *LinuxSystem) ApplyGateway(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := system.validatePreparedGatewayOwnership(ownership); err != nil {
		return nil, err
	}
	link, exists, err := system.link(ctx, ownership.Device)
	if err != nil {
		return nil, err
	}
	if !exists || link.Alias != linuxOwnershipAlias(ownership.Token) {
		return nil, errors.New("Linux gateway ownership no longer matches the system device")
	}
	commands := system.gatewayCommands(ownership)
	if err := system.ensureCommandRuleRangeFree(ctx, commands); err != nil {
		return nil, err
	}
	if err := system.ensureRouteTableFree(ctx, ownership); err != nil {
		return nil, err
	}
	applied := make([]runtimeapi.NetworkObject, 0, len(commands)+1)
	for _, command := range commands {
		if _, err := system.run(ctx, nil, command.name, command.arguments...); err != nil {
			_, _ = system.cleanupGateway(context.Background(), ownership)
			return applied, err
		}
		applied = append(applied, command.object)
	}
	rules, err := linuxGatewayNFTRules(ownership, system.RuntimeUID)
	if err != nil {
		_, _ = system.cleanupGateway(context.Background(), ownership)
		return applied, err
	}
	if _, err := system.run(ctx, []byte(rules), "nft", "-f", "-"); err != nil {
		_, _ = system.run(
			context.Background(),
			nil,
			"nft", "delete", "table", "inet", linuxNFTTable(ownership.Token),
		)
		_, _ = system.cleanupGateway(context.Background(), ownership)
		return applied, err
	}
	applied = append(applied, runtimeapi.NetworkObject{
		Kind:  "nft_table",
		ID:    "nft:" + linuxNFTTable(ownership.Token),
		Name:  linuxNFTTable(ownership.Token),
		State: runtimeapi.NetworkStateActive,
	})
	return mergeNetworkObjects(applied), nil
}

func (system *LinuxSystem) cleanupGateway(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := system.validatePreparedGatewayOwnership(ownership); err != nil {
		return nil, err
	}
	link, exists, linkErr := system.link(ctx, ownership.Device)
	if linkErr != nil {
		return nil, linkErr
	}
	if exists && link.Alias != linuxOwnershipAlias(ownership.Token) {
		return []runtimeapi.NetworkObject{{
			Kind:  "tun",
			ID:    "tun:" + ownership.Device,
			Name:  ownership.Device,
			State: runtimeapi.NetworkStateUnknown,
		}}, errors.New("Linux gateway TUN ownership changed; cleanup refused")
	}
	var cleanupErr error
	if hasNetworkObject(ownership.Objects, "nft:"+linuxNFTTable(ownership.Token)) {
		cleanupErr = errors.Join(cleanupErr, system.deleteOwnedNFT(ctx, ownership.Token))
	}
	var commandErr error
	commands := system.gatewayCommands(ownership)
	for index := len(commands) - 1; index >= 0; index-- {
		if deletion, ok := exactLinuxDeletion(commands[index]); ok {
			if _, err := system.run(ctx, nil, deletion.name, deletion.arguments...); err != nil {
				commandErr = errors.Join(commandErr, err)
			}
		}
	}
	if exists {
		if _, err := system.run(ctx, nil, "ip", "link", "delete", "dev", ownership.Device); err != nil {
			commandErr = errors.Join(commandErr, err)
		}
	}
	residuals, residualErr := system.gatewayResiduals(ctx, ownership)
	// Exact deletion is intentionally idempotent: ip reports an error when an
	// object was already absent. Surface command errors only when the
	// post-cleanup inspection failed or still found an owned object.
	if residualErr != nil || len(residuals) > 0 {
		cleanupErr = errors.Join(cleanupErr, commandErr)
	}
	return residuals, errors.Join(cleanupErr, residualErr)
}

func (system *LinuxSystem) observeGateway(
	ctx context.Context,
	ownership Ownership,
) (runtimeapi.NetworkStatus, error) {
	if err := system.validatePreparedGatewayOwnership(ownership); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	status := runtimeapi.NetworkStatus{
		Mode:            runtimeapi.RunModeGateway,
		State:           ownership.State,
		Device:          ownership.Device,
		GatewaySettings: cloneGatewaySettings(ownership.GatewaySettings),
		Objects:         append([]runtimeapi.NetworkObject(nil), ownership.Objects...),
		Routes:          append([]runtimeapi.NetworkRoute(nil), ownership.Routes...),
	}
	missing, err := system.missingGatewayObjects(ctx, ownership)
	if err != nil || len(missing) > 0 {
		status.State = runtimeapi.NetworkStateUnknown
		status.Residuals = missing
	}
	return status, err
}

func (system *LinuxSystem) gatewayCommands(ownership Ownership) []linuxCommand {
	table := strconv.Itoa(ownership.RouteTable)
	return []linuxCommand{
		linuxRuleCommand(
			"-4",
			ownership.RulePriority,
			[]string{"fwmark", strconv.Itoa(ownership.RoutingMark), "lookup", "main"},
			"gateway:mark:bypass",
			strconv.Itoa(ownership.RoutingMark),
		),
		{
			name: "ip",
			arguments: []string{
				"-4", "route", "add", "table", table,
				"0.0.0.0/0", "dev", ownership.Device, "proto", linuxRouteProtocol,
			},
			object: runtimeapi.NetworkObject{
				Kind:  "route",
				ID:    "gateway:route:default",
				Name:  "0.0.0.0/0",
				State: runtimeapi.NetworkStateActive,
			},
		},
		linuxRuleCommand(
			"-4",
			ownership.RulePriority+1,
			[]string{"fwmark", strconv.Itoa(ownership.CaptureMark), "lookup", table},
			"gateway:mark:capture",
			strconv.Itoa(ownership.CaptureMark),
		),
	}
}

func (system *LinuxSystem) ensureCommandRuleRangeFree(
	ctx context.Context,
	commands []linuxCommand,
) error {
	for family, expected := range expectedPolicyRules(commands) {
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

func (system *LinuxSystem) missingGatewayObjects(
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
	for family, commands := range expectedPolicyRuleCommands(system.gatewayCommands(ownership)) {
		rules, ruleErr := system.policyRules(ctx, family)
		if ruleErr != nil {
			return nil, ruleErr
		}
		for _, command := range commands {
			if hasExactPolicyRule(rules, command) {
				continue
			}
			object := command.object
			object.State = runtimeapi.NetworkStateUnknown
			missing = append(missing, object)
		}
	}
	routes, routeErr := system.routesInTable(ctx, "-4", ownership.RouteTable)
	if routeErr != nil || !hasOwnedDefaultRoute(routes, ownership.Device, linuxRouteProtocol) {
		missing = append(missing, runtimeapi.NetworkObject{
			Kind:  "route_table",
			ID:    "gateway:route_table",
			Name:  strconv.Itoa(ownership.RouteTable),
			State: runtimeapi.NetworkStateUnknown,
		})
	}
	if _, nftErr := system.run(
		ctx,
		nil,
		"nft", "list", "table", "inet", linuxNFTTable(ownership.Token),
	); nftErr != nil {
		missing = append(missing, runtimeapi.NetworkObject{
			Kind:  "nft_table",
			ID:    "nft:" + linuxNFTTable(ownership.Token),
			Name:  linuxNFTTable(ownership.Token),
			State: runtimeapi.NetworkStateUnknown,
		})
	}
	return mergeNetworkObjects(missing), routeErr
}

func (system *LinuxSystem) gatewayResiduals(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	residuals := make([]runtimeapi.NetworkObject, 0)
	_, exists, err := system.link(ctx, ownership.Device)
	if err != nil {
		return nil, err
	}
	if exists {
		residuals = append(residuals, runtimeapi.NetworkObject{
			Kind:  "tun",
			ID:    "tun:" + ownership.Device,
			Name:  ownership.Device,
			State: runtimeapi.NetworkStateUnknown,
		})
	}
	routes, routeErr := system.routesInTable(ctx, "-4", ownership.RouteTable)
	if routeErr == nil && len(routes) > 0 {
		residuals = append(residuals, runtimeapi.NetworkObject{
			Kind:  "route_table",
			ID:    "gateway:route_table",
			Name:  strconv.Itoa(ownership.RouteTable),
			State: runtimeapi.NetworkStateUnknown,
		})
	}
	for family, commands := range expectedPolicyRuleCommands(system.gatewayCommands(ownership)) {
		rules, ruleErr := system.policyRules(ctx, family)
		if ruleErr != nil {
			return nil, errors.Join(routeErr, ruleErr)
		}
		for _, command := range commands {
			if !hasPolicyRuleAtPriority(rules, policyRulePriority(command)) {
				continue
			}
			object := command.object
			object.State = runtimeapi.NetworkStateUnknown
			residuals = append(residuals, object)
		}
	}
	if _, nftErr := system.run(
		ctx,
		nil,
		"nft", "list", "table", "inet", linuxNFTTable(ownership.Token),
	); nftErr == nil {
		residuals = append(residuals, runtimeapi.NetworkObject{
			Kind:  "nft_table",
			ID:    "nft:" + linuxNFTTable(ownership.Token),
			Name:  linuxNFTTable(ownership.Token),
			State: runtimeapi.NetworkStateUnknown,
		})
	}
	return mergeNetworkObjects(residuals), routeErr
}

func linuxGatewayNFTRules(ownership Ownership, runtimeUID uint32) (string, error) {
	if ownership.GatewaySettings == nil {
		return "", errors.New("Linux gateway settings are missing")
	}
	if err := validateGatewaySettings(*ownership.GatewaySettings); err != nil {
		return "", err
	}
	table := linuxNFTTable(ownership.Token)
	comment := linuxOwnershipAlias(ownership.Token)
	lanInterfaces := make([]string, 0)
	lanCIDRs := make([]string, 0)
	directCIDRs := append([]string(nil), fixedBypassCIDRs("ipv4")...)
	for _, route := range ownership.Routes {
		if route.CIDR != "" && route.CIDR != "0.0.0.0/0" {
			directCIDRs = append(directCIDRs, route.CIDR)
		}
		if route.Role != runtimeapi.NetworkRouteRoleGatewayLAN || route.Bypass {
			continue
		}
		if !validNFTInterface(route.Interface) {
			return "", errors.New("Linux gateway LAN interface name is not safe for nftables")
		}
		lanInterfaces = append(lanInterfaces, route.Interface)
		lanCIDRs = append(lanCIDRs, route.CIDR)
	}
	lanInterfaces = uniqueSortedStrings(lanInterfaces)
	lanCIDRs, err := nonOverlappingIPv4CIDRs(lanCIDRs)
	if err != nil {
		return "", fmt.Errorf("normalize Linux gateway LAN routes: %w", err)
	}
	directCIDRs, err = nonOverlappingIPv4CIDRs(directCIDRs)
	if err != nil {
		return "", fmt.Errorf("normalize Linux gateway direct routes: %w", err)
	}
	if len(lanInterfaces) == 0 || len(lanCIDRs) == 0 {
		return "", errors.New("Linux gateway has no enabled LAN route")
	}
	var script strings.Builder
	fmt.Fprintf(&script, "add table inet %s { comment %s; }\n", table, nftQuote(comment))
	writeNFTStringSet(&script, table, "lan_ifaces", "ifname", lanInterfaces, true)
	writeNFTStringSet(&script, table, "lan_v4", "ipv4_addr", lanCIDRs, false)
	writeNFTStringSet(&script, table, "direct_v4", "ipv4_addr", directCIDRs, false)
	if len(ownership.GatewaySettings.DNSDirectCIDRs) > 0 {
		dnsDirectCIDRs, normalizeErr := nonOverlappingIPv4CIDRs(
			ownership.GatewaySettings.DNSDirectCIDRs,
		)
		if normalizeErr != nil {
			return "", fmt.Errorf("normalize Linux gateway DNS direct routes: %w", normalizeErr)
		}
		writeNFTStringSet(
			&script,
			table,
			"dns_direct_v4",
			"ipv4_addr",
			dnsDirectCIDRs,
			false,
		)
	}
	fmt.Fprintf(
		&script,
		"add chain inet %s prerouting { type filter hook prerouting priority %d; policy accept; }\n",
		table,
		linuxGatewayNFTPriority,
	)
	fmt.Fprintf(&script, "add chain inet %s capture_lan\n", table)
	fmt.Fprintf(
		&script,
		"add rule inet %s prerouting counter comment %s\n",
		table,
		nftQuote(comment),
	)
	fmt.Fprintf(
		&script,
		"add rule inet %s prerouting iifname @lan_ifaces ip saddr @lan_v4 jump capture_lan\n",
		table,
	)
	writeGatewayCaptureChain(
		&script,
		table,
		"capture_lan",
		*ownership.GatewaySettings,
		ownership.CaptureMark,
		false,
	)
	if ownership.GatewaySettings.ProxyHostTraffic {
		fmt.Fprintf(
			&script,
			"add chain inet %s output { type route hook output priority %d; policy accept; }\n",
			table,
			linuxGatewayNFTPriority,
		)
		fmt.Fprintf(
			&script,
			"add rule inet %s output meta skuid { 0, %d } return\n",
			table,
			runtimeUID,
		)
		writeGatewayCaptureChain(
			&script,
			table,
			"output",
			*ownership.GatewaySettings,
			ownership.CaptureMark,
			true,
		)
	}
	if ownership.IPv6Available &&
		ownership.GatewaySettings.IPv6Policy == runtimeapi.TUNIPv6Block {
		fmt.Fprintf(
			&script,
			"add chain inet %s forward { type filter hook forward priority -150; policy accept; }\n",
			table,
		)
		fmt.Fprintf(
			&script,
			"add rule inet %s forward iifname @lan_ifaces meta nfproto ipv6 drop comment %s\n",
			table,
			nftQuote(comment),
		)
	}
	return script.String(), nil
}

func writeGatewayCaptureChain(
	script *strings.Builder,
	table string,
	chain string,
	settings runtimeapi.GatewaySettings,
	captureMark int,
	host bool,
) {
	fmt.Fprintf(
		script,
		"add rule inet %s %s ct mark %d ct direction original meta mark set %d return\n",
		table,
		chain,
		captureMark,
		captureMark,
	)
	fmt.Fprintf(script, "add rule inet %s %s ct status dnat return\n", table, chain)
	fmt.Fprintf(
		script,
		"add rule inet %s %s ct state established,related return\n",
		table,
		chain,
	)
	fmt.Fprintf(script, "add rule inet %s %s ip daddr @direct_v4 return\n", table, chain)
	if host {
		for _, exception := range settings.HostExceptions {
			writeGatewayException(script, table, chain, exception, "tcp")
			writeGatewayException(script, table, chain, exception, "udp")
		}
	}
	if len(settings.DNSDirectCIDRs) > 0 {
		fmt.Fprintf(
			script,
			"add rule inet %s %s ip daddr @dns_direct_v4 udp dport 53 return\n",
			table,
			chain,
		)
		fmt.Fprintf(
			script,
			"add rule inet %s %s ip daddr @dns_direct_v4 tcp dport 53 return\n",
			table,
			chain,
		)
	}
	if settings.DNSPolicy == runtimeapi.TUNDNSHijack {
		writeGatewayMarkRule(script, table, chain, "udp dport 53", captureMark)
		writeGatewayMarkRule(script, table, chain, "tcp dport 53", captureMark)
	} else {
		fmt.Fprintf(script, "add rule inet %s %s udp dport 53 return\n", table, chain)
		fmt.Fprintf(script, "add rule inet %s %s tcp dport 53 return\n", table, chain)
	}
	for _, exception := range settings.UDPExceptions {
		writeGatewayException(script, table, chain, exception, "udp")
	}
	if settings.CaptureTCP {
		writeGatewayMarkRule(script, table, chain, "meta l4proto tcp", captureMark)
	}
	if settings.CaptureUDP {
		writeGatewayMarkRule(script, table, chain, "meta l4proto udp", captureMark)
	}
}

func writeGatewayException(
	script *strings.Builder,
	table string,
	chain string,
	exception runtimeapi.GatewayTrafficException,
	protocol string,
) {
	parts := make([]string, 0, 5)
	if exception.UID != nil {
		parts = append(parts, "meta skuid "+strconv.FormatUint(uint64(*exception.UID), 10))
	}
	if exception.SourceCIDR != "" {
		parts = append(parts, "ip saddr "+exception.SourceCIDR)
	}
	if exception.DestinationCIDR != "" {
		parts = append(parts, "ip daddr "+exception.DestinationCIDR)
	}
	if len(exception.DestinationPorts) > 0 {
		ports := make([]string, 0, len(exception.DestinationPorts))
		for _, portRange := range exception.DestinationPorts {
			if portRange.Start == portRange.End {
				ports = append(ports, strconv.Itoa(int(portRange.Start)))
			} else {
				ports = append(
					ports,
					fmt.Sprintf("%d-%d", portRange.Start, portRange.End),
				)
			}
		}
		parts = append(parts, protocol+" dport { "+strings.Join(ports, ", ")+" }")
	} else {
		parts = append(parts, "meta l4proto "+protocol)
	}
	fmt.Fprintf(
		script,
		"add rule inet %s %s %s return\n",
		table,
		chain,
		strings.Join(parts, " "),
	)
}

func writeGatewayMarkRule(
	script *strings.Builder,
	table string,
	chain string,
	match string,
	captureMark int,
) {
	fmt.Fprintf(
		script,
		"add rule inet %s %s %s ct mark set %d meta mark set %d return\n",
		table,
		chain,
		match,
		captureMark,
		captureMark,
	)
}

func writeNFTStringSet(
	script *strings.Builder,
	table string,
	name string,
	setType string,
	values []string,
	quoted bool,
) {
	flags := ""
	if setType == "ipv4_addr" {
		flags = " flags interval;"
	}
	fmt.Fprintf(
		script,
		"add set inet %s %s { type %s;%s }\n",
		table,
		name,
		setType,
		flags,
	)
	elements := make([]string, len(values))
	for index, value := range values {
		if quoted {
			elements[index] = nftQuote(value)
		} else {
			elements[index] = value
		}
	}
	fmt.Fprintf(
		script,
		"add element inet %s %s { %s }\n",
		table,
		name,
		strings.Join(elements, ", "),
	)
}

func nonOverlappingIPv4CIDRs(values []string) ([]string, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range uniqueSortedStrings(values) {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() {
			return nil, fmt.Errorf("invalid IPv4 CIDR %q", value)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	sort.Slice(prefixes, func(left, right int) bool {
		if prefixes[left].Bits() != prefixes[right].Bits() {
			return prefixes[left].Bits() < prefixes[right].Bits()
		}
		return prefixes[left].Addr().Less(prefixes[right].Addr())
	})
	result := make([]netip.Prefix, 0, len(prefixes))
	for _, candidate := range prefixes {
		covered := false
		for _, existing := range result {
			if existing.Bits() <= candidate.Bits() &&
				existing.Contains(candidate.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	values = make([]string, len(result))
	for index, prefix := range result {
		values[index] = prefix.String()
	}
	return values, nil
}

func nftQuote(value string) string {
	return `"` + strings.ReplaceAll(
		strings.ReplaceAll(value, `\`, `\\`),
		`"`,
		`\"`,
	) + `"`
}

func (system *LinuxSystem) validateGatewayOwnership(ownership Ownership) error {
	if err := system.validate(); err != nil {
		return err
	}
	if ownership.Mode != runtimeapi.RunModeGateway ||
		ownership.Device != linuxGatewayTUNDevice ||
		!validHex(ownership.Token, 64) ||
		!validOpaqueIdentifier(ownership.ID, 3, 96) ||
		ownership.GatewaySettings == nil {
		return errors.New("Linux gateway ownership is invalid")
	}
	return validateGatewaySettings(*ownership.GatewaySettings)
}

func (system *LinuxSystem) validatePreparedGatewayOwnership(ownership Ownership) error {
	if err := system.validateGatewayOwnership(ownership); err != nil {
		return err
	}
	table, routingMark, captureMark, priority, err := linuxGatewayOwnershipNumbers(ownership.Token)
	if err != nil {
		return err
	}
	if ownership.RouteTable != table ||
		ownership.RoutingMark != routingMark ||
		ownership.CaptureMark != captureMark ||
		ownership.RulePriority != priority {
		return errors.New("Linux gateway ownership numbers do not match its token")
	}
	return nil
}

func validateGatewaySettings(settings runtimeapi.GatewaySettings) error {
	if settings.IPv6Policy != runtimeapi.TUNIPv6Direct &&
		settings.IPv6Policy != runtimeapi.TUNIPv6Block {
		return errors.New("Linux gateway IPv6 policy is invalid")
	}
	if settings.DNSPolicy != runtimeapi.TUNDNSHijack &&
		settings.DNSPolicy != runtimeapi.TUNDNSOff {
		return errors.New("Linux gateway DNS policy is invalid")
	}
	if len(settings.ExcludedRouteIDs) > 512 {
		return errors.New("Linux gateway route exclusion list is too large")
	}
	for _, routeID := range settings.ExcludedRouteIDs {
		if !validOpaqueIdentifier(routeID, 3, 96) {
			return errors.New("Linux gateway route exclusion is invalid")
		}
	}
	if len(settings.DNSDirectCIDRs) > 128 {
		return errors.New("Linux gateway DNS direct list is too large")
	}
	for _, cidr := range settings.DNSDirectCIDRs {
		if _, err := normalizeGatewayCIDR(cidr); err != nil {
			return fmt.Errorf("Linux gateway DNS direct CIDR is invalid: %w", err)
		}
	}
	if err := validateLinuxGatewayExceptions(settings.UDPExceptions, false); err != nil {
		return fmt.Errorf("Linux gateway UDP exception is invalid: %w", err)
	}
	if err := validateLinuxGatewayExceptions(settings.HostExceptions, true); err != nil {
		return fmt.Errorf("Linux gateway host exception is invalid: %w", err)
	}
	return nil
}

func validateLinuxGatewayExceptions(
	exceptions []runtimeapi.GatewayTrafficException,
	allowUID bool,
) error {
	if len(exceptions) > 128 {
		return errors.New("exception list is too large")
	}
	for _, exception := range exceptions {
		if exception.UID != nil && !allowUID {
			return errors.New("UID is only valid for host traffic")
		}
		if exception.SourceCIDR != "" {
			if _, err := normalizeGatewayCIDR(exception.SourceCIDR); err != nil {
				return fmt.Errorf("source CIDR: %w", err)
			}
		}
		if exception.DestinationCIDR != "" {
			if _, err := normalizeGatewayCIDR(exception.DestinationCIDR); err != nil {
				return fmt.Errorf("destination CIDR: %w", err)
			}
		}
		if len(exception.DestinationPorts) > 128 {
			return errors.New("destination port list is too large")
		}
		for _, portRange := range exception.DestinationPorts {
			if portRange.Start == 0 ||
				portRange.End == 0 ||
				portRange.End < portRange.Start {
				return errors.New("destination port range is invalid")
			}
		}
		if exception.SourceCIDR == "" &&
			exception.DestinationCIDR == "" &&
			len(exception.DestinationPorts) == 0 &&
			exception.UID == nil {
			return errors.New("exception has no typed condition")
		}
	}
	return nil
}

func linuxGatewayOwnershipNumbers(token string) (int, int, int, int, error) {
	body, err := hex.DecodeString(token)
	if err != nil || len(body) != 32 {
		return 0, 0, 0, 0, errors.New("Linux gateway ownership token is invalid")
	}
	table := 30000 + int(binary.BigEndian.Uint16(body[:2]))%20000
	routingMark := table
	captureMark := int(uint32(0x53000000) | uint32(binary.BigEndian.Uint16(body[4:6])))
	priority := 18000 + int(binary.BigEndian.Uint16(body[2:4]))%6000
	return table, routingMark, captureMark, priority, nil
}

func linuxGatewayPreparedObjects(ownership Ownership) []runtimeapi.NetworkObject {
	return []runtimeapi.NetworkObject{{
		Kind:  "tun",
		ID:    "tun:" + ownership.Device,
		Name:  ownership.Device,
		State: runtimeapi.NetworkStatePrepared,
	}, {
		Kind:  "address",
		ID:    "address:gateway:ipv4",
		Name:  linuxGatewayTUNIPv4,
		State: runtimeapi.NetworkStatePrepared,
	}}
}

func validNFTInterface(value string) bool {
	if value == "" || len(value) > 15 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("_.:-", character) {
			continue
		}
		return false
	}
	return true
}

func uniqueSortedStrings(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if value == "" || (len(result) > 0 && result[len(result)-1] == value) {
			continue
		}
		result = append(result, value)
	}
	return result
}
