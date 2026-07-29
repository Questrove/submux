package runtimeprocess

import (
	"errors"
	"fmt"
	"net"
)

func CheckLoopbackPortsAvailable(addresses []string) error {
	if len(addresses) == 0 {
		return errors.New("explicit proxy has no loopback listeners")
	}
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("invalid explicit proxy address %q", address)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("explicit proxy address %q is not loopback", address)
		}
		network := "tcp6"
		if ip.To4() != nil {
			network = "tcp4"
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			return fmt.Errorf("explicit proxy address %s is already in use or unavailable: %w", address, err)
		}
		listeners = append(listeners, listener)
	}
	return nil
}
