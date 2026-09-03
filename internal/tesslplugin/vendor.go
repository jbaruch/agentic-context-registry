package tesslplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"go.yaml.in/yaml/v3"
)

type vendorPluginDocument struct {
	Rules  json.RawMessage                           `json:"rules"`
	Skills json.RawMessage                           `json:"skills"`
	Hooks  map[string][]vendorPluginGroup            `json:"hooks"`
	Native map[string]map[string][]vendorPluginGroup `json:"nativeHooks"`
}

type vendorPluginGroup struct {
	Hooks []vendorPluginCommand `json:"hooks"`
}

type vendorPluginCommand struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type vendorTileDocument struct {
	Rules map[string]struct {
		Rules string `json:"rules"`
	} `json:"rules"`
	Skills map[string]struct {
		Path string `json:"path"`
	} `json:"skills"`
}

// DeclaredPathError identifies a non-empty plugin artifact path that is absent
// from the installed package.
type DeclaredPathError struct {
	Kind string
	Path string
}

func (err *DeclaredPathError) Error() string {
	return fmt.Sprintf("declared plugin %s path %q does not exist", err.Kind, err.Path)
}

func (err *DeclaredPathError) Unwrap() error { return fs.ErrNotExist }

// SynthesizeVendorManifest derives the ACR artifact manifest for a copied
// Tessl package. Migration and later offline materialization call this same
// function so a converged vendor tree cannot change artifact identity.
func SynthesizeVendorManifest(packageFS fs.FS, identity, version string) (manifest.Manifest, error) {
	value := manifest.Manifest{SchemaVersion: manifest.CurrentSchemaVersion, Name: identity, Version: version}
	pluginContent, err := fs.ReadFile(packageFS, pluginManifestRel)
	pluginMissing := errors.Is(err, fs.ErrNotExist)
	if err == nil {
		var document vendorPluginDocument
		if err := json.Unmarshal(pluginContent, &document); err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode %s: %w", pluginManifestRel, err)
		}
		rules, err := vendorPaths(document.Rules)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode plugin rules: %w", err)
		}
		expandedRules, err := expandVendorRules(packageFS, rules)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("expand plugin rules: %w", err)
		}
		for _, relative := range expandedRules {
			content, err := fs.ReadFile(packageFS, relative)
			if err != nil {
				return manifest.Manifest{}, fmt.Errorf("read vendor rule %s: %w", relative, err)
			}
			activation, err := vendorRuleActivation(content)
			if err != nil {
				return manifest.Manifest{}, fmt.Errorf("read vendor rule activation %s: %w", relative, err)
			}
			value.Artifacts.Rules = append(value.Artifacts.Rules, manifest.RuleArtifact{ID: vendorArtifactID(relative), Path: relative, Activation: activation})
		}
		skills, err := vendorPaths(document.Skills)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode plugin skills: %w", err)
		}
		expandedSkills, err := expandVendorSkills(packageFS, skills)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("expand plugin skills: %w", err)
		}
		for _, relative := range expandedSkills {
			value.Artifacts.Skills = append(value.Artifacts.Skills, manifest.SkillArtifact{ID: vendorArtifactID(relative), Path: relative})
		}
		if err := appendVendorHooks(&value, document.Hooks); err != nil {
			return manifest.Manifest{}, err
		}
		agents := sortedVendorKeys(document.Native)
		for _, agent := range agents {
			if err := appendVendorHooks(&value, document.Native[agent]); err != nil {
				return manifest.Manifest{}, err
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return manifest.Manifest{}, fmt.Errorf("read %s: %w", pluginManifestRel, err)
	}
	if pluginMissing {
		content, err := fs.ReadFile(packageFS, tileManifestName)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("read %s: %w", tileManifestName, err)
		}
		var tile vendorTileDocument
		if err := json.Unmarshal(content, &tile); err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode %s: %w", tileManifestName, err)
		}
		for id, rule := range tile.Rules {
			value.Artifacts.Rules = append(value.Artifacts.Rules, manifest.RuleArtifact{ID: id, Path: rule.Rules, Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}})
		}
		for id, skill := range tile.Skills {
			value.Artifacts.Skills = append(value.Artifacts.Skills, manifest.SkillArtifact{ID: id, Path: path.Dir(skill.Path)})
		}
	}
	sort.Slice(value.Artifacts.Rules, func(i, j int) bool { return value.Artifacts.Rules[i].ID < value.Artifacts.Rules[j].ID })
	sort.Slice(value.Artifacts.Skills, func(i, j int) bool { return value.Artifacts.Skills[i].ID < value.Artifacts.Skills[j].ID })
	sort.Slice(value.Artifacts.Hooks, func(i, j int) bool {
		left := value.Artifacts.Hooks[i]
		right := value.Artifacts.Hooks[j]
		return left.ID+"\x00"+string(left.Event) < right.ID+"\x00"+string(right.Event)
	})
	return value, nil
}

func vendorPaths(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		one = strings.TrimSuffix(strings.TrimSpace(one), "/")
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(many))
	for _, declared := range many {
		declared = strings.TrimSuffix(strings.TrimSpace(declared), "/")
		if declared != "" {
			paths = append(paths, declared)
		}
	}
	return paths, nil
}

