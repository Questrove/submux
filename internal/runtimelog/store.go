package runtimelog

import (
	"bufio"
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

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprivacy"
	"submux/internal/safepath"
)

const (
	DefaultMaxAge       = 7 * 24 * time.Hour
	DefaultMaxBytes     = int64(50 << 20)
	DefaultMaxFileBytes = int64(5 << 20)
	DefaultGCInterval   = time.Hour
	maxLogMessageBytes  = 8 << 10
	maxRecentLogEntries = 2000
)

type Store struct {
	Root         string
	MaxAge       time.Duration
	MaxBytes     int64
	MaxFileBytes int64
	Now          func() time.Time

	mu            sync.Mutex
	lastCursor    uint64
	startupCursor uint64
	recentFloor   uint64
	recent        []runtimeapi.LogEntry
}

type Writer struct {
	store  *Store
	source string
	stream string

	mu      sync.Mutex
	pending []byte
}

type record struct {
	Cursor  uint64    `json:"cursor,omitempty"`
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Stream  string    `json:"stream"`
	Level   string    `json:"level,omitempty"`
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
	store := &Store{Root: absolute}
	store.mu.Lock()
	cursor, err := store.latestCursorLocked()
	if err == nil {
		store.lastCursor = cursor
	} else if err != nil {
		store.lastCursor = uint64(time.Now().UTC().UnixNano())
	}
	store.startupCursor = store.lastCursor
	store.recentFloor = store.lastCursor
	store.mu.Unlock()
	return store, nil
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
	for _, source := range []string{runtimeapi.LogComponentRuntime, runtimeapi.LogComponentMihomo, runtimeapi.LogComponentNetwork} {
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
	message = runtimeprivacy.RedactLogText(strings.TrimSpace(message))
	if len(message) > maxLogMessageBytes {
		message = message[:maxLogMessageBytes] + "…"
	}
	cursor := uint64(now.UnixNano())
	if cursor <= s.lastCursor {
		cursor = s.lastCursor + 1
	}
	s.lastCursor = cursor
	level := detectLevel(stream, message)
	encoded, err := json.Marshal(record{
		Cursor: cursor,
		At:     now, Source: source, Stream: stream,
		Level: level, Message: message,
	})
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
	if err := s.gcSource(source, now); err != nil {
		return err
	}
	s.recent = append(s.recent, runtimeapi.LogEntry{
		Cursor: cursor, At: now, Component: source, Stream: stream, Level: level, Message: message,
	})
	if len(s.recent) > maxRecentLogEntries {
		dropped := len(s.recent) - maxRecentLogEntries
		s.recentFloor = s.recent[dropped-1].Cursor
		s.recent = append([]runtimeapi.LogEntry(nil), s.recent[dropped:]...)
	}
	return nil
}

func (s *Store) Query(query runtimeapi.LogQuery) (runtimeapi.LogPage, error) {
	if s == nil {
		return runtimeapi.LogPage{}, errors.New("Runtime log store is unavailable")
	}
	if query.Before != 0 && query.After != 0 {
		return runtimeapi.LogPage{}, errors.New("Runtime log query cannot combine before and after cursors")
	}
	if query.Limit < 0 || query.Limit > runtimeapi.LogPageMaxSize {
		return runtimeapi.LogPage{}, errors.New("Runtime log query limit is invalid")
	}
	if query.Limit == 0 {
		query.Limit = runtimeapi.LogPageDefaultSize
	}
	if query.Component != "" && !validLogComponent(query.Component) {
		return runtimeapi.LogPage{}, errors.New("Runtime log component filter is invalid")
	}
	if query.Level != "" && !validLogLevel(query.Level) {
		return runtimeapi.LogPage{}, errors.New("Runtime log level filter is invalid")
	}
	query.Text = strings.TrimSpace(query.Text)
	if len(query.Text) > 256 || (!query.Since.IsZero() && !query.Until.IsZero() && query.Since.After(query.Until)) {
		return runtimeapi.LogPage{}, errors.New("Runtime log filter is invalid")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var all []runtimeapi.LogEntry
	var err error
	resetRequired := false
	if query.After != 0 && query.After >= s.startupCursor {
		all = append([]runtimeapi.LogEntry(nil), s.recent...)
		resetRequired = query.After < s.recentFloor
	} else {
		all, err = s.readEntriesLocked(query.Component)
		if err != nil {
			return runtimeapi.LogPage{}, err
		}
		resetRequired = query.After != 0 && len(all) > 0 && query.After < all[0].Cursor
	}
	page := runtimeapi.LogPage{Items: []runtimeapi.LogEntry{}, ObservedAt: s.now()}
	page.ResetRequired = resetRequired
	filtered := make([]runtimeapi.LogEntry, 0, len(all))
	text := strings.ToLower(query.Text)
	for _, entry := range all {
		if query.Component != "" && entry.Component != query.Component {
			continue
		}
		if query.Level != "" && entry.Level != query.Level {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(entry.Message), text) {
			continue
		}
		if !query.Since.IsZero() && entry.At.Before(query.Since) {
			continue
		}
		if !query.Until.IsZero() && entry.At.After(query.Until) {
			continue
		}
		if query.Before != 0 && entry.Cursor >= query.Before {
			continue
		}
		if query.After != 0 && !resetRequired && entry.Cursor <= query.After {
			continue
		}
		entry.Message = runtimeprivacy.RedactLogText(entry.Message)
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		return page, nil
	}
	start, end := 0, len(filtered)
	if query.After != 0 && !resetRequired {
		if end > query.Limit {
			end = query.Limit
			page.HasNewer = len(filtered) > end
		}
	} else if end > query.Limit {
		start = end - query.Limit
		page.HasOlder = true
	}
	page.Items = append(page.Items, filtered[start:end]...)
	page.EarliestCursor = page.Items[0].Cursor
	page.LatestCursor = page.Items[len(page.Items)-1].Cursor
	return page, nil
}

func (s *Store) latestCursorLocked() (uint64, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return 0, err
	}
	latest := uint64(0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		component := strings.SplitN(entry.Name(), "-", 2)[0]
		component = strings.TrimSuffix(component, ".log")
		if !validLogComponent(component) {
			continue
		}
		line, err := readLastLogLine(filepath.Join(s.Root, entry.Name()))
		if err != nil {
			return 0, err
		}
		if len(line) == 0 {
			continue
		}
		var item record
		if err := json.Unmarshal(line, &item); err != nil {
			return 0, err
		}
		cursor := item.Cursor
		if cursor == 0 && !item.At.IsZero() {
			cursor = uint64(item.At.UnixNano())
		}
		if cursor > latest {
			latest = cursor
		}
	}
	return latest, nil
}

func readLastLogLine(name string) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return nil, err
	}
	const tailBytes = maxLogMessageBytes + (16 << 10)
	start := info.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	body := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(body, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, nil
	}
	if index := strings.LastIndexByte(string(body), '\n'); index >= 0 {
		body = body[index+1:]
	}
	return body, nil
}

