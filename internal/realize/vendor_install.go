package realize

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
)

const vendorPromotion = "vendor-promote"

// VendorTreeInstall contains the regular files of a new vendor package.
type VendorTreeInstall struct {
	Path  string
	Files []VendorTreeFile
}

// VendorTreeFile is a file relative to its package root.
type VendorTreeFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
}

// The staging slot is keyed by the promotion entry's journal ordinal.
func vendorStageName(index int) string { return fmt.Sprintf("vendor-%06d", index) }

func stageVendorTree(staging, name string, files []VendorTreeFile) (string, error) {
	root, err := os.OpenRoot(staging)
	if err != nil {
		return "", diagnostic.OpaqueError(err)
	}
	defer root.Close()
	if err := root.Mkdir(name, 0o700); err != nil {
		return "", diagnostic.OpaqueError(err)
	}
	for _, file := range files {
		if file.Path == "." || path.Clean(file.Path) != file.Path || strings.ContainsAny(file.Path, "\\\x00") || !filepath.IsLocal(filepath.FromSlash(file.Path)) {
			return "", fmt.Errorf("invalid vendored file path %q", file.Path)
		}
		target := path.Join(name, file.Path)
		if _, err := ensureParentDirectories(root, target); err != nil {
			return "", err
		}
		if err := writeFileAtomic(root, target, file.Content, file.Mode); err != nil {
			return "", err
		}
	}
	return fingerprintVendorTree(root, name, true)
}

// The package's directory identity survives rename. Including it and the
// exact tree in the fingerprint keeps rollback from taking a replacement
// directory or a package that an operator has changed since promotion.
func fingerprintVendorTree(root *os.Root, filename string, sync bool) (string, error) {
	info, err := root.Lstat(filename)
	if err != nil {
		return "", diagnostic.OpaqueError(err)
	}
	device, inode, known := filesystemIdentity(info)
	if !known || !info.IsDir() {
		return "", fmt.Errorf("vendor tree %q must be an identifiable directory", filename)
	}
	var tree VendorTreeRemovalPlan
	if err := walkVendorRemovalTree(root, filename, &tree); err != nil {
		return "", err
	}
	var signature bytes.Buffer
	fmt.Fprintf(&signature, "%d %d\n", device, inode)
	for i := len(tree.directories) - 1; i >= 0; i-- {
		directory := tree.directories[i]
		current, err := root.Lstat(directory)
		if err != nil {
			return "", diagnostic.OpaqueError(err)
		}
		fmt.Fprintf(&signature, "directory %q %o\n", strings.TrimPrefix(directory, filename), current.Mode())
		if sync {
			if err := journalDirectorySync(filepath.Join(root.Name(), filepath.FromSlash(directory))); err != nil {
				return "", fmt.Errorf("sync staged vendor directory %q: %w", directory, err)
			}
		}
	}
	for _, edit := range tree.edits {
		fmt.Fprintf(&signature, "file %q %o %q %s\n", strings.TrimPrefix(edit.Path, filename), edit.BeforeMode, edit.LinkTarget, contentHash(edit.Before))
	}
	current, err := root.Lstat(filename)
	if err != nil {
		return "", diagnostic.OpaqueError(err)
	}
	if !os.SameFile(info, current) || vendorTreeMetadataChanged(info, current) {
		return "", fmt.Errorf("vendor tree %q changed while being read; keep it stable and retry", filename)
	}
	return contentHash(signature.Bytes()), nil
}

func vendorPromotionNeedsRollback(root *os.Root, entry journalEntry, stage string) (bool, error) {
	// The same tree occupies the private staging slot before promotion and
	// after rollback. In either case, an occupant at the live path is foreign.
	for _, filename := range []string{stage, entry.Path} {
		if err := ValidateParentDirectories(root, filename); err != nil {
			return false, err
		}
		if _, err := root.Lstat(filename); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return false, diagnostic.OpaqueError(err)
		}
		hash, err := fingerprintVendorTree(root, filename, false)
		if err != nil {
			return false, err
		}
		if hash != entry.AfterHash {
			return false, fmt.Errorf("vendor tree %q differs from the staged package recorded in the journal", filename)
		}
		return filename == entry.Path, nil
	}
	return false, nil
}

func moveVendorTree(root *os.Root, from, to string) error {
	if err := ValidateParentDirectories(root, from); err != nil {
		return err
	}
	if err := ValidateParentDirectories(root, to); err != nil {
		return err
	}
	if _, err := root.Lstat(to); err == nil {
		return fmt.Errorf("vendor tree destination %q already exists; keep it intact and retry after reconciling its contents", to)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return diagnostic.OpaqueError(err)
	}
	if err := root.Rename(from, to); err != nil {
		return diagnostic.OpaqueError(err)
	}
	for _, filename := range []string{to, from} {
		if err := journalDirectorySync(filepath.Join(root.Name(), filepath.FromSlash(path.Dir(filename)))); err != nil {
			return fmt.Errorf("sync vendor tree parent for %q: %w", filename, err)
		}
	}
	return nil
}

func isVendorPromotionParent(directory string, entries []journalEntry) bool {
	for _, entry := range entries {
		if entry.Operation == vendorPromotion && strings.HasPrefix(entry.Path, directory+"/") {
			return true
		}
	}
	return false
}
