//go:build linux || darwin

package runtimeipc

func containsGroupID(groups []uint32, expected uint32) bool {
	for _, group := range groups {
		if group == expected {
			return true
		}
	}
	return false
}
