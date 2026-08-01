package runtimeupdate

import (
	"archive/zip"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

type testRepository struct {
	root      *metadata.Metadata[metadata.RootType]
	targets   *metadata.Metadata[metadata.TargetsType]
	snapshot  *metadata.Metadata[metadata.SnapshotType]
	timestamp *metadata.Metadata[metadata.TimestampType]
	signers   map[string][]signature.Signer
}

func TestOfflineVerifierUsesFullTUFWorkflowAndVerifiesTarget(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t, time.Now().Add(24*time.Hour), 1)
	body := []byte("official-mihomo-archive")
	targetPath := addTestTarget(t, repository, body, "v1.2.3")
	root, entries := repository.signedEntries(t)
	bundle := writeBundle(t, entries, targetPath, body)

	verifier := &Verifier{
		InitialRoot: root,
		StateRoot:   filepath.Join(t.TempDir(), "trust"),
	}
	target, verified, err := verifier.RefreshOffline(t.Context(), bundle, "linux", "amd64", "")
	if err != nil {
		t.Fatalf("verify offline bundle: %v", err)
	}
	if target.Version != "v1.2.3" || target.Repository != OfficialRepository ||
		target.AssetName != "mihomo-linux-amd64-compatible-v1.2.3.gz" ||
		string(verified) != string(body) {
		t.Fatalf("unexpected verified target: %#v %q", target, verified)
	}
	if err := verifier.VerifyTarget(target, verified); err != nil {
		t.Fatalf("verify signed target bytes: %v", err)
	}
	targetCache, err := os.ReadDir(filepath.Join(verifier.StateRoot, "targets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(targetCache) != 0 {
		t.Fatalf("offline verification persisted target bytes: %#v", targetCache)
	}
	if _, _, err := verifier.RefreshOffline(t.Context(), bundle, "linux", "amd64", "v1.2.3"); err != nil {
		t.Fatalf("repeat the same verified metadata without rollback: %v", err)
	}
}

func TestOfflineVerifierRejectsExpiredMetadata(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t, time.Now().Add(-time.Hour), 1)
	body := []byte("official-mihomo-archive")
	targetPath := addTestTarget(t, repository, body, "v1.2.3")
	root, entries := repository.signedEntries(t)
	bundle := writeBundle(t, entries, targetPath, body)

	verifier := &Verifier{InitialRoot: root, StateRoot: filepath.Join(t.TempDir(), "trust")}
	_, _, err := verifier.RefreshOffline(t.Context(), bundle, "linux", "amd64", "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "expired") {
		t.Fatalf("expected expired metadata rejection, got %v", err)
	}
}

func TestOfflineVerifierRejectsThresholdFailure(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t, time.Now().Add(24*time.Hour), 2)
	body := []byte("official-mihomo-archive")
	targetPath := addTestTarget(t, repository, body, "v1.2.3")
	root, entries := repository.signedEntries(t)
	bundle := writeBundle(t, entries, targetPath, body)

	verifier := &Verifier{InitialRoot: root, StateRoot: filepath.Join(t.TempDir(), "trust")}
	_, _, err := verifier.RefreshOffline(t.Context(), bundle, "linux", "amd64", "")
	if err == nil || (!strings.Contains(strings.ToLower(err.Error()), "threshold") &&
		!strings.Contains(strings.ToLower(err.Error()), "not enough signatures")) {
		t.Fatalf("expected signature threshold rejection, got %v", err)
	}
}

func TestOfflineVerifierRejectsMetadataRollback(t *testing.T) {
	t.Parallel()
	expires := time.Now().Add(24 * time.Hour)
	newer := newTestRepository(t, expires, 1)
	newer.targets.Signed.Version = 2
	newer.snapshot.Signed.Version = 2
	newer.timestamp.Signed.Version = 2
	body := []byte("official-mihomo-archive")
	targetPath := addTestTarget(t, newer, body, "v1.2.3")
	root, entries := newer.signedEntries(t)
	newerBundle := writeBundle(t, entries, targetPath, body)

	stateRoot := filepath.Join(t.TempDir(), "trust")
	verifier := &Verifier{InitialRoot: root, StateRoot: stateRoot}
	if _, _, err := verifier.RefreshOffline(t.Context(), newerBundle, "linux", "amd64", ""); err != nil {
		t.Fatalf("accept newer metadata: %v", err)
	}

	newer.targets.Signed.Version = 1
	newer.snapshot.Signed.Version = 1
	newer.timestamp.Signed.Version = 1
	_, olderEntries := newer.signedEntries(t)
	olderBundle := writeBundle(t, olderEntries, targetPath, body)
	if _, _, err := verifier.RefreshOffline(t.Context(), olderBundle, "linux", "amd64", ""); err == nil ||
		(!strings.Contains(strings.ToLower(err.Error()), "rollback") &&
			!strings.Contains(strings.ToLower(err.Error()), "version")) {
		t.Fatalf("expected rollback rejection, got %v", err)
	}
}

