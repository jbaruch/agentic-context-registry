package realize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const maxTargetBytes = 32 << 20

type fileSnapshot struct {
	exists  bool
	content []byte
	mode    os.FileMode
	hash    string
}

func snapshotFile(root *os.Root, filename string) (fileSnapshot, error) {
	if err := validateParentDirectories(root, filename); err != nil {
		return fileSnapshot{}, err
	}
	info, err := root.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %q: %w", filename, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("target %q must be a regular file, not a symlink or special file", filename)
	}
	file, err := root.Open(filename)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("open %q: %w", filename, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect opened %q: %w", filename, err)
	}
	beforeRead, err := root.Lstat(filename)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %q after opening: %w", filename, err)
	}
	if beforeRead.Mode()&os.ModeSymlink != 0 || !beforeRead.Mode().IsRegular() || !os.SameFile(opened, beforeRead) {
		return fileSnapshot{}, fmt.Errorf("target %q changed while being opened; keep it stable and retry", filename)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxTargetBytes+1))
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("read %q: %w", filename, err)
	}
	if len(content) > maxTargetBytes {
		return fileSnapshot{}, fmt.Errorf("target %q exceeds %d MiB; reduce the file size and retry", filename, maxTargetBytes>>20)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect opened %q after reading: %w", filename, err)
	}
	current, err := root.Lstat(filename)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %q after reading: %w", filename, err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(openedAfter, current) ||
		opened.Size() != openedAfter.Size() || !opened.ModTime().Equal(openedAfter.ModTime()) || opened.Mode() != openedAfter.Mode() || openedAfter.Mode() != current.Mode() {
		return fileSnapshot{}, fmt.Errorf("target %q changed while being read; keep it stable and retry", filename)
	}
	return fileSnapshot{exists: true, content: content, mode: current.Mode().Perm(), hash: contentHash(content)}, nil
}

func validateParentDirectories(root *os.Root, filename string) error {
	directory := path.Dir(filename)
	if directory == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(directory, "/") {
		if current == "" {
			current = component
		} else {
			current = path.Join(current, component)
		}
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect parent %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("parent %q must be a directory, not a symlink or special file", current)
		}
	}
	return nil
}

func contentHash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
