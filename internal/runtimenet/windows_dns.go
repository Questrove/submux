//go:build windows

package runtimenet

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsAdapterBufferLimit = 1 << 20
	windowsDNSServerLimit     = 64
)

func windowsDNSServers(excludedInterface uint32) ([]netip.Addr, error) {
	flags := uint32(
		windows.GAA_FLAG_SKIP_UNICAST |
			windows.GAA_FLAG_SKIP_ANYCAST |
			windows.GAA_FLAG_SKIP_MULTICAST |
			windows.GAA_FLAG_SKIP_FRIENDLY_NAME,
	)
	var size uint32
	err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, nil, &size)
	if err != nil && !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
		return nil, fmt.Errorf("size Windows adapter table: %w", err)
	}
	if size == 0 || size > windowsAdapterBufferLimit {
		return nil, errors.New("Windows adapter table size is invalid")
	}
	var (
		buffer []byte
		table  *windows.IpAdapterAddresses
	)
	for attempt := 0; attempt < 3; attempt++ {
		if size == 0 || size > windowsAdapterBufferLimit {
			return nil, errors.New("Windows adapter table size is invalid")
		}
		buffer = make([]byte, size)
		table = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		err = windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, table, &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return nil, fmt.Errorf("read Windows DNS servers: %w", err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("Windows adapter table changed repeatedly: %w", err)
	}

	seen := make(map[netip.Addr]struct{})
	servers := make([]netip.Addr, 0)
	for adapter := table; adapter != nil; adapter = adapter.Next {
		if adapter.OperStatus != windows.IfOperStatusUp ||
			adapter.IfIndex == excludedInterface ||
			adapter.Ipv6IfIndex == excludedInterface {
			continue
		}
		for item := adapter.FirstDnsServerAddress; item != nil; item = item.Next {
			address, ok := netip.AddrFromSlice(item.Address.IP())
			if !ok {
				continue
			}
			address = address.Unmap()
			if !address.IsValid() ||
				address.IsUnspecified() ||
				address.IsLoopback() ||
				address.IsMulticast() {
				continue
			}
			if _, exists := seen[address]; exists {
				continue
			}
			if len(servers) >= windowsDNSServerLimit {
				return nil, errors.New("Windows DNS server list exceeds the Runtime limit")
			}
			seen[address] = struct{}{}
			servers = append(servers, address)
		}
	}
	sort.Slice(servers, func(left, right int) bool {
		return servers[left].Compare(servers[right]) < 0
	})
	return servers, nil
}
