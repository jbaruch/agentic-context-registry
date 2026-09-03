package dependency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

const vendorContentDomain = "acr-tessl-vendor-v1\x00"

type vendorFileRecord struct {
	path string
	mode fs.FileMode
	size int64
	hash string
}

var hashVendorTreeAfterOpen = func(string) {}

func materializeVendor(projectDirectory string, locked LockedDependency) (MaterializedPackage, func() error, error) {
	identity, err := ParseVendorSource(locked.Source)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	root := filepath.Join(projectDirectory, ".agents", "vendor", identity.Workspace, identity.Package)
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	defer projectRoot.Close()
	relativeRoot := path.Join(".agents/vendor", identity.Workspace, identity.Package)
	if err := realize.ValidateParentDirectories(projectRoot, relativeRoot); err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("vendor_escape: %w", err)
	}
	info, err := projectRoot.Lstat(relativeRoot)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return MaterializedPackage{}, nil, fmt.Errorf("vendor_escape: %s must be a regular directory", relativeRoot)
	}
	if err := verifyLockedVendorTree(root, locked); err != nil {
		return MaterializedPackage{}, nil, err
	}
	value, err := tesslplugin.SynthesizeVendorManifest(os.DirFS(root), identity.FullName(), locked.PackageVersion)
	if err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("synthesize manifest for %s: %w", locked.Source, err)
	}
	if err := manifest.ValidateArtifactsAt(root, value); err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("validate vendored package %s: %w", locked.Source, err)
	}
	return MaterializedPackage{Root: root, Manifest: value}, func() error { return nil }, nil
}

func verifyLockedVendorTree(root string, locked LockedDependency) error {
	contentHash, err := HashVendorTree(root)
	if err != nil {
		return fmt.Errorf("verify vendored package %s: %w", locked.Source, err)
	}
	if contentHash != locked.ContentHash {
		return fmt.Errorf("content hash mismatch for %s: expected %s, found %s; restore the vendored tree from version control or rerun 'acr migrate tessl --vendor-unmapped'", locked.Source, locked.ContentHash, contentHash)
	}
	return nil
}

func rebuildVendorLock(projectDirectory string, declaration Declaration) (LockedDependency, error) {
	identity, err := ParseVendorSource(declaration.Source)
	if err != nil {
		return LockedDependency{}, err
	}
	root := filepath.Join(projectDirectory, ".agents", "vendor", identity.Workspace, identity.Package)
	hash, err := HashVendorTree(root)
	if err != nil {
		return LockedDependency{}, fmt.Errorf("rebuild vendored lock %s: %w", declaration.Source, err)
	}
	version, err := tesslPackageVersion(root)
	if err != nil {
		return LockedDependency{}, fmt.Errorf("read vendored version %s: %w", declaration.Source, err)
	}
	return LockedDependency{Source: declaration.Source, Requested: "vendored", Kind: ResolutionVendor, PackageVersion: version, ContentHash: hash}, nil
}

func tesslPackageVersion(root string) (string, error) {
	for _, filename := range []string{filepath.Join(root, ".tessl-plugin", "plugin.json"), filepath.Join(root, "tile.json")} {
		content, err := os.ReadFile(filename)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		var header struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(content, &header); err != nil {
			return "", err
		}
		if header.Version != "" {
			return header.Version, nil
		}
	}
	return "", errors.New("plugin.json or tile.json does not record a version")
}

