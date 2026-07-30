//go:build darwin

package runtimenet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"submux/internal/runtimeapi"
)

const (
	darwinTUNDevice       = "utun"
	darwinMaxRoutes       = 4096
	darwinMaxSelections   = 512
	darwinOwnedMetricBase = 40000
	darwinRuntimeRoot     = "/Library/Application Support/SubmuxRuntime"
	darwinControlSocket   = "/var/run/submux-runtime-privileged/mihomo.sock"
)

var darwinIPv4CapturePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/1"),
	netip.MustParsePrefix("128.0.0.0/1"),
}

var darwinIPv6CapturePrefixes = []netip.Prefix{
	netip.MustParsePrefix("::/1"),
	netip.MustParsePrefix("8000::/1"),
}

type darwinRoute struct {
	Destination netip.Prefix
	Gateway     string
	Interface   string
	Blackhole   bool
}

type DarwinPlatform interface {
	Interfaces(context.Context) ([]string, error)
	Routes(context.Context) ([]darwinRoute, error)
	DNSServers(context.Context) ([]netip.Addr, error)
	AddRoute(context.Context, darwinRoute) error
	DeleteRoute(context.Context, darwinRoute) error
}

type DarwinSystem struct {
	RuntimeRoot     string
	RuntimeUID      uint32
	RuntimeGID      uint32
	ControlEndpoint string
	Platform        DarwinPlatform
	Runner          CommandRunner

	core darwinCoreState

	coreExecutionRoot string
	allowTestPaths    bool
}

type commandDarwinPlatform struct {
	Runner CommandRunner
}

func NewDarwinSystem(
	runtimeRoot string,
	runtimeUID uint32,
	runtimeGID uint32,
	controlEndpoint string,
) (*DarwinSystem, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("macOS ordinary TUN networking is only available on macOS")
	}
	if runtimeUID == 0 || runtimeGID == 0 || controlEndpoint == "" {
		return nil, errors.New("macOS ordinary TUN requires a low-privilege Runtime identity and fixed control Socket")
	}
	if filepath.Clean(runtimeRoot) != darwinRuntimeRoot ||
		filepath.Clean(controlEndpoint) != darwinControlSocket {
		return nil, errors.New("macOS ordinary TUN requires the fixed Runtime object paths")
	}
	runner := execCommandRunner{}
	return &DarwinSystem{
		RuntimeRoot:     runtimeRoot,
		RuntimeUID:      runtimeUID,
		RuntimeGID:      runtimeGID,
		ControlEndpoint: controlEndpoint,
		Platform:        commandDarwinPlatform{Runner: runner},
		Runner:          runner,
	}, nil
}

func (system *DarwinSystem) Discover(
	ctx context.Context,
	settings runtimeapi.TUNSettings,
) (Discovery, error) {
	if err := system.validate(); err != nil {
		return Discovery{}, err
	}
	interfaces, err := system.Platform.Interfaces(ctx)
	if err != nil {
		return Discovery{}, err
	}
	routes, err := system.Platform.Routes(ctx)
	if err != nil {
		return Discovery{}, err
	}
	if len(routes) > darwinMaxRoutes {
		return Discovery{}, errors.New("macOS route table exceeds the Runtime inspection limit")
	}
	dnsServers, err := system.Platform.DNSServers(ctx)
	if err != nil {
		return Discovery{}, err
	}
	baseline := darwinUtunInterfaces(interfaces)
	original := map[string]string{
		"utun_interfaces": strings.Join(baseline, ","),
		"dns_servers":     joinDarwinAddresses(dnsServers),
	}
	conflicts := make([]runtimeapi.NetworkConflict, 0)
	discovered := make([]runtimeapi.NetworkRoute, 0, len(routes))
	ipv6Available := false
	for _, route := range routes {
		if route.Destination.Addr().Is6() {
			ipv6Available = true
		}
		if darwinFullTunnelRoute(route) {
			conflicts = append(conflicts, runtimeapi.NetworkConflict{
				Kind:   "full_tunnel",
				Owner:  route.Interface,
				Detail: fmt.Sprintf("route %s is owned by another macOS tunnel", route.Destination),
			})
		}
		if route.Destination.Bits() == 0 || route.Destination.IsSingleIP() {
			continue
		}
		family := "ipv4"
		if route.Destination.Addr().Is6() {
			family = "ipv6"
		}
		id := darwinRouteID(route)
		discovered = append(discovered, runtimeapi.NetworkRoute{
			ID:        id,
			Family:    family,
			CIDR:      route.Destination.String(),
			Interface: route.Interface,
			Table:     "main",
			Source:    "route_socket",
		})
		original["route:"+id] = darwinRouteSnapshot(route)
	}
	warnings := []string{
		"macOS 普通 TUN 仍处于预览状态；需要在 macOS 13 及以上的 Intel 和 Apple Silicon 主机完成权限、流量、睡眠唤醒和卸载验收。",
	}
	if ipv6Available && settings.IPv6Policy == runtimeapi.TUNIPv6Direct {
		warnings = append(warnings, "IPv6 将在普通 TUN 运行期间直连，IPv6 流量不会经过代理。")
	}
	return Discovery{
		Device:        darwinTUNDevice,
		IPv6Available: ipv6Available,
		Routes:        uniqueNetworkRoutes(discovered),
		Conflicts:     uniqueNetworkConflicts(conflicts),
		Warnings:      warnings,
		Original:      original,
		PreviewOnly:   true,
	}, nil
}

