package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"

	"submux/internal/runtimeupdate"
	"submux/internal/safepath"
)

const (
	targetManifestSchema = 1
	maxPublishTargetSize = 600 << 20
)

type publishManifest struct {
	Schema           int             `json:"schema"`
	MetadataVersion  int64           `json:"metadata_version"`
	TargetsExpires   time.Time       `json:"targets_expires"`
	SnapshotExpires  time.Time       `json:"snapshot_expires"`
	TimestampExpires time.Time       `json:"timestamp_expires"`
	Targets          []publishTarget `json:"targets"`
}

type publishTarget struct {
	Path   string          `json:"path"`
	Source string          `json:"source"`
	Custom json.RawMessage `json:"custom"`
	Body   []byte          `json:"-"`
}

type publishedRepository struct {
	OutputDir     string
	Bundle        string
	TargetCount   int
	MetadataFiles map[string][]byte
	TargetFiles   map[string][]byte
}

func runPublish(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	privateDir := flags.String("private-dir", "", "directory containing the non-Root role keys and manifest")
	rootPath := flags.String("root", "", "trusted public root.json")
	targetsManifest := flags.String("targets-manifest", "", "signed target inputs and expiry policy")
	outputDir := flags.String("output-dir", "", "new public repository output directory")
	confirm := flags.Bool("confirm-publish", false, "confirm use of release signing keys")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "publish does not accept positional arguments")
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr, "refusing to use release signing keys without --confirm-publish")
		return 2
	}
	result, err := publishRepository(ctx, *privateDir, *rootPath, *targetsManifest, *outputDir, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "publish TUF repository: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "published TUF metadata v%d with %d targets\n",
		mustMetadataVersion(result.MetadataFiles["targets.json"]), result.TargetCount)
	fmt.Fprintf(stdout, "public repository: %s\n", result.OutputDir)
	fmt.Fprintf(stdout, "offline verification bundle: %s\n", result.Bundle)
	return 0
}

func publishRepository(
	ctx context.Context,
	privateDir string,
	rootPath string,
	targetsManifestPath string,
	outputDir string,
	now time.Time,
) (publishedRepository, error) {
	privateDir, rootPath, targetsManifestPath, outputDir, err :=
		validatePublishPaths(privateDir, rootPath, targetsManifestPath, outputDir)
	if err != nil {
		return publishedRepository{}, err
	}
	select {
	case <-ctx.Done():
		return publishedRepository{}, ctx.Err()
	default:
	}
	if err := securePrivateDirectory(privateDir); err != nil {
		return publishedRepository{}, err
	}
	private, err := loadPrivateManifest(privateDir)
	if err != nil {
		return publishedRepository{}, err
	}
	rootBody, root, err := loadTrustedRoot(rootPath, private)
	if err != nil {
		return publishedRepository{}, err
	}
	if err := validatePrivateDirectoryContents(privateDir, private); err != nil {
		return publishedRepository{}, err
	}
	publication, err := loadPublishManifest(targetsManifestPath, now)
	if err != nil {
		return publishedRepository{}, err
	}
	signers, err := loadRoleSigners(privateDir, private, root)
	if err != nil {
		return publishedRepository{}, err
	}
	metadataFiles, targetFiles, err := buildSignedRepository(root, publication, signers)
	if err != nil {
		return publishedRepository{}, err
	}
	metadataFiles["root.json"] = rootBody

	if err := os.Mkdir(outputDir, 0755); err != nil {
		return publishedRepository{}, fmt.Errorf("create public TUF output directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(outputDir)
		}
	}()
	if err := writePublishedFiles(outputDir, metadataFiles, targetFiles); err != nil {
		return publishedRepository{}, err
	}
	bundlePath := filepath.Join(outputDir, "offline-verification-bundle.zip")
	if err := writeOfflineBundle(bundlePath, metadataFiles, targetFiles); err != nil {
		return publishedRepository{}, err
	}
	cleanup = false
	return publishedRepository{
		OutputDir:     outputDir,
		Bundle:        bundlePath,
		TargetCount:   len(publication.Targets),
		MetadataFiles: metadataFiles,
		TargetFiles:   targetFiles,
	}, nil
}

