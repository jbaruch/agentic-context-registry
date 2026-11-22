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
	"syscall"

	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
)

const (
	vendorPromotion  = "vendor-promote"
	vendorRetirement = "vendor-retire"
)

// VendorTreeInstall contains the regular files of a new vendor package.
type VendorTreeInstall struct {
	Path  string
	Files []VendorTreeFile
}

// VendorTreeChanges are the package directory changes in a migration apply.
type VendorTreeChanges struct {
	Installs    []VendorTreeInstall
	Retirements []VendorTreeRetirement
}

// VendorTreeRetirement binds a package removal to its observed directory and contents.
type VendorTreeRetirement struct {
	path string
	hash string
}

// PlanVendorTreeRetirement snapshots the old package before migration replaces it.
func PlanVendorTreeRetirement(projectDirectory, filename string) (VendorTreeRetirement, error) {
	if err := validateVendorTreeRoot(filename); err != nil {
		return VendorTreeRetirement{}, err
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return VendorTreeRetirement{}, WrapProjectAccessError(projectDirectory, "open project directory for vendor retirement", err)
	}
	defer root.Close()
	hash, err := fingerprintVendorTree(root, filename, false, nil)
	return VendorTreeRetirement{path: filename, hash: hash}, err
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
	return fingerprintVendorTree(root, name, true, nil)
}

// Linux directories inherit setgid from their parent. The bit grants no access.
func isPrivateVendorDirectory(mode fs.FileMode) bool {
	return mode&^fs.ModeSetgid == fs.ModeDir|0o700
}

// The package's directory identity survives rename. Including it and the
// exact tree in the fingerprint keeps rollback from taking a replacement
// directory or a package that an operator has changed since promotion.
func fingerprintVendorTree(root *os.Root, filename string, sync bool, expected *legacyVendorShape) (string, error) {
	info, err := root.Lstat(filename)
	if err != nil {
		return "", diagnostic.OpaqueError(err)
	}
	if expected != nil && !isPrivateVendorDirectory(info.Mode()) {
		return "", errLegacyVendorMismatch
	}
	device, inode, known := filesystemIdentity(info)
	if !known || !info.IsDir() {
		return "", fmt.Errorf("vendor tree %q must be an identifiable directory", filename)
	}
	var tree VendorTreeRemovalPlan
	if err := walkVendorRemovalTree(root, filename, &tree, expected); err != nil {
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

// vendorTreeLocation returns the first occupied path, whatever its contents.
func vendorTreeLocation(root *os.Root, filenames ...string) (string, error) {
	for _, filename := range filenames {
		if err := ValidateParentDirectories(root, filename); err != nil {
			return "", err
		}
		if _, err := root.Lstat(filename); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return "", diagnostic.OpaqueError(err)
		}
		return filename, nil
	}
	return "", nil
}

func requireVendorTree(root *os.Root, filename, hash string) error {
	location, err := vendorTreeLocation(root, filename)
	if err != nil {
		return err
	}
	if location == "" {
		return fmt.Errorf("recorded vendor tree %q is missing", filename)
	}
	actual, err := fingerprintVendorTree(root, filename, false, nil)
	if err != nil {
		return err
	}
	if actual != hash {
		return fmt.Errorf("vendor tree %q differs from the package recorded in the journal", filename)
	}
	return nil
}

// Preserved trees can still have open writers after a rename. Keep their inodes
// outside the disposable journal, including after a committed retirement.
func recoveredVendorDirectory(id string) string { return ".agents/.acr-recovered-vendors-" + id }

func recoveredVendorPath(id string, index int) string {
	return path.Join(recoveredVendorDirectory(id), vendorStageName(index))
}

func preserveVendorTree(root *os.Root, from, to string) error {
	directory := path.Dir(to)
	if err := ValidateParentDirectories(root, directory); err != nil {
		return err
	}
	if err := root.Mkdir(directory, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create vendor recovery directory %q: %w", directory, diagnostic.OpaqueError(err))
	}
	info, err := root.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect vendor recovery directory %q: %w", directory, diagnostic.OpaqueError(err))
	}
	if !isPrivateVendorDirectory(info.Mode()) {
		return fmt.Errorf("vendor recovery directory %q must be a private directory with mode 0700", directory)
	}
	if err := journalDirectorySync(filepath.Join(root.Name(), ".agents")); err != nil {
		return fmt.Errorf("sync vendor recovery directory parent: %w", err)
	}
	return moveVendorTree(root, from, to)
}

func removeEmptyVendorRecoveryDirectory(root *os.Root, id string) error {
	directory := recoveredVendorDirectory(id)
	info, err := root.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect vendor recovery directory %q: %w", directory, diagnostic.OpaqueError(err))
	}
	if err := root.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("remove empty vendor recovery directory %q: %w", directory, diagnostic.OpaqueError(err))
	}
	return nil
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
