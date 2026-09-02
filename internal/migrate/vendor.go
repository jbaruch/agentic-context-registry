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

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
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

// PlanVendor validates, inventories, and hashes one installed Tessl package.
// It is pure: callers own all writes and receive copies of every source byte.
func PlanVendor(snapshot adapter.Snapshot, install PackageInstall) (VendorPlan, error) {
	if !validTesslIdentity(install.TesslIdentity) {
		return VendorPlan{}, fmt.Errorf("vendor_escape: invalid Tessl package identity %q", install.TesslIdentity)
	}
	entries, err := adapter.WalkSnapshot(snapshot, install.Root)
	if err != nil {
		return VendorPlan{}, fmt.Errorf("inventory vendored package %s: %w", install.TesslIdentity, err)
	}
	files := make([]VendorFile, 0, len(entries))
	for _, entry := range entries {
		relative := strings.TrimPrefix(entry.Path, strings.TrimSuffix(install.Root, "/")+"/")
		if !validVendorPath(relative) {
			return VendorPlan{}, fmt.Errorf("vendor_escape: package %s contains unsafe path %q", install.TesslIdentity, entry.Path)
		}
		if entry.Mode.IsDir() && entry.Mode&fs.ModeSymlink == 0 {
			continue
		}
		if entry.Mode&fs.ModeSymlink != 0 || !entry.Mode.IsRegular() {
			return VendorPlan{}, fmt.Errorf("vendor_escape: package %s path %q is not a regular file", install.TesslIdentity, entry.Path)
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
	value, err := synthesizeVendorManifest(snapshot, install)
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

func synthesizeVendorManifest(snapshot adapter.Snapshot, install PackageInstall) (manifest.Manifest, error) {
	value := manifest.Manifest{SchemaVersion: manifest.CurrentSchemaVersion, Name: install.TesslIdentity, Version: install.Version}
	rules, err := NormalizeRules(snapshot, install)
	if err != nil {
		return manifest.Manifest{}, err
	}
	for _, rule := range rules {
		if rule.Ambiguous {
			continue
		}
		value.Artifacts.Rules = append(value.Artifacts.Rules, manifest.RuleArtifact{ID: rule.ID, Path: rule.Path, Activation: rule.Activation})
	}
	skills, err := NormalizeSkills(snapshot, install)
	if err != nil {
		return manifest.Manifest{}, err
	}
	for _, skill := range skills {
		if skill.Ambiguous || skill.Unsupported {
			continue
		}
		value.Artifacts.Skills = append(value.Artifacts.Skills, manifest.SkillArtifact{ID: skill.ID, Path: skill.Path})
	}
	hooks, err := NormalizeHooks(snapshot, install)
	if err != nil {
		return manifest.Manifest{}, err
	}
	for _, hook := range hooks {
		if hook.Ambiguous || hook.Unsupported {
			continue
		}
		args := []string{}
		if len(hook.Argv) > 2 {
			args = append(args, hook.Argv[2:]...)
		}
		value.Artifacts.Hooks = append(value.Artifacts.Hooks, manifest.HookArtifact{ID: hook.ID, Path: hook.RelPath, Event: hook.Event, Args: args})
	}
	return value, nil
}
