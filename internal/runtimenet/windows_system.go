//go:build windows

package runtimenet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"submux/internal/runtimeapi"
)

const (
	windowsTUNDevice     = "SubmuxRuntime"
	windowsMaxRoutes     = 4096
	windowsMaxSelections = 512
)

var windowsCapturePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/1"),
	netip.MustParsePrefix("128.0.0.0/1"),
}

var windowsIPv6CapturePrefixes = []netip.Prefix{
	netip.MustParsePrefix("::/1"),
	netip.MustParsePrefix("8000::/1"),
}

type WindowsSystem struct {
	API        windowsNetworkAPI
	Runner     CommandRunner
	Interfaces func() ([]windowsInterface, error)
	DNSServers func(uint32) ([]netip.Addr, error)
}

type windowsInterface struct {
	Index uint32
	Name  string
	Type  uint32
	GUID  string
}

func NewWindowsSystem() (*WindowsSystem, error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("Windows ordinary TUN networking is only available on Windows")
	}
	return &WindowsSystem{
		API:        ipHelperNetworkAPI{},
		Runner:     execCommandRunner{},
		Interfaces: windowsInterfaces,
		DNSServers: windowsDNSServers,
	}, nil
}

func (system *WindowsSystem) Discover(
	ctx context.Context,
	settings runtimeapi.TUNSettings,
) (Discovery, error) {
	if err := system.validate(); err != nil {
		return Discovery{}, err
	}
	if err := contextError(ctx); err != nil {
		return Discovery{}, err
	}
	interfaces, err := system.Interfaces()
	if err != nil {
		return Discovery{}, err
	}
	routes, err := system.API.Routes()
	if err != nil {
		return Discovery{}, err
	}
	if len(routes) > windowsMaxRoutes {
		return Discovery{}, errors.New("Windows route table exceeds the Runtime inspection limit")
	}

	byIndex := make(map[uint32]windowsInterface, len(interfaces))
	var adapter windowsInterface
	for _, candidate := range interfaces {
		byIndex[candidate.Index] = candidate
		if candidate.Name == windowsTUNDevice {
			adapter = candidate
		}
	}

	conflicts := make([]runtimeapi.NetworkConflict, 0)
	original := make(map[string]string)
	if adapter.Index == 0 {
		conflicts = append(conflicts, runtimeapi.NetworkConflict{
			Kind:   "tun_device_missing",
			Detail: "The fixed SubmuxRuntime Wintun adapter has not been provisioned by the installer",
		})
	} else {
		original["interface_index"] = strconv.FormatUint(uint64(adapter.Index), 10)
		original["interface_guid"] = adapter.GUID
		dnsServers, err := system.DNSServers(adapter.Index)
		if err != nil {
			return Discovery{}, err
		}
		original["dns_servers"] = joinWindowsAddresses(dnsServers)
	}

	discovered := make([]runtimeapi.NetworkRoute, 0, len(routes))
	ipv6Available := false
	for _, route := range routes {
		if !route.Destination.IsValid() {
			continue
		}
		family := "ipv4"
		if route.Destination.Addr().Is6() {
			family = "ipv6"
			if !route.Destination.Addr().IsLoopback() {
				ipv6Available = true
			}
		}
		if route.InterfaceIndex != adapter.Index && windowsFullTunnelRoute(route, byIndex[route.InterfaceIndex]) {
			conflicts = append(conflicts, runtimeapi.NetworkConflict{
				Kind:   "full_tunnel",
				Owner:  byIndex[route.InterfaceIndex].Name,
				Detail: fmt.Sprintf("route %s is owned by another full-tunnel interface", route.Destination),
			})
		}
		if adapter.Index != 0 &&
			route.InterfaceIndex == adapter.Index &&
			route.Protocol == windows.MIB_IPPROTO_NETMGMT &&
			windowsCapturePrefix(route.Destination) {
			conflicts = append(conflicts, runtimeapi.NetworkConflict{
				Kind:   "tun_stale",
				Owner:  windowsTUNDevice,
				Detail: fmt.Sprintf("stale Runtime-style route %s remains on the fixed Wintun adapter", route.Destination),
			})
		}
		if route.Destination.Bits() == 0 || route.Destination.IsSingleIP() {
			continue
		}
		name := byIndex[route.InterfaceIndex].Name
		if name == "" {
			name = strconv.FormatUint(uint64(route.InterfaceIndex), 10)
		}
		discovered = append(discovered, runtimeapi.NetworkRoute{
			ID:        windowsRouteID(route),
			Family:    family,
			CIDR:      route.Destination.String(),
			Interface: name,
			Table:     "main",
			Source:    strconv.FormatUint(uint64(route.Protocol), 10),
		})
	}

	warnings := []string{
		"Windows 普通 TUN 仍处于预览状态；需要在真实 amd64 和 arm64 主机完成 Wintun、服务权限、崩溃恢复与卸载测试。",
	}
	if ipv6Available && settings.IPv6Policy == runtimeapi.TUNIPv6Direct {
		warnings = append(warnings, "IPv6 将在普通 TUN 运行期间直连，IPv6 流量不会经过代理。")
	}
	return Discovery{
		Device:        windowsTUNDevice,
		IPv6Available: ipv6Available,
		Routes:        uniqueNetworkRoutes(discovered),
		Conflicts:     uniqueNetworkConflicts(conflicts),
		Warnings:      warnings,
		Original:      original,
		PreviewOnly:   true,
	}, nil
}

