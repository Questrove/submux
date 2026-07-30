package runtimebackup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimebackupfile"
	"submux/internal/runtimestate"
	"submux/internal/safepath"
)

const (
	FormatVersion           = 1
	MaxArchiveBytes         = runtimeapi.RuntimeBackupMaxBytes
	maxArchiveExpandedBytes = 400 << 20
	maxArchiveEntries       = 256
	maxManifestBytes        = 1 << 20
	maxRecentConfigBytes    = 16 << 20
)

var excludedState = []string{
	"logs",
	"operations",
	"events",
	"audit",
	"imports",
	"cache",
	"core binaries",
	"system accounts and operator groups",
	"network ownership and privileges",
}

var pendingMachineSettings = []string{
	"listeners",
	"run_mode",
	"tun",
	"gateway",
	"network_permissions",
}

type Service struct {
	State      *runtimestate.Store
	Root       string
	ConfigRoot string
	Now        func() time.Time

	mu sync.Mutex
}

type RestoreResult struct {
	BackupSHA256           string
	AutomaticBackupFile    string
	MachineSettingsPending bool
}

type manifest struct {
	FormatVersion        int                                  `json:"format_version"`
	CreatedAt            time.Time                            `json:"created_at"`
	RuntimeProtocol      int                                  `json:"runtime_protocol"`
	SourceInstallationID string                               `json:"source_installation_id"`
	Restorable           bool                                 `json:"restorable"`
	IncludeSecrets       bool                                 `json:"include_secrets"`
	State                archiveEntry                         `json:"state"`
	RecentConfigurations []archiveEntry                       `json:"recent_configurations"`
	MachineSettings      runtimestate.PortableMachineSettings `json:"machine_settings"`
	PendingSettings      []string                             `json:"pending_settings"`
	Excluded             []string                             `json:"excluded"`
}

type archiveEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type parsedArchive struct {
	Manifest manifest
	State    runtimestate.PortableState
	Recent   []filePayload
	SHA256   string
	Size     int64
}

func (s *Service) Preview(includeSecrets bool) (runtimeapi.BackupPreview, error) {
	if s == nil || s.State == nil {
		return runtimeapi.BackupPreview{}, errors.New("Runtime backup service is unavailable")
	}
	state, err := s.State.ExportPortableState(includeSecrets, s.now())
	if err != nil {
		return runtimeapi.BackupPreview{}, err
	}
	sourceBytes, resourceBytes, overrideBytes := portableSizes(state)
	recent, err := collectRecentConfigurations(s.ConfigRoot)
	if err != nil {
		return runtimeapi.BackupPreview{}, err
	}
	if !includeSecrets {
		recent = nil
	}
	warning := "备份是未加密的明文文件。仅在受信任的位置保存，并限制为当前所有者读取。"
	if includeSecrets {
		warning += " 本次备份包含来源地址、凭据、配置正文、私钥资源和其他敏感内容。"
	} else {
		warning += " 本次只导出脱敏清单，不含秘密内容，因此不能用于恢复。"
	}
	return runtimeapi.BackupPreview{
		FormatVersion:  FormatVersion,
		Restorable:     includeSecrets,
		IncludeSecrets: includeSecrets,
		Items: []runtimeapi.BackupItem{
			{Name: "configuration_sources", Included: true, Sensitive: includeSecrets, Count: len(state.Sources), Size: sourceBytes},
			{Name: "advanced_override", Included: state.AdvancedOverride != nil, Sensitive: includeSecrets, Count: boolCount(state.AdvancedOverride != nil), Size: overrideBytes},
			{Name: "managed_resources", Included: true, Sensitive: includeSecrets, Count: len(state.ManagedResources), Size: resourceBytes},
			{Name: "recent_good_configurations", Included: includeSecrets && len(recent) > 0, Sensitive: includeSecrets, Count: countConfigurationSets(recent), Size: entriesSize(recent)},
			{Name: "runtime_state_version", Included: true, Sensitive: false, Count: 1},
		},
		Excluded: append([]string(nil), excludedState...),
		Warning:  warning,
	}, nil
}

