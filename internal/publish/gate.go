package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/antigravity"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

const CodeAdapterRealization = "adapter_realization_failed"

// Gate proves the exact archive about to be uploaded realizes idempotently
// through every supported adapter.
type Gate struct {
	standalone []adapter.Adapter
}

// NewGate constructs the production release gate.
func NewGate() *Gate {
	return newGate(claudecode.New(), codex.New(), cursor.New())
}

func newGate(adapters ...adapter.Adapter) *Gate {
	return &Gate{standalone: append([]adapter.Adapter(nil), adapters...)}
}

// Descriptors returns the adapter versions recorded in release metadata.
func (gate *Gate) Descriptors() []adapter.Descriptor {
	result := make([]adapter.Descriptor, len(gate.standalone))
	for index, candidate := range gate.standalone {
		result[index] = candidate.Descriptor()
	}
	return append(result, antigravity.Adapter{}.Descriptor())
}

// Validate checks the release archive in standalone projects and in the
// Codex and Antigravity configuration that delivers their shared surfaces.
func (gate *Gate) Validate(ctx context.Context, assets ReleaseAssets) (err error) {
	workspace, err := os.MkdirTemp("", "acr-publish-gate-*")
	if err != nil {
		return publishError(CodeAdapterRealization, "create publication gate directory: %v", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(workspace); removeErr != nil {
			err = errors.Join(err, publishError(CodeAdapterRealization, "remove publication gate directory: %v", removeErr))
		}
	}()
	return gate.validateArchive(ctx, assets, workspace)
}

func (gate *Gate) validateArchive(ctx context.Context, assets ReleaseAssets, workspace string) error {
	packageRoot := filepath.Join(workspace, "package")
	if err := dependency.ExtractPackageArchive(assets.Archive.Bytes, packageRoot); err != nil {
		return publishError(CodeAdapterRealization, "extract package archive for realization gate: %v", err)
	}
	value, err := manifest.Load(packageRoot)
	if err != nil {
		return publishError(CodeAdapterRealization, "load extracted package for realization gate: %v", err)
	}
	pkg := adapter.Package{Source: "github:" + value.Name, Root: os.DirFS(packageRoot), Manifest: value}
	for _, candidate := range gate.standalone {
		if err := runAdapterGate(ctx, pkg, candidate); err != nil {
			return publishError(CodeAdapterRealization, "adapter %q failed the publication realization gate: %v; fix the package for every supported adapter before publishing", candidate.Descriptor().ID, err)
		}
	}
	archive := extractedPackage{Root: packageRoot, Manifest: value}
	if err := archive.checkAntigravity(ctx, filepath.Join(workspace, "project"), assets.Evidence); err != nil {
		return publishError(CodeAdapterRealization, "codex and antigravity failed the publication realization gate: %v; fix the package before publishing", err)
	}
	return nil
}

// The gate owns this single extracted dependency for both consumer runs.
type extractedPackage dependency.MaterializedPackage

func (pkg extractedPackage) MaterializeLocked(context.Context, dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error) {
	return dependency.MaterializedPackage(pkg), func() error { return nil }, nil
}

func (pkg extractedPackage) checkAntigravity(ctx context.Context, projectRoot string, evidence Metadata) error {
	if err := os.Mkdir(projectRoot, 0o700); err != nil {
		return fmt.Errorf("create paired project: %w", err)
	}
	pin := dependency.Declaration{Source: "github:" + pkg.Manifest.Name, Requested: evidence.Commit}
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion, Agents: []string{"codex", "antigravity"},
			SharedSkills: true, Freshness: "none", Dependencies: []dependency.Declaration{pin},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Dependencies: []dependency.LockedDependency{{
				Source: pin.Source, Requested: pin.Requested, Kind: dependency.ResolutionCommit,
				Commit: evidence.Commit, PackageVersion: pkg.Manifest.Version, ContentHash: evidence.ContentHash,
			}},
		},
	}
	if err := dependency.WriteState(projectRoot, state); err != nil {
		return fmt.Errorf("configure paired project: %w", err)
	}
	service := realizeapp.NewService(pkg)
	if _, err := service.Run(ctx, projectRoot, nil, realize.ModeApply); err != nil {
		return fmt.Errorf("apply realization: %w", err)
	}
	result, err := service.Run(ctx, projectRoot, nil, realize.ModeCheck)
	if err != nil {
		return fmt.Errorf("check realization: %w", err)
	}
	if result.Plan.HasChanges() {
		return errors.New("second realization has changes")
	}
	return nil
}

func runAdapterGate(ctx context.Context, pkg adapter.Package, candidate adapter.Adapter) (err error) {
	projectRoot, err := os.MkdirTemp("", "acr-publish-gate-project-*")
	if err != nil {
		return fmt.Errorf("create empty project: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(projectRoot); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove adapter project: %w", removeErr))
		}
	}()
	snapshot, err := adapter.NewRootSnapshot(projectRoot)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close adapter project snapshot: %w", closeErr))
		}
	}()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), candidate)
	if err != nil {
		return err
	}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	intents, err := coordinator.Realize(ctx, snapshot, []adapter.Package{pkg}, previous)
	if err != nil {
		return err
	}
	var applied realize.Ledger
	if _, err := realize.NewEngine().Run(projectRoot, previous, intents, realize.ModeApply, func(next realize.Ledger) error {
		applied = next
		return nil
	}); err != nil {
		return fmt.Errorf("apply realization: %w", err)
	}
	checkIntents, err := coordinator.Realize(ctx, snapshot, []adapter.Package{pkg}, applied)
	if err != nil {
		return fmt.Errorf("render second realization: %w", err)
	}
	if _, err := realize.NewEngine().Run(projectRoot, applied, checkIntents, realize.ModeCheck, nil); err != nil {
		return fmt.Errorf("second realization is not idempotent: %w", err)
	}
	return nil
}