func validatePublishPaths(privateDir, rootPath, manifestPath, outputDir string) (string, string, string, string, error) {
	values := []*string{&privateDir, &rootPath, &manifestPath, &outputDir}
	for _, value := range values {
		if *value == "" || !filepath.IsAbs(*value) {
			return "", "", "", "", errors.New("publish paths must be fixed absolute paths")
		}
		*value = filepath.Clean(*value)
	}
	if sameOrWithin(privateDir, outputDir) || sameOrWithin(outputDir, privateDir) {
		return "", "", "", "", errors.New("private keys and public TUF output must use separate directory trees")
	}
	for name, candidate := range map[string]string{
		"private key directory": privateDir,
		"trusted Root":          rootPath,
		"targets manifest":      manifestPath,
	} {
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", "", "", "", fmt.Errorf("inspect %s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", "", "", fmt.Errorf("%s must not be a symbolic or reparse link", name)
		}
		if name == "private key directory" {
			if !info.IsDir() {
				return "", "", "", "", errors.New("private key directory is not a directory")
			}
		} else if !info.Mode().IsRegular() {
			return "", "", "", "", fmt.Errorf("%s is not a regular file", name)
		}
	}
	if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", "", "", "", errors.New("public TUF output directory already exists")
		}
		return "", "", "", "", fmt.Errorf("inspect public TUF output directory: %w", err)
	}
	for _, candidate := range []string{privateDir, filepath.Dir(rootPath), filepath.Dir(manifestPath), filepath.Dir(outputDir)} {
		linked, err := safepath.ContainsLinkInExistingPath(candidate)
		if err != nil {
			return "", "", "", "", fmt.Errorf("inspect TUF publication path: %w", err)
		}
		if linked {
			return "", "", "", "", errors.New("TUF publication paths must not contain symbolic or reparse links")
		}
	}
	return privateDir, rootPath, manifestPath, outputDir, nil
}

func loadPrivateManifest(privateDir string) (privateManifest, error) {
	name := filepath.Join(privateDir, "manifest.json")
	body, err := os.ReadFile(name)
	if err != nil {
		return privateManifest{}, fmt.Errorf("read private key manifest: %w", err)
	}
	var manifest privateManifest
	if err := decodeStrictJSON(body, &manifest); err != nil {
		return privateManifest{}, fmt.Errorf("decode private key manifest: %w", err)
	}
	if manifest.Schema != manifestSchema {
		return privateManifest{}, fmt.Errorf("private key manifest schema must be %d", manifestSchema)
	}
	return manifest, nil
}

func validatePrivateDirectoryContents(privateDir string, private privateManifest) error {
	allowed := map[string]struct{}{"manifest.json": {}}
	roles := make(map[string]struct{}, len(private.RolePolicies))
	for _, role := range private.RolePolicies {
		if _, duplicate := roles[role.Name]; duplicate {
			return fmt.Errorf("private key manifest repeats role %q", role.Name)
		}
		roles[role.Name] = struct{}{}
		if role.Name == metadata.ROOT {
			continue
		}
		for _, key := range role.Keys {
			if !plainKeyFile(key.File) {
				return fmt.Errorf("%s key file name is unsafe", role.Name)
			}
			if _, duplicate := allowed[key.File]; duplicate {
				return fmt.Errorf("private key file %q is assigned more than once", key.File)
			}
			allowed[key.File] = struct{}{}
		}
	}
	entries, err := os.ReadDir(privateDir)
	if err != nil {
		return fmt.Errorf("inspect private key directory: %w", err)
	}
	for _, entry := range entries {
		if _, expected := allowed[entry.Name()]; !expected {
			return fmt.Errorf("private key directory contains unexpected entry %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("private key directory entry %q is not a regular non-linked file", entry.Name())
		}
	}
	return nil
}

func loadTrustedRoot(name string, private privateManifest) ([]byte, *metadata.Metadata[metadata.RootType], error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), private.RootSHA256) {
		return nil, nil, errors.New("trusted Root digest does not match the private key manifest")
	}
	root, err := metadata.Root().FromBytes(body)
	if err != nil {
		return nil, nil, fmt.Errorf("decode trusted Root: %w", err)
	}
	if err := root.VerifyDelegate(metadata.ROOT, root); err != nil {
		return nil, nil, fmt.Errorf("verify trusted Root threshold: %w", err)
	}
	return body, root, nil
}

