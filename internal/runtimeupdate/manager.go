package runtimeupdate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimecore"
)

const (
	DefaultPlanTTL   = 30 * time.Minute
	DefaultBundleTTL = 30 * time.Minute
)

type OfficialSource interface {
	FetchStable(context.Context, string, string, string) (runtimecore.ReleaseBinary, error)
}

type CandidateVerifier interface {
	VerifyCandidateCore(context.Context, string, string) error
}

type Reporter func(stage string, progress int, cancellable bool) error

type Manager struct {
	Root              string
	Platform          string
	Arch              string
	Trust             *Verifier
	Official          OfficialSource
	Core              *runtimecore.Store
	CandidateVerifier CandidateVerifier
	Now               func() time.Time
	PlanTTL           time.Duration
	BundleTTL         time.Duration

	mu sync.Mutex
}

type bundleRecord struct {
	Bundle runtimeapi.MihomoUpdateBundle `json:"bundle"`
	Owner  string                        `json:"owner"`
}

type planRecord struct {
	Plan  runtimeapi.MihomoUpdatePlan `json:"plan"`
	Owner string                      `json:"owner"`
}

func (m *Manager) UploadBundle(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	expectedSize int64,
	expectedSHA256 string,
	body io.Reader,
) (runtimeapi.MihomoUpdateBundle, error) {
	if ctx == nil || body == nil {
		return runtimeapi.MihomoUpdateBundle{}, errors.New("Mihomo update bundle upload is incomplete")
	}
	if expectedSize <= 0 || expectedSize > MaxBundleBytes || !validSHA256(expectedSHA256) {
		return runtimeapi.MihomoUpdateBundle{}, errors.New("Mihomo update bundle size or SHA-256 is invalid")
	}
	owner := peer.Key()
	if owner == "" {
		return runtimeapi.MihomoUpdateBundle{}, errors.New("Mihomo update bundle caller identity is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prepare(); err != nil {
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	if err := m.gcLocked(); err != nil {
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	id, err := randomID("bundle_")
	if err != nil {
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	dir := filepath.Join(m.Root, "uploads")
	temp, err := os.CreateTemp(dir, ".bundle-")
	if err != nil {
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(body, expectedSize+1))
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return runtimeapi.MihomoUpdateBundle{}, errors.Join(copyErr, syncErr, closeErr)
	}
	if written != expectedSize {
		return runtimeapi.MihomoUpdateBundle{}, errors.New("Mihomo update bundle size does not match upload metadata")
	}
	actualDigest := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actualDigest, expectedSHA256) {
		return runtimeapi.MihomoUpdateBundle{}, errors.New("Mihomo update bundle SHA-256 does not match upload metadata")
	}
	finalPath := filepath.Join(dir, id+".zip")
	if err := os.Rename(tempName, finalPath); err != nil {
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	now := m.now()
	record := bundleRecord{
		Bundle: runtimeapi.MihomoUpdateBundle{
			ID:        id,
			Size:      written,
			SHA256:    strings.ToLower(actualDigest),
			ExpiresAt: now.Add(m.bundleTTL()),
		},
		Owner: owner,
	}
	if err := writeJSONAtomic(filepath.Join(dir, id+".json"), record); err != nil {
		_ = os.Remove(finalPath)
		return runtimeapi.MihomoUpdateBundle{}, err
	}
	return record.Bundle, nil
}

func (m *Manager) Preview(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.MihomoUpdatePreviewRequest,
) (runtimeapi.MihomoUpdatePlan, error) {
	if ctx == nil {
		return runtimeapi.MihomoUpdatePlan{}, errors.New("Mihomo update preview context is required")
	}
	owner := peer.Key()
	if owner == "" {
		return runtimeapi.MihomoUpdatePlan{}, errors.New("Mihomo update preview caller identity is required")
	}
	if err := m.validateRequest(request); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.prepare(); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	if err := m.gcLocked(); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}

	var release runtimecore.ReleaseBinary
	trust := runtimeapi.MihomoUpdateTrustTUF
	switch request.Source {
	case runtimeapi.MihomoUpdateSourceOnlineTUF:
		target, err := m.Trust.RefreshOnline(ctx, m.platform(), m.arch(), request.Version)
		if err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
		release, err = m.Official.FetchStable(ctx, target.Version, m.platform(), m.arch())
		if err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
		if err := verifyReleaseAgainstTarget(m.Trust, target, release); err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
	case runtimeapi.MihomoUpdateSourceOfflineTUF:
		record, bundlePath, err := m.bundleLocked(request.BundleID, owner)
		if err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
		target, archive, err := m.Trust.RefreshOffline(ctx, bundlePath, m.platform(), m.arch(), request.Version)
		if err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
		if err := m.Trust.VerifyTarget(target, archive); err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
		release, err = runtimecore.VerifyOfficialArchive(
			target.Version,
			m.platform(),
			m.arch(),
			target.AssetName,
			target.UpstreamSHA256,
			archive,
		)
		if err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
		if err := m.removeBundleLocked(record.Bundle.ID); err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
	case runtimeapi.MihomoUpdateSourceUpstreamOnly:
		trust = runtimeapi.MihomoUpdateTrustUpstreamOnly
		var err error
		release, err = m.Official.FetchStable(ctx, request.Version, m.platform(), m.arch())
		if err != nil {
			return runtimeapi.MihomoUpdatePlan{}, err
		}
	default:
		return runtimeapi.MihomoUpdatePlan{}, errors.New("Mihomo update source is unsupported")
	}

	planID, err := randomID("plan_")
	if err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	planDir := filepath.Join(m.Root, "plans", planID)
	if err := preparePrivateDir(planDir); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	removePlan := true
	defer func() {
		if removePlan {
			_ = os.RemoveAll(planDir)
		}
	}()
	binaryPath := filepath.Join(planDir, binaryName(m.platform()))
	if err := writePrivateFile(binaryPath, release.Data, 0700); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	if m.CandidateVerifier == nil {
		return runtimeapi.MihomoUpdatePlan{}, errors.New("candidate Mihomo core verifier is unavailable")
	}
	if err := m.CandidateVerifier.VerifyCandidateCore(ctx, binaryPath, release.Version); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, fmt.Errorf("candidate Mihomo core rejected the current configuration: %w", err)
	}
	status, err := m.Core.Status()
	if err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	now := m.now()
	plan := runtimeapi.MihomoUpdatePlan{
		PlanID:               planID,
		Source:               request.Source,
		Trust:                trust,
		Version:              release.Version,
		CurrentVersion:       status.Version,
		Platform:             m.platform(),
		Arch:                 m.arch(),
		Repository:           OfficialRepository,
		AssetName:            release.AssetName,
		AssetSize:            release.AssetSize,
		AssetSHA256:          strings.ToLower(release.AssetDigest),
		BinarySHA256:         strings.ToLower(release.BinaryDigest),
		StaticConfigVerified: true,
		ExpiresAt:            now.Add(m.planTTL()),
	}
	plan.PreviousVersion = status.PreviousVersion
	if trust == runtimeapi.MihomoUpdateTrustUpstreamOnly {
		plan.Warning = "此安装只依赖固定的 MetaCubeX/mihomo GitHub Release、HTTPS 和上游摘要，不受 Submux TUF Targets 保护；每次安装都必须重新确认。"
	}
	if err := writeJSONAtomic(filepath.Join(planDir, "plan.json"), planRecord{Plan: plan, Owner: owner}); err != nil {
		return runtimeapi.MihomoUpdatePlan{}, err
	}
	removePlan = false
	return plan, nil
}

func (m *Manager) Activate(
	ctx context.Context,
	operation runtimeapi.Operation,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	if !operation.Action.Params.Confirm {
		return nil, errors.New("Mihomo core update requires explicit operator confirmation")
	}
	m.mu.Lock()
	if err := m.prepare(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	record, binary, err := m.consumePlanLocked(
		operation.Action.Params.PlanID,
		operation.CallerIdentity,
		operation.Action.Params.Trust,
	)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := report("revalidating_candidate_core", 20, true); err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(m.Root, ".activate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	if err := os.Chmod(tempDir, 0700); err != nil {
		return nil, err
	}
	tempBinary := filepath.Join(tempDir, binaryName(m.platform()))
	if err := writePrivateFile(tempBinary, binary, 0700); err != nil {
		return nil, err
	}
	if err := m.CandidateVerifier.VerifyCandidateCore(ctx, tempBinary, record.Plan.Version); err != nil {
		return nil, fmt.Errorf("candidate Mihomo core no longer accepts the current configuration: %w", err)
	}
	if err := report("activating_core", 55, false); err != nil {
		return nil, err
	}
	before, err := m.Core.Status()
	if err != nil {
		return nil, err
	}
	if err := m.Core.Activate(ctx, runtimecore.Binary{
		Version:      record.Plan.Version,
		BinaryDigest: record.Plan.BinarySHA256,
		Data:         binary,
	}); err != nil {
		return nil, err
	}
	if err := report("verifying_core_activation", 90, false); err != nil {
		return nil, err
	}
	after, err := m.Core.Status()
	if err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		Verified:            after.Installed && after.Version == record.Plan.Version,
		CoreVersion:         after.Version,
		PreviousCoreVersion: before.Version,
		Trust:               record.Plan.Trust,
	}, nil
}

func (m *Manager) Rollback(
	ctx context.Context,
	operation runtimeapi.Operation,
	report Reporter,
) (*runtimeapi.OperationResult, error) {
	if !operation.Action.Params.Confirm {
		return nil, errors.New("Mihomo core rollback requires explicit operator confirmation")
	}
	if err := report("rolling_back_core", 45, false); err != nil {
		return nil, err
	}
	before, err := m.Core.Status()
	if err != nil {
		return nil, err
	}
	if err := m.Core.Rollback(ctx); err != nil {
		return nil, err
	}
	after, err := m.Core.Status()
	if err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		Verified:            after.Installed,
		CoreVersion:         after.Version,
		PreviousCoreVersion: before.Version,
		Trust:               runtimeapi.MihomoUpdateTrustTUF,
	}, nil
}

