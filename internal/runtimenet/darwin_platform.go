//go:build darwin

package runtimenet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

func (platform commandDarwinPlatform) Interfaces(ctx context.Context) ([]string, error) {
	output, err := platform.Runner.Run(ctx, nil, "/sbin/ifconfig", "-l")
	if err != nil {
		return nil, err
	}
	interfaces := strings.Fields(string(output))
	for _, name := range interfaces {
		if !validDarwinInterface(name) {
			return nil, errors.New("macOS returned an invalid interface name")
		}
	}
	return interfaces, nil
}

func (platform commandDarwinPlatform) Routes(ctx context.Context) ([]darwinRoute, error) {
	var result []darwinRoute
	for _, family := range []string{"inet", "inet6"} {
		output, err := platform.Runner.Run(ctx, nil, "/usr/sbin/netstat", "-rn", "-f", family)
		if err != nil {
			return nil, err
		}
		routes, err := parseDarwinNetstat(string(output), family)
		if err != nil {
			return nil, err
		}
		result = append(result, routes...)
	}
	return result, nil
}

func (platform commandDarwinPlatform) DNSServers(ctx context.Context) ([]netip.Addr, error) {
	output, err := platform.Runner.Run(ctx, nil, "/usr/sbin/scutil", "--dns")
	if err != nil {
		return nil, err
	}
	seen := make(map[netip.Addr]struct{})
	var result []netip.Addr
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "nameserver[") {
			continue
		}
		separator := strings.Index(line, ":")
		if separator < 0 {
			continue
		}
		value := strings.TrimSpace(line[separator+1:])
		if percent := strings.Index(value, "%"); percent >= 0 {
			value = value[:percent]
		}
		address, err := netip.ParseAddr(value)
		if err != nil {
			continue
		}
		address = address.Unmap()
		if address.IsUnspecified() || address.IsMulticast() {
			continue
		}
		if _, exists := seen[address]; exists {
			continue
		}
		if len(result) >= 64 {
			return nil, errors.New("macOS DNS server list exceeds the Runtime limit")
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result, nil
}

func (platform commandDarwinPlatform) AddRoute(ctx context.Context, route darwinRoute) error {
	arguments, err := darwinRouteArguments("add", route)
	if err != nil {
		return err
	}
	_, err = platform.Runner.Run(ctx, nil, "/sbin/route", arguments...)
	return err
}

func (platform commandDarwinPlatform) DeleteRoute(ctx context.Context, route darwinRoute) error {
	arguments, err := darwinRouteArguments("delete", route)
	if err != nil {
		return err
	}
	_, err = platform.Runner.Run(ctx, nil, "/sbin/route", arguments...)
	return err
}

func darwinRouteArguments(operation string, route darwinRoute) ([]string, error) {
	if operation != "add" && operation != "delete" {
		return nil, errors.New("macOS route operation is invalid")
	}
	if !route.Destination.IsValid() {
		return nil, errors.New("macOS route destination is invalid")
	}
	family := "-inet"
	if route.Destination.Addr().Is6() {
		family = "-inet6"
	}
	arguments := []string{"-n", operation, family}
	if route.Blackhole {
		arguments = append(arguments, "-blackhole")
	}
	arguments = append(arguments, "-net", route.Destination.String())
	if route.Blackhole {
		gateway := route.Gateway
		if gateway == "" {
			if route.Destination.Addr().Is6() {
				gateway = "::1"
			} else {
				gateway = "127.0.0.1"
			}
		}
		arguments = append(arguments, gateway)
		return arguments, nil
	}
	if route.Gateway != "" && !strings.HasPrefix(route.Gateway, "link#") {
		if _, err := netip.ParseAddr(strings.TrimSuffix(route.Gateway, "%"+route.Interface)); err != nil {
			return nil, errors.New("macOS route gateway is invalid")
		}
		return append(arguments, route.Gateway), nil
	}
	if !validDarwinInterface(route.Interface) {
		return nil, errors.New("macOS route interface is invalid")
	}
	return append(arguments, "-interface", route.Interface), nil
}

func parseDarwinNetstat(body string, family string) ([]darwinRoute, error) {
	var routes []darwinRoute
	inTable := false
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "Destination" {
			inTable = true
			continue
		}
		if !inTable || len(fields) < 4 {
			continue
		}
		destination, err := parseDarwinDestination(fields[0], family)
		if err != nil {
			continue
		}
		// Darwin netstat -rn emits Destination, Gateway, Flags and Netif
		// before any optional Expire column.
		interfaceName := fields[3]
		if !validDarwinInterface(interfaceName) {
			continue
		}
		routes = append(routes, darwinRoute{
			Destination: destination,
			Gateway:     fields[1],
			Interface:   interfaceName,
			Blackhole:   strings.Contains(fields[2], "B"),
		})
	}
	return routes, nil
}

func parseDarwinDestination(value string, family string) (netip.Prefix, error) {
	if value == "default" {
		if family == "inet6" {
			return netip.MustParsePrefix("::/0"), nil
		}
		return netip.MustParsePrefix("0.0.0.0/0"), nil
	}
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		address := value[:slash]
		suffix := value[slash:]
		if percent := strings.IndexByte(address, '%'); percent >= 0 {
			address = address[:percent]
		}
		value = address + suffix
	} else if percent := strings.IndexByte(value, '%'); percent >= 0 {
		value = value[:percent]
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Masked(), nil
	}
	address, err := netip.ParseAddr(value)
	if err == nil {
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	if family == "inet" {
		if prefix, ok := parseDarwinAbbreviatedIPv4(value); ok {
			return prefix, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("invalid macOS route destination %q", value)
}

func parseDarwinAbbreviatedIPv4(value string) (netip.Prefix, bool) {
	addressPart, bitsPart, hasBits := strings.Cut(value, "/")
	octets := strings.Split(addressPart, ".")
	if len(octets) < 1 || len(octets) > 4 {
		return netip.Prefix{}, false
	}
	values := make([]byte, 4)
	for index, octet := range octets {
		parsed, err := strconv.ParseUint(octet, 10, 8)
		if err != nil {
			return netip.Prefix{}, false
		}
		values[index] = byte(parsed)
	}
	bits := len(octets) * 8
	if hasBits {
		parsed, err := strconv.Atoi(bitsPart)
		if err != nil || parsed < 0 || parsed > 32 {
			return netip.Prefix{}, false
		}
		bits = parsed
	}
	return netip.PrefixFrom(netip.AddrFrom4([4]byte(values)), bits).Masked(), true
}