func loadPublishManifest(name string, now time.Time) (publishManifest, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return publishManifest{}, err
	}
	var manifest publishManifest
	if err := decodeStrictJSON(body, &manifest); err != nil {
		return publishManifest{}, err
	}
	if manifest.Schema != targetManifestSchema || manifest.MetadataVersion <= 0 || len(manifest.Targets) == 0 {
		return publishManifest{}, errors.New("targets manifest schema, version, or targets are invalid")
	}
	now = now.UTC()
	for role, expires := range map[string]time.Time{
		"targets":   manifest.TargetsExpires,
		"snapshot":  manifest.SnapshotExpires,
		"timestamp": manifest.TimestampExpires,
	} {
		if expires.IsZero() || !expires.After(now.Add(24*time.Hour)) || expires.After(now.AddDate(1, 0, 0)) {
			return publishManifest{}, fmt.Errorf("%s expiry must be more than 24 hours and at most one year in the future", role)
		}
	}
	if manifest.TimestampExpires.After(now.Add(14 * 24 * time.Hour)) {
		return publishManifest{}, errors.New("timestamp expiry must not exceed 14 days")
	}
	seen := make(map[string]struct{}, len(manifest.Targets))
	for index := range manifest.Targets {
		target := &manifest.Targets[index]
		if err := validatePublishTarget(*target); err != nil {
			return publishManifest{}, fmt.Errorf("target %d: %w", index+1, err)
		}
		target.Source = filepath.Clean(target.Source)
		target.Body, err = readPublishTarget(target.Source)
		if err != nil {
			return publishManifest{}, fmt.Errorf("target %d source: %w", index+1, err)
		}
		if _, duplicate := seen[target.Path]; duplicate {
			return publishManifest{}, fmt.Errorf("target path %q is duplicated", target.Path)
		}
		seen[target.Path] = struct{}{}
	}
	return manifest, nil
}

