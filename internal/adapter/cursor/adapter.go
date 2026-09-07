// Package cursor implements the Cursor native adapter.
package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	adapterID      = "cursor"
	adapterVersion = "1.0.1"
	hooksPath      = ".cursor/hooks.json"
)

var versionOwner = adapter.OwnerRef{
	Source:     "github:jbaruch/agentic-context-registry",
	ArtifactID: "cursor-hooks-schema",
	SourcePath: "internal/adapter/cursor/render.go",
	Kind:       adapter.ArtifactHook,
}

// Adapter renders Cursor native layouts.
type Adapter struct{}

// New returns the Cursor adapter.
func New() Adapter { return Adapter{} }

func (Adapter) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: adapterID, Version: adapterVersion, Boundary: adapter.CurrentBoundaryVersion}
}

func (Adapter) Detect(_ context.Context, request adapter.DetectRequest) (adapter.Detection, error) {
	evidence, err := adapter.ExistingEvidence(request.Project,
		[]string{hooksPath, ".cursor/mcp.json"}, []string{".cursor/skills"})
	if err != nil {
		return adapter.Detection{}, err
	}
	if snapshot, ok := request.Project.(adapter.DirectorySnapshot); ok {
		entries, readErr := snapshot.ReadDir(".cursor/rules")
		if readErr == nil {
			for _, entry := range entries {
				if entry.Mode.IsRegular() && strings.HasSuffix(entry.Path, ".mdc") {
					evidence = append(evidence, entry.Path)
				}
			}
		} else if !errors.Is(readErr, fs.ErrNotExist) {
			return adapter.Detection{}, fmt.Errorf("inspect detection evidence %q: %w", ".cursor/rules", readErr)
		}
	}
	sort.Strings(evidence)
	return adapter.Detection{Detected: len(evidence) != 0, Evidence: evidence}, nil
}

func (Adapter) SupportedArtifacts() []adapter.ArtifactKind {
	return []adapter.ArtifactKind{adapter.ArtifactHook, adapter.ArtifactRule, adapter.ArtifactScript, adapter.ArtifactSkill}
}

func (Adapter) SupportedEvents() []manifest.HookEvent { return adapter.SortedEvents() }

func (candidate Adapter) Plan(_ context.Context, request adapter.PlanRequest) (adapter.NativePlan, error) {
	items, hookCount, err := planGeneratedFiles(request.Packages)
	if err != nil {
		return adapter.NativePlan{}, err
	}
	if hookCount != 0 {
		absent, err := cursorVersionAbsent(request.Project)
		if err != nil {
			return adapter.NativePlan{}, err
		}
		if absent || cursorVersionOwned(request.Previous) {
			items = append(items, adapter.PlanItem{Owner: versionOwner, Target: hooksPath, Kind: adapter.OutputConfigMerge, Mode: 0o644})
		}
	}
	sort.SliceStable(items, func(left, right int) bool { return planItemKey(items[left]) < planItemKey(items[right]) })
	return adapter.NativePlan{Adapter: candidate.Descriptor(), Items: items}, nil
}

func cursorVersionOwned(previous realize.Ledger) bool {
	for _, target := range previous.Targets {
		if target.Path != hooksPath {
			continue
		}
		for _, entry := range target.Entries {
			if entry.Source == versionOwner.Source &&
				entry.ArtifactID == versionOwner.ArtifactID &&
				entry.SourcePath == versionOwner.SourcePath &&
				entry.ArtifactKind == realize.ArtifactStructuredEntry &&
				entry.Adapter == adapterID {
				return true
			}
		}
	}
	return false
}

