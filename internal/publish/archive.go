// Package publish builds and publishes immutable ACR package releases.
package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// File is one manifest-declared file read from an immutable Git tree.
type File struct {
	Path    string
	Mode    fs.FileMode
	Content []byte
}

// Archive is the deterministic package asset and the digest of its raw tar
// stream. The gzip digest is computed when release checksums are assembled.
type Archive struct {
	Name      string
	Bytes     []byte
	TarSHA256 string
}

// BuildArchive creates a normalized tar+gzip asset rooted at
// <repository>-<version>. Input order and the process umask do not affect it.
func BuildArchive(repository, version string, files []File) (Archive, error) {
	root := repository + "-" + version
	if repository == "" || version == "" || strings.Contains(root, "/") || strings.Contains(root, "\\") || strings.ContainsRune(root, '\x00') || path.Clean(root) != root {
		return Archive{}, fmt.Errorf("build package archive: repository and version must form one normalized root directory")
	}

	ordered := append([]File(nil), files...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Path < ordered[right].Path })
	seen := make(map[string]struct{}, len(ordered))
	var tarBytes bytes.Buffer
	tarWriter := tar.NewWriter(&tarBytes)
	for _, file := range ordered {
		if err := validateArchiveFile(file.Path); err != nil {
			return Archive{}, err
		}
		if _, exists := seen[file.Path]; exists {
			return Archive{}, fmt.Errorf("build package archive: file %q appears more than once", file.Path)
		}
		seen[file.Path] = struct{}{}
		header := &tar.Header{
			Name:       root + "/" + file.Path,
			Mode:       int64(normalizedFileMode(file.Mode)),
			Size:       int64(len(file.Content)),
			ModTime:    time.Unix(0, 0).UTC(),
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Typeflag:   tar.TypeReg,
			Format:     tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return Archive{}, fmt.Errorf("write package archive header %q: %w", file.Path, err)
		}
		if _, err := tarWriter.Write(file.Content); err != nil {
			return Archive{}, fmt.Errorf("write package archive file %q: %w", file.Path, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return Archive{}, fmt.Errorf("finish package tar stream: %w", err)
	}

	tarDigest := sha256.Sum256(tarBytes.Bytes())
	var compressed bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return Archive{}, fmt.Errorf("create package gzip stream: %w", err)
	}
	if _, err := gzipWriter.Write(tarBytes.Bytes()); err != nil {
		return Archive{}, fmt.Errorf("compress package tar stream: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return Archive{}, fmt.Errorf("finish package gzip stream: %w", err)
	}
	return Archive{
		Name:      root + ".tar.gz",
		Bytes:     compressed.Bytes(),
		TarSHA256: hex.EncodeToString(tarDigest[:]),
	}, nil
}

func validateArchiveFile(name string) error {
	if name == "" || name == "." || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("build package archive: file path %q is not normalized package-relative POSIX syntax", name)
	}
	return nil
}

func normalizedFileMode(mode fs.FileMode) fs.FileMode {
	if mode.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}
