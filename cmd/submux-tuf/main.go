package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
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
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"

	"submux/internal/safepath"
)

const (
	rootLifetimeYears = 10
	manifestSchema    = 1
)

type rolePolicy struct {
	Name      string
	KeyCount  int
	Threshold int
}

var initialRolePolicies = []rolePolicy{
	{Name: metadata.ROOT, KeyCount: 3, Threshold: 2},
	{Name: metadata.TARGETS, KeyCount: 3, Threshold: 2},
	{Name: metadata.SNAPSHOT, KeyCount: 1, Threshold: 1},
	{Name: metadata.TIMESTAMP, KeyCount: 1, Threshold: 1},
}

type generatedKey struct {
	Role       string
	Index      int
	KeyID      string
	PrivateKey ed25519.PrivateKey
	File       string
}

type privateManifest struct {
	Schema       int            `json:"schema"`
	CreatedAt    time.Time      `json:"created_at"`
	RootExpires  time.Time      `json:"root_expires"`
	RootSHA256   string         `json:"root_sha256"`
	PublicRoot   string         `json:"public_root"`
	RolePolicies []roleManifest `json:"roles"`
}

type roleManifest struct {
	Name      string        `json:"name"`
	Threshold int           `json:"threshold"`
	Keys      []keyManifest `json:"keys"`
}

type keyManifest struct {
	KeyID string `json:"key_id"`
	File  string `json:"file"`
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "bootstrap-root" {
		fmt.Fprintln(stderr, "usage: submux-tuf bootstrap-root --private-dir ABS --public-root ABS --confirm-new-root")
		return 2
	}
	flags := flag.NewFlagSet("bootstrap-root", flag.ContinueOnError)
	flags.SetOutput(stderr)
	privateDir := flags.String("private-dir", "", "new directory outside the source tree for private signing keys")
	publicRoot := flags.String("public-root", "", "new root.json output path")
	confirm := flags.Bool("confirm-new-root", false, "confirm creation of a new TUF trust root")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "bootstrap-root does not accept positional arguments")
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr, "refusing to create signing keys without --confirm-new-root")
		return 2
	}
	result, err := bootstrapRoot(*privateDir, *publicRoot, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "bootstrap TUF Root: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created TUF Root v1: sha256=%s expires=%s\n", result.RootSHA256, result.Expires.Format(time.RFC3339))
	fmt.Fprintf(stdout, "private signing keys: %s\n", result.PrivateDir)
	fmt.Fprintf(stdout, "public root metadata: %s\n", result.PublicRoot)
	return 0
}

type bootstrapResult struct {
	RootSHA256 string
	Expires    time.Time
	PrivateDir string
	PublicRoot string
}

func bootstrapRoot(privateDir, publicRoot string, now time.Time) (bootstrapResult, error) {
	privateDir, publicRoot, err := validateDestinations(privateDir, publicRoot)
	if err != nil {
		return bootstrapResult{}, err
	}
	expires := now.UTC().Truncate(time.Second).AddDate(rootLifetimeYears, 0, 0)
	root, keys, err := buildInitialRoot(expires)
	if err != nil {
		return bootstrapResult{}, err
	}
	rootBytes, err := root.ToBytes(true)
	if err != nil {
		return bootstrapResult{}, fmt.Errorf("encode public Root: %w", err)
	}
	rootBytes = append(rootBytes, '\n')
	rootDigest := sha256.Sum256(rootBytes)
	rootSHA256 := hex.EncodeToString(rootDigest[:])
	manifest := makeManifest(now.UTC().Truncate(time.Second), expires, rootSHA256, publicRoot, keys)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return bootstrapResult{}, fmt.Errorf("encode private manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	if err := os.Mkdir(privateDir, 0700); err != nil {
		return bootstrapResult{}, fmt.Errorf("create private key directory: %w", err)
	}
	written := make([]string, 0, len(keys)+1)
	cleanup := func() {
		for index := len(written) - 1; index >= 0; index-- {
			_ = os.Remove(written[index])
		}
		_ = os.Remove(privateDir)
	}
	if err := securePrivateDirectory(privateDir); err != nil {
		cleanup()
		return bootstrapResult{}, err
	}
	for _, key := range keys {
		name := filepath.Join(privateDir, key.File)
		body, marshalErr := marshalPrivateKey(key.PrivateKey)
		if marshalErr != nil {
			cleanup()
			return bootstrapResult{}, marshalErr
		}
		if writeErr := writeNewFile(name, body, 0600); writeErr != nil {
			cleanup()
			return bootstrapResult{}, fmt.Errorf("write private signing key: %w", writeErr)
		}
		written = append(written, name)
	}
	manifestPath := filepath.Join(privateDir, "manifest.json")
	if err := writeNewFile(manifestPath, manifestBytes, 0600); err != nil {
		cleanup()
		return bootstrapResult{}, fmt.Errorf("write private key manifest: %w", err)
	}
	written = append(written, manifestPath)
	if err := writeNewFile(publicRoot, rootBytes, 0644); err != nil {
		cleanup()
		return bootstrapResult{}, fmt.Errorf("write public Root: %w", err)
	}
	return bootstrapResult{
		RootSHA256: rootSHA256,
		Expires:    expires,
		PrivateDir: privateDir,
		PublicRoot: publicRoot,
	}, nil
}

