// Package publish builds and publishes immutable ACR package releases.
package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/tarball"
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

	if err := checkRootLicenses(files); err != nil {
		return Archive{}, err
	}
	var writer tarball.Writer
	for _, file := range files {
		if err := validateArchiveFile(file.Path); err != nil {
			return Archive{}, err
		}
		if err := writer.Add(root+"/"+file.Path, normalizedFileMode(file.Mode), file.Content); err != nil {
			return Archive{}, fmt.Errorf("build package archive: %w", err)
		}
	}
	compressed, rawTar, err := writer.Bytes()
	if err != nil {
		return Archive{}, fmt.Errorf("build package archive: %w", err)
	}
	tarDigest := sha256.Sum256(rawTar)
	return Archive{
		Name:      root + ".tar.gz",
		Bytes:     compressed,
		TarSHA256: hex.EncodeToString(tarDigest[:]),
	}, nil
}

func validateArchiveFile(name string) error {
	if err := tarball.ValidateName(name); err != nil {
		return fmt.Errorf("build package archive: %w", err)
	}
	return nil
}

func normalizedFileMode(mode fs.FileMode) fs.FileMode {
	if mode.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// checkRootLicenses also protects direct archive and content callers, which do
// not necessarily pass through manifest inventory selection.
func checkRootLicenses(files []File) error {
	names := make([]string, 0, len(files))
	modes := make(map[string]fs.FileMode, len(files))
	for _, file := range files {
		names = append(names, file.Path)
		modes[file.Path] = file.Mode
	}
	for _, file := range files {
		_, err := manifest.CheckRootLicense(file.Path, names, func(name string) (fs.FileMode, error) {
			return modes[name], nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