func TestOfflineVerifierRejectsTargetDigestMismatch(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t, time.Now().Add(24*time.Hour), 1)
	body := []byte("official-mihomo-archive")
	targetPath := addTestTarget(t, repository, body, "v1.2.3")
	root, entries := repository.signedEntries(t)
	bundle := writeBundleWithSignedBody(
		t,
		entries,
		targetPath,
		[]byte("tampered-mihomo-archive"),
		body,
	)

	verifier := &Verifier{InitialRoot: root, StateRoot: filepath.Join(t.TempDir(), "trust")}
	_, _, err := verifier.RefreshOffline(t.Context(), bundle, "linux", "amd64", "")
	if err == nil || (!strings.Contains(strings.ToLower(err.Error()), "length") &&
		!strings.Contains(strings.ToLower(err.Error()), "hash")) {
		t.Fatalf("expected target verification failure, got %v", err)
	}
}

func TestOfflineProductVerifierUsesSameTUFWorkflow(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t, time.Now().Add(24*time.Hour), 1)
	body := []byte("signed-runtime-product-package")
	targetPath := addProductTestTarget(t, repository, body, "v2.3.4")
	root, entries := repository.signedEntries(t)
	bundle := writeBundle(t, entries, targetPath, body)

	verifier := &Verifier{
		InitialRoot: root,
		StateRoot:   filepath.Join(t.TempDir(), "trust"),
	}
	target, verified, err := verifier.RefreshProductOffline(
		t.Context(),
		bundle,
		"linux",
		"amd64",
		"",
	)
	if err != nil {
		t.Fatalf("verify offline Runtime product bundle: %v", err)
	}
	if target.Version != "v2.3.4" ||
		target.Channel != ProductChannelStable ||
		target.RuntimeSchemaTarget != 1 ||
		target.ProtocolMin != 1 ||
		string(verified) != string(body) {
		t.Fatalf("unexpected verified Runtime product target: %#v %q", target, verified)
	}
	if err := verifier.VerifyProductTarget(target, verified); err != nil {
		t.Fatalf("verify signed Runtime product bytes: %v", err)
	}
}

func TestBundleRejectsTraversalAndDuplicateEntries(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		entries []zipEntry
	}{
		{name: "traversal", entries: []zipEntry{{name: "metadata/../evil", body: []byte("x")}}},
		{name: "duplicate", entries: []zipEntry{
			{name: "metadata/root.json", body: []byte("x")},
			{name: "metadata/root.json", body: []byte("y")},
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bundle := writeRawBundle(t, testCase.entries)
			if _, err := newBundleFetcher(bundle); err == nil {
				t.Fatal("expected unsafe bundle rejection")
			}
		})
	}
}

func newTestRepository(t *testing.T, expires time.Time, targetsThreshold int) *testRepository {
	t.Helper()
	repository := &testRepository{
		root:      metadata.Root(expires),
		targets:   metadata.Targets(expires),
		snapshot:  metadata.Snapshot(expires),
		timestamp: metadata.Timestamp(expires),
		signers:   make(map[string][]signature.Signer),
	}
	for _, role := range metadata.TOP_LEVEL_ROLE_NAMES {
		count := 1
		if role == metadata.TARGETS && targetsThreshold > 1 {
			count = targetsThreshold
		}
		for index := 0; index < count; index++ {
			signer, key := newSigner(t)
			if err := repository.root.Signed.AddKey(key, role); err != nil {
				t.Fatalf("add %s key: %v", role, err)
			}
			repository.signers[role] = append(repository.signers[role], signer)
		}
	}
	repository.root.Signed.Roles[metadata.TARGETS].Threshold = targetsThreshold
	return repository
}