func validatePublishTarget(target publishTarget) error {
	if target.Path == "" || path.Clean(target.Path) != target.Path ||
		strings.HasPrefix(target.Path, "/") || strings.HasPrefix(target.Path, "../") ||
		(!strings.HasPrefix(target.Path, "product/") && !strings.HasPrefix(target.Path, "mihomo/")) {
		return errors.New("path must be a safe product/ or mihomo/ target path")
	}
	if target.Source == "" || !filepath.IsAbs(target.Source) {
		return errors.New("source must be an absolute path")
	}
	info, err := os.Lstat(target.Source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > maxPublishTargetSize {
		return errors.New("source must be a bounded regular non-linked file")
	}
	var custom map[string]json.RawMessage
	if err := decodeStrictJSON(target.Custom, &custom); err != nil || len(custom) == 0 {
		return errors.New("custom target metadata must be a non-empty JSON object")
	}
	var kind string
	if value := custom["kind"]; value != nil {
		if err := json.Unmarshal(value, &kind); err != nil {
			return errors.New("custom target kind is invalid")
		}
	}
	if strings.HasPrefix(target.Path, "product/") && kind != runtimeupdate.TargetKindProduct {
		return fmt.Errorf("product target path requires custom kind %s", runtimeupdate.TargetKindProduct)
	}
	if strings.HasPrefix(target.Path, "mihomo/") && kind != runtimeupdate.TargetKindMihomo {
		return fmt.Errorf("Mihomo target path requires custom kind %s", runtimeupdate.TargetKindMihomo)
	}
	return nil
}

func readPublishTarget(name string) ([]byte, error) {
	linked, err := safepath.ContainsLinkInExistingPath(name)
	if err != nil {
		return nil, fmt.Errorf("inspect target source path: %w", err)
	}
	if linked {
		return nil, errors.New("target source path must not contain symbolic or reparse links")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maxPublishTargetSize {
		return nil, errors.New("target source must be a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxPublishTargetSize+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) ||
		int64(len(body)) != before.Size() || len(body) > maxPublishTargetSize {
		return nil, errors.New("target source changed while it was being read")
	}
	return body, nil
}

func loadRoleSigners(
	privateDir string,
	private privateManifest,
	root *metadata.Metadata[metadata.RootType],
) (map[string][]signature.Signer, error) {
	result := make(map[string][]signature.Signer, 3)
	for _, role := range private.RolePolicies {
		if role.Name == metadata.ROOT {
			continue
		}
		if role.Name != metadata.TARGETS && role.Name != metadata.SNAPSHOT && role.Name != metadata.TIMESTAMP {
			return nil, fmt.Errorf("private key manifest contains unsupported role %q", role.Name)
		}
		rootRole := root.Signed.Roles[role.Name]
		if rootRole == nil || role.Threshold != rootRole.Threshold {
			return nil, fmt.Errorf("%s policy does not match trusted Root", role.Name)
		}
		for _, key := range role.Keys {
			if !plainKeyFile(key.File) {
				return nil, fmt.Errorf("%s key file name is unsafe", role.Name)
			}
			name := filepath.Join(privateDir, key.File)
			info, err := os.Lstat(name)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			signer, keyID, err := loadPrivateSigner(name)
			if err != nil {
				return nil, fmt.Errorf("load %s signer: %w", role.Name, err)
			}
			if keyID != key.KeyID || !containsString(rootRole.KeyIDs, keyID) {
				return nil, fmt.Errorf("%s signer identity does not match trusted Root", role.Name)
			}
			result[role.Name] = append(result[role.Name], signer)
		}
		if len(result[role.Name]) < role.Threshold {
			return nil, fmt.Errorf("%s requires %d signing keys, found %d", role.Name, role.Threshold, len(result[role.Name]))
		}
	}
	for _, role := range []string{metadata.TARGETS, metadata.SNAPSHOT, metadata.TIMESTAMP} {
		if len(result[role]) == 0 {
			return nil, fmt.Errorf("private key manifest omits %s signers", role)
		}
	}
	return result, nil
}

func plainKeyFile(name string) bool {
	return name != "" && filepath.Base(name) == name && filepath.Ext(name) == ".pem"
}

func loadPrivateSigner(name string) (signature.Signer, string, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return nil, "", err
	}
	block, rest := pem.Decode(body)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("signing key is not one PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, "", errors.New("signing key is not Ed25519")
	}
	tufKey, err := metadata.KeyFromPublicKey(privateKey.Public())
	if err != nil {
		return nil, "", err
	}
	keyID, err := tufKey.ID()
	if err != nil {
		return nil, "", err
	}
	signer, err := signature.LoadSigner(privateKey, crypto.Hash(0))
	return signer, keyID, err
}

func buildSignedRepository(
	root *metadata.Metadata[metadata.RootType],
	publication publishManifest,
	signers map[string][]signature.Signer,
) (map[string][]byte, map[string][]byte, error) {
	targets := metadata.Targets(publication.TargetsExpires.UTC())
	targets.Signed.Version = publication.MetadataVersion
	targetFiles := make(map[string][]byte, len(publication.Targets))
	for _, input := range publication.Targets {
		body := input.Body
		if len(body) == 0 || len(body) > maxPublishTargetSize {
			return nil, nil, fmt.Errorf("target %s was not loaded within its size bound", input.Path)
		}
		target, err := metadata.TargetFile().FromBytes(input.Path, body, "sha256")
		if err != nil {
			return nil, nil, fmt.Errorf("describe target %s: %w", input.Path, err)
		}
		custom := json.RawMessage(bytes.Clone(input.Custom))
		target.Custom = &custom
		targets.Signed.Targets[input.Path] = target
		digest := hex.EncodeToString(target.Hashes["sha256"])
		directory, base := path.Split(input.Path)
		targetFiles[path.Join(directory, digest+"."+base)] = body
	}
	targetsBody, err := signRole(targets, signers[metadata.TARGETS])
	if err != nil {
		return nil, nil, err
	}
	if err := root.VerifyDelegate(metadata.TARGETS, targets); err != nil {
		return nil, nil, fmt.Errorf("verify Targets threshold: %w", err)
	}

	snapshot := metadata.Snapshot(publication.SnapshotExpires.UTC())
	snapshot.Signed.Version = publication.MetadataVersion
	targetsDigest := sha256.Sum256(targetsBody)
	snapshot.Signed.Meta["targets.json"] = &metadata.MetaFiles{
		Version: publication.MetadataVersion,
		Length:  int64(len(targetsBody)),
		Hashes:  metadata.Hashes{"sha256": metadata.HexBytes(targetsDigest[:])},
	}
	snapshotBody, err := signRole(snapshot, signers[metadata.SNAPSHOT])
	if err != nil {
		return nil, nil, err
	}
	if err := root.VerifyDelegate(metadata.SNAPSHOT, snapshot); err != nil {
		return nil, nil, fmt.Errorf("verify Snapshot threshold: %w", err)
	}

	timestamp := metadata.Timestamp(publication.TimestampExpires.UTC())
	timestamp.Signed.Version = publication.MetadataVersion
	snapshotDigest := sha256.Sum256(snapshotBody)
	timestamp.Signed.Meta["snapshot.json"] = &metadata.MetaFiles{
		Version: publication.MetadataVersion,
		Length:  int64(len(snapshotBody)),
		Hashes:  metadata.Hashes{"sha256": metadata.HexBytes(snapshotDigest[:])},
	}
	timestampBody, err := signRole(timestamp, signers[metadata.TIMESTAMP])
	if err != nil {
		return nil, nil, err
	}
	if err := root.VerifyDelegate(metadata.TIMESTAMP, timestamp); err != nil {
		return nil, nil, fmt.Errorf("verify Timestamp threshold: %w", err)
	}
	version := strconv.FormatInt(publication.MetadataVersion, 10)
	return map[string][]byte{
		"timestamp.json":           timestampBody,
		"snapshot.json":            snapshotBody,
		version + ".snapshot.json": snapshotBody,
		"targets.json":             targetsBody,
		version + ".targets.json":  targetsBody,
	}, targetFiles, nil
}

func signRole[T metadata.Roles](value *metadata.Metadata[T], signers []signature.Signer) ([]byte, error) {
	value.ClearSignatures()
	for _, signer := range signers {
		if _, err := value.Sign(signer); err != nil {
			return nil, err
		}
	}
	body, err := value.ToBytes(true)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func writePublishedFiles(outputDir string, metadataFiles, targetFiles map[string][]byte) error {
	metadataDir := filepath.Join(outputDir, "repository", "metadata")
	targetsDir := filepath.Join(outputDir, "repository", "targets")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(targetsDir, 0755); err != nil {
		return err
	}
	for name, body := range metadataFiles {
		if err := writeNewFile(filepath.Join(outputDir, name), body, 0644); err != nil {
			return err
		}
		if err := writeNewFile(filepath.Join(metadataDir, name), body, 0644); err != nil {
			return err
		}
	}
	for name, body := range targetFiles {
		destination := filepath.Join(targetsDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			return err
		}
		if err := writeNewFile(destination, body, 0644); err != nil {
			return err
		}
	}
	return nil
}

func writeOfflineBundle(name string, metadataFiles, targetFiles map[string][]byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	closeWithError := func(source error) error {
		zipErr := writer.Close()
		fileErr := file.Close()
		_ = os.Remove(name)
		return errors.Join(source, zipErr, fileErr)
	}
	version := mustMetadataVersion(metadataFiles["targets.json"])
	entries := map[string][]byte{
		"metadata/root.json":      metadataFiles["root.json"],
		"metadata/timestamp.json": metadataFiles["timestamp.json"],
		"metadata/" + strconv.FormatInt(version, 10) + ".snapshot.json": metadataFiles[strconv.FormatInt(version, 10)+".snapshot.json"],
		"metadata/" + strconv.FormatInt(version, 10) + ".targets.json":  metadataFiles[strconv.FormatInt(version, 10)+".targets.json"],
	}
	for targetPath, body := range targetFiles {
		entries["targets/"+targetPath] = body
	}
	names := make([]string, 0, len(entries))
	for entry := range entries {
		names = append(names, entry)
	}
	slicesSort(names)
	for _, entry := range names {
		header := &zip.FileHeader{Name: entry, Method: zip.Deflate}
		header.SetMode(0644)
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		destination, err := writer.CreateHeader(header)
		if err != nil {
			return closeWithError(err)
		}
		if _, err := destination.Write(entries[entry]); err != nil {
			return closeWithError(err)
		}
	}
	if err := writer.Close(); err != nil {
		file.Close()
		_ = os.Remove(name)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(name)
		return err
	}
	return file.Close()
}

func mustMetadataVersion(body []byte) int64 {
	var envelope struct {
		Signed struct {
			Version int64 `json:"version"`
		} `json:"signed"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	return envelope.Signed.Version
}

func decodeStrictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("JSON contains trailing data")
		}
		return err
	}
	return nil
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func slicesSort(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}