// HashVendorTree computes the normalized all-file hash for a vendor root.
func HashVendorTree(root string) (result string, err error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("vendor_escape: %q must be a regular directory", root)
	}
	vendorRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("open vendor root %q: %w", root, err)
	}
	defer func() {
		if closeErr := vendorRoot.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close vendor root %q: %w", root, closeErr))
		}
	}()
	if err := verifyVendorHashRoot(root, vendorRoot, rootInfo); err != nil {
		return "", err
	}
	var records []vendorFileRecord
	if err := walkVendorHashTree(vendorRoot, ".", &records); err != nil {
		return "", err
	}
	if err := verifyVendorHashRoot(root, vendorRoot, rootInfo); err != nil {
		return "", err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	hash := sha256.New()
	_, _ = io.WriteString(hash, vendorContentDomain)
	for _, record := range records {
		_, _ = fmt.Fprintf(hash, "%s\x00%04o\x00%d\x00%s\x00", record.path, record.mode.Perm(), record.size, record.hash)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func walkVendorHashTree(root *os.Root, directory string, records *[]vendorFileRecord) error {
	entries, err := readVendorHashDirectory(root, directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		relative := path.Join(directory, entry.Name())
		info, err := root.Lstat(relative)
		if err != nil {
			return fmt.Errorf("inspect vendor entry %q: %w", relative, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("vendor_escape: %q is a symbolic link", relative)
		}
		if info.IsDir() {
			if err := walkVendorHashTree(root, relative, records); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("vendor_escape: %q is not a regular file", relative)
		}
		record, err := hashVendorFile(root, relative)
		if err != nil {
			return err
		}
		*records = append(*records, record)
	}
	return nil
}

func readVendorHashDirectory(root *os.Root, directory string) (entries []fs.DirEntry, err error) {
	if err := realize.ValidateParentDirectories(root, directory); err != nil {
		return nil, err
	}
	info, err := root.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect vendor directory %q: %w", directory, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("vendor_escape: %q must be a regular directory", directory)
	}
	dir, err := root.Open(directory)
	if err != nil {
		return nil, fmt.Errorf("open vendor directory %q: %w", directory, err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close vendor directory %q: %w", directory, closeErr))
		}
	}()
	opened, err := dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened vendor directory %q: %w", directory, err)
	}
	current, err := root.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect vendor directory %q after opening: %w", directory, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(opened, current) {
		return nil, fmt.Errorf("vendor directory %q changed while being opened; keep it stable and retry", directory)
	}
	entries, err = dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read vendor directory %q: %w", directory, err)
	}
	openedAfter, err := dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened vendor directory %q after reading: %w", directory, err)
	}
	current, err = root.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect vendor directory %q after reading: %w", directory, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(openedAfter, current) || vendorMetadataChanged(opened, openedAfter) || vendorMetadataChanged(openedAfter, current) {
		return nil, fmt.Errorf("vendor directory %q changed while being read; keep it stable and retry", directory)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func hashVendorFile(root *os.Root, relative string) (record vendorFileRecord, err error) {
	if err := realize.ValidateParentDirectories(root, relative); err != nil {
		return vendorFileRecord{}, err
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return vendorFileRecord{}, fmt.Errorf("inspect vendor file %q: %w", relative, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return vendorFileRecord{}, fmt.Errorf("vendor_escape: %q must be a regular file", relative)
	}
	file, err := root.Open(relative)
	if err != nil {
		return vendorFileRecord{}, fmt.Errorf("open vendor file %q: %w", relative, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close vendor file %q: %w", relative, closeErr))
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return vendorFileRecord{}, fmt.Errorf("inspect opened vendor file %q: %w", relative, err)
	}
	hashVendorTreeAfterOpen(relative)
	current, err := root.Lstat(relative)
	if err != nil {
		return vendorFileRecord{}, fmt.Errorf("inspect vendor file %q after opening: %w", relative, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return vendorFileRecord{}, fmt.Errorf("vendor file %q changed while being opened; keep it stable and retry", relative)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return vendorFileRecord{}, fmt.Errorf("hash vendor file %q: %w", relative, err)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return vendorFileRecord{}, fmt.Errorf("inspect opened vendor file %q after hashing: %w", relative, err)
	}
	current, err = root.Lstat(relative)
	if err != nil {
		return vendorFileRecord{}, fmt.Errorf("inspect vendor file %q after hashing: %w", relative, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(openedAfter, current) || vendorMetadataChanged(opened, openedAfter) || vendorMetadataChanged(openedAfter, current) {
		return vendorFileRecord{}, fmt.Errorf("vendor file %q changed while being hashed; keep it stable and retry", relative)
	}
	mode := fs.FileMode(0o644)
	if current.Mode().Perm()&0o111 != 0 {
		mode = 0o755
	}
	return vendorFileRecord{path: relative, mode: mode, size: current.Size(), hash: hex.EncodeToString(digest.Sum(nil))}, nil
}

func verifyVendorHashRoot(rootName string, root *os.Root, expected fs.FileInfo) error {
	opened, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open vendor root for verification: %w", err)
	}
	openedInfo, statErr := opened.Stat()
	closeErr := opened.Close()
	if statErr != nil || closeErr != nil {
		return fmt.Errorf("inspect vendor root: %w", errors.Join(statErr, closeErr))
	}
	current, err := os.Lstat(rootName)
	if err != nil {
		return fmt.Errorf("inspect vendor root after opening: %w", err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(expected, openedInfo) || !os.SameFile(openedInfo, current) {
		return fmt.Errorf("vendor root %q changed while being read; keep it stable and retry", rootName)
	}
	return nil
}

func vendorMetadataChanged(before, after fs.FileInfo) bool {
	return before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode()
}