func (system *DarwinSystem) DiscoverGateway(
	context.Context,
	runtimeapi.GatewaySettings,
) (Discovery, error) {
	return Discovery{}, errors.New("Runtime gateway mode is not available on macOS")
}

func (system *DarwinSystem) PrepareTUN(
	ctx context.Context,
	ownership Ownership,
) (SystemPreparation, error) {
	if err := system.validateOwnership(ownership); err != nil {
		return SystemPreparation{}, err
	}
	interfaces, err := system.Platform.Interfaces(ctx)
	if err != nil {
		return SystemPreparation{}, err
	}
	current := darwinUtunInterfaces(interfaces)
	baseline := splitDarwinValues(ownership.Original["utun_interfaces"])
	if !equalStrings(current, baseline) {
		return SystemPreparation{}, errors.New("macOS utun interfaces changed after preview")
	}
	metric, err := darwinOwnershipMetric(ownership.Token)
	if err != nil {
		return SystemPreparation{}, err
	}
	return SystemPreparation{
		Objects: []runtimeapi.NetworkObject{{
			Kind:  "utun_plan",
			ID:    "utun:pending",
			Name:  "utun",
			State: runtimeapi.NetworkStatePrepared,
		}},
		RoutingMark: int(metric),
	}, nil
}

func (system *DarwinSystem) PrepareGateway(
	context.Context,
	Ownership,
) (SystemPreparation, error) {
	return SystemPreparation{}, errors.New("Runtime gateway mode is not available on macOS")
}

func (system *DarwinSystem) ApplyTUN(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := system.validatePreparedOwnership(ownership); err != nil {
		return nil, err
	}
	if len(ownership.Routes) > darwinMaxSelections {
		return nil, errors.New("ordinary TUN discovered too many macOS specific routes")
	}
	device, err := system.newUtunDevice(ctx, ownership)
	if err != nil {
		return nil, err
	}
	desired, replacements, err := darwinDesiredRoutes(ownership, device)
	if err != nil {
		return nil, err
	}
	current, err := system.Platform.Routes(ctx)
	if err != nil {
		return nil, err
	}
	for _, route := range desired {
		if _, replacement := replacements[route.Destination]; replacement {
			continue
		}
		if darwinHasDestination(current, route.Destination) {
			return nil, fmt.Errorf("macOS route %s already exists", route.Destination)
		}
	}

	applied := make([]darwinRoute, 0, len(desired))
	removed := make([]darwinRoute, 0, len(replacements))
	rollback := func() error {
		var rollbackErrors []error
		for index := len(applied) - 1; index >= 0; index-- {
			if err := system.Platform.DeleteRoute(context.Background(), applied[index]); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
		}
		for index := len(removed) - 1; index >= 0; index-- {
			if err := system.Platform.AddRoute(context.Background(), removed[index]); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
		}
		return errors.Join(rollbackErrors...)
	}
	objects := []runtimeapi.NetworkObject{{
		Kind:  "utun",
		ID:    "utun:" + device,
		Name:  device,
		State: runtimeapi.NetworkStateActive,
	}}
	for _, route := range desired {
		if original, ok := replacements[route.Destination]; ok {
			if err := system.Platform.DeleteRoute(ctx, original); err != nil {
				return objects, errors.Join(err, rollback())
			}
			removed = append(removed, original)
		}
		if err := system.Platform.AddRoute(ctx, route); err != nil {
			return objects, errors.Join(err, rollback())
		}
		applied = append(applied, route)
		objects = append(objects, darwinRouteObject(route, runtimeapi.NetworkStateActive))
	}
	return mergeNetworkObjects(objects), nil
}

