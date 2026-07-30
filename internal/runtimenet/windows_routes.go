//go:build windows

package runtimenet

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsOwnedRouteMetric = 20000
	windowsRouteMetricSpan  = 20000
	windowsInfiniteLifetime = ^uint32(0)
)

var (
	ipHelperDLL              = windows.NewLazySystemDLL("iphlpapi.dll")
	initializeIPForwardEntry = ipHelperDLL.NewProc("InitializeIpForwardEntry")
	createIPForwardEntry     = ipHelperDLL.NewProc("CreateIpForwardEntry2")
	deleteIPForwardEntry     = ipHelperDLL.NewProc("DeleteIpForwardEntry2")
)

type windowsRoute struct {
	InterfaceIndex uint32
	Destination    netip.Prefix
	NextHop        netip.Addr
	Metric         uint32
	Protocol       uint32
}

type windowsNetworkAPI interface {
	Routes() ([]windowsRoute, error)
	CreateRoute(windowsRoute) error
	DeleteRoute(windowsRoute) error
}

type ipHelperNetworkAPI struct{}

func (ipHelperNetworkAPI) Routes() ([]windowsRoute, error) {
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &table); err != nil {
		return nil, fmt.Errorf("read Windows route table: %w", err)
	}
	if table == nil {
		return nil, errors.New("Windows route table is unavailable")
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))

	rows := table.Rows()
	routes := make([]windowsRoute, 0, len(rows))
	for index := range rows {
		route, err := windowsRouteFromRow(&rows[index])
		if err != nil {
			continue
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func (ipHelperNetworkAPI) CreateRoute(route windowsRoute) error {
	row, err := windowsRouteRow(route)
	if err != nil {
		return err
	}
	result, _, _ := createIPForwardEntry.Call(uintptr(unsafe.Pointer(&row)))
	if result != 0 {
		return fmt.Errorf("create Windows route %s: %w", route.Destination, syscall.Errno(result))
	}
	return nil
}

func (ipHelperNetworkAPI) DeleteRoute(route windowsRoute) error {
	row, err := windowsRouteRow(route)
	if err != nil {
		return err
	}
	result, _, _ := deleteIPForwardEntry.Call(uintptr(unsafe.Pointer(&row)))
	if result != 0 {
		return fmt.Errorf("delete Windows route %s: %w", route.Destination, syscall.Errno(result))
	}
	return nil
}

func windowsRouteRow(route windowsRoute) (windows.MibIpForwardRow2, error) {
	if route.InterfaceIndex == 0 {
		return windows.MibIpForwardRow2{}, errors.New("Windows route interface index is invalid")
	}
	destination := route.Destination.Masked()
	if !destination.IsValid() {
		return windows.MibIpForwardRow2{}, errors.New("Windows route destination is invalid")
	}
	nextHop := route.NextHop
	if !nextHop.IsValid() {
		if destination.Addr().Is4() {
			nextHop = netip.IPv4Unspecified()
		} else {
			nextHop = netip.IPv6Unspecified()
		}
	}
	if destination.Addr().Is4() != nextHop.Is4() {
		return windows.MibIpForwardRow2{}, errors.New("Windows route address families do not match")
	}

	var row windows.MibIpForwardRow2
	initializeIPForwardEntry.Call(uintptr(unsafe.Pointer(&row)))
	row.InterfaceIndex = route.InterfaceIndex
	row.DestinationPrefix.Prefix = windowsRawSockaddr(destination.Addr())
	row.DestinationPrefix.PrefixLength = uint8(destination.Bits())
	row.NextHop = windowsRawSockaddr(nextHop)
	row.SitePrefixLength = uint8(destination.Bits())
	row.ValidLifetime = windowsInfiniteLifetime
	row.PreferredLifetime = windowsInfiniteLifetime
	row.Metric = route.Metric
	row.Protocol = route.Protocol
	row.Origin = windows.NlroManual
	return row, nil
}

func windowsRouteFromRow(row *windows.MibIpForwardRow2) (windowsRoute, error) {
	if row == nil {
		return windowsRoute{}, errors.New("Windows route row is unavailable")
	}
	address, err := windowsAddrFromRaw(&row.DestinationPrefix.Prefix)
	if err != nil {
		return windowsRoute{}, err
	}
	nextHop, err := windowsAddrFromRaw(&row.NextHop)
	if err != nil {
		return windowsRoute{}, err
	}
	bits := int(row.DestinationPrefix.PrefixLength)
	if bits < 0 || bits > address.BitLen() {
		return windowsRoute{}, errors.New("Windows route prefix length is invalid")
	}
	return windowsRoute{
		InterfaceIndex: row.InterfaceIndex,
		Destination:    netip.PrefixFrom(address, bits).Masked(),
		NextHop:        nextHop,
		Metric:         row.Metric,
		Protocol:       row.Protocol,
	}, nil
}

func windowsRawSockaddr(address netip.Addr) windows.RawSockaddrInet {
	var raw windows.RawSockaddrInet
	if address.Is4() {
		value := (*windows.RawSockaddrInet4)(unsafe.Pointer(&raw))
		value.Family = windows.AF_INET
		value.Addr = address.As4()
		return raw
	}
	value := (*windows.RawSockaddrInet6)(unsafe.Pointer(&raw))
	value.Family = windows.AF_INET6
	value.Addr = address.As16()
	return raw
}

func windowsAddrFromRaw(raw *windows.RawSockaddrInet) (netip.Addr, error) {
	if raw == nil {
		return netip.Addr{}, errors.New("Windows socket address is unavailable")
	}
	switch raw.Family {
	case windows.AF_INET:
		value := (*windows.RawSockaddrInet4)(unsafe.Pointer(raw))
		return netip.AddrFrom4(value.Addr), nil
	case windows.AF_INET6:
		value := (*windows.RawSockaddrInet6)(unsafe.Pointer(raw))
		return netip.AddrFrom16(value.Addr).Unmap(), nil
	default:
		return netip.Addr{}, fmt.Errorf("Windows socket address family %d is unsupported", raw.Family)
	}
}
