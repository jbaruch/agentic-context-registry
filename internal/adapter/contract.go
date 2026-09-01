// Package adapter defines the versioned boundary between native agent
// renderers and the transactional realization engine. An adapter inspects
// resolved packages and the project tree and renders data-only Output
// values; it never writes files and never constructs realize.Intent
// directly. compileOutputs is the sole trusted bridge from adapter output to
// realize.Intent.
package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// BoundaryVersion is the exact adapter/coordinator API and semantic contract
// an adapter implementation was built against.
type BoundaryVersion uint32

// CurrentBoundaryVersion is the boundary this package implements. A signature
// or semantic compatibility break to this package increments it; an
// adapter's own output changes bump Descriptor.Version instead.
const CurrentBoundaryVersion BoundaryVersion = 1

// Descriptor identifies one adapter implementation and its contract version.
type Descriptor struct {
	ID       string
	Version  string
	Boundary BoundaryVersion
}

// ArtifactKind is the manifest-neutral artifact class an adapter may support.
type ArtifactKind string

const (
	ArtifactRule   ArtifactKind = "rule"
	ArtifactSkill  ArtifactKind = "skill"
	ArtifactScript ArtifactKind = "script"
	ArtifactHook   ArtifactKind = "hook"
)

// ObservedFile is one file read from the project tree.
type ObservedFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
	Hash    string
}

// Snapshot is read-only access to the project tree an adapter inspects. A
// missing file is reported as an error satisfying errors.Is(err,
// fs.ErrNotExist).
type Snapshot interface {
	ReadFile(path string) (ObservedFile, error)
}

// Package is one resolved dependency's manifest plus its declared file tree.
type Package struct {
	Source   string
	Root     fs.FS
	Manifest manifest.Manifest
}

// DetectRequest is the input to Adapter.Detect.
type DetectRequest struct {
	Project Snapshot
}

// Detection reports whether an adapter's agent is already in use in a
// project. Absence of evidence is not an error.
type Detection struct {
	Detected bool
	Evidence []string
}

// OwnerRef identifies the manifest artifact and event responsible for one
// plan item, output, or ledger entry.
type OwnerRef struct {
	Source     string
	ArtifactID string
	SourcePath string
	Kind       ArtifactKind
	Event      manifest.HookEvent
}

// PlanItem is one native target an adapter intends to render, without its
// final bytes.
type PlanItem struct {
	Owner  OwnerRef
	Target string
	Kind   OutputKind
	Mode   fs.FileMode
}

// NativePlan is the deterministic target map an adapter proposes before
// Render, used for dry-run and check summaries.
type NativePlan struct {
	Adapter Descriptor
	Items   []PlanItem
}

// PlanRequest is the input to Adapter.Plan.
type PlanRequest struct {
	Project  Snapshot
	Packages []Package
	Previous realize.Ledger
}

// RenderRequest is the input to Adapter.Render.
type RenderRequest struct {
	Packages []Package
	Plan     NativePlan
}

// CandidateFile is one compiled, pre-write native file an adapter validates
// before its intents reach realize.Engine.
type CandidateFile struct {
	Path      string
	Content   []byte
	Mode      fs.FileMode
	Ownership realize.Ownership
}

// ValidateRequest is the input to Adapter.Validate.
type ValidateRequest struct {
	Plan  NativePlan
	Files []CandidateFile
}

// Adapter renders one native agent's configuration from resolved packages.
// It may inspect the project snapshot but never writes files and never
// returns a realize.Intent directly; compileOutputs is the only bridge.
type Adapter interface {
	Descriptor() Descriptor
	Detect(ctx context.Context, request DetectRequest) (Detection, error)
	SupportedArtifacts() []ArtifactKind
	SupportedEvents() []manifest.HookEvent
	Plan(ctx context.Context, request PlanRequest) (NativePlan, error)
	Render(ctx context.Context, request RenderRequest) ([]Output, error)
	Validate(ctx context.Context, request ValidateRequest) error
}