func addTestTarget(
	t *testing.T,
	repository *testRepository,
	body []byte,
	version string,
) string {
	t.Helper()
	asset := "mihomo-linux-amd64-compatible-" + version + ".gz"
	targetPath := "mihomo/" + version + "/linux/amd64/" + asset
	target, err := metadata.TargetFile().FromBytes(targetPath, body, "sha256")
	if err != nil {
		t.Fatalf("create target metadata: %v", err)
	}
	sum := sha256.Sum256(body)
	custom, err := json.Marshal(targetCustom{
		Kind:           TargetKindMihomo,
		Version:        version,
		Platform:       "linux",
		Arch:           "amd64",
		Repository:     OfficialRepository,
		AssetName:      asset,
		UpstreamSHA256: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(custom)
	target.Custom = &raw
	repository.targets.Signed.Targets[targetPath] = target
	return targetPath
}

func addProductTestTarget(
	t *testing.T,
	repository *testRepository,
	body []byte,
	version string,
) string {
	t.Helper()
	asset := "submux-runtime-" + version + "-linux-amd64.zip"
	targetPath := "product/" + version + "/linux/amd64/" + asset
	target, err := metadata.TargetFile().FromBytes(targetPath, body, "sha256")
	if err != nil {
		t.Fatalf("create Runtime product target metadata: %v", err)
	}
	custom, err := json.Marshal(productTargetCustom{
		Kind:                TargetKindProduct,
		Version:             version,
		Platform:            "linux",
		Arch:                "amd64",
		Channel:             ProductChannelStable,
		AssetName:           asset,
		ReleaseNotes:        "Stable Runtime product update.",
		MigrationSummary:    "No database migration is required.",
		RuntimeSchemaMin:    1,
		RuntimeSchemaMax:    1,
		RuntimeSchemaTarget: 1,
		ProtocolMin:         1,
		ProtocolMax:         1,
		RequiredFreeBytes:   int64(len(body)) * 4,
		Components: []string{
			"submux-runtime",
			"submux-runtime-net",
			"submux-runtime-gui",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(custom)
	target.Custom = &raw
	repository.targets.Signed.Targets[targetPath] = target
	return targetPath
}

func (r *testRepository) signedEntries(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	targets := signTestMetadata(t, r.targets, r.signers[metadata.TARGETS][:1])
	targetsSum := sha256.Sum256(targets)
	r.snapshot.Signed.Meta["targets.json"] = &metadata.MetaFiles{
		Version: r.targets.Signed.Version,
		Length:  int64(len(targets)),
		Hashes:  metadata.Hashes{"sha256": metadata.HexBytes(targetsSum[:])},
	}
	snapshot := signTestMetadata(t, r.snapshot, r.signers[metadata.SNAPSHOT])
	snapshotSum := sha256.Sum256(snapshot)
	r.timestamp.Signed.Meta["snapshot.json"] = &metadata.MetaFiles{
		Version: r.snapshot.Signed.Version,
		Length:  int64(len(snapshot)),
		Hashes:  metadata.Hashes{"sha256": metadata.HexBytes(snapshotSum[:])},
	}
	timestamp := signTestMetadata(t, r.timestamp, r.signers[metadata.TIMESTAMP])
	root := signTestMetadata(t, r.root, r.signers[metadata.ROOT])
	return root, map[string][]byte{
		"metadata/timestamp.json": timestamp,
		"metadata/" + intString(r.snapshot.Signed.Version) + ".snapshot.json": snapshot,
		"metadata/" + intString(r.targets.Signed.Version) + ".targets.json":   targets,
	}
}

func newSigner(t *testing.T) (signature.Signer, *metadata.Key) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signature.LoadSigner(private, crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	key, err := metadata.KeyFromPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return signer, key
}

func signTestMetadata[T metadata.Roles](
	t *testing.T,
	value *metadata.Metadata[T],
	signers []signature.Signer,
) []byte {
	t.Helper()
	value.ClearSignatures()
	for _, signer := range signers {
		if _, err := value.Sign(signer); err != nil {
			t.Fatal(err)
		}
	}
	body, err := value.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func writeBundle(
	t *testing.T,
	metadataEntries map[string][]byte,
	targetPath string,
	target []byte,
) string {
	return writeBundleWithSignedBody(t, metadataEntries, targetPath, target, target)
}

func writeBundleWithSignedBody(
	t *testing.T,
	metadataEntries map[string][]byte,
	targetPath string,
	target []byte,
	signedBody []byte,
) string {
	t.Helper()
	sum := sha256.Sum256(signedBody)
	parts := strings.Split(targetPath, "/")
	parts[len(parts)-1] = hex.EncodeToString(sum[:]) + "." + parts[len(parts)-1]
	entries := make([]zipEntry, 0, len(metadataEntries)+1)
	for name, body := range metadataEntries {
		entries = append(entries, zipEntry{name: name, body: body})
	}
	entries = append(entries, zipEntry{name: "targets/" + strings.Join(parts, "/"), body: target})
	return writeRawBundle(t, entries)
}

type zipEntry struct {
	name string
	body []byte
}

func writeRawBundle(t *testing.T, entries []zipEntry) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "offline.zip")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range entries {
		writer, err := archive.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func intString(value int64) string {
	return strconv.FormatInt(value, 10)
}
