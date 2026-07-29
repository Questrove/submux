package runtimediag

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprivacy"
	"submux/internal/runtimestate"
	"submux/internal/safepath"
)

const maxBundleInputBytes = int64(64 << 20)

var ErrSensitiveConfirmationRequired = errors.New("sensitive diagnostics require explicit confirmation")

type Service struct {
	State          *runtimestate.Store
	StateRoot      string
	RuntimeVersion string
	Now            func() time.Time
}

type manifest struct {
	ProtocolVersion int                           `json:"protocol_version"`
	RuntimeVersion  string                        `json:"runtime_version"`
	CreatedAt       time.Time                     `json:"created_at"`
	Warning         string                        `json:"warning"`
	Options         runtimeapi.DiagnosticsRequest `json:"options"`
}

func (s *Service) Preview(
	ctx context.Context,
	request runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsPreview, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.DiagnosticsPreview{}, err
	}
	entries, err := s.collect(request)
	if err != nil {
		return runtimeapi.DiagnosticsPreview{}, err
	}
	items := make([]runtimeapi.DiagnosticItem, 0, len(entries))
	for name, body := range entries {
		items = append(items, runtimeapi.DiagnosticItem{
			Name:      name,
			Included:  true,
			Sensitive: sensitiveEntry(name),
			Size:      int64(len(body)),
		})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Name < items[right].Name })
	return runtimeapi.DiagnosticsPreview{
		Items:   items,
		Warning: runtimeapi.SensitiveDataWarning,
	}, nil
}

func (s *Service) Create(
	ctx context.Context,
	request runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsResult, error) {
	if err := ctx.Err(); err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	if includesSensitive(request) && !request.ConfirmSensitive {
		return runtimeapi.DiagnosticsResult{}, ErrSensitiveConfirmationRequired
	}
	entries, err := s.collect(request)
	if err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	root, err := s.diagnosticsRoot()
	if err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	now := s.now()
	name := "submux-runtime-diagnostics-" + now.Format("20060102T150405.000000000Z") + ".zip"
	finalPath := filepath.Join(root, name)
	if filepath.Dir(finalPath) != root {
		return runtimeapi.DiagnosticsResult{}, errors.New("Runtime diagnostics path escaped its managed directory")
	}
	temporary, err := os.CreateTemp(root, ".diagnostics-*.tmp")
	if err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return runtimeapi.DiagnosticsResult{}, err
	}
	archive := zip.NewWriter(temporary)
	names := make([]string, 0, len(entries))
	for entryName := range entries {
		names = append(names, entryName)
	}
	sort.Strings(names)
	for _, entryName := range names {
		header := &zip.FileHeader{Name: entryName, Method: zip.Deflate}
		header.SetModTime(now)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			_ = archive.Close()
			_ = temporary.Close()
			return runtimeapi.DiagnosticsResult{}, err
		}
		if _, err := writer.Write(entries[entryName]); err != nil {
			_ = archive.Close()
			_ = temporary.Close()
			return runtimeapi.DiagnosticsResult{}, err
		}
	}
	if err := archive.Close(); err != nil {
		_ = temporary.Close()
		return runtimeapi.DiagnosticsResult{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return runtimeapi.DiagnosticsResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	body, err := os.ReadFile(temporaryPath)
	if err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	digest := sha256.Sum256(body)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return runtimeapi.DiagnosticsResult{}, err
	}
	cleanup = false
	return runtimeapi.DiagnosticsResult{
		FileName:  name,
		Size:      int64(len(body)),
		SHA256:    hex.EncodeToString(digest[:]),
		CreatedAt: now,
	}, nil
}