func (s *Service) Export(request runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error) {
	if s == nil || s.State == nil {
		return runtimeapi.BackupArchive{}, errors.New("Runtime backup service is unavailable")
	}
	if !request.ConfirmPlaintext {
		return runtimeapi.BackupArchive{}, errors.New("plaintext Runtime backup export requires explicit confirmation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exportLocked(request.IncludeSecrets, s.now())
}

func (s *Service) Inspect(body []byte, contentID string) (runtimeapi.BackupRestorePreview, error) {
	parsed, err := parseArchive(body)
	if err != nil {
		return runtimeapi.BackupRestorePreview{}, err
	}
	return runtimeapi.BackupRestorePreview{
		ContentID:                contentID,
		FormatVersion:            parsed.Manifest.FormatVersion,
		CreatedAt:                parsed.Manifest.CreatedAt,
		SourceInstallationID:     parsed.Manifest.SourceInstallationID,
		Restorable:               parsed.Manifest.Restorable,
		SourceCount:              len(parsed.State.Sources),
		ManagedResourceCount:     len(parsed.State.ManagedResources),
		HasAdvancedOverride:      parsed.State.AdvancedOverride != nil,
		RecentConfigurationCount: countConfigurationSets(parsed.Manifest.RecentConfigurations),
		MachineSettings: map[string]string{
			"run_mode":             parsed.Manifest.MachineSettings.RunMode,
			"mihomo_desired_state": parsed.Manifest.MachineSettings.MihomoDesiredState,
		},
		PendingSettings: append([]string(nil), parsed.Manifest.PendingSettings...),
		Warning:         "恢复会先创建当前状态的完整自动备份，然后整体替换可迁移状态。Mihomo 保持停止，监听、TUN、网关和网络权限必须重新确认。",
	}, nil
}

func (s *Service) Restore(
	ctx context.Context,
	body []byte,
	operationID string,
	afterReplace func(context.Context, runtimestate.PortableState) error,
) (RestoreResult, error) {
	if s == nil || s.State == nil {
		return RestoreResult{}, errors.New("Runtime backup service is unavailable")
	}
	if ctx == nil || operationID == "" {
		return RestoreResult{}, errors.New("Runtime backup restore context and operation are required")
	}
	parsed, err := parseArchive(body)
	if err != nil {
		return RestoreResult{}, err
	}
	if !parsed.Manifest.Restorable || !parsed.Manifest.IncludeSecrets || !parsed.State.Complete {
		return RestoreResult{}, errors.New("redacted Runtime backup inventories cannot be restored")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	now := s.now()
	currentState, err := s.State.ExportPortableState(true, now)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("capture current Runtime state before restore: %w", err)
	}
	automatic, err := s.exportStateLocked(currentState, true, now)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("create automatic pre-restore backup: %w", err)
	}
	automaticFile, err := s.writeAutomaticBackup(automatic)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("save automatic pre-restore backup: %w", err)
	}
	configurations, err := stageConfigurationReplacement(s.ConfigRoot, parsed.Recent, parsed.SHA256)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("stage recent Runtime configurations: %w", err)
	}
	if err := s.State.ReplacePortableState(parsed.State, operationID, now); err != nil {
		return RestoreResult{}, errors.Join(
			fmt.Errorf("replace Runtime portable state: %w", err),
			configurations.Rollback(),
		)
	}
	if afterReplace != nil {
		if err := afterReplace(ctx, parsed.State); err != nil {
			rollbackErr := s.State.ReplacePortableState(
				currentState,
				operationID+"_rollback",
				s.now(),
			)
			return RestoreResult{}, errors.Join(
				errors.New("restored Runtime state failed validation; original portable state was restored; original configurations were restored"),
				err,
				rollbackErr,
				configurations.Rollback(),
			)
		}
	}
	if err := configurations.Commit(); err != nil {
		rollbackErr := s.State.ReplacePortableState(
			currentState,
			operationID+"_rollback",
			s.now(),
		)
		return RestoreResult{}, errors.Join(
			errors.New("restored Runtime state could not be committed; original portable state was restored; original configurations were restored"),
			err,
			rollbackErr,
			configurations.Rollback(),
		)
	}
	return RestoreResult{
		BackupSHA256:           parsed.SHA256,
		AutomaticBackupFile:    filepath.Base(automaticFile),
		MachineSettingsPending: true,
	}, nil
}

