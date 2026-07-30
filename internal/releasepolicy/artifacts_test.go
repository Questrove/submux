package releasepolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactAuditRejectsSecretsAndAllowsBoundedApproval(t *testing.T) {
	root := t.TempDir()
	policy := ArtifactPolicy{
		Schema:                 ArtifactPolicySchema,
		ForbiddenTokens:        []string{"submux-agent"},
		SecretPrefixes:         []string{"ghp_"},
		BudgetRequiredPatterns: []string{"*.bin"},
		Budgets: []ArtifactBudget{{
			Pattern:          "*.bin",
			MaxBytes:         4,
			ApprovedMaxBytes: 16,
			Approval:         "preview exception",
		}},
	}
	name := filepath.Join(root, "runtime.bin")
	if err := os.WriteFile(name, []byte("12345678"), 0644); err != nil {
		t.Fatal(err)
	}
	audit, err := AuditArtifacts(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.ApprovedExceptions) != 1 {
		t.Fatalf("expected one size approval, got %#v", audit)
	}
	if len(audit.Artifacts) != 1 || audit.Artifacts[0].Path != "runtime.bin" ||
		audit.Artifacts[0].Bytes != 8 {
		t.Fatalf("unexpected artifact size records: %#v", audit.Artifacts)
	}
	if err := os.WriteFile(name, []byte("GHP_EXAMPLECREDENTIAL"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := AuditArtifacts(root, policy); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected secret rejection, got %v", err)
	}
}

func TestArtifactAuditIgnoresShortBinarySecretPrefixCollision(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "metadata.bin"), []byte{'x', 'g', 'h', 's', '_', 0, 1}, 0644); err != nil {
		t.Fatal(err)
	}
	policy := ArtifactPolicy{
		Schema:          ArtifactPolicySchema,
		ForbiddenTokens: []string{"submux-agent"},
		SecretPrefixes:  []string{"ghs_"},
		Budgets: []ArtifactBudget{{
			Pattern:  "*.bin",
			MaxBytes: 16,
		}},
	}
	if _, err := AuditArtifacts(root, policy); err != nil {
		t.Fatalf("short binary prefix collision must not be treated as a credential: %v", err)
	}
}

func TestArtifactAuditRejectsBudgetRequiredArtifactWithoutBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "runtime.zip"), []byte("zip"), 0644); err != nil {
		t.Fatal(err)
	}
	policy := ArtifactPolicy{
		Schema:                 ArtifactPolicySchema,
		ForbiddenTokens:        []string{"submux-agent"},
		BudgetRequiredPatterns: []string{"*.zip"},
		Budgets: []ArtifactBudget{{
			Pattern:  "*.bin",
			MaxBytes: 16,
		}},
	}
	if _, err := AuditArtifacts(root, policy); err == nil ||
		!strings.Contains(err.Error(), "requires a declared size budget") {
		t.Fatalf("expected missing budget rejection, got %v", err)
	}
}
