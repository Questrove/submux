package runtimesource

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"
)

func dialSOCKS5(ctx context.Context, proxyAddress, targetAddress string) (net.Conn, error) {
	targetHost, targetPortText, err := net.SplitHostPort(targetAddress)
	if err != nil {
		return nil, errors.New("SOCKS5 target address is invalid")
	}
	targetIP, err := netip.ParseAddr(targetHost)
	if err != nil {
		return nil, errors.New("SOCKS5 target must be a resolved IP address")
	}
	targetPort, err := strconv.Atoi(targetPortText)
	if err != nil || targetPort < 1 || targetPort > 65535 {
		return nil, errors.New("SOCKS5 target port is invalid")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if deadline, exists := ctx.Deadline(); exists {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	} else if err := conn.SetDeadline(time.Now().Add(MaximumFetchTimeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return nil, err
	}
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return nil, err
	}
	if greeting != [2]byte{0x05, 0x00} {
		return nil, errors.New("Mihomo SOCKS5 listener rejected unauthenticated access")
	}

	targetIP = targetIP.Unmap()
	request := []byte{0x05, 0x01, 0x00}
	if targetIP.Is4() {
		request = append(request, 0x01)
		bytes := targetIP.As4()
		request = append(request, bytes[:]...)
	} else {
		request = append(request, 0x04)
		bytes := targetIP.As16()
		request = append(request, bytes[:]...)
	}
	var encodedPort [2]byte
	binary.BigEndian.PutUint16(encodedPort[:], uint16(targetPort))
	request = append(request, encodedPort[:]...)
	if _, err := conn.Write(request); err != nil {
		return nil, err
	}
	var response [4]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return nil, err
	}
	if response[0] != 0x05 || response[1] != 0x00 || response[2] != 0x00 {
		return nil, fmt.Errorf("Mihomo SOCKS5 connection failed with reply %d", response[1])
	}
	if err := discardSOCKS5Address(conn, response[3]); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}

func discardSOCKS5Address(reader io.Reader, addressType byte) error {
	length := 0
	switch addressType {
	case 0x01:
		length = 4
	case 0x04:
		length = 16
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(reader, size[:]); err != nil {
			return err
		}
		length = int(size[0])
	default:
		return errors.New("SOCKS5 reply address type is invalid")
	}
	buffer := make([]byte, length+2)
	_, err := io.ReadFull(reader, buffer)
	return err
}
