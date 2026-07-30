package runtimebackup

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"submux/internal/owneracl"
	"submux/internal/safepath"
)

type configurationReplacement struct {
	root        string
	holder      string
	previous    string
	hadPrevious bool
	noop        bool
}

const configurationCommitMarker = ".committed"

func stageConfigurationReplacement(
	root string,
	recent []filePayload,
	archiveSHA256 string,
) (*configurationReplacement, error) {
	if root == "" {
		return &configurationReplacement{noop: true}, nil
	}
	if !filepath.IsAbs(root) || len(archiveSHA256) != 64 {
		return nil, errors.New("Runtime configuration restore target is invalid")
	}
	root = filepath.Clean(root)
	if root == filepath.VolumeName(root)+string(filepath.Separator) {
		return nil, errors.New("Runtime configuration restore target is too broad")
	}
	parent := filepath.Dir(root)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Runtime configuration restore parent is invalid")
	}
	linked, err := safepath.ContainsLink(parent)
	if err != nil || linked {
		return nil, errors.New("Runtime configuration restore path must not contain symbolic or reparse links")
	}
	hadPrevious := false
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("Runtime configuration restore target is unmanaged")
		}
		linked, err := safepath.ContainsLink(root)
		if err != nil || linked {
			return nil, errors.New("Runtime configuration restore target contains a linked entry")
		}
		hadPrevious = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	stage, err := os.MkdirTemp(parent, ".submux-config-restore-")
	if err != nil {
		return nil, err
	}
	stageLive := true
	defer func() {
		if stageLive {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := restrictConfigurationDirectory(stage); err != nil {
		return nil, err
	}
	destinations := make(map[string]struct{}, len(recent))
	for _, file := range recent {
		relative, err := portableConfigurationName(file.Name, archiveSHA256)
		if err != nil {
			return nil, err
		}
		if _, duplicate := destinations[relative]; duplicate {
			return nil, errors.New("Runtime backup maps multiple configurations to one restore target")
		}
		destinations[relative] = struct{}{}
		target := filepath.Join(stage, filepath.FromSlash(relative))
		if !strings.HasPrefix(target, stage+string(filepath.Separator)) {
			return nil, errors.New("Runtime configuration restore entry escaped its staging root")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return nil, err
		}
		if err := restrictConfigurationAncestors(stage, filepath.Dir(target)); err != nil {
			return nil, err
		}
		if err := writeConfigurationFile(target, file.Body); err != nil {
			return nil, err
		}
	}

	holder, err := os.MkdirTemp(parent, ".submux-config-before-restore-")
	if err != nil {
		return nil, err
	}
	holderLive := true
	defer func() {
		if holderLive {
			_ = os.RemoveAll(holder)
		}
	}()
	if err := restrictConfigurationDirectory(holder); err != nil {
		return nil, err
	}
	previous := filepath.Join(holder, "config")
	if hadPrevious {
		if err := os.Rename(root, previous); err != nil {
			return nil, fmt.Errorf("preserve current Runtime configurations: %w", err)
		}
	}
	if err := os.Rename(stage, root); err != nil {
		if hadPrevious {
			_ = os.Rename(previous, root)
		}
		return nil, fmt.Errorf("activate restored Runtime configuration staging area: %w", err)
	}
	stageLive = false
	holderLive = false
	return &configurationReplacement{
		root:        root,
		holder:      holder,
		previous:    previous,
		hadPrevious: hadPrevious,
	}, nil
}

func (replacement *configurationReplacement) Rollback() error {
	if replacement == nil || replacement.noop {
		return nil
	}
	if err := removeConfigurationTree(replacement.root); err != nil {
		return fmt.Errorf("remove failed restored Runtime configurations: %w", err)
	}
	if replacement.hadPrevious {
		if err := os.Rename(replacement.previous, replacement.root); err != nil {
			return fmt.Errorf("restore original Runtime configurations: %w", err)
		}
	}
	if err := os.RemoveAll(replacement.holder); err != nil {
		return fmt.Errorf("remove Runtime configuration rollback holder: %w", err)
	}
	replacement.noop = true
	return nil
}

func (replacement *configurationReplacement) Commit() error {
	if replacement == nil || replacement.noop {
		return nil
	}
	if err := replacement.markCommitted(); err != nil {
		return err
	}
	if err := os.RemoveAll(replacement.holder); err != nil {
		return fmt.Errorf("remove replaced Runtime configurations: %w", err)
	}
	replacement.noop = true
	return nil
}

func (replacement *configurationReplacement) markCommitted() error {
	return writeConfigurationFile(
		filepath.Join(replacement.holder, configurationCommitMarker),
		[]byte("committed\n"),
	)
}

func RecoverConfigurationReplacement(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("Runtime configuration recovery target must be a fixed absolute path")
	}
	root = filepath.Clean(root)
	parent := filepath.Dir(root)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var rollbackHolders []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidate := filepath.Join(parent, entry.Name())
		switch {
		case strings.HasPrefix(entry.Name(), ".submux-config-restore-"):
			if err := removeConfigurationTree(candidate); err != nil {
				return fmt.Errorf("remove abandoned Runtime configuration staging area: %w", err)
			}
		case strings.HasPrefix(entry.Name(), ".submux-config-before-restore-"):
			linked, err := safepath.ContainsLink(candidate)
			if err != nil || linked {
				return errors.New("Runtime configuration recovery holder contains a linked entry")
			}
			marker := filepath.Join(candidate, configurationCommitMarker)
			if info, err := os.Lstat(marker); err == nil {
				if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
					return errors.New("Runtime configuration recovery marker is invalid")
				}
				if err := os.RemoveAll(candidate); err != nil {
					return fmt.Errorf("remove committed Runtime configuration recovery holder: %w", err)
				}
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			previous := filepath.Join(candidate, "config")
			if info, err := os.Lstat(previous); err == nil {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return errors.New("Runtime configuration recovery snapshot is invalid")
				}
				rollbackHolders = append(rollbackHolders, candidate)
			} else if errors.Is(err, os.ErrNotExist) {
				if err := os.RemoveAll(candidate); err != nil {
					return err
				}
			} else {
				return err
			}
		}
	}
	if len(rollbackHolders) > 1 {
		return errors.New("multiple unfinished Runtime configuration replacements require manual recovery")
	}
	if len(rollbackHolders) == 0 {
		return nil
	}
	holder := rollbackHolders[0]
	if err := removeConfigurationTree(root); err != nil {
		return fmt.Errorf("remove interrupted Runtime configuration replacement: %w", err)
	}
	if err := os.Rename(filepath.Join(holder, "config"), root); err != nil {
		return fmt.Errorf("restore Runtime configurations after interrupted replacement: %w", err)
	}
	return os.RemoveAll(holder)
}

