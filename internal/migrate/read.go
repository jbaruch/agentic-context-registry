package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func directorySnapshot(snapshot adapter.Snapshot) (adapter.DirectorySnapshot, error) {
	directories, ok := snapshot.(adapter.DirectorySnapshot)
	if !ok {
		return nil, fmt.Errorf("project snapshot does not support directory inspection; inventory a real project tree")
	}
	return directories, nil
}

func readOptional(snapshot adapter.Snapshot, filename string) ([]byte, bool, error) {
	observed, err := snapshot.ReadFile(filename)
	if err == nil {
		return observed.Content, true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("read %q: %w", filename, err)
}

func readDir(snapshot adapter.DirectorySnapshot, directory string) ([]adapter.ObservedEntry, error) {
	entries, err := snapshot.ReadDir(directory)
	if err == nil {
		sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
		return entries, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return nil, fmt.Errorf("read directory %q: %w", directory, err)
}

func posixJoin(parts ...string) string {
	return path.Join(parts...)
}

func trimDotPrefix(value string) string {
	return strings.TrimPrefix(value, "./")
}
