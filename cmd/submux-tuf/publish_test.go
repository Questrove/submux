package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeupdate"
)

func TestPublishRepositoryCreatesVerifiableOfflineBundle(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	publicDir := filepath.Join(root, "public")
	if err := os.Mkdir(publicDir, 0755); err != nil {
		t.Fatal(err)
	}
	publicRoot := filepath.Join(publicDir, "root.json")
	if _, err := bootstrapRoot(privateDir, publicRoot, now); err != nil {
		t.Fatal(err)
	}
	removeRootPrivateKeys(t, privateDir)
	targetBody := []byte("fixed mihomo release bytes")
	targetSource := filepath.Join(root, "mihomo-linux-amd64-v1.2.3.gz")
	if err := os.WriteFile(targetSource, targetBody, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(targetBody)
	targetPath := "mihomo/v1.2.3/linux/amd64/mihomo-linux-amd64-v1.2.3.gz"
	custom, err := json.Marshal(map[string]any{
		"kind":            "mihomo",
		"version":         "v1.2.3",
		"platform":        "linux",
		"arch":            "amd64",
		"repository":      runtimeupdate.OfficialRepository,
		"asset_name":      filepath.Base(targetSource),
		"upstream_sha256": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := publishManifest{
		Schema:           targetManifestSchema,
		MetadataVersion:  1,
		TargetsExpires:   now.Add(90 * 24 * time.Hour),
		SnapshotExpires:  now.Add(30 * 24 * time.Hour),
		TimestampExpires: now.Add(7 * 24 * time.Hour),
		Targets: []publishTarget{{
			Path:   targetPath,
			Source: targetSource,
			Custom: custom,
		}},
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "targets-manifest.json")
	if err := os.WriteFile(manifestPath, manifestBody, 0600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(root, "repository")
	result, err := publishRepository(
		context.Background(),
		privateDir,
		publicRoot,
		manifestPath,
		outputDir,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetCount != 1 {
		t.Fatalf("unexpected target count %d", result.TargetCount)
	}
	bundle, err := zip.OpenReader(result.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	foundRoot := false
	for _, entry := range bundle.File {
		if entry.Name == "metadata/root.json" {
			foundRoot = true
			break
		}
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}
	if !foundRoot {
		t.Fatal("offline verification bundle omits the public Root")
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pem") || entry.Name() == "manifest.json" {
			t.Fatalf("private material leaked into public output: %s", entry.Name())
		}
	}
	rootBody, err := os.ReadFile(publicRoot)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &runtimeupdate.Verifier{
		InitialRoot: rootBody,
		StateRoot:   filepath.Join(root, "verify-state"),
		Now:         func() time.Time { return now },
	}
	target, downloaded, err := verifier.RefreshOffline(
		context.Background(),
		result.Bundle,
		"linux",
		"amd64",
		"v1.2.3",
	)
	if err != nil {
		t.Fatal(err)
	}
	if target.Path != targetPath || string(downloaded) != string(targetBody) {
		t.Fatalf("unexpected verified target %#v body %q", target, downloaded)
	}
	if err := verifier.VerifyTarget(target, downloaded); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRejectsRootPrivateKeys(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	publicDir := filepath.Join(root, "public")
	if err := os.Mkdir(publicDir, 0755); err != nil {
		t.Fatal(err)
	}
	publicRoot := filepath.Join(publicDir, "root.json")
	if _, err := bootstrapRoot(privateDir, publicRoot, now); err != nil {
		t.Fatal(err)
	}
	private := mustLoadPrivateManifest(t, privateDir)
	_, trustedRoot, err := loadTrustedRoot(publicRoot, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateDirectoryContents(privateDir, private); err == nil ||
		!strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("expected Root private key rejection, got %v", err)
	}
	if trustedRoot.Signed.Roles["root"] == nil {
		t.Fatal("trusted Root omits Root role")
	}
}

func TestPublishRejectsMissingThresholdKeys(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	publicDir := filepath.Join(root, "public")
	if err := os.Mkdir(publicDir, 0755); err != nil {
		t.Fatal(err)
	}
	publicRoot := filepath.Join(publicDir, "root.json")
	if _, err := bootstrapRoot(privateDir, publicRoot, now); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "targets-") && strings.HasSuffix(entry.Name(), ".pem") {
			if err := os.Remove(filepath.Join(privateDir, entry.Name())); err != nil {
				t.Fatal(err)
			}
			removed++
			if removed == 2 {
				break
			}
		}
	}
	rootBody, rootMetadata, err := loadTrustedRoot(publicRoot, mustLoadPrivateManifest(t, privateDir))
	if err != nil || len(rootBody) == 0 {
		t.Fatal(err)
	}
	if _, err := loadRoleSigners(privateDir, mustLoadPrivateManifest(t, privateDir), rootMetadata); err == nil ||
		!strings.Contains(err.Error(), "requires 2 signing keys") {
		t.Fatalf("expected threshold rejection, got %v", err)
	}
}

func removeRootPrivateKeys(t *testing.T, privateDir string) {
	t.Helper()
	manifest := mustLoadPrivateManifest(t, privateDir)
	for _, role := range manifest.RolePolicies {
		if role.Name != "root" {
			continue
		}
		for _, key := range role.Keys {
			if err := os.Remove(filepath.Join(privateDir, key.File)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func mustLoadPrivateManifest(t *testing.T, privateDir string) privateManifest {
	t.Helper()
	manifest, err := loadPrivateManifest(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
