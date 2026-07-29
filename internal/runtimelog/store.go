package runtimelog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"submux/internal/runtimeprivacy"
	"submux/internal/safepath"
)

const (
	DefaultMaxAge       = 7 * 24 * time.Hour
	DefaultMaxBytes     = int64(50 << 20)
	DefaultMaxFileBytes = int64(5 << 20)
	DefaultGCInterval   = time.Hour
	maxLogMessageBytes  = 64 << 10
)

type Store struct {
	Root         string
	MaxAge       time.Duration
	MaxBytes     int64
	MaxFileBytes int64
	Now          func() time.Time

	mu sync.Mutex
}

type Writer struct {
	store  *Store
	source string
	stream string

	mu      sync.Mutex
	pending []byte
}

type record struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
}

func Open(root string) (*Store, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("Runtime log root must be an absolute path")
	}
	absolute, err := filepath.Abs(root)
	if err != nil || absolute == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return nil, errors.New("Runtime log root must be a fixed non-root path")
	}
	linked, err := safepath.ContainsLinkInExistingPath(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect Runtime log root: %w", err)
	}
	if linked {
		return nil, errors.New("Runtime log root must not contain symbolic or reparse links")
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(absolute, 0700); err != nil {
		return nil, err
	}
	return &Store{Root: absolute}, nil
}

func (s *Store) Writer(source, stream string) (*Writer, error) {
	if !validComponent(source) || !validComponent(stream) {
		return nil, errors.New("Runtime log source or stream is invalid")
	}
	return &Writer{store: s, source: source, stream: stream}, nil
}

func (w *Writer) Write(value []byte) (int, error) {
	if w == nil || w.store == nil {
		return 0, errors.New("Runtime log writer is unavailable")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, value...)
	for {
		index := strings.IndexByte(string(w.pending), '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), w.pending[:index]...)
		w.pending = append(w.pending[:0], w.pending[index+1:]...)
		if err := w.store.writeRecord(w.source, w.stream, string(line)); err != nil {
			return 0, err
		}
	}
	if len(w.pending) > maxLogMessageBytes {
		line := append([]byte(nil), w.pending...)
		w.pending = w.pending[:0]
		if err := w.store.writeRecord(w.source, w.stream, string(line)); err != nil {
			return 0, err
		}
	}
	return len(value), nil
}

func (w *Writer) Flush() error {
	if w == nil || w.store == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	line := append([]byte(nil), w.pending...)
	w.pending = w.pending[:0]
	return w.store.writeRecord(w.source, w.stream, string(line))
}

func (s *Store) GC() error {
	if s == nil {
		return errors.New("Runtime log store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, source := range []string{"runtime", "mihomo"} {
		if err := s.gcSource(source, s.now()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RunGC(ctx context.Context, interval time.Duration) error {
	if s == nil {
		return errors.New("Runtime log store is unavailable")
	}
	if interval <= 0 {
		return errors.New("Runtime log cleanup interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.GC(); err != nil {
				return err
			}
		}
	}
}

func (s *Store) writeRecord(source, stream, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	message = runtimeprivacy.RedactText(strings.TrimSpace(message))
	if len(message) > maxLogMessageBytes {
		message = message[:maxLogMessageBytes] + "…"
	}
	encoded, err := json.Marshal(record{At: now, Source: source, Stream: stream, Message: message})
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	current := filepath.Join(s.Root, source+".log")
	if info, err := os.Stat(current); err == nil && info.Size()+int64(len(encoded)) > s.maxFileBytes() {
		rotated := filepath.Join(s.Root, fmt.Sprintf("%s-%020d.log", source, now.UnixNano()))
		if err := os.Rename(current, rotated); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(current, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return s.gcSource(source, now)
}

func (s *Store) gcSource(source string, now time.Time) error {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return err
	}
	type fileInfo struct {
		path    string
		modTime time.Time
		size    int64
	}
	var files []fileInfo
	for _, entry := range entries {
		name := entry.Name()
		managedName := name == source+".log" ||
			(strings.HasPrefix(name, source+"-") && strings.HasSuffix(name, ".log"))
		if entry.IsDir() || !managedName {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, fileInfo{
			path:    filepath.Join(s.Root, entry.Name()),
			modTime: info.ModTime(),
			size:    info.Size(),
		})
	}
	sort.Slice(files, func(left, right int) bool {
		if files[left].modTime.Equal(files[right].modTime) {
			return files[left].path < files[right].path
		}
		return files[left].modTime.Before(files[right].modTime)
	})
	total := int64(0)
	for _, file := range files {
		total += file.size
	}
	cutoff := now.Add(-s.maxAge())
	for _, file := range files {
		if !file.modTime.Before(cutoff) && total <= s.maxBytes() {
			continue
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= file.size
	}
	return nil
}

func (s *Store) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) maxAge() time.Duration {
	if s.MaxAge > 0 {
		return s.MaxAge
	}
	return DefaultMaxAge
}

func (s *Store) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
}

func (s *Store) maxFileBytes() int64 {
	if s.MaxFileBytes > 0 {
		return s.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

func validComponent(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' ||
			character == '_' {
			continue
		}
		return false
	}
	return true
}

var _ io.Writer = (*Writer)(nil)
