package runtimeupdate

import _ "embed"

// embeddedInitialRoot is the public half of the project's out-of-band TUF
// trust bootstrap. Private signing keys are deliberately kept outside the
// source repository.
//
//go:embed assets/root.json
var embeddedInitialRoot []byte

func InitialRoot() []byte {
	return append([]byte(nil), embeddedInitialRoot...)
}