func cursorVersionAbsent(project adapter.Snapshot) (bool, error) {
	file, err := project.ReadFile(hooksPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := adapter.ValidateUniqueJSONMembers(file.Content); err != nil {
		return false, fmt.Errorf("%s: %w", hooksPath, err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(file.Content, &document); err != nil {
		return false, adapter.NativeError(adapter.CodeInvalidNativeEvent, "decode %s: %v", hooksPath, err)
	}
	_, exists := document["version"]
	return !exists, nil
}

func planGeneratedFiles(packages []adapter.Package) ([]adapter.PlanItem, int, error) {
	var items []adapter.PlanItem
	hookCount := 0
	for _, pkg := range sortedPackages(packages) {
		for _, rule := range sortedRules(pkg.Manifest.Artifacts.Rules) {
			name, err := adapter.NativeArtifactName(pkg.Source, rule.ID)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: rule.ID, SourcePath: rule.Path, Kind: adapter.ArtifactRule},
				Target: path.Join(".cursor/rules", name+".mdc"), Kind: adapter.OutputGeneratedFile, Mode: 0o644,
			})
		}
		skills := append([]manifest.SkillArtifact(nil), pkg.Manifest.Artifacts.Skills...)
		sort.SliceStable(skills, func(left, right int) bool { return skills[left].ID < skills[right].ID })
		for _, skill := range skills {
			name, err := adapter.NativeArtifactName(pkg.Source, skill.ID)
			if err != nil {
				return nil, 0, err
			}
			files, err := adapter.ReadPackageTree(pkg, skill.Path)
			if err != nil {
				return nil, 0, fmt.Errorf("read skill %q from %s: %w", skill.ID, pkg.Source, err)
			}
			for _, file := range files {
				relative := strings.TrimPrefix(strings.TrimPrefix(file.Path, skill.Path), "/")
				mode := fs.FileMode(0o644)
				if file.Mode.Perm()&0o111 != 0 {
					mode = 0o755
				}
				items = append(items, adapter.PlanItem{
					Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: skill.ID, SourcePath: file.Path, Kind: adapter.ArtifactSkill},
					Target: path.Join(".cursor/skills", name, relative), Kind: adapter.OutputGeneratedFile, Mode: mode,
				})
			}
		}
		for _, script := range sortedScripts(pkg.Manifest.Artifacts.Scripts) {
			name, err := adapter.NativeArtifactName(pkg.Source, script.ID)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: script.ID, SourcePath: script.Path, Kind: adapter.ArtifactScript},
				Target: path.Join(".cursor/scripts", name, adapter.SourceBasename(script.Path)), Kind: adapter.OutputGeneratedFile, Mode: 0o755,
			})
		}
		for _, hook := range sortedHooks(pkg.Manifest.Artifacts.Hooks) {
			name, err := adapter.NativeArtifactName(pkg.Source, hook.ID)
			if err != nil {
				return nil, 0, err
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: hook.ID, SourcePath: hook.Path, Kind: adapter.ArtifactHook, Event: hook.Event}
			items = append(items,
				adapter.PlanItem{Owner: owner, Target: path.Join(".cursor/hooks", name, adapter.SourceBasename(hook.Path)), Kind: adapter.OutputGeneratedFile, Mode: 0o755},
				adapter.PlanItem{Owner: owner, Target: hooksPath, Kind: adapter.OutputConfigMerge, Mode: 0o644},
			)
			hookCount++
		}
	}
	return items, hookCount, nil
}

func nativeEvent(event manifest.HookEvent) (string, bool) {
	values := map[manifest.HookEvent]string{
		manifest.HookSessionStart: "sessionStart", manifest.HookSessionEnd: "sessionEnd",
		manifest.HookUserPromptSubmit: "beforeSubmitPrompt", manifest.HookPreToolUse: "preToolUse",
		manifest.HookPostToolUse: "postToolUse", manifest.HookStop: "stop",
	}
	value, ok := values[event]
	return value, ok
}

func sortedPackages(packages []adapter.Package) []adapter.Package {
	result := append([]adapter.Package(nil), packages...)
	sort.SliceStable(result, func(left, right int) bool { return result[left].Source < result[right].Source })
	return result
}

func sortedRules(values []manifest.RuleArtifact) []manifest.RuleArtifact {
	result := append([]manifest.RuleArtifact(nil), values...)
	sort.SliceStable(result, func(left, right int) bool { return result[left].ID < result[right].ID })
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
