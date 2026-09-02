package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func (Adapter) Render(_ context.Context, request adapter.RenderRequest) ([]adapter.Output, error) {
	var outputs []adapter.Output
	var entries []adapter.ConfigEntry
	for _, pkg := range sortedPackages(request.Packages) {
		for _, rule := range sortedRules(pkg.Manifest.Artifacts.Rules) {
			name, err := adapter.NativeArtifactName(pkg.Source, rule.ID)
			if err != nil {
				return nil, err
			}
			file, err := adapter.ReadPackageFile(pkg, rule.Path)
			if err != nil {
				return nil, fmt.Errorf("read rule %q from %s: %w", rule.ID, pkg.Source, err)
			}
			body, err := adapter.StripLeadingFrontmatter(file.Content)
			if err != nil {
				return nil, fmt.Errorf("rule %q from %s: %w", rule.ID, pkg.Source, err)
			}
			content, err := cursorRuleContent(rule.Activation, body)
			if err != nil {
				return nil, fmt.Errorf("rule %q from %s: %w", rule.ID, pkg.Source, err)
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: rule.ID, SourcePath: rule.Path, Kind: adapter.ArtifactRule}
			outputs = append(outputs, generated(path.Join(".cursor/rules", name+".mdc"), 0o644, owner, content))
		}
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
				owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: skill.ID, SourcePath: file.Path, Kind: adapter.ArtifactSkill}
				outputs = append(outputs, generated(path.Join(".cursor/skills", name, relative), mode, owner, file.Content))
			}
		}
		for _, script := range sortedScripts(pkg.Manifest.Artifacts.Scripts) {
			name, err := adapter.NativeArtifactName(pkg.Source, script.ID)
			if err != nil {
				return nil, err
			}
			file, err := adapter.ReadPackageFile(pkg, script.Path)
			if err != nil {
				return nil, fmt.Errorf("read script %q from %s: %w", script.ID, pkg.Source, err)
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: script.ID, SourcePath: script.Path, Kind: adapter.ArtifactScript}
			outputs = append(outputs, generated(path.Join(".cursor/scripts", name, adapter.SourceBasename(script.Path)), 0o755, owner, file.Content))
		}
		for _, hook := range sortedHooks(pkg.Manifest.Artifacts.Hooks) {
			name, err := adapter.NativeArtifactName(pkg.Source, hook.ID)
			if err != nil {
				return nil, err
			}
			file, err := adapter.ReadPackageFile(pkg, hook.Path)
			if err != nil {
				return nil, fmt.Errorf("read hook %q from %s: %w", hook.ID, pkg.Source, err)
			}
			event, ok := nativeEvent(hook.Event)
			if !ok {
				return nil, adapter.NativeError(adapter.CodeInvalidNativeEvent, "hook %q uses event %q", hook.ID, hook.Event)
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: hook.ID, SourcePath: hook.Path, Kind: adapter.ArtifactHook, Event: hook.Event}
			target := path.Join(".cursor/hooks", name, adapter.SourceBasename(hook.Path))
			outputs = append(outputs, generated(target, 0o755, owner, file.Content))
			encoded, err := json.Marshal(cursorCommandHook{Command: adapter.ShellJoin(adapter.ShellQuote(target), hook.Args)})
			if err != nil {
				return nil, fmt.Errorf("encode hook %q: %w", hook.ID, err)
			}
			entries = append(entries, adapter.ConfigEntry{
				Owner: owner, Container: []string{"hooks", event}, Kind: adapter.ConfigElement,
				Key: adapter.CanonicalConfigOwnerKey(owner, adapterID, hooksPath, event), EncodedValue: encoded,
			})
		}
	}
	if planHasOwner(request.Plan, versionOwner) {
		entries = append(entries, adapter.ConfigEntry{Owner: versionOwner, Kind: adapter.ConfigField, Key: "version", EncodedValue: []byte("1")})
	}
	if len(entries) != 0 {
		sort.SliceStable(entries, func(left, right int) bool {
			return adapter.CanonicalEntryKey(entries[left].Container, entries[left].Kind, entries[left].Key) <
				adapter.CanonicalEntryKey(entries[right].Container, entries[right].Kind, entries[right].Key)
		})
		outputs = append(outputs, adapter.Output{Target: hooksPath, Mode: 0o644, Kind: adapter.OutputConfigMerge, Config: &adapter.ConfigMerge{Format: adapter.ConfigJSON, Entries: entries}})
	}
	sort.SliceStable(outputs, func(left, right int) bool {
		if outputs[left].Target != outputs[right].Target {
			return outputs[left].Target < outputs[right].Target
		}
		return outputOwnerKey(outputs[left]) < outputOwnerKey(outputs[right])
	})
	return outputs, nil
}

func cursorRuleContent(activation manifest.RuleActivation, body []byte) ([]byte, error) {
	var header string
	switch activation.Mode {
	case manifest.ActivationAlways:
		header = "---\nalwaysApply: true\n---\n"
	case manifest.ActivationPaths:
		quoted := make([]string, len(activation.Paths))
		for index, value := range activation.Paths {
			quoted[index] = strconv.Quote(value)
		}
		header = "---\nglobs: [" + strings.Join(quoted, ", ") + "]\nalwaysApply: false\n---\n"
	default:
		return nil, adapter.NativeError(adapter.CodeMalformedFrontmatter, "unsupported activation mode %q", activation.Mode)
	}
	return append([]byte(header), body...), nil
}

type cursorCommandHook struct {
	Command string `json:"command"`
}

func generated(target string, mode fs.FileMode, owner adapter.OwnerRef, content []byte) adapter.Output {
	return adapter.Output{Target: target, Mode: mode, Kind: adapter.OutputGeneratedFile, File: &adapter.GeneratedFile{Owner: owner, Content: content}}
}

func outputOwnerKey(output adapter.Output) string {
	if output.File != nil {
		return output.File.Owner.Source + "\x00" + output.File.Owner.ArtifactID + "\x00" + output.File.Owner.SourcePath
	}
	return output.Target
}

func planHasOwner(plan adapter.NativePlan, owner adapter.OwnerRef) bool {
	for _, item := range plan.Items {
		if item.Owner == owner {
			return true
		}
	}
	return false
}
