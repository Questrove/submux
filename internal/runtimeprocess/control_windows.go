//go:build windows

package runtimeprocess

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func dialControl(ctx context.Context, endpoint string) (net.Conn, error) {
	if !validControlPipeEndpoint(endpoint) {
		return nil, errors.New("Mihomo Named Pipe must use the Runtime-owned fixed prefix")
	}
	connection, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if err := validateControlPipeSecurity(connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

func validControlPipeEndpoint(endpoint string) bool {
	const fixed = `\\.\pipe\submux-runtime-mihomo`
	if endpoint == fixed {
		return true
	}
	const testPrefix = fixed + "-test-"
	return strings.HasPrefix(endpoint, testPrefix) &&
		len(endpoint) > len(testPrefix) &&
		len(endpoint) <= len(testPrefix)+64 &&
		!strings.ContainsAny(endpoint[len(testPrefix):], `\/:`)
}

func validateControlPipeSecurity(connection net.Conn) error {
	handleProvider, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("Mihomo Named Pipe does not expose its security handle")
	}
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(handleProvider.Fd()),
		windows.SE_KERNEL_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read Mihomo Named Pipe security: %w", err)
	}
	if descriptor == nil || !descriptor.IsValid() {
		return errors.New("Mihomo Named Pipe security descriptor is invalid")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("Mihomo Named Pipe must have a non-empty DACL")
	}
	currentSID, err := currentProcessSID()
	if err != nil {
		return err
	}
	allowed := map[string]struct{}{
		currentSID:     {},
		"S-1-5-18":     {},
		"S-1-5-32-544": {},
	}
	currentAllowed := false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("read Mihomo Named Pipe ACL entry: %w", err)
		}
		if ace == nil {
			return errors.New("Mihomo Named Pipe ACL entry is invalid")
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return errors.New("Mihomo Named Pipe has an unsupported access rule")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if _, ok := allowed[sid]; !ok {
			return fmt.Errorf("Mihomo Named Pipe grants access to unauthorized SID %s", sid)
		}
		if sid == currentSID {
			currentAllowed = true
		}
	}
	if !currentAllowed {
		return errors.New("Mihomo Named Pipe does not explicitly grant the Runtime service SID")
	}
	return nil
}

func currentProcessSID() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("open Runtime process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read Runtime service SID: %w", err)
	}
	return user.User.Sid.String(), nil
}
