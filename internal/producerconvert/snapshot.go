package producerconvert

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
)

type fileState struct {
	Link      string `json:"link,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Mode      uint32 `json:"mode"`
	Directory bool   `json:"directory,omitempty"`
	Content   []byte `json:"-"`
}

type tree map[string]fileState

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// repositoryBoundary locates the enclosing checkout without invoking Git or
// following a symlink in the selected path. Standalone packages use themselves.
func repositoryBoundary(selected string) (string, string, error) {
	if selected == "" {
		selected = "."
	}
	for _, part := range strings.Split(filepath.ToSlash(selected), "/") {
		if part == ".." {
			return "", "", refuse("unsafe_path", selected, "parent traversal is unsupported; pass a direct package path")
		}
	}
	absolute, err := filepath.Abs(selected)
	if err != nil {
		return "", "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", refuse("unsafe_path", selected, "selected package must be a directory; symlinks and regular files are unsupported")
	}
	boundary := absolute
	found := false
	for current := absolute; ; current = filepath.Dir(current) {
		if info, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			if info.Mode()&fs.ModeSymlink != 0 {
				return "", "", refuse("unsafe_path", current, ".git cannot be a symlink")
			}
			boundary, found = current, true
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if !found {
		boundary = absolute
	}
	relative, err := filepath.Rel(boundary, absolute)
	if err != nil {
		return "", "", err
	}
	root, err := os.OpenRoot(boundary)
	if err != nil {
		return "", "", err
	}
	err = checkParents(root, path.Join(filepath.ToSlash(relative), "placeholder"))
	err = errors.Join(err, root.Close())
	return boundary, filepath.ToSlash(relative), err
}

func checkParents(root *os.Root, filename string) error {
	if !fs.ValidPath(filename) || strings.Contains(filename, "\\") {
		return refuse("unsafe_path", filename, "use a confined relative path")
	}
	directory := path.Dir(filename)
	if directory == "." {
		return nil
	}
	parts := strings.Split(directory, "/")
	for i := range parts {
		current := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect parent %s: %w", current, err)
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return refuse("unsafe_path", current, "symlink or non-directory parent; restore a regular directory")
		}
	}
	return nil
}

func readState(root *os.Root, filename string) (fileState, error) {
	if err := checkParents(root, filename); err != nil {
		return fileState{}, err
	}
	info, err := root.Lstat(filename)
	if err != nil {
		return fileState{}, err
	}
	if !info.Mode().IsRegular() {
		return fileState{}, refuse("unsafe_path", filename, "only regular files can be converted; replace the symlink or special file")
	}
	f, err := root.Open(filename)
	if err != nil {
		return fileState{}, err
	}
	opened, err := f.Stat()
	if err != nil {
		return fileState{}, errors.Join(err, f.Close())
	}
	if !os.SameFile(info, opened) {
		return fileState{}, errors.Join(refuse("source_changed", filename, "file changed while opening; rerun the dry-run"), f.Close())
	}
	data, readErr := io.ReadAll(f)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return fileState{}, err
	}
	again, err := root.Lstat(filename)
	if err != nil {
		return fileState{}, err
	}
	if !os.SameFile(info, again) || info.Mode() != again.Mode() {
		return fileState{}, refuse("source_changed", filename, "file changed while reading; rerun the dry-run")
	}
	return fileState{Digest: digest(data), Mode: uint32(info.Mode().Perm()), Content: data}, nil
}

// Consumer surfaces are outside producer ownership. They are neither followed
// nor edited; Git internals likewise are not source files or receipt inputs.
func excluded(filename string) bool {
	first := strings.Split(filename, "/")[0]
	switch first {
	case ".git", ".tessl", ".agents", ".claude", ".codex", ".cursor":
		return true
	}
	return filename == ReceiptPath || first == transactionPath
}

func snapshot(root *os.Root, selected string, semantic ...bool) (tree, error) {
	result := tree{}
	err := fs.WalkDir(root.FS(), ".", func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Unrelated directories are only searched for producer markers.
			// An inaccessible directory is outside the selected input tree.
			if errors.Is(walkErr, fs.ErrPermission) && !relevantPath(selected, filename) && !(len(semantic) > 0 && semantic[0] && (filename == "tests" || strings.HasPrefix(filename, "tests/"))) && !within(filename, selected) {
				return fs.SkipDir
			}
			return walkErr
		}
		if filename == "." {
			return nil
		}
		if len(semantic) > 0 && semantic[0] && semanticConsumerPath(filename) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if excluded(filename) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relevant := relevantPath(selected, filename) || len(semantic) > 0 && semantic[0] && (filename == "tests" || strings.HasPrefix(filename, "tests/"))
		marker := filename == ".tessl-plugin/plugin.json" || path.Base(filename) == "agent-plugin.yaml" || path.Base(filename) == "tile.json" || strings.HasSuffix(filename, "/.tessl-plugin/plugin.json")
		notice := distributionNotice(filename) && (path.Dir(filename) == "." || within(path.Dir(filename), selected))
		if notice && len(semantic) > 0 && semantic[0] {
			relevant = true
		}
		if !relevant && !entry.IsDir() {
			// Discovery uses directory entries only: never open unrelated
			// documents, secrets, symlinks or special files for fingerprinting.
			if marker || notice {
				result[filename] = fileState{Mode: 0o644}
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := root.Readlink(filename)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			result[filename] = fileState{Link: target, Digest: digest([]byte(target)), Mode: uint32(info.Mode().Perm())}
			return nil
		}
		if entry.IsDir() {
			if !relevant {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			result[filename] = fileState{Directory: true, Mode: uint32(info.Mode().Perm())}
			return nil
		}
		state, err := readState(root, filename)
		if err != nil {
			return err
		}
		result[filename] = state
		return nil
	})
	return result, err
}

func fingerprints(value tree) tree {
	result := tree{}
	for name, state := range value {
		state.Content = nil
		result[name] = state
	}
	return result
}
func matches(a, b tree) bool { return reflect.DeepEqual(fingerprints(a), fingerprints(b)) }

// The selected authored tree and delivery files are conversion inputs. Consumer
// configuration is preserved without reading it. Other repository entries are
// inspected only for competing producer markers and ancestor license notices.
func relevantPath(selected, filename string) bool {
	return ((selected == filename || within(selected, filename)) && !consumerFile(filename)) || strings.HasPrefix(filename, ".github/") || filename == ".github" || filename == "agent-plugin.yaml"
}

// Live transactions compare exact modes and directories. Portable receipts bind
// file bytes and executable status, matching Git's checkout contract; empty
// directories and owner/group read permissions are not versioned output.
func receiptFingerprints(value tree) tree {
	result := tree{}
	for name, state := range value {
		if state.Directory {
			continue
		}
		state.Content = nil
		if state.Mode&0o111 != 0 {
			state.Mode = 0o755
		} else {
			state.Mode = 0o644
		}
		result[name] = state
	}
	return result
}

func semanticConsumerPath(name string) bool {
	first := strings.Split(name, "/")[0]
	return first == ".gemini" || first == ".vscode" || first == ".openhands" || name == ".github/mcp.json"
}
