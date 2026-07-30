package runtimeupdate

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"

	"submux/internal/safepath"
)

const (
	MaxBundleBytes       = 300 << 20
	maxBundleEntries     = 512
	maxBundleExpanded    = 400 << 20
	maxBundleEntryLength = 300 << 20
)

type bundleFetcher struct {
	file    *os.File
	archive *zip.Reader
	entries map[string]*zip.File
}

func newBundleFetcher(bundlePath string) (*bundleFetcher, error) {
	if bundlePath == "" {
		return nil, errors.New("offline TUF bundle path is required")
	}
	file, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open offline TUF bundle: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect opened offline TUF bundle: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > MaxBundleBytes {
		file.Close()
		return nil, errors.New("offline TUF bundle must be a bounded regular non-linked ZIP file")
	}
	linked, err := safepath.ContainsLink(bundlePath)
	if err != nil || linked {
		file.Close()
		return nil, errors.New("offline TUF bundle path must not contain symbolic or reparse links")
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		file.Close()
		return nil, errors.New("offline TUF bundle is not a valid ZIP archive")
	}
	if len(archive.File) == 0 || len(archive.File) > maxBundleEntries {
		file.Close()
		return nil, errors.New("offline TUF bundle has an invalid entry count")
	}
	entries := make(map[string]*zip.File, len(archive.File))
	var expanded uint64
	for _, entry := range archive.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if entry.FileInfo().IsDir() || name == "" || path.Clean(name) != name ||
			strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") ||
			(!strings.HasPrefix(name, "metadata/") && !strings.HasPrefix(name, "targets/")) {
			file.Close()
			return nil, errors.New("offline TUF bundle contains an unsafe entry path")
		}
		if entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > maxBundleEntryLength {
			file.Close()
			return nil, errors.New("offline TUF bundle contains an invalid entry size")
		}
		expanded += entry.UncompressedSize64
		if expanded > maxBundleExpanded {
			file.Close()
			return nil, errors.New("offline TUF bundle expands beyond its limit")
		}
		if _, duplicate := entries[name]; duplicate {
			file.Close()
			return nil, errors.New("offline TUF bundle contains duplicate entries")
		}
		entries[name] = entry
	}
	return &bundleFetcher{
		file:    file,
		archive: archive,
		entries: entries,
	}, nil
}

func (f *bundleFetcher) Close() error {
	if f == nil || f.file == nil {
		return nil
	}
	return f.file.Close()
}

func (f *bundleFetcher) DownloadFile(urlPath string, maxLength int64, _ time.Duration) ([]byte, error) {
	parsed, err := url.Parse(urlPath)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "offline.submux.invalid" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("offline TUF bundle request is invalid")
	}
	name := strings.TrimPrefix(parsed.EscapedPath(), "/")
	unescaped, err := url.PathUnescape(name)
	if err != nil || path.Clean(unescaped) != unescaped {
		return nil, errors.New("offline TUF bundle request path is invalid")
	}
	entry := f.entries[unescaped]
	if entry == nil {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: 404, URL: urlPath}
	}
	if maxLength <= 0 || entry.UncompressedSize64 > uint64(maxLength) {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: "offline bundle entry exceeds signed or configured maximum length"}
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, maxLength+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) != int64(entry.UncompressedSize64) || int64(len(body)) > maxLength {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: "offline bundle entry length is invalid"}
	}
	return body, nil
}
