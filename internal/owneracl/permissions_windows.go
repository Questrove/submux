//go:build windows

package owneracl

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func RestrictFile(name string) error {
	return restrict(name, false)
}

func RestrictDirectory(name string) error {
	return restrict(name, true)
}

func restrict(name string, directory bool) error {
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("resolve current Windows user SID for owner-only ACL: %w", err)
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(current.User.Sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("build owner-only Windows ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		name,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("set owner-only Windows ACL: %w", err)
	}
	return nil
}
