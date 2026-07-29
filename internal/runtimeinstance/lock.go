package runtimeinstance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"submux/internal/safepath"
)

var ErrAlreadyRunning = errors.New("Submux Runtime is already running")

type Guard struct {
	file *os.File
	once sync.Once
	err  error
}

func Acquire(path string) (*Guard, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("Runtime instance lock must use a fixed absolute non-root path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil || absolute == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return nil, errors.New("Runtime instance lock must use a fixed absolute non-root path")
	}
	parent := filepath.Dir(absolute)
	linked, err := safepath.ContainsLinkInExistingPath(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect Runtime lock ancestors: %w", err)
	}
	if linked {
		return nil, errors.New("Runtime lock path must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	linked, err = safepath.ContainsLink(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect Runtime lock directory: %w", err)
	}
	if linked {
		return nil, errors.New("Runtime lock path must not contain symbolic or reparse links")
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(absolute, 0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		if isLockConflict(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("acquire Runtime instance lock: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		_ = unlockFile(file)
		_ = file.Close()
		return nil, err
	}
	if _, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = unlockFile(file)
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = unlockFile(file)
		_ = file.Close()
		return nil, err
	}
	return &Guard{file: file}, nil
}

func (g *Guard) Close() error {
	if g == nil {
		return nil
	}
	g.once.Do(func() {
		if g.file == nil {
			return
		}
		g.err = errors.Join(unlockFile(g.file), g.file.Close())
		g.file = nil
	})
	return g.err
}