// FSSnapshot is a read-only Snapshot backed by an fs.FS. It is test-only
// scaffolding, not safe for a real project tree: fs.FS implementations such
// as os.DirFS are permitted to follow a symlink outside their root, so a
// project symlink can make ReadFile return bytes from outside the project.
// Production code must use RootSnapshot instead, which is confined the same
// way internal/realize's own write boundary is.
type FSSnapshot struct {
	fsys fs.FS
}

// NewFSSnapshot returns a test-only Snapshot reading files from fsys.
func NewFSSnapshot(fsys fs.FS) FSSnapshot {
	return FSSnapshot{fsys: fsys}
}

// ReadFile implements Snapshot.
func (snapshot FSSnapshot) ReadFile(path string) (ObservedFile, error) {
	content, err := fs.ReadFile(snapshot.fsys, path)
	if err != nil {
		return ObservedFile{}, err
	}
	info, err := fs.Stat(snapshot.fsys, path)
	if err != nil {
		return ObservedFile{}, err
	}
	return ObservedFile{Path: path, Content: content, Mode: info.Mode(), Hash: hashContent(content)}, nil
}

// maxSnapshotBytes bounds one RootSnapshot read, matching
// internal/realize's own per-target size limit.
const maxSnapshotBytes = 32 << 20

// RootSnapshot is the production-safe, read-only Snapshot: it is backed by
// os.OpenRoot, so no path component and no symlink it follows can resolve
// outside the project directory, matching the confinement
// internal/realize's write boundary already relies on. It rejects symlinks
// and special files at the leaf, and caps how many bytes it will read.
type RootSnapshot struct {
	root *os.Root
}

// NewRootSnapshot opens dir as a root-confined project Snapshot. The caller
// must Close it when done.
func NewRootSnapshot(dir string) (*RootSnapshot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open project root %q: %w", dir, err)
	}
	return &RootSnapshot{root: root}, nil
}

// Close releases the underlying root directory handle.
func (snapshot *RootSnapshot) Close() error {
	return snapshot.root.Close()
}

// ReadFile implements Snapshot.
func (snapshot *RootSnapshot) ReadFile(path string) (ObservedFile, error) {
	info, err := snapshot.root.Lstat(path)
	if err != nil {
		return ObservedFile{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ObservedFile{}, fmt.Errorf("%q must be a regular file, not a symlink or special file", path)
	}
	file, err := snapshot.root.Open(path)
	if err != nil {
		return ObservedFile{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxSnapshotBytes+1))
	if err != nil {
		return ObservedFile{}, fmt.Errorf("read %q: %w", path, err)
	}
	if len(content) > maxSnapshotBytes {
		return ObservedFile{}, fmt.Errorf("%q exceeds %d MiB; reduce the file size and retry", path, maxSnapshotBytes>>20)
	}
	current, err := snapshot.root.Lstat(path)
	if err != nil {
		return ObservedFile{}, fmt.Errorf("inspect %q after reading: %w", path, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() {
		return ObservedFile{}, fmt.Errorf("%q changed to a symlink or special file while being read", path)
	}
	return ObservedFile{Path: path, Content: content, Mode: current.Mode(), Hash: hashContent(content)}, nil
}

var errFileNotFound = fs.ErrNotExist

// readOptional reads path from snapshot, returning a zero-value, non-existent
// ObservedFile when the file is absent rather than propagating that as an
// error to the caller.
func readOptional(snapshot Snapshot, path string) (ObservedFile, bool, error) {
	observed, err := snapshot.ReadFile(path)
	if err == nil {
		return observed, true, nil
	}
	if errors.Is(err, errFileNotFound) {
		return ObservedFile{}, false, nil
	}
	return ObservedFile{}, false, fmt.Errorf("read %q: %w", path, err)
}

func hashContent(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

var (
	adapterIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	// semverPattern is the official semver.org regular expression.
	semverPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
)
