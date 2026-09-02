package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func (Adapter) Validate(_ context.Context, request adapter.ValidateRequest) error {
	if err := adapter.ValidateFileProjection(request, ".cursor/skills"); err != nil {
		return err
	}
	if err := validateRuleFrontmatter(request); err != nil {
		return err
	}
	hookPlan := request.Plan
	hookPlan.Items = hookConfigItems(request.Plan)
	if err := adapter.UniquePlanOwnerKeys(hookPlan, hooksPath, func(owner adapter.OwnerRef) (string, bool) {
		return nativeEvent(owner.Event)
	}); err != nil {
		return err
	}
	owners := plannedHookOwners(hookPlan)
	if len(owners) == 0 {
		return nil
	}
	file, ok := candidateByPath(request.Files, hooksPath)
	if !ok {
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "%s is missing", hooksPath)
	}
	if err := adapter.ValidateUniqueJSONMembers(file.Content); err != nil {
		return fmt.Errorf("%s: %w", hooksPath, err)
	}
	var document struct {
		Version json.RawMessage                         `json:"version"`
		Hooks   map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(file.Content, &document); err != nil {
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "decode %s: %v", hooksPath, err)
	}
	var version int
	if len(document.Version) == 0 || json.Unmarshal(document.Version, &version) != nil || version != 1 {
		return adapter.NativeError(adapter.CodeInvalidNativeEvent, "%s requires root version 1", hooksPath)
	}
	for _, owner := range owners {
		wantEvent, _ := nativeEvent(owner.Event)
		name, err := adapter.NativeArtifactName(owner.Source, owner.ArtifactID)
		if err != nil {
			return err
		}
		target := strings.Join([]string{".cursor/hooks", name, adapter.SourceBasename(owner.SourcePath)}, "/")
		wantCommand := adapter.ShellJoin(adapter.ShellQuote(target), plannedHookArgs(request, owner))
		matches := 0
		for event, hooks := range document.Hooks {
			for _, rawHook := range hooks {
				commandRaw, exists := rawHook["command"]
				if !exists {
					continue
				}
				var command string
				if json.Unmarshal(commandRaw, &command) != nil || command != wantCommand {
					continue
				}
				matches++
				if event != wantEvent {
					return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q uses native event %q, want %q", owner.ArtifactID, event, wantEvent)
				}
				if len(rawHook) != 1 {
					return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q has an invalid Cursor command handler shape", owner.ArtifactID)
				}
			}
		}
		if matches == 0 {
			return adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q has no matching Cursor command handler", owner.ArtifactID)
		}
		if matches > 1 {
			return adapter.NativeError(adapter.CodeDuplicateConfigEntry, "hook %q command handler occurs more than once", owner.ArtifactID)
		}
	}
	return nil
}

func validateRuleFrontmatter(request adapter.ValidateRequest) error {
	for _, item := range request.Plan.Items {
		if item.Owner.Kind != adapter.ArtifactRule || item.Kind != adapter.OutputGeneratedFile || !strings.HasPrefix(item.Target, ".cursor/rules/") {
			continue
		}
		candidate, ok := candidateByPath(request.Files, item.Target)
		if !ok {
			return adapter.NativeError(adapter.CodeMalformedFrontmatter, "Cursor rule %q is missing", item.Target)
		}
		metadata, _, err := adapter.ValidateSingleCursorFrontmatter(candidate.Content)
		if err != nil {
			return fmt.Errorf("%s: %w", item.Target, err)
		}
		activation, ok := plannedRuleActivation(request.Packages, item.Owner)
		if !ok {
			return adapter.NativeError(adapter.CodeMalformedFrontmatter, "Cursor rule %q has no source activation", item.Target)
		}
		always, ok := metadata["alwaysApply"].(bool)
		if !ok {
			return adapter.NativeError(adapter.CodeMalformedFrontmatter, "Cursor rule %q requires boolean alwaysApply", item.Target)
		}
		switch activation.Mode {
		case manifest.ActivationAlways:
			if len(metadata) != 1 || !always {
				return adapter.NativeError(adapter.CodeMalformedFrontmatter, "Cursor rule %q must contain only alwaysApply: true", item.Target)
			}
		case manifest.ActivationPaths:
			globs, ok := stringSlice(metadata["globs"])
			if len(metadata) != 2 || always || !ok || !reflect.DeepEqual(globs, activation.Paths) {
				return adapter.NativeError(adapter.CodeMalformedFrontmatter, "Cursor rule %q has invalid path activation metadata", item.Target)
			}
		default:
			return adapter.NativeError(adapter.CodeMalformedFrontmatter, "Cursor rule %q has unsupported activation mode %q", item.Target, activation.Mode)
		}
	}
	return nil
}

func stringSlice(value any) ([]string, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(values))
	for index, value := range values {
		item, ok := value.(string)
		if !ok {
			return nil, false
		}
		result[index] = item
	}
	return result, true
}

func plannedRuleActivation(packages []adapter.Package, owner adapter.OwnerRef) (manifest.RuleActivation, bool) {
	for _, pkg := range packages {
		if pkg.Source != owner.Source {
			continue
		}
		for _, rule := range pkg.Manifest.Artifacts.Rules {
			if rule.ID == owner.ArtifactID && rule.Path == owner.SourcePath {
				return rule.Activation, true
			}
		}
	}
	return manifest.RuleActivation{}, false
}

func hookConfigItems(plan adapter.NativePlan) []adapter.PlanItem {
	var result []adapter.PlanItem
	for _, item := range plan.Items {
		if item.Target == hooksPath && item.Kind == adapter.OutputConfigMerge && item.Owner.Event != "" {
			result = append(result, item)
		}
	}
	return result
}

func plannedHookOwners(plan adapter.NativePlan) []adapter.OwnerRef {
	result := make([]adapter.OwnerRef, 0, len(plan.Items))
	for _, item := range plan.Items {
		result = append(result, item.Owner)
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
