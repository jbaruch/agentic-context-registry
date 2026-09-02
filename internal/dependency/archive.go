package dependency

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	maxExtractedBytes = 256 << 20
	maxArchiveEntries = 10000
)

type verifiedPackage struct {
	Version     string
	ContentHash string
}

func verifyPackageArchive(contents []byte, repository Repository) (verifiedPackage, error) {
	root, err := os.MkdirTemp("", "acr-package-*")
	if err != nil {
		return verifiedPackage{}, fmt.Errorf("create package verification directory: %w; verify temporary storage is writable and retry", err)
	}
	defer os.RemoveAll(root)

	if err := extractGitHubArchive(contents, root); err != nil {
		return verifiedPackage{}, err
	}
	value, err := manifest.Load(root)
	if err != nil {
		return verifiedPackage{}, fmt.Errorf("validate downloaded %s package: %w; fix the package manifest and publish a new release", repository.String(), err)
	}
	if value.Name != repository.FullName() || value.Source.Repository != "https://github.com/"+repository.FullName() {
		return verifiedPackage{}, fmt.Errorf("downloaded package identity %q does not match %s; fix agent-plugin.yaml and publish a new release", value.Name, repository.String())
	}
	hash, err := HashPackageFiles(root, value)
	if err != nil {
		return verifiedPackage{}, err
	}
	return verifiedPackage{Version: value.Version, ContentHash: hash}, nil
}

func extractGitHubArchive(contents []byte, destination string) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("open downloaded archive: %w; verify the repository provides a valid GitHub tarball and retry", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	rootName := ""
	seen := make(map[string]struct{})
	var skippedTrees []string
	var extractedBytes int64
	entries := 0

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read downloaded archive: %w; retry the download or report a malformed package", err)
		}
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("downloaded archive contains more than %d entries; reduce package size and retry", maxArchiveEntries)
		}
		relative, currentRoot, err := archivePath(header.Name, rootName)
		if err != nil {
			return err
		}
		if rootName == "" {
			rootName = currentRoot
		}
		if relative == "" {
			continue
		}
		if belowSkippedTree(relative, skippedTrees) {
			continue
		}
		if _, exists := seen[relative]; exists {
			return fmt.Errorf("downloaded archive repeats path %q; publish an archive with unique paths", relative)
		}
		seen[relative] = struct{}{}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create package directory %q: %w; verify temporary storage is writable and retry", relative, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || extractedBytes+header.Size > maxExtractedBytes {
				return fmt.Errorf("downloaded archive expands beyond %d MiB; reduce package size and retry", maxExtractedBytes>>20)
			}
			extractedBytes += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent for package file %q: %w; verify temporary storage is writable and retry", relative, err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("create package file %q: %w; retry with a valid archive", relative, err)
			}
			_, copyErr := io.CopyN(file, tarReader, header.Size)
			chmodErr := file.Chmod(os.FileMode(header.Mode).Perm())
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract package file %q: %w; retry the download", relative, copyErr)
			}
			if chmodErr != nil {
				return fmt.Errorf("set package file mode %q: %w; verify temporary storage and retry", relative, chmodErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close package file %q: %w; verify temporary storage and retry", relative, closeErr)
			}
		case tar.TypeSymlink, tar.TypeLink:
			// GitHub repository archives can contain undeclared links. Do not
			// materialize them; declared links fail manifest filesystem checks.
			skippedTrees = append(skippedTrees, relative)
			continue
		default:
			// Ignore undeclared metadata and special entries. A declared path
			// still fails manifest validation because it was not materialized.
			skippedTrees = append(skippedTrees, relative)
			continue
		}
	}
	if rootName == "" {
		return errors.New("downloaded archive is empty; publish package content and retry")
	}
	return nil
}

func belowSkippedTree(relative string, skipped []string) bool {
	for _, prefix := range skipped {
		if strings.HasPrefix(relative, prefix+"/") {
			return true
		}
	}
	return false
}

func archivePath(name, expectedRoot string) (string, string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", "", fmt.Errorf("downloaded archive contains unsafe path %q; publish a normalized archive", name)
	}
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("downloaded archive contains unsafe path %q; publish a normalized archive", name)
	}
	parts := strings.Split(clean, "/")
	root := parts[0]
	if expectedRoot != "" && root != expectedRoot {
		return "", "", fmt.Errorf("downloaded archive has multiple roots %q and %q; publish one package root", expectedRoot, root)
	}
	if len(parts) == 1 {
		return "", root, nil
	}
	relative := strings.Join(parts[1:], "/")
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || hasWindowsArchiveVolume(relative) || path.Clean(relative) != relative {
		return "", "", fmt.Errorf("downloaded archive contains unsafe path %q; publish normalized package paths", name)
	}
	return relative, root, nil
}

func hasWindowsArchiveVolume(relative string) bool {
	return len(relative) >= 2 && relative[1] == ':' && (relative[0] >= 'A' && relative[0] <= 'Z' || relative[0] >= 'a' && relative[0] <= 'z')
}

// HashPackageFiles returns the canonical digest of the manifest-declared
// package files rooted at root. File paths, modes, sizes, and byte digests are
// all part of the hash so publishers and consumers agree on package identity.
func HashPackageFiles(root string, value manifest.Manifest) (string, error) {
	files, err := manifest.PackageFiles(root, value)
	if err != nil {
		return "", fmt.Errorf("enumerate package content: %w; fix the package and publish a new release", err)
	}
	sort.Strings(files)
	hash := sha256.New()
	if _, err := io.WriteString(hash, "acr-package-content-v1\x00"); err != nil {
		return "", fmt.Errorf("initialize package content hash: %w; retry archive verification", err)
	}
	for _, relative := range files {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(filename)
		if err != nil {
			return "", fmt.Errorf("inspect package file %q: %w; retry archive verification", relative, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("package file %q is not regular; replace links or special files and publish a new release", relative)
		}
		file, err := os.Open(filename)
		if err != nil {
			return "", fmt.Errorf("open package file %q: %w; retry archive verification", relative, err)
		}
		fileHash := sha256.New()
		if _, err := io.Copy(fileHash, file); err != nil {
			file.Close()
			return "", fmt.Errorf("hash package file %q: %w; retry archive verification", relative, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close package file %q: %w; retry archive verification", relative, err)
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%04o\x00%d\x00%s\x00", relative, info.Mode().Perm(), info.Size(), hex.EncodeToString(fileHash.Sum(nil))); err != nil {
			return "", fmt.Errorf("record package file hash %q: %w; retry archive verification", relative, err)
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