func (system *DarwinSystem) ApplyGateway(
	context.Context,
	Ownership,
) ([]runtimeapi.NetworkObject, error) {
	return nil, errors.New("Runtime gateway mode is not available on macOS")
}

func (system *DarwinSystem) Cleanup(
	ctx context.Context,
	ownership Ownership,
) ([]runtimeapi.NetworkObject, error) {
	if err := system.validatePreparedOwnership(ownership); err != nil {
		return nil, err
	}
	device := darwinOwnedDevice(ownership.Objects)
	if device == "" {
		return nil, nil
	}
	desired, replacements, err := darwinDesiredRoutes(ownership, device)
	if err != nil {
		return nil, err
	}
	current, err := system.Platform.Routes(ctx)
	if err != nil {
		return nil, err
	}
	var cleanupErrors []error
	for index := len(desired) - 1; index >= 0; index-- {
		route := desired[index]
		if darwinContainsRoute(current, route) {
			if err := system.Platform.DeleteRoute(ctx, route); err != nil {
				cleanupErrors = append(cleanupErrors, err)
				continue
			}
		}
		if original, ok := replacements[route.Destination]; ok {
			after, routeErr := system.Platform.Routes(ctx)
			if routeErr != nil {
				cleanupErrors = append(cleanupErrors, routeErr)
				continue
			}
			if darwinHasDestination(after, route.Destination) {
				cleanupErrors = append(
					cleanupErrors,
					fmt.Errorf("macOS route %s changed externally; original route was not restored", route.Destination),
				)
				continue
			}
			if err := system.Platform.AddRoute(ctx, original); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}
	residuals, residualErr := system.residuals(ctx, ownership, device)
	return residuals, errors.Join(append(cleanupErrors, residualErr)...)
}

func (system *DarwinSystem) Observe(
	ctx context.Context,
	ownership *Ownership,
) (runtimeapi.NetworkStatus, error) {
	if err := system.validate(); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	if ownership == nil {
		return runtimeapi.NetworkStatus{
			Available:   true,
			Mode:        runtimeapi.RunModeExplicit,
			State:       runtimeapi.NetworkStateInactive,
			ObservedAt:  time.Now().UTC(),
			PreviewOnly: true,
		}, nil
	}
	if err := system.validatePreparedOwnership(*ownership); err != nil {
		return runtimeapi.NetworkStatus{}, err
	}
	device := darwinOwnedDevice(ownership.Objects)
	residuals, err := system.residuals(ctx, *ownership, device)
	status := runtimeapi.NetworkStatus{
		Available:      len(residuals) == 0,
		Mode:           ownership.Mode,
		State:          ownership.State,
		Device:         device,
		Settings:       ownership.Settings,
		OwnershipID:    ownership.ID,
		Objects:        append([]runtimeapi.NetworkObject(nil), ownership.Objects...),
		Routes:         append([]runtimeapi.NetworkRoute(nil), ownership.Routes...),
		Residuals:      residuals,
		LeaseExpiresAt: &ownership.LeaseExpiresAt,
		ObservedAt:     time.Now().UTC(),
		PreviewOnly:    true,
	}
	if len(residuals) > 0 {
		status.State = runtimeapi.NetworkStateUnknown
	}
	return status, err
}

func (system *DarwinSystem) residuals(
	ctx context.Context,
	ownership Ownership,
	device string,
) ([]runtimeapi.NetworkObject, error) {
	if device == "" {
		return nil, nil
	}
	current, err := system.Platform.Routes(ctx)
	if err != nil {
		return nil, err
	}
	desired, _, err := darwinDesiredRoutes(ownership, device)
	if err != nil {
		return nil, err
	}
	residuals := make([]runtimeapi.NetworkObject, 0)
	for _, route := range desired {
		if darwinContainsRoute(current, route) {
			residuals = append(residuals, darwinRouteObject(route, runtimeapi.NetworkStateUnknown))
		}
	}
	return residuals, nil
}

func (system *DarwinSystem) newUtunDevice(
	ctx context.Context,
	ownership Ownership,
) (string, error) {
	interfaces, err := system.Platform.Interfaces(ctx)
	if err != nil {
		return "", err
	}
	current := darwinUtunInterfaces(interfaces)
	baseline := make(map[string]struct{})
	for _, name := range splitDarwinValues(ownership.Original["utun_interfaces"]) {
		baseline[name] = struct{}{}
	}
	var created []string
	for _, name := range current {
		if _, exists := baseline[name]; !exists {
			created = append(created, name)
		}
	}
	if len(created) != 1 {
		return "", fmt.Errorf("expected one new Mihomo utun interface, found %d", len(created))
	}
	return created[0], nil
}

func (system *DarwinSystem) validate() error {
	if runtime.GOOS != "darwin" {
		return errors.New("macOS ordinary TUN networking is only available on macOS")
	}
	if system == nil ||
		system.RuntimeRoot == "" ||
		system.RuntimeUID == 0 ||
		system.RuntimeGID == 0 ||
		system.ControlEndpoint == "" ||
		system.Platform == nil ||
		system.Runner == nil {
		return errors.New("macOS ordinary TUN system is unavailable")
	}
	return nil
}

func (system *DarwinSystem) validateOwnership(ownership Ownership) error {
	if err := system.validate(); err != nil {
		return err
	}
	if ownership.Mode != runtimeapi.RunModeTUN ||
		ownership.Device != darwinTUNDevice ||
		!validHex(ownership.Token, 64) {
		return errors.New("macOS ordinary TUN ownership is invalid")
	}
	return nil
}

func (system *DarwinSystem) validatePreparedOwnership(ownership Ownership) error {
	if err := system.validateOwnership(ownership); err != nil {
		return err
	}
	metric, err := darwinOwnershipMetric(ownership.Token)
	if err != nil {
		return err
	}
	if ownership.RoutingMark != int(metric) {
		return errors.New("macOS ordinary TUN ownership marker is invalid")
	}
	return nil
}

func darwinOwnershipMetric(token string) (uint32, error) {
	if !validHex(token, 64) {
		return 0, errors.New("macOS ordinary TUN ownership token is invalid")
	}
	digest := sha256.Sum256([]byte(token))
	return darwinOwnedMetricBase + uint32(binary.BigEndian.Uint16(digest[:2]))%20000, nil
}

func darwinDesiredRoutes(
	ownership Ownership,
	device string,
) ([]darwinRoute, map[netip.Prefix]darwinRoute, error) {
	prefixes := append([]netip.Prefix(nil), darwinIPv4CapturePrefixes...)
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Proxy {
		prefixes = append(prefixes, darwinIPv6CapturePrefixes...)
	}
	if ownership.Settings.DNSPolicy == runtimeapi.TUNDNSHijack {
		addresses, err := parseDarwinAddresses(ownership.Original["dns_servers"])
		if err != nil {
			return nil, nil, err
		}
		for _, address := range addresses {
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
		}
	}
	replacements := make(map[netip.Prefix]darwinRoute)
	for _, route := range ownership.Routes {
		if route.Bypass {
			continue
		}
		prefix, err := netip.ParsePrefix(route.CIDR)
		if err != nil {
			return nil, nil, errors.New("macOS ordinary TUN selected route is invalid")
		}
		if prefix.Addr().Is6() && ownership.Settings.IPv6Policy != runtimeapi.TUNIPv6Proxy {
			continue
		}
		snapshot, err := parseDarwinRouteSnapshot(ownership.Original["route:"+route.ID])
		if err != nil || snapshot.Destination != prefix.Masked() {
			return nil, nil, errors.New("macOS ordinary TUN selected route snapshot is invalid")
		}
		prefixes = append(prefixes, prefix.Masked())
		replacements[prefix.Masked()] = snapshot
	}
	prefixes = uniqueDarwinPrefixes(prefixes)
	routes := make([]darwinRoute, 0, len(prefixes))
	for _, prefix := range prefixes {
		blackhole := ownership.IPv6Available &&
			ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Block &&
			prefix.Addr().Is6()
		routes = append(routes, darwinRoute{
			Destination: prefix,
			Interface:   device,
			Blackhole:   blackhole,
		})
	}
	if ownership.IPv6Available && ownership.Settings.IPv6Policy == runtimeapi.TUNIPv6Block {
		for _, prefix := range darwinIPv6CapturePrefixes {
			routes = append(routes, darwinRoute{
				Destination: prefix,
				Gateway:     "::1",
				Blackhole:   true,
			})
		}
	}
	routes = uniqueDarwinRoutes(routes)
	sort.Slice(routes, func(left, right int) bool {
		return routes[left].Destination.String() < routes[right].Destination.String()
	})
	return routes, replacements, nil
}

func darwinUtunInterfaces(interfaces []string) []string {
	result := make([]string, 0)
	for _, name := range interfaces {
		if strings.HasPrefix(name, "utun") {
			if _, err := strconv.ParseUint(strings.TrimPrefix(name, "utun"), 10, 32); err == nil {
				result = append(result, name)
			}
		}
	}
	sort.Strings(result)
	return result
}

func darwinFullTunnelRoute(route darwinRoute) bool {
	if !strings.HasPrefix(route.Interface, "utun") {
		return false
	}
	return route.Destination.Bits() == 0 || route.Destination.Bits() == 1
}

func darwinRouteID(route darwinRoute) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		route.Destination.String(),
		route.Gateway,
		route.Interface,
	}, "\x00")))
	return fmt.Sprintf("mac_%x", digest[:12])
}

