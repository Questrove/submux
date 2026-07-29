//go:build darwin

package runtimepaths

func platformDefaults() Defaults {
	return Defaults{
		StateRoot: "/Library/Application Support/SubmuxRuntime",
		Endpoint:  "/var/run/submux-runtime/runtime.sock",
		LockFile:  "/var/run/submux-runtime/runtime.lock",
	}
}