func (system *WindowsSystem) DiscoverGateway(
	context.Context,
	runtimeapi.GatewaySettings,
) (Discovery, error) {
	return Discovery{}, errors.New("Runtime gateway mode is not available on Windows")
}

func (system *WindowsSystem) PrepareTUN(
	ctx context.Context,
	ownership Ownership,
) (SystemPreparation, error) {
	if err := system.validateOwnership(ownership); err != nil {
		return SystemPreparation{}, err
	}
	if err := contextError(ctx); err != nil {
		return SystemPreparation{}, err
	}
	index, err := windowsOwnershipInterfaceIndex(ownership)
	if err != nil {
		return SystemPreparation{}, err
	}
	interfaces, err := system.Interfaces()
	if err != nil {
		return SystemPreparation{}, err
	}
	var adapter windowsInterface
	for _, candidate := range interfaces {
		if candidate.Name == windowsTUNDevice {
			adapter = candidate
			break
		}
	}
	if adapter.Index == 0 {
		return SystemPreparation{}, errors.New("fixed SubmuxRuntime Wintun adapter is unavailable")
	}
	if adapter.Index != index || adapter.GUID != ownership.Original["interface_guid"] {
		return SystemPreparation{}, errors.New("fixed SubmuxRuntime Wintun adapter identity changed after preview")
	}
	metric, err := windowsOwnershipMetric(ownership.Token)
	if err != nil {
		return SystemPreparation{}, err
	}
	routes, err := system.API.Routes()
	if err != nil {
		return SystemPreparation{}, err
	}
	for _, route := range routes {
		if route.InterfaceIndex == index && route.Metric == metric &&
			route.Protocol == windows.MIB_IPPROTO_NETMGMT {
			return SystemPreparation{}, errors.New("stale Runtime-owned Windows routes require cleanup before takeover")
		}
	}
	return SystemPreparation{
		Objects: []runtimeapi.NetworkObject{{
			Kind:  "wintun_adapter",
			ID:    "wintun:" + windowsTUNDevice,
			Name:  windowsTUNDevice,
			State: runtimeapi.NetworkStatePrepared,
		}},
		RoutingMark: int(metric),
	}, nil
}

func (system *WindowsSystem) PrepareGateway(
	context.Context,
	Ownership,
) (SystemPreparation, error) {
	return SystemPreparation{}, errors.New("Runtime gateway mode is not available on Windows")
}

func (system *WindowsSystem) ApplyTUN(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := system.validatePreparedOwnership(ownership); err != nil {
		return nil, err
	}
	if len(ownership.Routes) > windowsMaxSelections {
		return nil, errors.New("ordinary TUN discovered too many Windows specific routes")
	}
	desired, err := windowsDesiredRoutes(ownership)
	if err != nil {
		return nil, err
	}
	existing, err := system.API.Routes()
	if err != nil {
		return nil, err
	}
	for _, candidate := range desired {
		for _, current := range existing {
			if windowsSameRouteKey(candidate, current) {
				return nil, fmt.Errorf("Windows route %s already exists on the Runtime adapter", candidate.Destination)
			}
		}
	}

	applied := make([]windowsRoute, 0, len(desired))
	objects := make([]runtimeapi.NetworkObject, 0, len(desired)+1)
	rollback := func() error {
		var rollbackErrors []error
		for index := len(applied) - 1; index >= 0; index-- {
			if err := system.API.DeleteRoute(applied[index]); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
		}
		return errors.Join(rollbackErrors...)
	}
	for _, route := range desired {
		if err := system.API.CreateRoute(route); err != nil {
			return objects, errors.Join(err, rollback())
		}
		applied = append(applied, route)
		objects = append(objects, windowsRouteObject(route, runtimeapi.NetworkStateActive))
	}
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Block {
		if err := system.addIPv6Block(ctx, ownership); err != nil {
			return objects, errors.Join(err, rollback())
		}
		objects = append(objects, windowsFirewallObject(ownership, runtimeapi.NetworkStateActive))
	}
	return mergeNetworkObjects(objects), nil
}