func (s *Store) readEntriesLocked(component string) ([]runtimeapi.LogEntry, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		nameComponent := strings.SplitN(entry.Name(), "-", 2)[0]
		if strings.HasSuffix(nameComponent, ".log") {
			nameComponent = strings.TrimSuffix(nameComponent, ".log")
		}
		managed := validLogComponent(nameComponent)
		if !managed || component != "" && nameComponent != component {
			continue
		}
		paths = append(paths, filepath.Join(s.Root, entry.Name()))
	}
	sort.Strings(paths)
	result := make([]runtimeapi.LogEntry, 0)
	for _, name := range paths {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), maxLogMessageBytes+(16<<10))
		for scanner.Scan() {
			var item record
			if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
				_ = file.Close()
				return nil, errors.New("Runtime log file contains an invalid record")
			}
			if item.At.IsZero() || !validLogComponent(item.Source) || !validComponent(item.Stream) {
				_ = file.Close()
				return nil, errors.New("Runtime log file contains invalid metadata")
			}
			level := item.Level
			if !validLogLevel(level) {
				level = detectLevel(item.Stream, item.Message)
			}
			result = append(result, runtimeapi.LogEntry{
				Cursor: item.Cursor, At: item.At.UTC(), Component: item.Source,
				Stream: item.Stream, Level: level, Message: item.Message,
			})
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil || closeErr != nil {
			return nil, errors.Join(scanErr, closeErr)
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].At.Equal(result[right].At) {
			return result[left].Cursor < result[right].Cursor
		}
		return result[left].At.Before(result[right].At)
	})
	var cursor uint64
	for index := range result {
		if result[index].Cursor <= cursor {
			candidate := uint64(result[index].At.UnixNano())
			if candidate <= cursor {
				candidate = cursor + 1
			}
			result[index].Cursor = candidate
		}
		cursor = result[index].Cursor
	}
	return result, nil
}

func detectLevel(stream, message string) string {
	value := strings.ToLower(message)
	for _, candidate := range []struct {
		needle string
		level  string
	}{
		{"debug", runtimeapi.LogLevelDebug},
		{"warn", runtimeapi.LogLevelWarn},
		{"error", runtimeapi.LogLevelError},
		{"fatal", runtimeapi.LogLevelError},
		{"failed", runtimeapi.LogLevelError},
	} {
		if strings.Contains(value, candidate.needle) {
			return candidate.level
		}
	}
	if stream == "stderr" {
		return runtimeapi.LogLevelError
	}
	return runtimeapi.LogLevelInfo
}

func validLogComponent(value string) bool {
	switch value {
	case runtimeapi.LogComponentRuntime, runtimeapi.LogComponentMihomo, runtimeapi.LogComponentNetwork:
		return true
	default:
		return false
	}
}

func validLogLevel(value string) bool {
	switch value {
	case runtimeapi.LogLevelDebug, runtimeapi.LogLevelInfo, runtimeapi.LogLevelWarn, runtimeapi.LogLevelError:
		return true
	default:
		return false
	}
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