func expandVendorRules(packageFS fs.FS, declared []string) ([]string, error) {
	var result []string
	for _, relative := range declared {
		if strings.HasSuffix(relative, ".md") {
			if _, err := fs.Stat(packageFS, relative); errors.Is(err, fs.ErrNotExist) {
				return nil, &DeclaredPathError{Kind: "rule", Path: relative}
			} else if err != nil {
				return nil, err
			}
			result = append(result, relative)
			continue
		}
		entries, err := fs.ReadDir(packageFS, relative)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &DeclaredPathError{Kind: "rule", Path: relative}
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
				result = append(result, path.Join(relative, entry.Name()))
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func expandVendorSkills(packageFS fs.FS, declared []string) ([]string, error) {
	var result []string
	for _, relative := range declared {
		if _, err := fs.Stat(packageFS, path.Join(relative, "SKILL.md")); err == nil {
			result = append(result, relative)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		entries, err := fs.ReadDir(packageFS, relative)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &DeclaredPathError{Kind: "skill", Path: relative}
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				child := path.Join(relative, entry.Name())
				if _, err := fs.Stat(packageFS, path.Join(child, "SKILL.md")); err == nil {
					result = append(result, child)
				} else if !errors.Is(err, fs.ErrNotExist) {
					return nil, err
				}
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func vendorRuleActivation(content []byte) (manifest.RuleActivation, error) {
	activation := manifest.RuleActivation{Mode: manifest.ActivationAlways}
	if !bytes.HasPrefix(content, []byte("---\n")) && !bytes.HasPrefix(content, []byte("---\r\n")) {
		return activation, nil
	}
	lines := bytes.Split(content, []byte("\n"))
	closing := -1
	for index := 1; index < len(lines); index++ {
		if bytes.Equal(bytes.TrimSuffix(lines[index], []byte("\r")), []byte("---")) {
			closing = index
			break
		}
	}
	if closing < 0 {
		return activation, nil
	}
	var metadata struct {
		AlwaysApply *bool  `yaml:"alwaysApply"`
		ApplyTo     string `yaml:"applyTo"`
		Globs       string `yaml:"globs"`
		Paths       string `yaml:"paths"`
	}
	if err := yaml.Unmarshal(bytes.Join(lines[1:closing], []byte("\n")), &metadata); err != nil {
		return manifest.RuleActivation{}, err
	}
	if metadata.AlwaysApply != nil && *metadata.AlwaysApply {
		return activation, nil
	}
	scoped := firstNonEmpty(metadata.ApplyTo, metadata.Globs, metadata.Paths)
	if scoped == "" {
		return activation, nil
	}
	globHalf, _, found := strings.Cut(scoped, "—")
	if !found {
		globHalf, _, found = strings.Cut(scoped, " -- ")
	}
	if !found {
		globHalf = scoped
	}
	for _, item := range strings.Split(globHalf, ",") {
		if item = strings.TrimSpace(item); item != "" {
			activation.Paths = append(activation.Paths, item)
		}
	}
	if len(activation.Paths) == 0 {
		return manifest.RuleActivation{}, errors.New("path-scoped Tessl rule has no usable glob")
	}
	activation.Mode = manifest.ActivationPaths
	return activation, nil
}

func appendVendorHooks(value *manifest.Manifest, events map[string][]vendorPluginGroup) error {
	names := sortedVendorKeys(events)
	for _, nativeEvent := range names {
		event, ok := vendorHookEvent(nativeEvent)
		if !ok {
			return unsupportedHookEventError(nativeEvent)
		}
		for _, group := range events[nativeEvent] {
			for _, command := range group.Hooks {
				if command.Type != "" && command.Type != "command" {
					continue
				}
				parsed, err := ParseHookCommand(command.Command, command.Args)
				if err != nil {
					return err
				}
				value.Artifacts.Hooks = append(value.Artifacts.Hooks, manifest.HookArtifact{ID: vendorArtifactID(parsed.Path), Path: parsed.Path, Event: event, Args: parsed.Args})
			}
		}
	}
	return nil
}

func vendorHookEvent(value string) (manifest.HookEvent, bool) {
	events := map[string]manifest.HookEvent{
		"SessionStart": manifest.HookSessionStart, "sessionStart": manifest.HookSessionStart,
		"SessionEnd": manifest.HookSessionEnd, "sessionEnd": manifest.HookSessionEnd,
		"UserPromptSubmit": manifest.HookUserPromptSubmit, "beforeSubmitPrompt": manifest.HookUserPromptSubmit,
		"PreToolUse": manifest.HookPreToolUse, "preToolUse": manifest.HookPreToolUse,
		"PostToolUse": manifest.HookPostToolUse, "postToolUse": manifest.HookPostToolUse,
		"Stop": manifest.HookStop, "stop": manifest.HookStop,
	}
	event, ok := events[value]
	return event, ok
}

func vendorArtifactID(relative string) string {
	value := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(path.Base(relative), path.Ext(relative))))
	value = strings.Map(func(character rune) rune {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			return character
		}
		return '-'
	}, value)
	value = strings.Trim(value, "-")
	if value == "" {
		return "artifact"
	}
	if value[0] < 'a' || value[0] > 'z' {
		return "artifact-" + value
	}
	return value
}

func sortedVendorKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