func (system *WindowsSystem) ApplyGateway(
	context.Context,
	Ownership,
) ([]runtimeapi.NetworkObject, error) {
	return nil, errors.New("Runtime gateway mode is not available on Windows")
}

func (system *WindowsSystem) Cleanup(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if ownership.Mode != runtimeapi.RunModeTUN {
		return nil, errors.New("Runtime gateway mode is not available on Windows")
	}
	if err := system.validatePreparedOwnership(ownership); err != nil {
		return nil, err
	}
	var cleanupErrors []error
	residuals := make([]runtimeapi.NetworkObject, 0)
	if hasNetworkObject(ownership.Objects, windowsFirewallObject(ownership, "").ID) {
		if err := system.deleteIPv6Block(ctx, ownership); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			residuals = append(
				residuals,
				windowsFirewallObject(ownership, runtimeapi.NetworkStateUnknown),
			)
		}
	}
	desired, err := windowsDesiredRoutes(ownership)
	if err != nil {
		return nil, err
	}
	current, err := system.API.Routes()
	if err != nil {
		return nil, err
	}
	for index := len(desired) - 1; index >= 0; index-- {
		route := desired[index]
		if !windowsContainsExactOwnedRoute(current, route) {
			continue
		}
		if err := system.API.DeleteRoute(route); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	routeResiduals, residualErr := system.residuals(ctx, ownership)
	residuals = mergeNetworkObjects(residuals, routeResiduals)
	return residuals, errors.Join(append(cleanupErrors, residualErr)...)
}

func (system *WindowsSystem) Observe(
	ctx context.Context,
	ownership *Ownership,
) (runtimeapi.NetworkStatus, error) {
	if err := system.validate(); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if ownership == nil {
		interfaces, err := system.Interfaces()
		status := runtimeapi.NetworkStatus{
			Mode:        runtimeapi.RunModeExplicit,
			State:       runtimeapi.NetworkStateInactive,
			PreviewOnly: true,
		}
		if err == nil {
			for _, candidate := range interfaces {
				if candidate.Name == windowsTUNDevice {
					status.Available = true
					break
				}
			}
		}
		return status, nil
	}
	if err := system.validatePreparedOwnership(*ownership); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	residuals, err := system.residuals(ctx, *ownership)
	status := runtimeapi.NetworkStatus{
		Available:      len(residuals) == 0,
		Mode:           ownership.Mode,
		State:          ownership.State,
		Device:         ownership.Device,
		Settings:       ownership.Settings,
		OwnershipID:    ownership.ID,
		Objects:        append([]runtimeapi.NetworkObject(nil), ownership.Objects...),
		Routes:         append([]runtimeapi.NetworkRoute(nil), ownership.Routes...),
		Residuals:      residuals,
		LeaseExpiresAt: &ownership.LeaseExpiresAt,
		ObservedAt:     systemTimeNow(),
		PreviewOnly:    true,
	}
	if len(residuals) > 0 {
		status.State = runtimeapi.NetworkStateUnknown
	}
	return status, err
}

func (system *WindowsSystem) residuals(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	current, err := system.API.Routes()
	if err != nil {
		return nil, err
	}
	desired, err := windowsDesiredRoutes(ownership)
	if err != nil {
		return nil, err
	}
	residuals := make([]runtimeapi.NetworkObject, 0)
	for _, route := range desired {
		if windowsContainsExactOwnedRoute(current, route) {
			residuals = append(residuals, windowsRouteObject(route, runtimeapi.NetworkStateUnknown))
		}
	}
	return residuals, nil
}

func (system *WindowsSystem) validate() error {
	if runtime.GOOS != "windows" {
		return errors.New("Windows ordinary TUN networking is only available on Windows")
	}
	if system == nil ||
		system.API == nil ||
		system.Runner == nil ||
		system.Interfaces == nil ||
		system.DNSServers == nil {
		return errors.New("Windows ordinary TUN system is unavailable")
	}
	return nil
}

