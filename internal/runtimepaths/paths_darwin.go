//go:build darwin

package runtimepaths

func platformDefaults() Defaults {
	return Defaults{
		StateRoot:       "/Library/Application Support/SubmuxRuntime",
		Endpoint:        "/var/run/submux-runtime/runtime.sock",
		LockFile:        "/Library/Application Support/SubmuxRuntime/runtime.lock",
		ControlEndpoint: "/var/run/submux-runtime-privileged/mihomo.sock",
		NetworkEndpoint: "/var/run/submux-runtime-privileged/runtime-net.sock",
	}
}
