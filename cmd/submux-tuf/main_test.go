package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
)

func TestBuildInitialRootUsesThresholdRolesAndContainsNoPrivateKey(t *testing.T) {
	expires := time.Date(2036, time.July, 30, 0, 0, 0, 0, time.UTC)
	root, keys, err := buildInitialRoot(expires)
	if err != nil {
		t.Fatal(err)
	}
	if root.Signed.Version != 1 || !root.Signed.ConsistentSnapshot || !root.Signed.Expires.Equal(expires) {
		t.Fatalf("unexpected Root policy: %#v", root.Signed)
	}
	for _, policy := range initialRolePolicies {
		role := root.Signed.Roles[policy.Name]
		if role == nil || role.Threshold != policy.Threshold || len(role.KeyIDs) != policy.KeyCount {
			t.Fatalf("%s role = %#v", policy.Name, role)
		}
	}
	if err := root.VerifyDelegate(metadata.ROOT, root); err != nil {
		t.Fatalf("Root threshold verification failed: %v", err)
	}
	if len(keys) != 8 {
		t.Fatalf("generated key count = %d", len(keys))
	}
	body, err := root.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if bytes.Contains(body, key.PrivateKey) {
			t.Fatalf("public Root contains private key bytes for %s", key.KeyID)
		}
	}
	if bytes.Contains(bytes.ToLower(body), []byte("private")) {
		t.Fatal("public Root contains a private-key field")
	}
}

func TestBootstrapRootWritesKeysOutsidePublicTree(t *testing.T) {
	root := t.TempDir()
	privateDir := filepath.Join(root, "private", "v1")
	if err := os.Mkdir(filepath.Dir(privateDir), 0700); err != nil {
		t.Fatal(err)
	}
	publicDir := filepath.Join(root, "public")
	if err := os.Mkdir(publicDir, 0700); err != nil {
		t.Fatal(err)
	}
	publicRoot := filepath.Join(publicDir, "root.json")
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	result, err := bootstrapRoot(privateDir, publicRoot, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.PrivateDir != privateDir || result.PublicRoot != publicRoot {
		t.Fatalf("bootstrap result = %#v", result)
	}
	rootMetadata, err := metadata.Root().FromFile(publicRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootMetadata.VerifyDelegate(metadata.ROOT, rootMetadata); err != nil {
		t.Fatalf("persisted Root threshold verification failed: %v", err)
	}
	manifestBody, err := os.ReadFile(filepath.Join(privateDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest privateManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RootSHA256 != result.RootSHA256 || len(manifest.RolePolicies) != 4 {
		t.Fatalf("private manifest = %#v", manifest)
	}
	for _, role := range manifest.RolePolicies {
		for _, key := range role.Keys {
			body, err := os.ReadFile(filepath.Join(privateDir, key.File))
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(body)
			if block == nil {
				t.Fatalf("private key %s is not PEM", key.File)
			}
			if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
				t.Fatalf("parse private key %s: %v", key.File, err)
			}
		}
	}
	if strings.Contains(string(mustRead(t, publicRoot)), "PRIVATE KEY") {
		t.Fatal("public output contains private key material")
	}
}

func TestValidateDestinationsRejectsOverlapAndExistingOutputs(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	if err := os.Mkdir(publicDir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateDestinations(filepath.Join(publicDir, "keys"), filepath.Join(publicDir, "root.json")); err == nil {
		t.Fatal("overlapping private and public output was accepted")
	}
	privateParent := filepath.Join(root, "private")
	if err := os.Mkdir(privateParent, 0700); err != nil {
		t.Fatal(err)
	}
	publicRoot := filepath.Join(publicDir, "root.json")
	if err := os.WriteFile(publicRoot, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateDestinations(filepath.Join(privateParent, "v1"), publicRoot); err == nil {
		t.Fatal("existing public Root was accepted")
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
