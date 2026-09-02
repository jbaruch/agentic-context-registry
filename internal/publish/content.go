package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// FileMetadata records enough evidence to independently reproduce the
// canonical package content hash.
type FileMetadata struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func describeContent(value manifest.Manifest, files []File) (contentHash string, descriptions []FileMetadata, err error) {
	root, err := os.MkdirTemp("", "acr-publish-content-*")
	if err != nil {
		return "", nil, fmt.Errorf("create package content verification directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(root); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove package content verification directory: %w", removeErr))
		}
	}()

	ordered := append([]File(nil), files...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Path < ordered[right].Path })
	descriptions = make([]FileMetadata, 0, len(ordered))
	for _, file := range ordered {
		if err := validateArchiveFile(file.Path); err != nil {
			return "", nil, err
		}
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", nil, fmt.Errorf("create content hash parent for %q: %w", file.Path, err)
		}
		mode := normalizedFileMode(file.Mode)
		if err := os.WriteFile(target, file.Content, 0o600); err != nil {
			return "", nil, fmt.Errorf("materialize package file %q for hashing: %w", file.Path, err)
		}
		if err := os.Chmod(target, mode); err != nil {
			return "", nil, fmt.Errorf("normalize package file mode %q for hashing: %w", file.Path, err)
		}
		digest := sha256.Sum256(file.Content)
		descriptions = append(descriptions, FileMetadata{
			Path: file.Path, Mode: fmt.Sprintf("%04o", mode), Size: int64(len(file.Content)), SHA256: hex.EncodeToString(digest[:]),
		})
	}
	contentHash, err = dependency.HashPackageFiles(root, value)
	if err != nil {
		return "", nil, fmt.Errorf("hash Git package content: %w", err)
	}
	return contentHash, descriptions, nil
}
