package publish

import (
	"context"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Prepared is the fully validated, gated release payload before any GitHub
// mutation occurs.
type Prepared struct {
	Manifest manifest.Manifest
	Identity Identity
	Assets   ReleaseAssets
}

// Builder runs publication stages P1-P5 against local package and Git state.
type Builder struct {
	git       gitSource
	gate      *Gate
	generator string
}

// NewBuilder constructs the production package builder.
func NewBuilder(version string) *Builder {
	return &Builder{git: commandGitSource{}, gate: NewGate(), generator: "acr " + version}
}

func newBuilder(source gitSource, gate *Gate, generator string) *Builder {
	return &Builder{git: source, gate: gate, generator: generator}
}

// Prepare validates the worktree package, binds it to one tag at HEAD, reads
// committed blobs, builds release assets, and runs the realization gate.
func (builder *Builder) Prepare(ctx context.Context, root string) (Prepared, error) {
	value, err := manifest.Load(root)
	if err != nil {
		return Prepared{}, err
	}
	names, err := manifest.PackageFiles(root, value)
	if err != nil {
		return Prepared{}, err
	}
	identity, err := resolveIdentity(ctx, root, value.Version, builder.git)
	if err != nil {
		return Prepared{}, err
	}
	files, err := readTreeFiles(ctx, root, identity.Tag, names, builder.git)
	if err != nil {
		return Prepared{}, err
	}
	assets, err := BuildReleaseAssets(value, identity, files, builder.gate.Descriptors(), builder.generator)
	if err != nil {
		return Prepared{}, fmt.Errorf("build release assets: %w", err)
	}
	if err := builder.gate.Validate(ctx, assets.Archive.Bytes); err != nil {
		return Prepared{}, err
	}
	return Prepared{Manifest: value, Identity: identity, Assets: assets}, nil
}