func (system *WindowsSystem) validateOwnership(ownership Ownership) error {
	if err := system.validate(); err != nil {
		return err
	}
	if ownership.Mode != runtimeapi.RunModeTUN ||
		ownership.Device != windowsTUNDevice ||
		!validHex(ownership.Token, 64) {
		return errors.New("Windows ordinary TUN ownership is invalid")
	}
	_, err := windowsOwnershipInterfaceIndex(ownership)
	return err
}

func (system *WindowsSystem) validatePreparedOwnership(ownership Ownership) error {
	if err := system.validateOwnership(ownership); err != nil {
		return err
	}
	metric, err := windowsOwnershipMetric(ownership.Token)
	if err != nil {
		return err
	}
	if ownership.RoutingMark != int(metric) {
		return errors.New("Windows ordinary TUN ownership metric is invalid")
	}
	return nil
}

func windowsInterfaces() ([]windowsInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("read Windows interfaces: %w", err)
	}
	result := make([]windowsInterface, 0, len(interfaces))
	for _, item := range interfaces {
		row := windows.MibIfRow2{InterfaceIndex: uint32(item.Index)}
		if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row); err != nil {
			return nil, fmt.Errorf("read Windows interface %d: %w", item.Index, err)
		}
		result = append(result, windowsInterface{
			Index: uint32(item.Index),
			Name:  item.Name,
			Type:  row.Type,
			GUID:  row.InterfaceGuid.String(),
		})
	}
	return result, nil
}

func windowsOwnershipInterfaceIndex(ownership Ownership) (uint32, error) {
	value := strings.TrimSpace(ownership.Original["interface_index"])
	index, err := strconv.ParseUint(value, 10, 32)
	if err != nil || index == 0 {
		return 0, errors.New("Windows ordinary TUN interface identity is invalid")
	}
	if strings.TrimSpace(ownership.Original["interface_guid"]) == "" {
		return 0, errors.New("Windows ordinary TUN interface GUID is invalid")
	}
	return uint32(index), nil
}

func windowsOwnershipMetric(token string) (uint32, error) {
	if !validHex(token, 64) {
		return 0, errors.New("Windows ordinary TUN ownership token is invalid")
	}
	digest := sha256.Sum256([]byte(token))
	return windowsOwnedRouteMetric +
		uint32(binary.BigEndian.Uint16(digest[:2]))%windowsRouteMetricSpan, nil
}