func (m *Manager) Status() (runtimeapi.UpdateStatus, error) {
	if m == nil || m.Core == nil {
		return runtimeapi.UpdateStatus{}, nil
	}
	status, err := m.Core.Status()
	if err != nil {
		return runtimeapi.UpdateStatus{}, err
	}
	result := runtimeapi.UpdateStatus{
		MihomoAvailable:      m.Trust != nil && len(m.Trust.InitialRoot) > 0,
		MihomoCurrentVersion: status.Version,
	}
	result.MihomoPreviousVersion = status.PreviousVersion
	return result, nil
}

func verifyReleaseAgainstTarget(verifier *Verifier, target Target, release runtimecore.ReleaseBinary) error {
	if release.Version != target.Version ||
		release.AssetName != target.AssetName ||
		release.AssetSize != target.Length ||
		!strings.EqualFold(release.AssetDigest, target.UpstreamSHA256) {
		return errors.New("official Mihomo Release does not match the signed TUF target")
	}
	return verifier.VerifyTarget(target, release.Archive)
}

func (m *Manager) validateRequest(request runtimeapi.MihomoUpdatePreviewRequest) error {
	if m == nil || m.Official == nil || m.Core == nil {
		return errors.New("Mihomo update service is unavailable")
	}
	if request.Source != runtimeapi.MihomoUpdateSourceUpstreamOnly &&
		(m.Trust == nil || len(m.Trust.InitialRoot) == 0) {
		return errors.New("embedded initial TUF Root is unavailable")
	}
	if request.Source == runtimeapi.MihomoUpdateSourceOfflineTUF {
		if !validID(request.BundleID, "bundle_") {
			return errors.New("offline Mihomo update preview requires a valid bundle_id")
		}
	} else if request.BundleID != "" {
		return errors.New("only an offline Mihomo update accepts bundle_id")
	}
	if request.Source == runtimeapi.MihomoUpdateSourceUpstreamOnly && request.Version == "" {
		return errors.New("upstream-only Mihomo update requires an exact stable version")
	}
	return nil
}

