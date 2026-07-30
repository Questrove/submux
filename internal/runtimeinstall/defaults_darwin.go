//go:build darwin

package runtimeinstall

type Defaults struct {
	InstallRoot string
	StateRoot   string
	ReceiptPath string
}

func CurrentDefaults() Defaults {
	return Defaults{
		InstallRoot: "/Library/PrivilegedHelperTools",
		StateRoot:   "/Library/Application Support/SubmuxRuntime",
		ReceiptPath: "/Library/Application Support/SubmuxRuntime/install-receipt.json",
	}
}
