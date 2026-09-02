package dependency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"go.yaml.in/yaml/v3"
)

const vendorContentDomain = "acr-tessl-vendor-v1\x00"

type vendorFileRecord struct {
	path string
	mode fs.FileMode
	size int64
	hash string
}

func materializeVendor(projectDirectory string, locked LockedDependency) (MaterializedPackage, func() error, error) {
	identity, err := ParseVendorSource(locked.Source)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	root := filepath.Join(projectDirectory, ".agents", "vendor", identity.Workspace, identity.Package)
	projectRoot, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return MaterializedPackage{}, nil, err
	}
	defer projectRoot.Close()
	relativeRoot := path.Join(".agents/vendor", identity.Workspace, identity.Package)
	if err := realize.ValidateParentDirectories(projectRoot, relativeRoot); err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("vendor_escape: %w", err)
	}
	info, err := projectRoot.Lstat(relativeRoot)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return MaterializedPackage{}, nil, fmt.Errorf("vendor_escape: %s must be a regular directory", relativeRoot)
	}
	contentHash, err := HashVendorTree(root)
	if err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("verify vendored package %s: %w", locked.Source, err)
	}
	if contentHash != locked.ContentHash {
		return MaterializedPackage{}, nil, fmt.Errorf("content hash mismatch for %s: expected %s, found %s; restore the vendored tree from version control or rerun 'acr migrate tessl --vendor-unmapped'", locked.Source, locked.ContentHash, contentHash)
	}
	value, err := synthesizeTesslManifest(root, identity.FullName(), locked.PackageVersion)
	if err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("synthesize manifest for %s: %w", locked.Source, err)
	}
	if err := manifest.ValidateArtifacts(root, value); err != nil {
		return MaterializedPackage{}, nil, fmt.Errorf("validate vendored package %s: %w", locked.Source, err)
	}
	return MaterializedPackage{Root: root, Manifest: value}, func() error { return nil }, nil
}

// HashVendorTree computes the normalized all-file hash for a vendor root.
func HashVendorTree(root string) (string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("vendor_escape: %q must be a regular directory", root)
	}
	var records []vendorFileRecord
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("vendor_escape: %q is a symbolic link", filename)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("vendor_escape: %q is not a regular file", filename)
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("hash %q: %w", relative, errors.Join(copyErr, closeErr))
		}
		mode := fs.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		records = append(records, vendorFileRecord{path: relative, mode: mode, size: info.Size(), hash: hex.EncodeToString(digest.Sum(nil))})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	hash := sha256.New()
	_, _ = io.WriteString(hash, vendorContentDomain)
	for _, record := range records {
		_, _ = fmt.Fprintf(hash, "%s\x00%04o\x00%d\x00%s\x00", record.path, record.mode.Perm(), record.size, record.hash)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

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

func synthesizeTesslManifest(root, identity, version string) (manifest.Manifest, error) {
	value := manifest.Manifest{SchemaVersion: manifest.CurrentSchemaVersion, Name: identity, Version: version}
	pluginPath := filepath.Join(root, ".tessl-plugin", "plugin.json")
	if content, err := os.ReadFile(pluginPath); err == nil {
		var document vendorPluginDocument
		if err := json.Unmarshal(content, &document); err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode .tessl-plugin/plugin.json: %w", err)
		}
		rules, err := vendorPaths(document.Rules)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode plugin rules: %w", err)
		}
		for _, relative := range expandVendorRules(root, rules) {
			activation, activationErr := vendorRuleActivation(filepath.Join(root, filepath.FromSlash(relative)))
			if activationErr != nil {
				return manifest.Manifest{}, activationErr
			}
			value.Artifacts.Rules = append(value.Artifacts.Rules, manifest.RuleArtifact{ID: vendorID(relative), Path: relative, Activation: activation})
		}
		skills, err := vendorPaths(document.Skills)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode plugin skills: %w", err)
		}
		for _, relative := range expandVendorSkills(root, skills) {
			value.Artifacts.Skills = append(value.Artifacts.Skills, manifest.SkillArtifact{ID: vendorID(relative), Path: relative})
		}
		appendHooks := func(events map[string][]vendorPluginGroup) {
			for nativeEvent, groups := range events {
				event, ok := vendorHookEvent(nativeEvent)
				if !ok {
					continue
				}
				for _, group := range groups {
					for _, command := range group.Hooks {
						relative, args, ok := vendorHookPath(command)
						if ok {
							value.Artifacts.Hooks = append(value.Artifacts.Hooks, manifest.HookArtifact{ID: vendorID(relative), Path: relative, Event: event, Args: args})
						}
					}
				}
			}
		}
		appendHooks(document.Hooks)
		for _, events := range document.Native {
			appendHooks(events)
		}
	} else if !os.IsNotExist(err) {
		return manifest.Manifest{}, err
	}
	if len(value.Artifacts.Rules)+len(value.Artifacts.Skills)+len(value.Artifacts.Hooks) == 0 {
		content, err := os.ReadFile(filepath.Join(root, "tile.json"))
		if err != nil {
			return manifest.Manifest{}, err
		}
		var tile vendorTileDocument
		if err := json.Unmarshal(content, &tile); err != nil {
			return manifest.Manifest{}, fmt.Errorf("decode tile.json: %w", err)
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
		return value.Artifacts.Hooks[i].ID+"\x00"+string(value.Artifacts.Hooks[i].Event) < value.Artifacts.Hooks[j].ID+"\x00"+string(value.Artifacts.Hooks[j].Event)
	})
	return value, nil
}

