package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func (Adapter) Validate(_ context.Context, request adapter.ValidateRequest) error {
	if err := adapter.ValidateFileProjection(request, ".claude/skills"); err != nil {
		return err
	}
	if err := adapter.UniquePlanOwnerKeys(request.Plan, settingsPath, func(owner adapter.OwnerRef) (string, bool) {
		return nativeEvent(owner.Event)
	}); err != nil {
		return err
	}
	owners := plannedHookOwners(request.Plan, settingsPath)
	if len(owners) == 0 {
		return nil
	}
	file, ok := candidateByPath(request.Files, settingsPath)
	if !ok {
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "%s is missing", settingsPath)
	}
	if err := adapter.ValidateUniqueJSONMembers(file.Content); err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	var document struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(file.Content, &document); err != nil {
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "decode %s: %v", settingsPath, err)
	}
	for _, owner := range owners {
		wantEvent, _ := nativeEvent(owner.Event)
		name, err := adapter.NativeArtifactName(owner.Source, owner.ArtifactID)
		if err != nil {
			return err
		}
		want := claudeCommandHook{Type: "command", Command: claudeProjectCommand(strings.Join([]string{".claude/hooks", name, adapter.SourceBasename(owner.SourcePath)}, "/"))}
		want.Args = plannedHookArgs(request, owner)
		matches := 0
		for event, groups := range document.Hooks {
			for _, rawGroup := range groups {
				var group claudeMatcherGroup
				if err := json.Unmarshal(rawGroup, &group); err != nil {
					continue
				}
				for _, hook := range group.Hooks {
					if hook.Command != want.Command {
						continue
					}
					matches++
					if event != wantEvent {
						return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q uses native event %q, want %q", owner.ArtifactID, event, wantEvent)
					}
					if hook.Type != "command" || !reflect.DeepEqual(hook.Args, want.Args) {
						return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q has an invalid Claude command handler shape", owner.ArtifactID)
					}
				}
			}
		}
		if matches == 0 {
			return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q has no matching Claude command handler", owner.ArtifactID)
		}
		if matches > 1 {
			return adapter.NativeError(adapter.CodeDuplicateConfigEntry, "hook %q command handler occurs more than once", owner.ArtifactID)
		}
	}
	return nil
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
