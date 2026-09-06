package adapter

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// SharedSkillsAdapter is the ledger adapter identity stamped on every
// shared-surface entry. It is not a registered Adapter and never appears in
// agents.yaml: coveredAgents and splitLedger key on the target-level owner, so
// this value never reaches selectAdapters.
const SharedSkillsAdapter = "coordinator"

// SharedSkillsVersion is the shared-surface compiler's own boundary version,
// recorded per entry the way an adapter records its descriptor version. It
// moves when the surface's rendering changes, independently of
// CurrentBoundaryVersion, because no adapter compiles this surface.
const SharedSkillsVersion = "1"

// SharedSkillIntents renders the coordinator-owned shared skill surface: one
// generated-only target per file of every package skill, at
// .agents/skills/acr__<workspace>__<package>__<skill>/…, with the same content
// and modes a per-agent skill tree receives.
//
// The surface is skills only. A generic consumer that reads .agents/skills has
// no hook runtime and no rule-activation vocabulary, so rules and hooks stay
// out of it.
//
// These intents bypass the adapter boundary deliberately. compileOutputs
// rejects every .agents target through realize.ValidateTargetPath, and that
// refusal is what keeps a package from naming the lock or the vendor tree; the
// coordinator compiles this surface itself and validates it through the
// narrower realize.ValidateSharedSurfacePath instead.
func SharedSkillIntents(packages []Package) ([]realize.Intent, error) {
	var intents []realize.Intent
	for _, pkg := range sortedSharedPackages(packages) {
		skills := append([]manifest.SkillArtifact(nil), pkg.Manifest.Artifacts.Skills...)
		sort.SliceStable(skills, func(left, right int) bool { return skills[left].ID < skills[right].ID })
		for _, skill := range skills {
			name, err := NativeArtifactName(pkg.Source, skill.ID)
			if err != nil {
				return nil, err
			}
			nativeRoot := path.Join(realize.SharedSurfaceRoot, name)
			files, err := ReadPackageTree(pkg, skill.Path)
			if err != nil {
				return nil, fmt.Errorf("read skill %q from %s: %w", skill.ID, pkg.Source, err)
			}
			for _, file := range files {
				relative := strings.TrimPrefix(strings.TrimPrefix(file.Path, skill.Path), "/")
				target := path.Join(nativeRoot, relative)
				if err := realize.ValidateSharedSurfacePath(target); err != nil {
					return nil, fmt.Errorf("shared skill %q from %s: %w", skill.ID, pkg.Source, err)
				}
				mode := fs.FileMode(0o644)
				if file.Mode.Perm()&0o111 != 0 {
					mode = 0o755
				}
				content := RebaseSkillReferences(file.Content, skill.Path, nativeRoot)
				intents = append(intents, realize.Intent{
					Action:    realize.ActionEnsure,
					Path:      target,
					Owner:     realize.OwnerCoordinator,
					Content:   content,
					Mode:      uint32(mode),
					Ownership: realize.OwnershipGenerated,
					Entries: []realize.Entry{{
						Source: pkg.Source, ArtifactID: skill.ID, ArtifactKind: realize.ArtifactFile,
						SourcePath: file.Path, Adapter: SharedSkillsAdapter, AdapterVersion: SharedSkillsVersion,
						ManagedHash: hashContent(content),
					}},
				})
			}
		}
	}
	sort.Slice(intents, func(left, right int) bool { return intents[left].Path < intents[right].Path })
	for index := 1; index < len(intents); index++ {
		if intents[index].Path == intents[index-1].Path {
			return nil, fmt.Errorf("shared skill surface target %q is rendered by more than one package skill; rename one skill before realizing", intents[index].Path)
		}
	}
	return intents, nil
}

func sortedSharedPackages(packages []Package) []Package {
	sorted := append([]Package(nil), packages...)
	sort.SliceStable(sorted, func(left, right int) bool { return sorted[left].Source < sorted[right].Source })
	return sorted
}
