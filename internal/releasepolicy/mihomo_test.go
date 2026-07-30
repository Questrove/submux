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
