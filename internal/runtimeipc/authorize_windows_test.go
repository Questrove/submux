//go:build windows

package runtimeipc

import (
	"testing"

	"submux/internal/runtimeapi"
)

func TestWindowsAuthorizerAcceptsOnlyExpectedLocalIdentities(t *testing.T) {
	current := runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-1000"}
	tests := []struct {
		name string
		peer runtimeapi.PeerIdentity
		want bool
	}{
		{
			name: "same SID",
			peer: runtimeapi.PeerIdentity{Platform: "windows", SID: current.SID},
			want: true,
		},
		{
			name: "LocalSystem",
			peer: runtimeapi.PeerIdentity{Platform: "windows", SID: windowsSystemSID},
			want: true,
		},
		{
			name: "elevated administrator",
			peer: runtimeapi.PeerIdentity{
				Platform:  "windows",
				SID:       "S-1-5-21-2000",
				GroupSIDs: []string{windowsAdministratorsSID},
				Elevated:  true,
			},
			want: true,
		},
		{
			name: "filtered administrator",
			peer: runtimeapi.PeerIdentity{
				Platform:  "windows",
				SID:       "S-1-5-21-2000",
				GroupSIDs: []string{windowsAdministratorsSID},
			},
			want: false,
		},
		{
			name: "unrelated user",
			peer: runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-3000"},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := authorizeCurrentPeer(current, test.peer)
			if (err == nil) != test.want {
				t.Fatalf("authorize Windows peer=%#v err=%v", test.peer, err)
			}
		})
	}
}

func TestWindowsAuthorizerAcceptsNewlyPersistedOperatorWithoutRelogin(t *testing.T) {
	original := windowsPersistedOperatorMember
	windowsPersistedOperatorMember = func(sid string) bool {
		return sid == "S-1-5-21-4000"
	}
	t.Cleanup(func() {
		windowsPersistedOperatorMember = original
	})
	err := authorizeCurrentPeer(
		runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-80-1"},
		runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-4000"},
	)
	if err != nil {
		t.Fatalf("newly persisted operator authorization: %v", err)
	}
}
