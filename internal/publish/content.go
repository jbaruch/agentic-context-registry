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

// FileMetadata records distribution bytes, including implicit root licenses.
// The v1 content hash covers only the manifest and declared artifacts.
type FileMetadata struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func describeContent(value manifest.Manifest, files []File) (contentHash string, descriptions []FileMetadata, err error) {
	if err := checkDistribution(files); err != nil {
		return "", nil, err
	}
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
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", nil, fmt.Errorf("create content hash parent for %q: %w", file.Path, err)
		}
		mode := normalizedFileMode(file.Mode)
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("materialize package file %q for hashing: %w; rename the conflicting source path and update its declarations before retrying", file.Path, err)
		}
		if err != nil {
			return "", nil, fmt.Errorf("create package file %q for hashing: %w; verify temporary storage is writable and retry", file.Path, err)
		}
		_, writeErr := output.Write(file.Content)
		modeErr := output.Chmod(mode)
		if err := errors.Join(writeErr, modeErr, output.Close()); err != nil {
			return "", nil, fmt.Errorf("materialize package file %q for hashing: %w; verify temporary storage is writable and retry", file.Path, err)
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
