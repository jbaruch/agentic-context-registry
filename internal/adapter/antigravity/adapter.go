// Package antigravity delivers native command hooks. The selected Markdown
// adapter and the coordinator own the shared rule and skill surfaces.
package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	id          = "antigravity"
	configPath  = ".agent/hooks.json"
	runtimePath = ".agent/hooks/" + adapter.HookTranslatorBasename
)

var events = map[manifest.HookEvent]string{
	manifest.HookPreToolUse: "PreToolUse", manifest.HookPostToolUse: "PostToolUse",
	manifest.HookSessionStart: "SessionStart", manifest.HookStop: "Stop",
}

var workspaceDirs = []string{".agents", ".agent", "_agents", "_agent"}

// Adapter takes its delegated capabilities from the current realization.
type Adapter struct{ Rules, Skills, Scripts bool }

func (Adapter) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: id, Version: "1.0.0", Boundary: adapter.CurrentBoundaryVersion}
}

func (Adapter) Detect(_ context.Context, request adapter.DetectRequest) (adapter.Detection, error) {
	roots := slices.Clone(workspaceDirs)
	project, listable := request.Project.(adapter.DirectorySnapshot)
	if listable {
		entries, err := project.ReadDir(".")
		if err != nil {
			return adapter.Detection{}, fmt.Errorf("inspect Antigravity workspace roots: %w", err)
		}
		// A root or evidence leaf that is a symlink or the wrong type holds no
		// evidence of its own and is not followed.
		roots = slices.DeleteFunc(roots, func(root string) bool {
			entry, ok := listed(entries, root)
			return ok && !entry.Mode.IsDir()
		})
	}
	var files []string
	var dirs []string
	for _, root := range roots {
		leaves := []string{root + "/rules", root + "/workflows"}
		if root != ".agents" {
			leaves = append(leaves, root+"/skills")
		}
		if !listable {
			files = append(files, root+"/hooks.json")
			dirs = append(dirs, leaves...)
			continue
		}
		entries, err := project.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return adapter.Detection{}, fmt.Errorf("inspect Antigravity workspace root %s: %w", root, err)
		}
		if entry, ok := listed(entries, root+"/hooks.json"); ok && entry.Mode.IsRegular() {
			files = append(files, root+"/hooks.json")
		}
		for _, leaf := range leaves {
			if entry, ok := listed(entries, leaf); ok && entry.Mode.IsDir() {
				dirs = append(dirs, leaf)
			}
		}
	}
	evidence, err := adapter.ExistingEvidence(request.Project, files, dirs)
	if err != nil {
		return adapter.Detection{}, err
	}
	// GEMINI.md is a shared Gemini-family signal. Only a regular file is
	// evidence here. Ignore aliases without following their destinations.
	instructions := []string{"GEMINI.md"}
	for _, root := range roots {
		instructions = append(instructions, root+"/GEMINI.md")
	}
	for _, name := range instructions {
		file, err := request.Project.ReadFile(name)
		var irregular *adapter.NonRegularFileError
		switch {
		case err == nil && file.Mode.IsRegular():
			evidence = append(evidence, name)
		case err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.As(err, &irregular):
			return adapter.Detection{}, fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	slices.Sort(evidence)
	return adapter.Detection{Detected: len(evidence) > 0, Evidence: evidence}, nil
}

// listed finds name in a directory listing, falling back to the spelling a
// case-insensitive volume reports.
func listed(entries []adapter.ObservedEntry, name string) (adapter.ObservedEntry, bool) {
	index := slices.IndexFunc(entries, func(entry adapter.ObservedEntry) bool { return entry.Path == name })
	if index < 0 {
		index = slices.IndexFunc(entries, func(entry adapter.ObservedEntry) bool { return strings.EqualFold(entry.Path, name) })
	}
	if index < 0 {
		return adapter.ObservedEntry{}, false
	}
	return entries[index], true
}

func (a Adapter) SupportedArtifacts() []adapter.ArtifactKind {
	kinds := []adapter.ArtifactKind{adapter.ArtifactHook}
	if a.Rules {
		kinds = append(kinds, adapter.ArtifactRule)
	}
	if a.Scripts {
		kinds = append(kinds, adapter.ArtifactScript)
	}
	if a.Skills {
		kinds = append(kinds, adapter.ArtifactSkill)
	}
	return kinds
}

func (Adapter) SupportedEvents() []manifest.HookEvent { return slices.Sorted(maps.Keys(events)) }

type projection struct {
	pkg          adapter.Package
	owner        adapter.OwnerRef
	name, target string
	args         []string
}

func hooks(packages []adapter.Package) ([]projection, error) {
	var result []projection
	for _, pkg := range packages {
		for _, h := range pkg.Manifest.Artifacts.Hooks {
			if _, ok := events[h.Event]; !ok {
				continue
			}
			name, err := adapter.NativeArtifactName(pkg.Source, h.ID)
			if err != nil {
				return nil, err
			}
			for index, arg := range h.Args {
				if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
					return nil, fmt.Errorf("Antigravity hook %q from %s: args[%d] must be valid UTF-8 without NUL for native command execution; edit args in agent-plugin.yaml", h.ID, pkg.Source, index)
				}
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: h.ID, SourcePath: h.Path, Kind: adapter.ArtifactHook, Event: h.Event}
			result = append(result, projection{pkg: pkg, owner: owner, name: name, target: path.Join(".agent/hooks", name, adapter.SourceBasename(h.Path)), args: h.Args})
		}
	}
	slices.SortFunc(result, func(a, b projection) int { return strings.Compare(a.name, b.name) })
	return result, nil
}