func (m *Manager) bundleLocked(id, owner string) (bundleRecord, string, error) {
	if !validID(id, "bundle_") {
		return bundleRecord{}, "", errors.New("Mihomo update bundle ID is invalid")
	}
	var record bundleRecord
	if err := readJSON(filepath.Join(m.Root, "uploads", id+".json"), &record); err != nil {
		return bundleRecord{}, "", errors.New("Mihomo update bundle is unavailable")
	}
	if record.Owner != owner || !record.Bundle.ExpiresAt.After(m.now()) {
		return bundleRecord{}, "", errors.New("Mihomo update bundle is unavailable, expired, or belongs to another caller")
	}
	bundlePath := filepath.Join(m.Root, "uploads", id+".zip")
	info, err := os.Lstat(bundlePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() != record.Bundle.Size {
		return bundleRecord{}, "", errors.New("Mihomo update bundle file is invalid")
	}
	return record, bundlePath, nil
}

func (m *Manager) consumePlanLocked(id, owner, trust string) (planRecord, []byte, error) {
	if !validID(id, "plan_") {
		return planRecord{}, nil, errors.New("Mihomo update plan ID is invalid")
	}
	dir := filepath.Join(m.Root, "plans", id)
	var record planRecord
	if err := readJSON(filepath.Join(dir, "plan.json"), &record); err != nil {
		return planRecord{}, nil, errors.New("Mihomo update plan is unavailable")
	}
	if record.Owner != owner || !record.Plan.ExpiresAt.After(m.now()) || record.Plan.Trust != trust {
		return planRecord{}, nil, errors.New("Mihomo update plan is unavailable, expired, changed, or belongs to another caller")
	}
	binaryPath := filepath.Join(dir, binaryName(record.Plan.Platform))
	info, err := os.Lstat(binaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return planRecord{}, nil, errors.New("staged Mihomo update binary is invalid")
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return planRecord{}, nil, err
	}
	sum := sha256.Sum256(binary)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), record.Plan.BinarySHA256) {
		return planRecord{}, nil, errors.New("staged Mihomo update binary SHA-256 is invalid")
	}
	if err := os.RemoveAll(dir); err != nil {
		return planRecord{}, nil, err
	}
	return record, binary, nil
}