func darwinRouteSnapshot(route darwinRoute) string {
	return strings.Join([]string{
		route.Destination.String(),
		route.Gateway,
		route.Interface,
		strconv.FormatBool(route.Blackhole),
	}, "|")
}

func parseDarwinRouteSnapshot(value string) (darwinRoute, error) {
	fields := strings.Split(value, "|")
	if len(fields) != 4 {
		return darwinRoute{}, errors.New("macOS route snapshot is invalid")
	}
	prefix, err := netip.ParsePrefix(fields[0])
	if err != nil || !validDarwinInterface(fields[2]) {
		return darwinRoute{}, errors.New("macOS route snapshot is invalid")
	}
	blackhole, err := strconv.ParseBool(fields[3])
	if err != nil || strings.ContainsAny(fields[1], " \t\r\n|") {
		return darwinRoute{}, errors.New("macOS route snapshot is invalid")
	}
	return darwinRoute{
		Destination: prefix.Masked(),
		Gateway:     fields[1],
		Interface:   fields[2],
		Blackhole:   blackhole,
	}, nil
}

func darwinRouteObject(route darwinRoute, state string) runtimeapi.NetworkObject {
	name := route.Destination.String()
	if route.Interface != "" {
		name += "@" + route.Interface
	}
	return runtimeapi.NetworkObject{
		Kind:  "route",
		ID:    "darwinroute:" + name,
		Name:  name,
		State: state,
	}
}