func (s *Service) exportLocked(includeSecrets bool, now time.Time) (runtimeapi.BackupArchive, error) {
	state, err := s.State.ExportPortableState(includeSecrets, now)
	if err != nil {
		return runtimeapi.BackupArchive{}, err
	}
	return s.exportStateLocked(state, includeSecrets, now)
}

func (s *Service) exportStateLocked(
	state runtimestate.PortableState,
	includeSecrets bool,
	now time.Time,
) (runtimeapi.BackupArchive, error) {
	recent, err := collectRecentConfigurations(s.ConfigRoot)
	if err != nil {
		return runtimeapi.BackupArchive{}, err
	}
	if !includeSecrets {
		recent = nil
	}
	body, err := buildArchive(state, recent, now)
	if err != nil {
		return runtimeapi.BackupArchive{}, err
	}
	if len(body) == 0 || len(body) > MaxArchiveBytes {
		return runtimeapi.BackupArchive{}, errors.New("Runtime backup archive exceeds its size limit")
	}
	digest := sha256.Sum256(body)
	stamp := now.UTC().Format("20060102T150405Z")
	suffix := "redacted"
	if includeSecrets {
		suffix = "plaintext"
	}
	return runtimeapi.BackupArchive{
		FileName:       fmt.Sprintf("submux-runtime-backup-%s-%s.zip", stamp, suffix),
		Size:           int64(len(body)),
		SHA256:         hex.EncodeToString(digest[:]),
		CreatedAt:      now.UTC(),
		Restorable:     includeSecrets,
		IncludeSecrets: includeSecrets,
		Body:           body,
	}, nil
}