func (m *Manager) removeBundleLocked(id string) error {
	dir := filepath.Join(m.Root, "uploads")
	return errors.Join(
		removeIfExists(filepath.Join(dir, id+".zip")),
		removeIfExists(filepath.Join(dir, id+".json")),
	)
}

func (m *Manager) prepare() error {
	if m == nil || m.Root == "" || !filepath.IsAbs(m.Root) {
		return errors.New("Mihomo update root must use a fixed absolute path")
	}
	for _, dir := range []string{m.Root, filepath.Join(m.Root, "uploads"), filepath.Join(m.Root, "plans")} {
		if err := preparePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) gcLocked() error {
	now := m.now()
	for _, location := range []struct {
		dir    string
		suffix string
	}{
		{dir: filepath.Join(m.Root, "uploads"), suffix: ".json"},
		{dir: filepath.Join(m.Root, "plans"), suffix: "plan.json"},
	} {
		entries, err := os.ReadDir(location.dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if location.suffix == ".json" {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				var record bundleRecord
				if readJSON(filepath.Join(location.dir, entry.Name()), &record) != nil ||
					!record.Bundle.ExpiresAt.After(now) {
					id := strings.TrimSuffix(entry.Name(), ".json")
					if err := m.removeBundleLocked(id); err != nil {
						return err
					}
				}
				continue
			}
			if !entry.IsDir() {
				continue
			}
			var record planRecord
			planDir := filepath.Join(location.dir, entry.Name())
			if readJSON(filepath.Join(planDir, "plan.json"), &record) != nil ||
				!record.Plan.ExpiresAt.After(now) {
				if err := os.RemoveAll(planDir); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *Manager) platform() string {
	if m.Platform != "" {
		return m.Platform
	}
	return runtime.GOOS
}

func (m *Manager) arch() string {
	if m.Arch != "" {
		return m.Arch
	}
	return runtime.GOARCH
}

func (m *Manager) now() time.Time {
	if m != nil && m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) planTTL() time.Duration {
	if m.PlanTTL > 0 {
		return m.PlanTTL
	}
	return DefaultPlanTTL
}

func (m *Manager) bundleTTL() time.Duration {
	if m.BundleTTL > 0 {
		return m.BundleTTL
	}
	return DefaultBundleTTL
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func validID(id, prefix string) bool {
	suffix, ok := strings.CutPrefix(id, prefix)
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func binaryName(platform string) string {
	if platform == "windows" {
		return "mihomo.exe"
	}
	return "mihomo"
}

func writePrivateFile(name string, body []byte, mode os.FileMode) error {
	if len(body) == 0 {
		return errors.New("refusing to write an empty Mihomo update file")
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
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

func writeJSONAtomic(name string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writePrivateAtomic(name, append(body, '\n'))
}

func readJSON(name string, value any) error {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Mihomo update metadata file is invalid")
	}
	body, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("Mihomo update metadata is invalid")
	}
	return nil
}

func removeIfExists(name string) error {
	err := os.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
