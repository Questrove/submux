package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestImportedContentIsBoundExpiringAndSingleUse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	other := runtimeapi.PeerIdentity{Platform: "test", UID: 1001}
	body := []byte("proxies: []\nrules: []\n")
	digest := sha256.Sum256(body)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	content, err := store.UploadImport(
		peer,
		"application/x-yaml",
		int64(len(body)),
		hex.EncodeToString(digest[:]),
		body,
		now,
	)
	if err != nil {
		t.Fatalf("upload Runtime import: %v", err)
	}
	if content.ID == "" || !content.ExpiresAt.Equal(now.Add(DefaultImportTTL)) {
		t.Fatalf("import metadata = %#v", content)
	}
	if _, _, err := store.ConsumeImport(content.ID, other.Key(), "", now); !errors.Is(err, ErrImportOwner) {
		t.Fatalf("other caller consume error = %v", err)
	}
	consumed, metadata, err := store.ConsumeImport(content.ID, peer.Key(), "", now)
	if err != nil {
		t.Fatalf("consume Runtime import: %v", err)
	}
	if string(consumed) != string(body) || metadata.SHA256 != content.SHA256 {
		t.Fatalf("consumed content = %q / %#v", consumed, metadata)
	}
	if _, _, err := store.ConsumeImport(content.ID, peer.Key(), "", now); !errors.Is(err, ErrImportConsumed) {
		t.Fatalf("second consume error = %v", err)
	}
	if err := store.RemoveImport(content.ID); err != nil {
		t.Fatalf("remove Runtime import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "imports", content.ID+".content")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import content still exists: %v", err)
	}
}

func TestExpiredImportIsCollected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	body := []byte("proxies: []\n")
	digest := sha256.Sum256(body)
	now := time.Now().UTC()
	content, err := store.UploadImport(peer, "text/yaml", int64(len(body)), hex.EncodeToString(digest[:]), body, now)
	if err != nil {
		t.Fatalf("upload Runtime import: %v", err)
	}
	if err := store.GCExpiredImports(content.ExpiresAt); err != nil {
		t.Fatalf("collect expired Runtime imports: %v", err)
	}
	if _, _, err := store.ConsumeImport(content.ID, peer.Key(), "", content.ExpiresAt); !errors.Is(err, ErrImportNotFound) {
		t.Fatalf("expired import error = %v", err)
	}
}

func TestAcceptedOperationReservesImportPastExpiry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	body := []byte("proxies: []\n")
	digest := sha256.Sum256(body)
	now := time.Now().UTC()
	content, err := store.UploadImport(peer, "text/yaml", int64(len(body)), hex.EncodeToString(digest[:]), body, now)
	if err != nil {
		t.Fatalf("upload Runtime import: %v", err)
	}
	operation, _, err := store.SubmitOperation(peer, "test", runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action: runtimeapi.Action{
			Kind:   runtimeapi.ActionApplyImportedConfig,
			Params: runtimeapi.ActionParams{ContentID: content.ID},
		},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit operation using import: %v", err)
	}
	if err := store.GCExpiredImports(content.ExpiresAt.Add(time.Hour)); err != nil {
		t.Fatalf("collect imports with queued reservation: %v", err)
	}
	consumed, _, err := store.ConsumeImport(content.ID, peer.Key(), operation.ID, content.ExpiresAt.Add(time.Hour))
	if err != nil || string(consumed) != string(body) {
		t.Fatalf("consume reserved import after expiry: body=%q err=%v", consumed, err)
	}
}

func TestConsumeImportRejectsTamperedContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	body := []byte("proxies: []\n")
	digest := sha256.Sum256(body)
	now := time.Now().UTC()
	content, err := store.UploadImport(peer, "text/yaml", int64(len(body)), hex.EncodeToString(digest[:]), body, now)
	if err != nil {
		t.Fatalf("upload Runtime import: %v", err)
	}
	path, err := store.importPath(content.ID)
	if err != nil {
		t.Fatalf("resolve Runtime import path: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered!!!\n"), 0600); err != nil {
		t.Fatalf("tamper Runtime import: %v", err)
	}

	if _, _, err := store.ConsumeImport(content.ID, peer.Key(), "", now); err == nil ||
		!strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("tampered import error = %v", err)
	}
}

func TestConsumeImportIsAtomicAcrossConcurrentCallers(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	body := []byte("proxies: []\n")
	digest := sha256.Sum256(body)
	now := time.Now().UTC()
	content, err := store.UploadImport(peer, "text/yaml", int64(len(body)), hex.EncodeToString(digest[:]), body, now)
	if err != nil {
		t.Fatalf("upload Runtime import: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, _, consumeErr := store.ConsumeImport(content.ID, peer.Key(), "", now)
			results <- consumeErr
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var succeeded, consumed int
	for result := range results {
		switch {
		case result == nil:
			succeeded++
		case errors.Is(result, ErrImportConsumed):
			consumed++
		default:
			t.Fatalf("unexpected concurrent consume error: %v", result)
		}
	}
	if succeeded != 1 || consumed != 1 {
		t.Fatalf("concurrent consume results: succeeded=%d consumed=%d", succeeded, consumed)
	}
}
