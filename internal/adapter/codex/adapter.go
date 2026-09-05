// Package codex implements the Codex native adapter.
package codex

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	adapterID      = "codex"
	adapterVersion = "1.0.1"
	configPath     = ".codex/config.toml"
)

// Adapter renders Codex native layouts.
type Adapter struct{}

// New returns the Codex adapter.
func New() Adapter { return Adapter{} }

func (Adapter) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: adapterID, Version: adapterVersion, Boundary: adapter.CurrentBoundaryVersion}
}

func (Adapter) Detect(_ context.Context, request adapter.DetectRequest) (adapter.Detection, error) {
	evidence, err := adapter.ExistingEvidence(request.Project,
		[]string{configPath, "AGENTS.md", "AGENTS.override.md"}, []string{".codex/skills"})
	if err != nil {
		return adapter.Detection{}, err
	}
	return adapter.Detection{Detected: len(evidence) != 0, Evidence: evidence}, nil
}

func (Adapter) SupportedArtifacts() []adapter.ArtifactKind {
	return []adapter.ArtifactKind{adapter.ArtifactHook, adapter.ArtifactRule, adapter.ArtifactScript, adapter.ArtifactSkill}
}

func (Adapter) SupportedEvents() []manifest.HookEvent { return adapter.SortedEvents() }

func (candidate Adapter) Plan(_ context.Context, request adapter.PlanRequest) (adapter.NativePlan, error) {
	items, err := planGeneratedFiles(request.Packages)
	if err != nil {
		return adapter.NativePlan{}, err
	}
	return adapter.NativePlan{Adapter: candidate.Descriptor(), Items: items}, nil
}

func planGeneratedFiles(packages []adapter.Package) ([]adapter.PlanItem, error) {
	var items []adapter.PlanItem
	for _, pkg := range sortedPackages(packages) {
		skills := append([]manifest.SkillArtifact(nil), pkg.Manifest.Artifacts.Skills...)
		sort.SliceStable(skills, func(left, right int) bool { return skills[left].ID < skills[right].ID })
		for _, skill := range skills {
			name, err := adapter.NativeArtifactName(pkg.Source, skill.ID)
			if err != nil {
				return nil, err
			}
			files, err := adapter.ReadPackageTree(pkg, skill.Path)
			if err != nil {
				return nil, fmt.Errorf("read skill %q from %s: %w", skill.ID, pkg.Source, err)
			}
			for _, file := range files {
				relative := strings.TrimPrefix(strings.TrimPrefix(file.Path, skill.Path), "/")
				mode := fs.FileMode(0o644)
				if file.Mode.Perm()&0o111 != 0 {
					mode = 0o755
				}
				items = append(items, adapter.PlanItem{
					Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: skill.ID, SourcePath: file.Path, Kind: adapter.ArtifactSkill},
					Target: path.Join(".codex/skills", name, relative), Kind: adapter.OutputGeneratedFile, Mode: mode,
				})
			}
		}
		for _, script := range sortedScripts(pkg.Manifest.Artifacts.Scripts) {
			name, err := adapter.NativeArtifactName(pkg.Source, script.ID)
			if err != nil {
				return nil, err
			}
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: script.ID, SourcePath: script.Path, Kind: adapter.ArtifactScript},
				Target: path.Join(".codex/scripts", name, adapter.SourceBasename(script.Path)), Kind: adapter.OutputGeneratedFile, Mode: 0o755,
			})
		}
		for _, hook := range sortedHooks(pkg.Manifest.Artifacts.Hooks) {
			name, err := adapter.NativeArtifactName(pkg.Source, hook.ID)
			if err != nil {
				return nil, err
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: hook.ID, SourcePath: hook.Path, Kind: adapter.ArtifactHook, Event: hook.Event}
			items = append(items,
				adapter.PlanItem{Owner: owner, Target: path.Join(".codex/hooks", name, adapter.SourceBasename(hook.Path)), Kind: adapter.OutputGeneratedFile, Mode: 0o755},
				adapter.PlanItem{Owner: owner, Target: configPath, Kind: adapter.OutputConfigMerge, Mode: 0o644},
			)
		}
	}
	sort.SliceStable(items, func(left, right int) bool { return planItemKey(items[left]) < planItemKey(items[right]) })
	return items, nil
}

func nativeEvent(event manifest.HookEvent) (string, bool) {
	values := map[manifest.HookEvent]string{
		manifest.HookSessionStart: "SessionStart", manifest.HookSessionEnd: "SessionEnd",
		manifest.HookUserPromptSubmit: "UserPromptSubmit", manifest.HookPreToolUse: "PreToolUse",
		manifest.HookPostToolUse: "PostToolUse", manifest.HookStop: "Stop",
	}
	value, ok := values[event]
	return value, ok
}

func sortedPackages(packages []adapter.Package) []adapter.Package {
	result := append([]adapter.Package(nil), packages...)
	sort.SliceStable(result, func(left, right int) bool { return result[left].Source < result[right].Source })
	return result
}

func sortedScripts(values []manifest.ScriptArtifact) []manifest.ScriptArtifact {
	result := append([]manifest.ScriptArtifact(nil), values...)
	sort.SliceStable(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result
}

func sortedHooks(values []manifest.HookArtifact) []manifest.HookArtifact {
	result := append([]manifest.HookArtifact(nil), values...)
	sort.SliceStable(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result
}

func planItemKey(item adapter.PlanItem) string {
	return strings.Join([]string{item.Target, string(item.Kind), item.Owner.Source, item.Owner.ArtifactID, item.Owner.SourcePath, string(item.Owner.Event)}, "\x00")
}
