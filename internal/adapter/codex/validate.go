package codex

import (
	"context"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func (Adapter) Validate(_ context.Context, request adapter.ValidateRequest) error {
	if err := adapter.ValidateFileProjection(request, ".codex/skills"); err != nil {
		return err
	}
	if err := adapter.UniquePlanOwnerKeys(request.Plan, configPath, func(owner adapter.OwnerRef) (string, bool) {
		return nativeEvent(owner.Event)
	}); err != nil {
		return err
	}
	owners := plannedHookOwners(request.Plan, configPath)
	if len(owners) == 0 {
		return nil
	}
	file, ok := candidateByPath(request.Files, configPath)
	if !ok {
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "%s is missing", configPath)
	}
	var document codexDocument
	if err := adapter.DecodeTOML(file.Content, &document); err != nil {
		if adapter.IsTOMLDuplicateDefinition(file.Content, err) {
			return adapter.NativeError(adapter.CodeDuplicateConfigEntry, "decode %s: %v", configPath, err)
		}
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "decode %s: %v", configPath, err)
	}
	for _, owner := range owners {
		wantEvent, _ := nativeEvent(owner.Event)
		name, err := adapter.NativeArtifactName(owner.Source, owner.ArtifactID)
		if err != nil {
			return err
		}
		target := strings.Join([]string{".codex/hooks", name, adapter.SourceBasename(owner.SourcePath)}, "/")
		wantCommand := adapter.ShellJoin(codexRootCommand(target), plannedHookArgs(request, owner))
		matches := 0
		for event, groups := range document.Hooks {
			for _, group := range groups {
				for _, hook := range group.Hooks {
					if hook.Command != wantCommand {
						continue
					}
					matches++
					if event != wantEvent {
						return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q uses native event %q, want %q", owner.ArtifactID, event, wantEvent)
					}
					if hook.Type != "command" {
						return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q has an invalid Codex command handler shape", owner.ArtifactID)
					}
				}
			}
		}
		if matches == 0 {
			return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q has no matching Codex command handler", owner.ArtifactID)
		}
		if matches > 1 {
			return adapter.NativeError(adapter.CodeDuplicateConfigEntry, "hook %q command handler occurs more than once", owner.ArtifactID)
		}
	}
	return nil
}

type codexDocument struct {
	Hooks map[string][]codexMatcherGroup `toml:"hooks"`
}

type codexMatcherGroup struct {
	Hooks []codexCommandHook `toml:"hooks"`
}

type codexCommandHook struct {
	Type    string `toml:"type"`
	Command string `toml:"command"`
}

func plannedHookOwners(plan adapter.NativePlan, target string) []adapter.OwnerRef {
	var result []adapter.OwnerRef
	for _, item := range plan.Items {
		if item.Target == target && item.Kind == adapter.OutputConfigMerge {
			result = append(result, item.Owner)
		}
	}
	return result
}

func candidateByPath(files []adapter.CandidateFile, target string) (adapter.CandidateFile, bool) {
	for _, file := range files {
		if file.Path == target {
			return file, true
		}
	}
	return adapter.CandidateFile{}, false
}

func plannedHookArgs(request adapter.ValidateRequest, owner adapter.OwnerRef) []string {
	for _, pkg := range request.Packages {
		if pkg.Source != owner.Source {
			continue
		}
		for _, hook := range pkg.Manifest.Artifacts.Hooks {
			if hook.ID == owner.ArtifactID {
				return append([]string(nil), hook.Args...)
			}
		}
	}
	return nil
}
