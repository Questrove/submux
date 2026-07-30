//go:build linux

package runtimeinstall

type Defaults struct {
	InstallRoot string
	StateRoot   string
	ReceiptPath string
}

func CurrentDefaults() Defaults {
	return Defaults{
		InstallRoot: "/usr/lib/submux-runtime",
		StateRoot:   "/var/lib/submux-runtime",
		ReceiptPath: "/var/lib/submux-runtime/install-receipt.json",
	}
}