func (h projection) config() ([]byte, error) {
	cdFailure := adapter.HookFailureCommand(id, h.owner.Event, fmt.Sprintf("ACR hook %s (%s): cannot enter project root; run from the .agent hooks directory.", h.target, h.owner.Event))
	command := "CDPATH= cd .. || {\n" + cdFailure + "\n}\n" + adapter.TranslatedHookCommand(runtimePath, id, h.owner.Event, h.target, h.args)
	handlers := []map[string]any{{"type": "command", "command": command, "timeout": adapter.HookTimeoutSeconds(id, h.owner.Event)}}
	if h.owner.Event == manifest.HookPreToolUse || h.owner.Event == manifest.HookPostToolUse {
		handlers = []map[string]any{{"matcher": "*", "hooks": handlers}}
	}
	return json.Marshal(map[string]any{events[h.owner.Event]: handlers})
}

func (a Adapter) Plan(_ context.Context, req adapter.PlanRequest) (adapter.NativePlan, error) {
	plan := adapter.NativePlan{Adapter: a.Descriptor()}
	hs, err := hooks(req.Packages)
	if err != nil {
		return plan, err
	}
	if len(hs) > 0 {
		for _, dir := range workspaceDirs {
			alias := dir + "/hooks.json"
			if alias == configPath {
				continue
			}
			file, err := req.Project.ReadFile(alias)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return plan, fmt.Errorf("inspect Antigravity hook config %s: %w", alias, err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(file.Content, &fields); err != nil {
				return plan, fmt.Errorf("Antigravity hook config %s: %w; use a JSON object or null and rerun acr realize --dry-run", alias, err)
			}
			for _, h := range hs {
				if _, exists := fields[h.name]; exists {
					return plan, fmt.Errorf("Antigravity hook %q in %s conflicts with the managed hook in %s; rename the user hook in %s and rerun acr realize --dry-run", h.name, alias, configPath, alias)
				}
			}
		}
	}
	for _, h := range hs {
		plan.Items = append(plan.Items, adapter.PlanItem{Owner: h.owner, Target: h.target, Mode: 0o755, Kind: adapter.OutputGeneratedFile}, adapter.PlanItem{Owner: h.owner, Target: configPath, Mode: 0o644, Kind: adapter.OutputConfigMerge})
	}
	if len(hs) > 0 {
		plan.Items = append(plan.Items, adapter.PlanItem{Owner: adapter.HookTranslatorOwner, Target: runtimePath, Mode: 0o755, Kind: adapter.OutputGeneratedFile})
	}
	return plan, nil
}

func (Adapter) Render(_ context.Context, req adapter.RenderRequest) ([]adapter.Output, error) {
	hs, err := hooks(req.Packages)
	if err != nil || len(hs) == 0 {
		return nil, err
	}
	var outputs []adapter.Output
	config := &adapter.ConfigMerge{Format: adapter.ConfigJSON}
	generated := func(target string, owner adapter.OwnerRef, content []byte) adapter.Output {
		return adapter.Output{Target: target, Kind: adapter.OutputGeneratedFile, Mode: 0o755, File: &adapter.GeneratedFile{Owner: owner, Content: content}}
	}
	for _, h := range hs {
		file, err := adapter.ReadPackageFile(h.pkg, h.owner.SourcePath)
		if err != nil {
			return nil, err
		}
		encoded, err := h.config()
		if err != nil {
			return nil, err
		}
		config.Entries = append(config.Entries, adapter.ConfigEntry{Owner: h.owner, Kind: adapter.ConfigField, Key: h.name, EncodedValue: encoded})
		outputs = append(outputs, generated(h.target, h.owner, file.Content))
	}
	return append(outputs, generated(runtimePath, adapter.HookTranslatorOwner, adapter.AntigravityHookTranslator()), adapter.Output{Target: configPath, Kind: adapter.OutputConfigMerge, Mode: 0o644, Config: config}), nil
}

func (Adapter) Validate(_ context.Context, req adapter.ValidateRequest) error {
	if err := adapter.ValidateFileProjection(req, ""); err != nil {
		return err
	}
	hs, err := hooks(req.Packages)
	if err != nil || len(hs) == 0 {
		return err
	}
	for _, file := range req.Files {
		if file.Path != configPath {
			continue
		}
		if err := adapter.ValidateUniqueJSONMembers(file.Content); err != nil {
			return err
		}
		// Keep the original bytes: re-encoding loses number spelling and member order.
		var native map[string]json.RawMessage
		if err := json.Unmarshal(file.Content, &native); err != nil {
			return err
		}
		// UseNumber admits numbers in user fields that a float64 cannot hold.
		decoder := json.NewDecoder(bytes.NewReader(file.Content))
		decoder.UseNumber()
		var members map[string]any
		if err := decoder.Decode(&members); err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(native)) {
			if first, second := repeatedMember(members[name], hookMembers); second != "" {
				return adapter.NativeError(adapter.CodeDuplicateConfigEntry, "%s: hook %q repeats one member as %q and %q, and Antigravity ignores letter case. Keep one spelling, then run acr realize --dry-run", configPath, name, first, second)
			}
			if err := validateNativeHook(native[name]); err != nil {
				return adapter.NativeError(adapter.CodeInvalidNativeEvent, "%s: Antigravity skips the entire file when hook %q is malformed: %v. Fix that hook or move it out of this file, then run acr realize --dry-run", configPath, name, err)
			}
		}
		for _, h := range hs {
			var field any
			if content, ok := native[h.name]; ok {
				if err := json.Unmarshal(content, &field); err != nil {
					return err
				}
			}
			actual, err := json.Marshal(field)
			if err != nil {
				return err
			}
			expected, err := h.config()
			if err != nil {
				return err
			}
			if !bytes.Equal(actual, expected) {
				return adapter.NativeError(adapter.CodeInvalidNativeEvent, "%s: expected the planned %s handler for %q", configPath, events[h.owner.Event], h.name)
			}
		}
		return nil
	}
	return adapter.NativeError(adapter.CodeInvalidNativeEvent, "missing %s", configPath)
}
