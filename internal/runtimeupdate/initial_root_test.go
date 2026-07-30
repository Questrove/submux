package runtimeupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
)

func TestEmbeddedInitialRootIsThresholdSignedPublicMetadata(t *testing.T) {
	body := InitialRoot()
	if len(body) == 0 || bytes.Contains(bytes.ToLower(body), []byte("private")) {
		t.Fatal("embedded initial Root is empty or contains private-key material")
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != "26f0bb3d036a9867b8adb28a66998e55f63f16d0e7c735358f5762e82edf15d0" {
		t.Fatalf("embedded initial Root SHA-256 = %s", got)
	}
	root, err := metadata.Root().FromBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if root.Signed.Version != 1 || !root.Signed.ConsistentSnapshot {
		t.Fatalf("embedded initial Root policy = %#v", root.Signed)
	}
	if !root.Signed.Expires.Equal(time.Date(2036, time.July, 30, 4, 39, 51, 0, time.UTC)) {
		t.Fatalf("embedded initial Root expiry = %s", root.Signed.Expires)
	}
	for name, want := range map[string]struct {
		keys      int
		threshold int
	}{
		metadata.ROOT:      {keys: 3, threshold: 2},
		metadata.TARGETS:   {keys: 3, threshold: 2},
		metadata.SNAPSHOT:  {keys: 1, threshold: 1},
		metadata.TIMESTAMP: {keys: 1, threshold: 1},
	} {
		role := root.Signed.Roles[name]
		if role == nil || len(role.KeyIDs) != want.keys || role.Threshold != want.threshold {
			t.Fatalf("embedded initial Root %s role = %#v", name, role)
		}
	}
	if err := root.VerifyDelegate(metadata.ROOT, root); err != nil {
		t.Fatalf("verify embedded initial Root threshold: %v", err)
	}
}
