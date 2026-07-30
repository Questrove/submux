//go:build darwin

package runtimenet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"submux/internal/runtimeapi"
)

type fakeDarwinPlatform struct {
	interfaces []string
	routes     []darwinRoute
	dns        []netip.Addr
	added      []darwinRoute
	deleted    []darwinRoute
}

func (platform *fakeDarwinPlatform) Interfaces(context.Context) ([]string, error) {
	return append([]string(nil), platform.interfaces...), nil
}

func (platform *fakeDarwinPlatform) Routes(context.Context) ([]darwinRoute, error) {
	return append([]darwinRoute(nil), platform.routes...), nil
}

func (platform *fakeDarwinPlatform) DNSServers(context.Context) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), platform.dns...), nil
}

func (platform *fakeDarwinPlatform) AddRoute(_ context.Context, route darwinRoute) error {
	platform.added = append(platform.added, route)
	platform.routes = append(platform.routes, route)
	return nil
}

func (platform *fakeDarwinPlatform) DeleteRoute(_ context.Context, route darwinRoute) error {
	for index, existing := range platform.routes {
		if !darwinContainsRoute([]darwinRoute{existing}, route) {
			continue
		}
		platform.deleted = append(platform.deleted, route)
		platform.routes = append(platform.routes[:index], platform.routes[index+1:]...)
		return nil
	}
	return errors.New("route not found")
}

type fakeDarwinRunner struct{}

func (fakeDarwinRunner) Run(
	context.Context,
	[]byte,
	string,
	...string,
) ([]byte, error) {
	return nil, nil
}

func TestParseDarwinNetstatUsesNetifAndPreservesScopedPrefix(t *testing.T) {
	body := `Routing tables

Internet6:
Destination                             Gateway                         Flags         Netif Expire
default                                 fe80::1%en0                     UGcg          en0
fe80::%en0/64                           link#4                          UcI           en0  1199
2001:db8::/32                           link#20                         UCS           utun7
`
	routes, err := parseDarwinNetstat(body, "inet6")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("parsed Darwin routes=%#v", routes)
	}
	if routes[1].Destination.String() != "fe80::/64" || routes[1].Interface != "en0" {
		t.Fatalf("scoped Darwin route=%#v", routes[1])
	}
	if routes[2].Interface != "utun7" {
		t.Fatalf("Darwin Netif column=%#v", routes[2])
	}
}

func TestParseDarwinDestinationExpandsAbbreviatedIPv4(t *testing.T) {
	for input, expected := range map[string]string{
		"127":          "127.0.0.0/8",
		"192.168.1":    "192.168.1.0/24",
		"10/8":         "10.0.0.0/8",
		"224.0.0/4":    "224.0.0.0/4",
		"192.0.2.1":    "192.0.2.1/32",
		"192.0.2.0/24": "192.0.2.0/24",
	} {
		prefix, err := parseDarwinDestination(input, "inet")
		if err != nil || prefix.String() != expected {
			t.Fatalf("parse macOS destination %q=%s err=%v; want %s", input, prefix, err, expected)
		}
	}
}