func (s *Service) collect(request runtimeapi.DiagnosticsRequest) (map[string][]byte, error) {
	if s == nil || s.State == nil {
		return nil, errors.New("Runtime diagnostics state is unavailable")
	}
	snapshot, err := s.State.Observe(s.RuntimeVersion, s.now())
	if err != nil {
		return nil, err
	}
	operations, err := s.State.RecentOperations(runtimestate.MaxRetainedOperations)
	if err != nil {
		return nil, err
	}
	events, err := s.State.RecentEvents(runtimestate.MaxRetainedEvents)
	if err != nil {
		return nil, err
	}
	audit, err := s.State.RecentAudit(runtimestate.MaxRetainedAuditRecords)
	if err != nil {
		return nil, err
	}
	entries := make(map[string][]byte)
	if entries["manifest.json"], err = marshalIndented(manifest{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		RuntimeVersion:  s.RuntimeVersion,
		CreatedAt:       s.now(),
		Warning:         runtimeapi.SensitiveDataWarning,
		Options:         request,
	}); err != nil {
		return nil, err
	}
	if entries["snapshot.json"], err = marshalIndented(runtimeprivacy.SanitizeSnapshot(snapshot)); err != nil {
		return nil, err
	}
	if entries["operations.json"], err = marshalIndented(operations); err != nil {
		return nil, err
	}
	if entries["events.json"], err = marshalIndented(events); err != nil {
		return nil, err
	}
	if entries["audit.json"], err = marshalIndented(audit); err != nil {
		return nil, err
	}
	if request.IncludeNetworkInfo {
		entries["sensitive/network.json"], err = marshalIndented(map[string]any{
			"run_mode":       snapshot.RunMode,
			"mihomo_state":   snapshot.Mihomo.State,
			"desired_state":  snapshot.Mihomo.DesiredState,
			"recovery_state": snapshot.Mihomo.Recovery,
		})
		if err != nil {
			return nil, err
		}
	}
	if request.IncludeRawConfig {
		configPath := filepath.Join(s.StateRoot, "config", "current", "config.yaml")
		body, err := readRegularFile(configPath, maxBundleInputBytes)
		if err != nil {
			return nil, fmt.Errorf("read current Mihomo configuration: %w", err)
		}
		entries["sensitive/current-config.yaml"] = body
	}
	if request.IncludeFullLogs {
		logEntries, err := s.collectLogs()
		if err != nil {
			return nil, err
		}
		for name, body := range logEntries {
			entries["sensitive/logs/"+name] = body
		}
	}
	total := int64(0)
	for _, body := range entries {
		total += int64(len(body))
		if total > maxBundleInputBytes {
			return nil, errors.New("Runtime diagnostics input exceeds the hard size limit")
		}
	}
	return entries, nil
}

func (s *Service) collectLogs() (map[string][]byte, error) {
	root := filepath.Join(s.StateRoot, "logs")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	total := int64(0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		body, err := readRegularFile(filepath.Join(root, entry.Name()), maxBundleInputBytes-total)
		if err != nil {
			return nil, err
		}
		total += int64(len(body))
		result[entry.Name()] = []byte(runtimeprivacy.RedactText(string(body)))
	}
	return result, nil
}

func (s *Service) diagnosticsRoot() (string, error) {
	if s.StateRoot == "" || !filepath.IsAbs(s.StateRoot) {
		return "", errors.New("Runtime state root is invalid")
	}
	root := filepath.Join(s.StateRoot, "diagnostics")
	absolute, err := filepath.Abs(root)
	if err != nil || filepath.Dir(absolute) != filepath.Clean(s.StateRoot) {
		return "", errors.New("Runtime diagnostics root is invalid")
	}
	linked, err := safepath.ContainsLinkInExistingPath(absolute)
	if err != nil {
		return "", err
	}
	if linked {
		return "", errors.New("Runtime diagnostics root must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(absolute, 0700); err != nil {
		return "", err
	}
	return absolute, nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("file exceeds diagnostics size limit")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit {
		return nil, errors.New("diagnostics input is not a bounded regular file")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil || linked {
		return nil, errors.New("diagnostics input contains a symbolic or reparse link")
	}
	return os.ReadFile(path)
}

func marshalIndented(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(bytes.TrimSpace(body), '\n'), nil
}

func includesSensitive(request runtimeapi.DiagnosticsRequest) bool {
	return request.IncludeRawConfig || request.IncludeFullLogs || request.IncludeNetworkInfo
}

func sensitiveEntry(name string) bool {
	return strings.HasPrefix(name, "sensitive/")
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
