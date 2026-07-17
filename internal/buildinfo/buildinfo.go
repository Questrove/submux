package buildinfo

import "runtime/debug"

// These values are replaced by the release workflow through -ldflags. Keeping
// useful defaults makes source and local development builds self-describing.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func Current() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date}
	if build, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "unknown" && setting.Value != "" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.Date == "unknown" && setting.Value != "" {
					info.Date = setting.Value
				}
			}
		}
	}
	return info
}
