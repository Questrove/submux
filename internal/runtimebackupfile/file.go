package runtimebackupfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"submux/internal/owneracl"
	"submux/internal/safepath"
)

func Read(name string, maximum int) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("backup file size limit must be positive")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	if err := validateParent(absolute); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 ||
		info.Size() > int64(maximum) {
		return nil, errors.New("backup must be a bounded regular non-linked file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() ||
		openedInfo.Size() != info.Size() ||
		!os.SameFile(info, openedInfo) {
		return nil, errors.New("backup file changed while it was being opened")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > maximum || int64(len(body)) != openedInfo.Size() {
		return nil, errors.New("backup file size changed while it was being read")
	}
	return body, nil
}

func WriteNew(name string, body []byte, maximum int) (string, error) {
	if len(body) == 0 || maximum <= 0 || len(body) > maximum {
		return "", errors.New("backup archive size is outside the allowed range")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	if err := validateParent(absolute); err != nil {
		return "", err
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("create new backup output file: %w", err)
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(absolute)
		}
	}()
	if err := owneracl.RestrictFile(absolute); err != nil {
		return "", err
	}
	if err := file.Chmod(0600); err != nil {
		return "", fmt.Errorf("restrict backup output permissions: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		return "", fmt.Errorf("write backup output file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync backup output file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close backup output file: %w", err)
	}
	success = true
	return absolute, nil
}

func RestrictDirectory(name string) error {
	return owneracl.RestrictDirectory(name)
}

func validateParent(absolute string) error {
	parent := filepath.Dir(absolute)
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect backup directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("backup directory must be an existing non-linked directory")
	}
	linked, err := safepath.ContainsLink(parent)
	if err != nil {
		return fmt.Errorf("inspect backup path: %w", err)
	}
	if linked {
		return errors.New("backup path must not contain symbolic or reparse links")
	}
	return nil
}
