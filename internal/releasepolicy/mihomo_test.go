package releasepolicy

import (
	"strings"
	"testing"
)

func TestMihomoProvenanceRequiresAllTargets(t *testing.T) {
	provenance := MihomoProvenance{
		Schema:     1,
		Repository: "MetaCubeX/mihomo",
		Version:    "v1.2.3",
		Tag:        "v1.2.3",
		Commit:     strings.Repeat("a", 40),
		License:    "GPL-3.0-only",
		LicenseURL: "https://raw.githubusercontent.com/MetaCubeX/mihomo/v1.2.3/LICENSE",
		SourceArchive: MihomoSourceArchive{
			Name:   "mihomo-v1.2.3-source.tar.gz",
			URL:    "https://api.github.com/repos/MetaCubeX/mihomo/tarball/v1.2.3",
			SHA256: strings.Repeat("b", 64),
		},
	}
	if err := ValidateMihomoProvenance(provenance); err == nil ||
		!strings.Contains(err.Error(), "omits") {
		t.Fatalf("expected platform omission error, got %v", err)
	}
}

func TestMihomoProvenanceRejectsAssetOutsideRuntimeContract(t *testing.T) {
	provenance := MihomoProvenance{
		Schema:     1,
		Repository: "MetaCubeX/mihomo",
		Version:    "v1.2.3",
		Tag:        "v1.2.3",
		Commit:     strings.Repeat("a", 40),
		License:    "GPL-3.0-only",
		LicenseURL: "https://raw.githubusercontent.com/MetaCubeX/mihomo/v1.2.3/LICENSE",
		SourceArchive: MihomoSourceArchive{
			Name:   "mihomo-v1.2.3-source.tar.gz",
			URL:    "https://api.github.com/repos/MetaCubeX/mihomo/tarball/v1.2.3",
			SHA256: strings.Repeat("b", 64),
		},
		Assets: []MihomoPlatformAsset{
			{Platform: "linux", Arch: "amd64", Name: "mihomo-linux-amd64-v1.2.3.gz", SHA256: strings.Repeat("c", 64)},
			{Platform: "linux", Arch: "arm64", Name: "mihomo-linux-arm64-v1.2.3.gz", SHA256: strings.Repeat("d", 64)},
			{Platform: "windows", Arch: "amd64", Name: "mihomo-windows-amd64-compatible-v1.2.3.zip", SHA256: strings.Repeat("e", 64)},
			{Platform: "windows", Arch: "arm64", Name: "mihomo-windows-arm64-v1.2.3.zip", SHA256: strings.Repeat("f", 64)},
			{Platform: "darwin", Arch: "amd64", Name: "mihomo-darwin-amd64-compatible-v1.2.3.gz", SHA256: strings.Repeat("1", 64)},
			{Platform: "darwin", Arch: "arm64", Name: "mihomo-darwin-arm64-v1.2.3.gz", SHA256: strings.Repeat("2", 64)},
		},
	}
	if err := ValidateMihomoProvenance(provenance); err == nil ||
		!strings.Contains(err.Error(), "fixed Runtime platform target") {
		t.Fatalf("expected fixed Runtime platform target error, got %v", err)
	}
}
