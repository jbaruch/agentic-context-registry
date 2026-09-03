package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

const vendorHashDomain = "acr-tessl-vendor-v1\x00"

// VendorFile is one normalized regular file copied from a Tessl package.
type VendorFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
}

// VendorPlan is a complete, deterministic copy plan for one Tessl package.
type VendorPlan struct {
	Identity    string
	Source      string
	Version     string
	Destination string
	ContentHash string
	Files       []VendorFile
	Manifest    manifest.Manifest
}

// VendorEscapeError reports a source identity or tree entry that cannot be
// confined to its package's vendor destination.
type VendorEscapeError struct {
	Reason string
}

func (err *VendorEscapeError) Error() string { return "vendor_escape: " + err.Reason }

// PlanVendor validates, inventories, and hashes one installed Tessl package.
// It is pure: callers own all writes and receive copies of every source byte.
func PlanVendor(snapshot adapter.Snapshot, install PackageInstall) (VendorPlan, error) {
	if !validTesslIdentity(install.TesslIdentity) {
		return VendorPlan{}, &VendorEscapeError{Reason: fmt.Sprintf("invalid Tessl package identity %q", install.TesslIdentity)}
	}
	entries, err := adapter.WalkSnapshot(snapshot, install.Root)
	if err != nil {
		return VendorPlan{}, fmt.Errorf("inventory vendored package %s: %w", install.TesslIdentity, err)
	}
	files := make([]VendorFile, 0, len(entries))
	for _, entry := range entries {
		relative := strings.TrimPrefix(entry.Path, strings.TrimSuffix(install.Root, "/")+"/")
		if !validVendorPath(relative) {
			return VendorPlan{}, &VendorEscapeError{Reason: fmt.Sprintf("package %s contains unsafe path %q", install.TesslIdentity, entry.Path)}
		}
		if entry.Mode.IsDir() && entry.Mode&fs.ModeSymlink == 0 {
			continue
		}
		if entry.Mode&fs.ModeSymlink != 0 || !entry.Mode.IsRegular() {
			return VendorPlan{}, &VendorEscapeError{Reason: fmt.Sprintf("package %s path %q is not a regular file", install.TesslIdentity, entry.Path)}
		}
		observed, readErr := snapshot.ReadFile(entry.Path)
		if readErr != nil {
			return VendorPlan{}, fmt.Errorf("read vendored package file %q: %w", entry.Path, readErr)
		}
		mode := fs.FileMode(0o644)
		if observed.Mode.Perm()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, VendorFile{Path: relative, Content: append([]byte(nil), observed.Content...), Mode: mode})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	packageFS := fstest.MapFS{}
	for _, file := range files {
		packageFS[file.Path] = &fstest.MapFile{Data: append([]byte(nil), file.Content...), Mode: file.Mode}
	}
	value, err := tesslplugin.SynthesizeVendorManifest(packageFS, install.TesslIdentity, install.Version)
	if err != nil {
		return VendorPlan{}, err
	}
	return VendorPlan{
		Identity: install.TesslIdentity, Source: "vendor:" + install.TesslIdentity, Version: install.Version,
		Destination: path.Join(".agents/vendor", install.TesslIdentity), ContentHash: HashVendorFiles(files), Files: files, Manifest: value,
	}, nil
}

// HashVendorFiles returns the canonical digest over every regular package file.
func HashVendorFiles(files []VendorFile) string {
	ordered := append([]VendorFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	hash := sha256.New()
	_, _ = io.WriteString(hash, vendorHashDomain)
	for _, file := range ordered {
		fileHash := sha256.Sum256(file.Content)
		_, _ = fmt.Fprintf(hash, "%s\x00%04o\x00%d\x00%s\x00", file.Path, file.Mode.Perm(), len(file.Content), hex.EncodeToString(fileHash[:]))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func validVendorPath(value string) bool {
	if value == "" || value == "." || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') || path.Clean(value) != value {
		return false
	}
	if len(value) >= 2 && value[1] == ':' && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z') {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == ".." {
			return false
		}
	}
	return true
}