func TestDarwinRouteArgumentsRestoreGatewayRoute(t *testing.T) {
	arguments, err := darwinRouteArguments("add", darwinRoute{
		Destination: netip.MustParsePrefix("10.0.0.0/8"),
		Gateway:     "192.0.2.1",
		Interface:   "en0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(arguments, "-interface") || arguments[len(arguments)-1] != "192.0.2.1" {
		t.Fatalf("Darwin gateway route arguments=%v", arguments)
	}
}

func TestDarwinApplyAndCleanupRestoreSelectedRoute(t *testing.T) {
	original := darwinRoute{
		Destination: netip.MustParsePrefix("10.0.0.0/8"),
		Gateway:     "192.0.2.1",
		Interface:   "en0",
	}
	platform := &fakeDarwinPlatform{
		interfaces: []string{"en0", "utun2", "utun7"},
		routes:     []darwinRoute{original},
	}
	system := testDarwinSystem(t, platform)
	ownership := testDarwinOwnership(t)
	routeID := darwinRouteID(original)
	ownership.Routes = []runtimeapi.NetworkRoute{{
		ID:        routeID,
		Family:    "ipv4",
		CIDR:      original.Destination.String(),
		Interface: original.Interface,
		Table:     "main",
	}}
	ownership.Original["route:"+routeID] = darwinRouteSnapshot(original)

	objects, err := system.ApplyTUN(context.Background(), ownership)
	if err != nil {
		t.Fatalf("apply macOS TUN: %v", err)
	}
	if darwinContainsRoute(platform.routes, original) {
		t.Fatalf("selected original route was not replaced: %#v", platform.routes)
	}
	if len(objects) != 6 || darwinOwnedDevice(objects) != "utun7" {
		t.Fatalf("macOS owned objects=%#v", objects)
	}
	ownership.Objects = append(ownership.Objects, objects...)
	residuals, err := system.Cleanup(context.Background(), ownership)
	if err != nil {
		t.Fatalf("cleanup macOS TUN: %v", err)
	}
	if len(residuals) != 0 || !darwinContainsRoute(platform.routes, original) {
		t.Fatalf("cleanup residuals=%#v routes=%#v", residuals, platform.routes)
	}
	if len(platform.routes) != 1 {
		t.Fatalf("cleanup changed foreign routes=%#v", platform.routes)
	}
}

func TestStageDarwinCoreObjectLocksVerifiedDigest(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source", "mihomo")
	execution := filepath.Join(root, "execution")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(execution, 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte("verified macOS core")
	if err := os.WriteFile(source, body, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	expected := hex.EncodeToString(digest[:])
	path, err := stageDarwinCoreObject(
		context.Background(),
		source,
		execution,
		"mihomo-",
		expected,
		0700,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, expected) {
		t.Fatalf("staged core path=%q", path)
	}
	if err := validateDarwinStagedObject(path, expected, 0700, true); err != nil {
		t.Fatalf("validate staged core: %v", err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinStagedObject(path, expected, 0700, true); err == nil {
		t.Fatal("changed staged core passed digest validation")
	}
}

func testDarwinSystem(t *testing.T, platform DarwinPlatform) *DarwinSystem {
	t.Helper()
	root := t.TempDir()
	return &DarwinSystem{
		RuntimeRoot:       root,
		RuntimeUID:        501,
		RuntimeGID:        20,
		ControlEndpoint:   filepath.Join(root, "control.sock"),
		Platform:          platform,
		Runner:            fakeDarwinRunner{},
		coreExecutionRoot: filepath.Join(root, "execution"),
		allowTestPaths:    true,
	}
}

func testDarwinOwnership(t *testing.T) Ownership {
	t.Helper()
	token := strings.Repeat("a", 64)
	metric, err := darwinOwnershipMetric(token)
	if err != nil {
		t.Fatal(err)
	}
	return Ownership{
		ID:            "ownership_darwin_test",
		Token:         token,
		Mode:          runtimeapi.RunModeTUN,
		Device:        darwinTUNDevice,
		IPv6Available: true,
		RoutingMark:   int(metric),
		Settings: runtimeapi.TUNSettings{
			IPv6Policy: runtimeapi.TUNIPv6Proxy,
			DNSPolicy:  runtimeapi.TUNDNSOff,
		},
		Original: map[string]string{
			"utun_interfaces": "utun2",
			"dns_servers":     "",
		},
		Objects: []runtimeapi.NetworkObject{{
			Kind:  "utun_plan",
			ID:    "utun:pending",
			Name:  "utun",
			State: runtimeapi.NetworkStatePrepared,
		}},
		State: runtimeapi.NetworkStatePrepared,
	}
}