func darwinOwnedDevice(objects []runtimeapi.NetworkObject) string {
	for _, object := range objects {
		if object.Kind == "utun" &&
			strings.HasPrefix(object.ID, "utun:") &&
			validDarwinInterface(object.Name) &&
			strings.HasPrefix(object.Name, "utun") {
			return object.Name
		}
	}
	return ""
}

func darwinContainsRoute(routes []darwinRoute, expected darwinRoute) bool {
	for _, route := range routes {
		if route.Destination == expected.Destination &&
			route.Gateway == expected.Gateway &&
			route.Interface == expected.Interface &&
			route.Blackhole == expected.Blackhole {
			return true
		}
	}
	return false
}

func darwinHasDestination(routes []darwinRoute, destination netip.Prefix) bool {
	for _, route := range routes {
		if route.Destination == destination {
			return true
		}
	}
	return false
}

func uniqueDarwinRoutes(routes []darwinRoute) []darwinRoute {
	seen := make(map[string]struct{}, len(routes))
	result := make([]darwinRoute, 0, len(routes))
	for _, route := range routes {
		key := darwinRouteSnapshot(route)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, route)
	}
	return result
}

func uniqueDarwinPrefixes(prefixes []netip.Prefix) []netip.Prefix {
	seen := make(map[netip.Prefix]struct{}, len(prefixes))
	result := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefix = prefix.Masked()
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func splitDarwinValues(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	result := strings.Split(value, ",")
	sort.Strings(result)
	return result
}

func joinDarwinAddresses(addresses []netip.Addr) string {
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.String())
	}
	return strings.Join(values, ",")
}

func parseDarwinAddresses(value string) ([]netip.Addr, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	items := strings.Split(value, ",")
	if len(items) > 64 {
		return nil, errors.New("macOS DNS server snapshot exceeds the Runtime limit")
	}
	result := make([]netip.Addr, 0, len(items))
	for _, item := range items {
		address, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil || address.IsUnspecified() || address.IsMulticast() {
			return nil, errors.New("macOS DNS server snapshot is invalid")
		}
		result = append(result, address.Unmap())
	}
	return result, nil
}

func validDarwinInterface(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' ||
			character == '-' {
			continue
		}
		return false
	}
	return true
}
