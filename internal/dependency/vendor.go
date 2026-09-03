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
func HashVendorTree(root string) (string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("vendor_escape: %q must be a regular directory", root)
	}
	var records []vendorFileRecord
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("vendor_escape: %q is a symbolic link", filename)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("vendor_escape: %q is not a regular file", filename)
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("hash %q: %w", relative, errors.Join(copyErr, closeErr))
		}
		mode := fs.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		records = append(records, vendorFileRecord{path: relative, mode: mode, size: info.Size(), hash: hex.EncodeToString(digest.Sum(nil))})
		return nil
	})
	if err != nil {
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
