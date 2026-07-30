package releasepolicy

import (
	"strings"
	"testing"
)

func TestPreviewRequiresMissingEvidence(t *testing.T) {
	matrix := SupportMatrix{
		Schema: SupportMatrixSchema,
		Entries: []SupportTarget{{
			ID:       "linux-amd64",
			Platform: "linux",
			System:   "Ubuntu",
			Arch:     "amd64",
			Status:   "preview",
		}},
	}
	if err := ValidateSupportMatrix(matrix); err == nil ||
		!strings.Contains(err.Error(), "must explain missing evidence") {
		t.Fatalf("expected preview evidence error, got %v", err)
	}
}

func TestStableRequiresNativeEvidence(t *testing.T) {
	target := SupportTarget{
		ID:       "windows-amd64",
		Platform: "windows",
		System:   "Windows 11",
		Arch:     "amd64",
		Status:   "stable",
	}
	for _, category := range RequiredStableEvidence {
		target.Evidence = append(target.Evidence, SupportEvidence{
			Category: category,
			Level:    "native",
			Source:   "evidence/example.json",
		})
	}
	if err := ValidateSupportMatrix(SupportMatrix{
		Schema:  SupportMatrixSchema,
		Entries: []SupportTarget{target},
	}); err != nil {
		t.Fatal(err)
	}
	target.Evidence[0].Level = "cross-build"
	if err := ValidateSupportMatrix(SupportMatrix{
		Schema:  SupportMatrixSchema,
		Entries: []SupportTarget{target},
	}); err == nil || !strings.Contains(err.Error(), "lacks native") {
		t.Fatalf("expected native evidence error, got %v", err)
	}
}

func TestLinuxStableRequiresGatewayEvidence(t *testing.T) {
	target := SupportTarget{
		ID:       "linux-amd64",
		Platform: "linux",
		System:   "Ubuntu",
		Arch:     "amd64",
		Status:   "stable",
	}
	for _, category := range RequiredStableEvidence {
		target.Evidence = append(target.Evidence, SupportEvidence{
			Category: category,
			Level:    "native",
			Source:   "evidence/example.json",
		})
	}
	if err := ValidateSupportMatrix(SupportMatrix{
		Schema:  SupportMatrixSchema,
		Entries: []SupportTarget{target},
	}); err == nil || !strings.Contains(err.Error(), "gateway-ipv4-tcp") {
		t.Fatalf("expected gateway evidence error, got %v", err)
	}
}
