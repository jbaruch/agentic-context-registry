// Package tarball writes deterministic tar+gzip archives shared by release producers.
package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type entry struct {
	name    string
	mode    fs.FileMode
	content []byte
}

// Writer collects regular files for a normalized deterministic archive.
type Writer struct {
	entries []entry
	seen    map[string]struct{}
}

// Add includes one normalized package-relative POSIX path in the archive.
func (writer *Writer) Add(name string, mode fs.FileMode, content []byte) error {
	if err := validateName(name); err != nil {
		return err
	}
	if writer.seen == nil {
		writer.seen = make(map[string]struct{})
	}
	if _, exists := writer.seen[name]; exists {
		return fmt.Errorf("build deterministic tarball: file %q appears more than once", name)
	}
	writer.seen[name] = struct{}{}
	writer.entries = append(writer.entries, entry{name: name, mode: normalizeMode(mode), content: append([]byte(nil), content...)})
	return nil
}

// Bytes returns the gzip archive and its uncompressed tar stream.
func (writer *Writer) Bytes() (compressed []byte, rawTar []byte, err error) {
	ordered := append([]entry(nil), writer.entries...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].name < ordered[right].name })

	var tarBytes bytes.Buffer
	tarWriter := tar.NewWriter(&tarBytes)
	for _, file := range ordered {
		header := &tar.Header{
			Name:       filepath.ToSlash(file.name),
			Mode:       int64(file.mode),
			Size:       int64(len(file.content)),
			ModTime:    time.Unix(0, 0).UTC(),
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Typeflag:   tar.TypeReg,
			Format:     tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, nil, fmt.Errorf("write deterministic tarball header %q: %w", file.name, err)
		}
		if _, err := tarWriter.Write(file.content); err != nil {
			return nil, nil, fmt.Errorf("write deterministic tarball file %q: %w", file.name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, nil, fmt.Errorf("finish deterministic tar stream: %w", err)
	}

	var gzipBytes bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&gzipBytes, gzip.BestCompression)
	if err != nil {
		return nil, nil, fmt.Errorf("create deterministic gzip stream: %w", err)
	}
	if _, err := gzipWriter.Write(tarBytes.Bytes()); err != nil {
		return nil, nil, fmt.Errorf("compress deterministic tar stream: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, nil, fmt.Errorf("finish deterministic gzip stream: %w", err)
	}
	return gzipBytes.Bytes(), tarBytes.Bytes(), nil
}

func validateName(name string) error {
	if name == "" || name == "." || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("build deterministic tarball: file path %q is not normalized relative POSIX syntax", name)
	}
	return nil
}

func normalizeMode(mode fs.FileMode) fs.FileMode {
	if mode.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}