func windowsDesiredRoutes(ownership Ownership) ([]windowsRoute, error) {
	index, err := windowsOwnershipInterfaceIndex(ownership)
	if err != nil {
		return nil, err
	}
	metric, err := windowsOwnershipMetric(ownership.Token)
	if err != nil {
		return nil, err
	}
	prefixes := append([]netip.Prefix(nil), windowsCapturePrefixes...)
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Proxy {
		prefixes = append(prefixes, windowsIPv6CapturePrefixes...)
	}
	if ownership.Settings.DNSPolicy == runtimeapi.TUNDNSHijack {
		dnsServers, err := parseWindowsAddresses(ownership.Original["dns_servers"])
		if err != nil {
			return nil, err
		}
		for _, address := range dnsServers {
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
		}
	}
	for _, route := range ownership.Routes {
		if route.Bypass {
			continue
		}
		prefix, err := netip.ParsePrefix(route.CIDR)
		if err != nil {
			return nil, errors.New("Windows ordinary TUN selected route is invalid")
		}
		if prefix.Addr().Is6() && ownership.Settings.IPv6Policy != runtimeapi.TUNIPv6Proxy {
			continue
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	prefixes = uniquePrefixes(prefixes)
	routes := make([]windowsRoute, 0, len(prefixes))
	for _, prefix := range prefixes {
		nextHop := netip.IPv4Unspecified()
		if prefix.Addr().Is6() {
			nextHop = netip.IPv6Unspecified()
		}
		routes = append(routes, windowsRoute{
			InterfaceIndex: index,
			Destination:    prefix,
			NextHop:        nextHop,
			Metric:         metric,
			Protocol:       windows.MIB_IPPROTO_NETMGMT,
		})
	}
	sort.Slice(routes, func(left, right int) bool {
		return routes[left].Destination.String() < routes[right].Destination.String()
	})
	return routes, nil
}

func windowsFullTunnelRoute(route windowsRoute, iface windowsInterface) bool {
	bits := route.Destination.Bits()
	if bits == 1 {
		return true
	}
	if bits != 0 {
		return false
	}
	const (
		ifTypePPP    = 23
		ifTypeTunnel = 131
	)
	return iface.Type == ifTypePPP || iface.Type == ifTypeTunnel
}

func windowsCapturePrefix(prefix netip.Prefix) bool {
	for _, candidate := range windowsCapturePrefixes {
		if prefix == candidate {
			return true
		}
	}
	for _, candidate := range windowsIPv6CapturePrefixes {
		if prefix == candidate {
			return true
		}
	}
	return false
}

func windowsRouteID(route windowsRoute) string {
	value := fmt.Sprintf(
		"%s\x00%d\x00%d",
		route.Destination,
		route.InterfaceIndex,
		route.Protocol,
	)
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("win_%x", digest[:12])
}

func windowsSameRouteKey(left, right windowsRoute) bool {
	return left.InterfaceIndex == right.InterfaceIndex &&
		left.Destination == right.Destination &&
		left.NextHop == right.NextHop
}

func windowsSameOwnedRoute(left, right windowsRoute) bool {
	return windowsSameRouteKey(left, right) &&
		left.Metric == right.Metric &&
		left.Protocol == right.Protocol
}

func windowsContainsExactOwnedRoute(routes []windowsRoute, expected windowsRoute) bool {
	for _, route := range routes {
		if windowsSameOwnedRoute(route, expected) {
			return true
		}
	}
	return false
}

func windowsRouteObject(route windowsRoute, state string) runtimeapi.NetworkObject {
	name := fmt.Sprintf("%s@if%d", route.Destination, route.InterfaceIndex)
	return runtimeapi.NetworkObject{
		Kind:  "route",
		ID:    fmt.Sprintf("winroute:%d:%s:%d", route.InterfaceIndex, route.Destination, route.Metric),
		Name:  name,
		State: state,
	}
}

func windowsFirewallRuleName(ownership Ownership) string {
	return "SubmuxRuntime-" + ownership.Token[:16]
}

func windowsFirewallObject(ownership Ownership, state string) runtimeapi.NetworkObject {
	name := windowsFirewallRuleName(ownership)
	return runtimeapi.NetworkObject{
		Kind:  "firewall_rule",
		ID:    "winfirewall:" + name,
		Name:  name,
		State: state,
	}
}

func (system *WindowsSystem) addIPv6Block(ctx context.Context, ownership Ownership) error {
	name := windowsFirewallRuleName(ownership)
	_, err := system.Runner.Run(
		ctx,
		nil,
		"netsh",
		"advfirewall", "firewall", "add", "rule",
		"name="+name,
		"dir=out",
		"action=block",
		"protocol=any",
		"remoteip=::/0",
	)
	if err != nil {
		return fmt.Errorf("add owned Windows IPv6 block rule: %w", err)
	}
	return nil
}

func (system *WindowsSystem) deleteIPv6Block(ctx context.Context, ownership Ownership) error {
	name := windowsFirewallRuleName(ownership)
	_, err := system.Runner.Run(
		ctx,
		nil,
		"netsh",
		"advfirewall", "firewall", "delete", "rule",
		"name="+name,
	)
	if err != nil {
		return fmt.Errorf("delete owned Windows IPv6 block rule: %w", err)
	}
	return nil
}

func uniquePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	seen := make(map[netip.Prefix]struct{}, len(prefixes))
	result := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefix = prefix.Masked()
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}
	return result
}

func systemTimeNow() time.Time {
	return time.Now().UTC()
}

func joinWindowsAddresses(addresses []netip.Addr) string {
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.String())
	}
	return strings.Join(values, ",")
}

func parseWindowsAddresses(value string) ([]netip.Addr, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	items := strings.Split(value, ",")
	if len(items) > windowsDNSServerLimit {
		return nil, errors.New("Windows DNS server snapshot exceeds the Runtime limit")
	}
	addresses := make([]netip.Addr, 0, len(items))
	for _, item := range items {
		address, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil ||
			address.IsUnspecified() ||
			address.IsLoopback() ||
			address.IsMulticast() {
			return nil, errors.New("Windows DNS server snapshot is invalid")
		}
		addresses = append(addresses, address.Unmap())
	}
	return addresses, nil
}