func buildArchive(
	state runtimestate.PortableState,
	recent []filePayload,
	now time.Time,
) ([]byte, error) {
	stateBody, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode Runtime portable state: %w", err)
	}
	stateBody = append(stateBody, '\n')
	stateDigest := sha256.Sum256(stateBody)
	recentEntries := make([]archiveEntry, 0, len(recent))
	for _, file := range recent {
		digest := sha256.Sum256(file.Body)
		recentEntries = append(recentEntries, archiveEntry{
			Name:   file.Name,
			Size:   int64(len(file.Body)),
			SHA256: hex.EncodeToString(digest[:]),
		})
	}
	meta := manifest{
		FormatVersion:        FormatVersion,
		CreatedAt:            now.UTC(),
		RuntimeProtocol:      runtimeapi.ProtocolVersion,
		SourceInstallationID: state.SourceInstallationID,
		Restorable:           state.Complete,
		IncludeSecrets:       state.Complete,
		State: archiveEntry{
			Name:   "state.json",
			Size:   int64(len(stateBody)),
			SHA256: hex.EncodeToString(stateDigest[:]),
		},
		RecentConfigurations: recentEntries,
		MachineSettings:      state.MachineSettings,
		PendingSettings:      append([]string(nil), pendingMachineSettings...),
		Excluded:             append([]string(nil), excludedState...),
	}
	manifestBody, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestBody = append(manifestBody, '\n')
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	if err := writeZipFile(writer, "manifest.json", manifestBody, now); err != nil {
		return nil, err
	}
	if err := writeZipFile(writer, "state.json", stateBody, now); err != nil {
		return nil, err
	}
	for _, file := range recent {
		if err := writeZipFile(writer, file.Name, file.Body, now); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func parseArchive(body []byte) (parsedArchive, error) {
	if len(body) == 0 || len(body) > MaxArchiveBytes {
		return parsedArchive{}, errors.New("Runtime backup archive is empty or too large")
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(reader.File) < 2 || len(reader.File) > maxArchiveEntries {
		return parsedArchive{}, errors.New("Runtime backup archive is not a bounded ZIP file")
	}
	files := make(map[string]*zip.File, len(reader.File))
	var expanded uint64
	for _, entry := range reader.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if entry.FileInfo().IsDir() || name == "" || path.Clean(name) != name ||
			strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") {
			return parsedArchive{}, errors.New("Runtime backup archive contains an unsafe path")
		}
		if _, duplicate := files[name]; duplicate {
			return parsedArchive{}, errors.New("Runtime backup archive contains duplicate entries")
		}
		expanded += entry.UncompressedSize64
		if expanded > maxArchiveExpandedBytes {
			return parsedArchive{}, errors.New("Runtime backup archive expands beyond its limit")
		}
		files[name] = entry
	}
	manifestBody, err := readZipFile(files["manifest.json"], maxManifestBytes)
	if err != nil {
		return parsedArchive{}, errors.New("Runtime backup manifest is unavailable")
	}
	var meta manifest
	if err := decodeStrictJSON(manifestBody, &meta); err != nil {
		return parsedArchive{}, errors.New("Runtime backup manifest is invalid")
	}
	if meta.FormatVersion != FormatVersion ||
		meta.RuntimeProtocol != runtimeapi.ProtocolVersion ||
		meta.CreatedAt.IsZero() ||
		meta.State.Name != "state.json" ||
		meta.Restorable != meta.IncludeSecrets {
		return parsedArchive{}, errors.New("Runtime backup manifest uses an unsupported format")
	}
	if err := validateRecentConfigurationManifest(meta.RecentConfigurations); err != nil {
		return parsedArchive{}, errors.New("Runtime backup manifest contains invalid recent configurations")
	}
	expected := map[string]archiveEntry{meta.State.Name: meta.State}
	for _, entry := range meta.RecentConfigurations {
		if _, duplicate := expected[entry.Name]; duplicate ||
			!validRecentConfigurationName(entry.Name) {
			return parsedArchive{}, errors.New("Runtime backup manifest contains duplicate or invalid entries")
		}
		expected[entry.Name] = entry
	}
	if len(files) != len(expected)+1 {
		return parsedArchive{}, errors.New("Runtime backup archive contains unlisted content")
	}
	payloads := make(map[string][]byte, len(expected))
	for name, descriptor := range expected {
		entry := files[name]
		if entry == nil || descriptor.Size <= 0 || descriptor.Size > maxArchiveExpandedBytes {
			return parsedArchive{}, errors.New("Runtime backup entry is missing or has an invalid size")
		}
		payload, err := readZipFile(entry, descriptor.Size)
		if err != nil || int64(len(payload)) != descriptor.Size {
			return parsedArchive{}, errors.New("Runtime backup entry size does not match its manifest")
		}
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != strings.ToLower(descriptor.SHA256) {
			return parsedArchive{}, errors.New("Runtime backup entry digest does not match its manifest")
		}
		payloads[name] = payload
	}
	var state runtimestate.PortableState
	if err := decodeStrictJSON(payloads[meta.State.Name], &state); err != nil {
		return parsedArchive{}, errors.New("Runtime portable state is invalid")
	}
	if state.Schema != runtimestate.PortableStateSchema ||
		state.Complete != meta.Restorable ||
		state.SourceInstallationID != meta.SourceInstallationID {
		return parsedArchive{}, errors.New("Runtime portable state does not match its manifest")
	}
	recent := make([]filePayload, 0, len(meta.RecentConfigurations))
	for _, descriptor := range meta.RecentConfigurations {
		recent = append(recent, filePayload{
			Name: descriptor.Name,
			Body: payloads[descriptor.Name],
		})
	}
	archiveDigest := sha256.Sum256(body)
	return parsedArchive{
		Manifest: meta,
		State:    state,
		Recent:   recent,
		SHA256:   hex.EncodeToString(archiveDigest[:]),
		Size:     int64(len(body)),
	}, nil
}

func (s *Service) writeAutomaticBackup(archive runtimeapi.BackupArchive) (string, error) {
	root, err := s.safeRoot()
	if err != nil {
		return "", err
	}
	name := filepath.Join(root, "before-restore-"+archive.CreatedAt.UTC().Format("20060102T150405.000000000Z")+"-"+archive.SHA256[:12]+".zip")
	return runtimebackupfile.WriteNew(name, archive.Body, MaxArchiveBytes)
}

func (s *Service) safeRoot() (string, error) {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return "", errors.New("Runtime backup root must be a fixed absolute path")
	}
	root := filepath.Clean(s.Root)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Runtime backup root must be a real directory")
	}
	linked, err := safepath.ContainsLink(root)
	if err != nil {
		return "", err
	}
	if linked {
		return "", errors.New("Runtime backup root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	if err := runtimebackupfile.RestrictDirectory(root); err != nil {
		return "", fmt.Errorf("restrict Runtime backup root permissions: %w", err)
	}
	return root, nil
}

type filePayload struct {
	Name string
	Body []byte
}