func vendorRuleActivation(filename string) (manifest.RuleActivation, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return manifest.RuleActivation{}, err
	}
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
	if metadata.AlwaysApply == nil || *metadata.AlwaysApply {
		return activation, nil
	}
	scoped := metadata.ApplyTo
	if strings.TrimSpace(scoped) == "" {
		scoped = metadata.Globs
	}
	if strings.TrimSpace(scoped) == "" {
		scoped = metadata.Paths
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
	sort.Strings(activation.Paths)
	return activation, nil
}

func vendorPaths(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{strings.TrimSuffix(one, "/")}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	for index := range many {
		many[index] = strings.TrimSuffix(many[index], "/")
	}
	return many, nil
}

func expandVendorRules(root string, declared []string) []string {
	var result []string
	for _, relative := range declared {
		if strings.HasSuffix(relative, ".md") {
			result = append(result, relative)
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			result = append(result, relative)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
				result = append(result, path.Join(relative, entry.Name()))
			}
		}
	}
	sort.Strings(result)
	return result
}

func expandVendorSkills(root string, declared []string) []string {
	var result []string
	for _, relative := range declared {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative), "SKILL.md")); err == nil {
			result = append(result, relative)
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			result = append(result, relative)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				child := path.Join(relative, entry.Name())
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(child), "SKILL.md")); err == nil {
					result = append(result, child)
				}
			}
		}
	}
	sort.Strings(result)
	return result
}

func vendorID(relative string) string {
	value := strings.TrimSuffix(path.Base(relative), path.Ext(relative))
	value = strings.ToLower(value)
	value = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
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

func vendorHookPath(command vendorPluginCommand) (string, []string, bool) {
	const prefix = "${TESSL_PLUGIN_DIR}/"
	if len(command.Args) != 0 && strings.HasPrefix(command.Args[0], prefix) {
		return strings.TrimPrefix(command.Args[0], prefix), append([]string(nil), command.Args[1:]...), true
	}
	const shellPrefix = `bash "${TESSL_PLUGIN_DIR}/`
	if strings.HasPrefix(command.Command, shellPrefix) && strings.HasSuffix(command.Command, `"`) {
		return strings.TrimSuffix(strings.TrimPrefix(command.Command, shellPrefix), `"`), []string{}, true
	}
	return "", nil, false
}
