//go:build linux

package runtimepaths

func platformDefaults() Defaults {
	return Defaults{
		StateRoot:       "/var/lib/submux-runtime",
		Endpoint:        "/run/submux-runtime/runtime.sock",
		LockFile:        "/run/submux-runtime/runtime.lock",
		ControlEndpoint: "/run/submux-runtime/mihomo.sock",
		NetworkEndpoint: "/run/submux-runtime/runtime-net.sock",
	}
}