func validRecentConfigurationName(name string) bool {
	_, err := exactConfigurationName(name)
	return err == nil
}

func validateRecentConfigurationManifest(entries []archiveEntry) error {
	sets := make(map[string]map[string]struct{})
	for _, entry := range entries {
		relative, err := exactConfigurationName(entry.Name)
		if err != nil {
			return err
		}
		directory, file := path.Split(relative)
		directory = strings.TrimSuffix(directory, "/")
		if sets[directory] == nil {
			sets[directory] = make(map[string]struct{}, 3)
		}
		if _, duplicate := sets[directory][file]; duplicate {
			return errors.New("Runtime backup repeats a recent configuration file")
		}
		sets[directory][file] = struct{}{}
	}
	for _, files := range sets {
		if len(files) != 3 {
			return errors.New("Runtime backup contains an incomplete recent configuration")
		}
		for _, name := range []string{"source.yaml", "config.yaml", "metadata.json"} {
			if _, ok := files[name]; !ok {
				return errors.New("Runtime backup contains an incomplete recent configuration")
			}
		}
	}
	return nil
}

func exactConfigurationName(name string) (string, error) {
	const prefix = "recent-configurations/"
	if !strings.HasPrefix(name, prefix) || path.Clean(name) != name {
		return "", errors.New("Runtime backup contains an invalid recent configuration path")
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), "/")
	if len(parts) == 2 && (parts[0] == "current" || parts[0] == "previous-good") &&
		validConfigurationFile(parts[1]) {
		return path.Join(parts...), nil
	}
	if len(parts) == 3 && parts[0] == "history" && validConfigurationSetName(parts[1]) &&
		validConfigurationFile(parts[2]) {
		return path.Join(parts...), nil
	}
	return "", errors.New("Runtime backup contains an invalid recent configuration path")
}

func portableConfigurationName(name, archiveSHA256 string) (string, error) {
	relative, err := exactConfigurationName(name)
	if err != nil {
		return "", err
	}
	parts := strings.Split(relative, "/")
	if parts[0] == "previous-good" {
		return path.Join("history", "restored-previous-good-"+archiveSHA256[:12], parts[1]), nil
	}
	return relative, nil
}

func validConfigurationSetName(name string) bool {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validConfigurationFile(name string) bool {
	return name == "source.yaml" || name == "config.yaml" || name == "metadata.json"
}

func writeConfigurationFile(name string, body []byte) error {
	if len(body) == 0 || len(body) > maxRecentConfigBytes {
		return errors.New("Runtime backup configuration file is empty or too large")
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(name)
		}
	}()
	if err := owneracl.RestrictFile(name); err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

func restrictConfigurationAncestors(root, leaf string) error {
	for current := leaf; ; current = filepath.Dir(current) {
		if err := restrictConfigurationDirectory(current); err != nil {
			return err
		}
		if current == root {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current || !strings.HasPrefix(current, root+string(filepath.Separator)) {
			return errors.New("Runtime configuration restore directory escaped its staging root")
		}
	}
}

func restrictConfigurationDirectory(name string) error {
	if err := owneracl.RestrictDirectory(name); err != nil {
		return err
	}
	return os.Chmod(name, 0700)
}

func removeConfigurationTree(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runtime configuration restore target became unmanaged")
	}
	linked, err := safepath.ContainsLink(root)
	if err != nil {
		return err
	}
	if linked {
		return errors.New("Runtime configuration restore target contains a linked entry")
	}
	return os.RemoveAll(root)
}