func validateDestinations(privateDir, publicRoot string) (string, string, error) {
	if privateDir == "" || publicRoot == "" || !filepath.IsAbs(privateDir) || !filepath.IsAbs(publicRoot) {
		return "", "", errors.New("private-dir and public-root must be fixed absolute paths")
	}
	privateDir = filepath.Clean(privateDir)
	publicRoot = filepath.Clean(publicRoot)
	if filepath.Base(publicRoot) != "root.json" {
		return "", "", errors.New("public-root must end in root.json")
	}
	if sameOrWithin(privateDir, publicRoot) || sameOrWithin(filepath.Dir(publicRoot), privateDir) {
		return "", "", errors.New("private signing keys and the public Root must use separate directory trees")
	}
	if _, err := os.Lstat(privateDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", "", errors.New("private key directory already exists")
		}
		return "", "", fmt.Errorf("inspect private key directory: %w", err)
	}
	if _, err := os.Lstat(publicRoot); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", "", errors.New("public Root already exists")
		}
		return "", "", fmt.Errorf("inspect public Root: %w", err)
	}
	publicParent := filepath.Dir(publicRoot)
	info, err := os.Lstat(publicParent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("public Root parent must be an existing real directory")
	}
	for _, existing := range []string{filepath.Dir(privateDir), publicParent} {
		linked, inspectErr := safepath.ContainsLinkInExistingPath(existing)
		if inspectErr != nil {
			return "", "", fmt.Errorf("inspect output path: %w", inspectErr)
		}
		if linked {
			return "", "", errors.New("TUF ceremony output paths must not contain symbolic or reparse links")
		}
	}
	return privateDir, publicRoot, nil
}

func sameOrWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func buildInitialRoot(expires time.Time) (*metadata.Metadata[metadata.RootType], []generatedKey, error) {
	root := metadata.Root(expires.UTC())
	keys := make([]generatedKey, 0, 8)
	for _, policy := range initialRolePolicies {
		root.Signed.Roles[policy.Name].Threshold = policy.Threshold
		for index := 1; index <= policy.KeyCount; index++ {
			_, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return nil, nil, fmt.Errorf("generate %s signing key: %w", policy.Name, err)
			}
			tufKey, err := metadata.KeyFromPublicKey(privateKey.Public())
			if err != nil {
				return nil, nil, fmt.Errorf("convert %s public key: %w", policy.Name, err)
			}
			if err := root.Signed.AddKey(tufKey, policy.Name); err != nil {
				return nil, nil, fmt.Errorf("add %s public key: %w", policy.Name, err)
			}
			keyID, err := tufKey.ID()
			if err != nil {
				return nil, nil, fmt.Errorf("identify %s public key: %w", policy.Name, err)
			}
			keys = append(keys, generatedKey{
				Role:       policy.Name,
				Index:      index,
				KeyID:      keyID,
				PrivateKey: privateKey,
				File:       fmt.Sprintf("%s-%d-%s.pem", policy.Name, index, keyID[:12]),
			})
		}
	}
	for _, key := range keys {
		if key.Role != metadata.ROOT {
			continue
		}
		signer, err := signature.LoadSigner(key.PrivateKey, crypto.Hash(0))
		if err != nil {
			return nil, nil, fmt.Errorf("load Root signer: %w", err)
		}
		if _, err := root.Sign(signer); err != nil {
			return nil, nil, fmt.Errorf("sign Root metadata: %w", err)
		}
	}
	if err := root.VerifyDelegate(metadata.ROOT, root); err != nil {
		return nil, nil, fmt.Errorf("verify signed Root threshold: %w", err)
	}
	return root, keys, nil
}

func makeManifest(
	createdAt time.Time,
	expires time.Time,
	rootSHA256 string,
	publicRoot string,
	keys []generatedKey,
) privateManifest {
	manifest := privateManifest{
		Schema:       manifestSchema,
		CreatedAt:    createdAt,
		RootExpires:  expires,
		RootSHA256:   rootSHA256,
		PublicRoot:   publicRoot,
		RolePolicies: make([]roleManifest, 0, len(initialRolePolicies)),
	}
	for _, policy := range initialRolePolicies {
		role := roleManifest{Name: policy.Name, Threshold: policy.Threshold}
		for _, key := range keys {
			if key.Role == policy.Name {
				role.Keys = append(role.Keys, keyManifest{KeyID: key.KeyID, File: key.File})
			}
		}
		manifest.RolePolicies = append(manifest.RolePolicies, role)
	}
	return manifest
}

func marshalPrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode private signing key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func writeNewFile(name string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