func collectRecentConfigurations(configRoot string) ([]filePayload, error) {
	if configRoot == "" {
		return nil, nil
	}
	if !filepath.IsAbs(configRoot) {
		return nil, errors.New("Runtime configuration root must be a fixed absolute path")
	}
	info, err := os.Lstat(configRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Runtime configuration root is invalid")
	}
	linked, err := safepath.ContainsLink(configRoot)
	if err != nil || linked {
		return nil, errors.New("Runtime configuration root must not contain symbolic or reparse links")
	}
	var directories []struct {
		path string
		name string
	}
	for _, name := range []string{"current", "previous-good"} {
		candidate := filepath.Join(configRoot, name)
		if info, err := os.Lstat(candidate); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			directories = append(directories, struct{ path, name string }{candidate, name})
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	historyRoot := filepath.Join(configRoot, "history")
	if entries, err := os.ReadDir(historyRoot); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".") {
				return nil, errors.New("Runtime configuration history contains an unmanaged entry")
			}
			directories = append(directories, struct{ path, name string }{
				filepath.Join(historyRoot, entry.Name()),
				path.Join("history", entry.Name()),
			})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Slice(directories, func(left, right int) bool { return directories[left].name < directories[right].name })
	var result []filePayload
	var total int64
	for _, directory := range directories {
		if linked, err := safepath.ContainsLink(directory.path); err != nil || linked {
			return nil, errors.New("Runtime configuration history contains a linked entry")
		}
		entries, err := os.ReadDir(directory.path)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, 3)
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
				(entry.Name() != "source.yaml" && entry.Name() != "config.yaml" && entry.Name() != "metadata.json") {
				return nil, errors.New("Runtime configuration history contains an unmanaged file")
			}
			filePath := filepath.Join(directory.path, entry.Name())
			info, err := os.Lstat(filePath)
			if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxRecentConfigBytes {
				return nil, errors.New("Runtime configuration history file is invalid")
			}
			body, err := os.ReadFile(filePath)
			if err != nil || int64(len(body)) != info.Size() {
				return nil, errors.New("Runtime configuration history file could not be read")
			}
			total += int64(len(body))
			if total > maxArchiveExpandedBytes {
				return nil, errors.New("Runtime configuration history exceeds the backup limit")
			}
			result = append(result, filePayload{
				Name: path.Join("recent-configurations", directory.name, entry.Name()),
				Body: body,
			})
			seen[entry.Name()] = true
		}
		if !seen["source.yaml"] || !seen["config.yaml"] || !seen["metadata.json"] {
			return nil, errors.New("Runtime configuration history entry is incomplete")
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func writeZipFile(writer *zip.Writer, name string, body []byte, now time.Time) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0600)
	header.SetModTime(now.UTC())
	target, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = target.Write(body)
	return err
}

func readZipFile(entry *zip.File, max int64) ([]byte, error) {
	if entry == nil || max <= 0 || entry.UncompressedSize64 > uint64(max) {
		return nil, errors.New("Runtime backup ZIP entry exceeds its limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, max+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > max || int64(len(body)) != int64(entry.UncompressedSize64) {
		return nil, errors.New("Runtime backup ZIP entry length is invalid")
	}
	return body, nil
}

func decodeStrictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func portableSizes(state runtimestate.PortableState) (sourceBytes, resourceBytes, overrideBytes int64) {
	for _, source := range state.Sources {
		sourceBytes += int64(len(source.Raw) + len(source.Candidate))
	}
	for _, resource := range state.ManagedResources {
		resourceBytes += int64(len(resource.Body))
	}
	if state.AdvancedOverride != nil {
		overrideBytes = int64(len(state.AdvancedOverride.Body))
	}
	return sourceBytes, resourceBytes, overrideBytes
}

func entriesSize(entries []filePayload) int64 {
	var size int64
	for _, entry := range entries {
		size += int64(len(entry.Body))
	}
	return size
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func countConfigurationSets[T interface {
	~[]filePayload | ~[]archiveEntry
}](entries T) int {
	sets := make(map[string]struct{})
	switch values := any(entries).(type) {
	case []filePayload:
		for _, entry := range values {
			sets[path.Dir(entry.Name)] = struct{}{}
		}
	case []archiveEntry:
		for _, entry := range values {
			sets[path.Dir(entry.Name)] = struct{}{}
		}
	}
	return len(sets)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
