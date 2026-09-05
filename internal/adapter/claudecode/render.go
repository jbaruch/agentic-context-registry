package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func (Adapter) Render(_ context.Context, request adapter.RenderRequest) ([]adapter.Output, error) {
	var outputs []adapter.Output
	var entries []adapter.ConfigEntry
	for _, pkg := range sortedPackages(request.Packages) {
		references, err := adapter.PackageSkillReferences(pkg, ".claude/skills")
		if err != nil {
			return nil, err
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
			nativeRoot := path.Join(".claude/skills", name)
			for _, file := range files {
				relative := strings.TrimPrefix(strings.TrimPrefix(file.Path, skill.Path), "/")
				mode := fs.FileMode(0o644)
				if file.Mode.Perm()&0o111 != 0 {
					mode = 0o755
				}
				owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: skill.ID, SourcePath: file.Path, Kind: adapter.ArtifactSkill}
				content := adapter.RebaseSkillReferences(file.Content, references)
				outputs = append(outputs, generated(path.Join(nativeRoot, relative), mode, owner, content))
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
			outputs = append(outputs, generated(path.Join(".claude/scripts", name, adapter.SourceBasename(script.Path)), 0o755, owner, file.Content))
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
			target := path.Join(".claude/hooks", name, adapter.SourceBasename(hook.Path))
			outputs = append(outputs, generated(target, 0o755, owner, file.Content))
			// Claude Code's "Exec form and shell form" hook contract spawns command with args as its argument vector after placeholder substitution; see https://code.claude.com/docs/en/hooks.md.
			encoded, err := json.Marshal(claudeMatcherGroup{Hooks: []claudeCommandHook{{Type: "command", Command: claudeProjectCommand(target), Args: hook.Args}}})
			if err != nil {
				return nil, fmt.Errorf("encode hook %q: %w", hook.ID, err)
			}
			entries = append(entries, adapter.ConfigEntry{
				Owner: owner, Container: []string{"hooks", event}, Kind: adapter.ConfigElement,
				Key: adapter.CanonicalConfigOwnerKey(owner, adapterID, settingsPath, event), EncodedValue: encoded,
			})
		}
	}
	if len(entries) != 0 {
		sort.SliceStable(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
		outputs = append(outputs, adapter.Output{Target: settingsPath, Mode: 0o644, Kind: adapter.OutputConfigMerge, Config: &adapter.ConfigMerge{Format: adapter.ConfigJSON, Entries: entries}})
	}
	sort.SliceStable(outputs, func(left, right int) bool {
		if outputs[left].Target != outputs[right].Target {
			return outputs[left].Target < outputs[right].Target
		}
		return outputOwnerKey(outputs[left]) < outputOwnerKey(outputs[right])
	})
	return outputs, nil
}

type claudeMatcherGroup struct {
	Hooks []claudeCommandHook `json:"hooks"`
}

type claudeCommandHook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
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

func claudeProjectCommand(target string) string {
	command := "${CLAUDE_PROJECT_DIR}/" + target
	if !strings.ContainsAny(target, " \t\r\n'\"\\$`") {
		return command
	}
	escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(target)
	return "\"${CLAUDE_PROJECT_DIR}/" + escaped + "\""
}
