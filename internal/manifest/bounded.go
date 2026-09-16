package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
)

// LoadPackageBounded validates the same release inventory through an opened
// source root. It limits bytes before YAML decoding and distinct filesystem
// entries (including directories) during validation and inventory traversal.
// Ordinary release callers retain Load/PackageFiles and their existing limits.
func LoadPackageBounded(root *os.Root, entryLimit int, byteLimit int64) (Manifest, []string, error) {
	if entryLimit < 1 || byteLimit < 0 || byteLimit == math.MaxInt64 {
		return Manifest{}, nil, errors.New("local package limits must be finite and positive")
	}
	contents, err := readManifestFromRootBounded(root, root.Name(), byteLimit)
	if err != nil {
		return Manifest{}, nil, err
	}
	value, err := decodeManifest(contents, root.Name())
	if err != nil {
		return Manifest{}, nil, err
	}
	bounded := &boundedPackageFS{FS: root.FS(), limit: entryLimit, seen: make(map[string]struct{}), directories: make(map[string][]fs.DirEntry)}
	if err := ValidateFS(bounded, value); err != nil {
		return Manifest{}, nil, err
	}
	files, err := collectPackageFilesFS(bounded, value)
	if err != nil {
		return Manifest{}, nil, err
	}
	return value, files, nil
}

// fs.WalkDir reads a whole directory before its callback. ReadDir therefore
// streams entries under the budget itself; a callback-only check is too late.
// Cache bounded listings so repeated validation of shared skill paths cannot
// multiply traversal or bypass the budget by changing the live directory.
type boundedPackageFS struct {
	fs.FS
	limit       int
	seen        map[string]struct{}
	directories map[string][]fs.DirEntry
}

func (bounded *boundedPackageFS) visit(name string) error {
	if _, exists := bounded.seen[name]; exists {
		return nil
	}
	if len(bounded.seen) >= bounded.limit {
		return fmt.Errorf("local package exceeds %d filesystem entries; reduce package size", bounded.limit)
	}
	bounded.seen[name] = struct{}{}
	return nil
}

func (bounded *boundedPackageFS) Lstat(name string) (fs.FileInfo, error) {
	if err := bounded.visit(name); err != nil {
		return nil, err
	}
	return fs.Lstat(bounded.FS, name)
}

func (bounded *boundedPackageFS) ReadDir(name string) (entries []fs.DirEntry, err error) {
	if cached, exists := bounded.directories[name]; exists {
		return cached, nil
	}
	if err := bounded.visit(name); err != nil {
		return nil, err
	}
	directory, err := bounded.FS.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	reader, ok := directory.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("directory %q does not support bounded reads", name)
	}
	for {
		batch, readErr := reader.ReadDir(1)
		for _, entry := range batch {
			if err := bounded.visit(path.Join(name, entry.Name())); err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	bounded.directories[name] = entries
	return entries, nil
}

func (bounded *boundedPackageFS) ReadLink(name string) (string, error) {
	return fs.ReadLink(bounded.FS, name)
}
