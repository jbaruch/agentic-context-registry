package publish

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const CodeAdapterRealization = "adapter_realization_failed"

// Gate proves the exact archive about to be uploaded realizes idempotently
// through every supported adapter.
type Gate struct {
	adapters []adapter.Adapter
}

// NewGate constructs the production release gate.
func NewGate() *Gate {
	return newGate(claudecode.New(), codex.New(), cursor.New())
}

func newGate(adapters ...adapter.Adapter) *Gate {
	return &Gate{adapters: append([]adapter.Adapter(nil), adapters...)}
}

// Descriptors returns the adapter versions recorded in release metadata.
func (gate *Gate) Descriptors() []adapter.Descriptor {
	result := make([]adapter.Descriptor, len(gate.adapters))
	for index, candidate := range gate.adapters {
		result[index] = candidate.Descriptor()
	}
	return result
}

// Validate extracts archive through the consumer path, realizes it in a fresh
// project for each adapter, and proves a second check reports no changes.
func (gate *Gate) Validate(ctx context.Context, archive []byte) (err error) {
	packageRoot, err := os.MkdirTemp("", "acr-publish-gate-package-*")
	if err != nil {
		return publishError(CodeAdapterRealization, "create publication gate package directory: %v", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(packageRoot); removeErr != nil {
			err = errors.Join(err, publishError(CodeAdapterRealization, "remove publication gate package directory: %v", removeErr))
		}
	}()
	if err := dependency.ExtractPackageArchive(archive, packageRoot); err != nil {
		return publishError(CodeAdapterRealization, "extract package archive for realization gate: %v", err)
	}
	value, err := manifest.Load(packageRoot)
	if err != nil {
		return publishError(CodeAdapterRealization, "load extracted package for realization gate: %v", err)
	}
	pkg := adapter.Package{Source: "github:" + value.Name, Root: os.DirFS(packageRoot), Manifest: value}
	for _, candidate := range gate.adapters {
		if err := runAdapterGate(ctx, pkg, candidate); err != nil {
			return publishError(CodeAdapterRealization, "adapter %q failed the publication realization gate: %v; fix the package for every supported adapter before publishing", candidate.Descriptor().ID, err)
		}
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
