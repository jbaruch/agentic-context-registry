// Package adaptertest provides a golden-fixture harness and a reference
// adapter for exercising the internal/adapter boundary end to end. It is
// itself test infrastructure (comparable to net/http/httptest), not a
// production adapter: issue #12 ships the real Claude Code, Codex, and
// Cursor implementations.
package adaptertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// nativeEventOverrideArg, when present as a hook artifact's first argument,
// makes the reference adapter render a deliberately wrong native event
// spelling. Production adapters compute native event names deterministically
// from manifest.HookEvent; this escape hatch exists solely so a golden
// fixture can drive a real Render-through-Validate failure and prove
// Validate independently rejects a bad rendering, rather than trusting its
// own Render output.
const nativeEventOverrideArg = "test-corrupt-native-event"

var nativeEventNames = map[manifest.HookEvent]string{
	manifest.HookSessionStart:     "SessionStart",
	manifest.HookSessionEnd:       "SessionEnd",
	manifest.HookUserPromptSubmit: "UserPromptSubmit",
	manifest.HookPreToolUse:       "PreToolUse",
	manifest.HookPostToolUse:      "PostToolUse",
	manifest.HookStop:             "Stop",
}

// referenceAdapter is a hostile-enough fixture adapter: it renders rule
// artifacts as pass-through generated files (so a fixture's source file can
// carry deliberately malformed frontmatter), skill artifacts as shared
// Markdown digest blocks, script artifacts as generated executables, and
// hook artifacts as structural JSON entries merged into one native config
// file, then validates every one of those shapes independently of how it
// rendered them.
type referenceAdapter struct {
	descriptor adapter.Descriptor
}

// NewReferenceAdapter returns the #10 boundary's reference/hostile fixture
// adapter. It is the test double golden fixtures render against; it is not
// a production adapter.
func NewReferenceAdapter(version string) adapter.Adapter {
	return referenceAdapter{descriptor: adapter.Descriptor{ID: "reference-fixture", Version: version, Boundary: adapter.CurrentBoundaryVersion}}
}

func (fixture referenceAdapter) Descriptor() adapter.Descriptor { return fixture.descriptor }

func (fixture referenceAdapter) Detect(_ context.Context, request adapter.DetectRequest) (adapter.Detection, error) {
	_, err := request.Project.ReadFile("AGENTS.md")
	if err == nil {
		return adapter.Detection{Detected: true, Evidence: []string{"AGENTS.md"}}, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return adapter.Detection{}, nil
	}
	return adapter.Detection{}, fmt.Errorf("detect: read AGENTS.md: %w", err)
}

func (fixture referenceAdapter) SupportedArtifacts() []adapter.ArtifactKind {
	return []adapter.ArtifactKind{adapter.ArtifactHook, adapter.ArtifactRule, adapter.ArtifactScript, adapter.ArtifactSkill}
}

func (fixture referenceAdapter) SupportedEvents() []manifest.HookEvent {
	return []manifest.HookEvent{
		manifest.HookPostToolUse, manifest.HookPreToolUse, manifest.HookSessionEnd,
		manifest.HookSessionStart, manifest.HookStop, manifest.HookUserPromptSubmit,
	}
}

func (fixture referenceAdapter) Plan(_ context.Context, request adapter.PlanRequest) (adapter.NativePlan, error) {
	var items []adapter.PlanItem
	for _, pkg := range request.Packages {
		for _, rule := range pkg.Manifest.Artifacts.Rules {
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: rule.ID, SourcePath: rule.Path, Kind: adapter.ArtifactRule},
				Target: "rules/" + rule.ID + ".md", Kind: adapter.OutputGeneratedFile, Mode: 0o644,
			})
		}
		for _, skill := range pkg.Manifest.Artifacts.Skills {
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: skill.ID, SourcePath: skill.Path, Kind: adapter.ArtifactSkill},
				Target: "AGENTS.md", Kind: adapter.OutputMarkdownInclude, Mode: 0o644,
			})
		}
		for _, script := range pkg.Manifest.Artifacts.Scripts {
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: script.ID, SourcePath: script.Path, Kind: adapter.ArtifactScript},
				Target: "scripts/" + script.ID, Kind: adapter.OutputGeneratedFile, Mode: 0o755,
			})
		}
		for _, hook := range pkg.Manifest.Artifacts.Hooks {
			items = append(items, adapter.PlanItem{
				Owner:  adapter.OwnerRef{Source: pkg.Source, ArtifactID: hook.ID, SourcePath: hook.Path, Kind: adapter.ArtifactHook, Event: hook.Event},
				Target: "hooks.json", Kind: adapter.OutputConfigMerge, Mode: 0o644,
			})
		}
	}
	return adapter.NativePlan{Adapter: fixture.descriptor, Items: items}, nil
}

func (fixture referenceAdapter) Render(_ context.Context, request adapter.RenderRequest) ([]adapter.Output, error) {
	var outputs []adapter.Output
	var hookEntries []adapter.ConfigEntry
	for _, pkg := range request.Packages {
		for _, rule := range pkg.Manifest.Artifacts.Rules {
			content, err := fs.ReadFile(pkg.Root, rule.Path)
			if err != nil {
				return nil, fmt.Errorf("read rule %q: %w", rule.ID, err)
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: rule.ID, SourcePath: rule.Path, Kind: adapter.ArtifactRule}
			outputs = append(outputs, adapter.Output{
				Target: "rules/" + rule.ID + ".md", Mode: 0o644, Kind: adapter.OutputGeneratedFile,
				File: &adapter.GeneratedFile{Owner: owner, Content: content},
			})
		}
		for _, skill := range pkg.Manifest.Artifacts.Skills {
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: skill.ID, SourcePath: skill.Path, Kind: adapter.ArtifactSkill}
			outputs = append(outputs, adapter.Output{
				Target: "AGENTS.md", Mode: 0o644, Kind: adapter.OutputMarkdownInclude,
				Markdown: []adapter.MarkdownInsertion{{Owner: owner, BlockID: "skill-" + skill.ID, Body: []byte("Skill: " + skill.ID + "\n")}},
			})
		}
		for _, script := range pkg.Manifest.Artifacts.Scripts {
			content, err := fs.ReadFile(pkg.Root, script.Path)
			if err != nil {
				return nil, fmt.Errorf("read script %q: %w", script.ID, err)
			}
			owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: script.ID, SourcePath: script.Path, Kind: adapter.ArtifactScript}
			outputs = append(outputs, adapter.Output{
				Target: "scripts/" + script.ID, Mode: 0o755, Kind: adapter.OutputGeneratedFile,
				File: &adapter.GeneratedFile{Owner: owner, Content: content},
			})
		}
		for _, hook := range pkg.Manifest.Artifacts.Hooks {
			eventName := nativeEventNames[hook.Event]
			if len(hook.Args) != 0 && hook.Args[0] == nativeEventOverrideArg {
				eventName = strings.ToLower(eventName)
			}
			encoded, err := json.Marshal(map[string]any{"event": eventName, "path": hook.Path, "args": hook.Args})
			if err != nil {
				return nil, fmt.Errorf("encode hook %q: %w", hook.ID, err)
			}
			hookEntries = append(hookEntries, adapter.ConfigEntry{
				Owner:     adapter.OwnerRef{Source: pkg.Source, ArtifactID: hook.ID, SourcePath: hook.Path, Kind: adapter.ArtifactHook, Event: hook.Event},
				Container: []string{"hooks"}, Kind: adapter.ConfigField, Key: hook.ID, EncodedValue: encoded,
			})
		}
	}
	if len(hookEntries) != 0 {
		outputs = append(outputs, adapter.Output{
			Target: "hooks.json", Mode: 0o644, Kind: adapter.OutputConfigMerge,
			Config: &adapter.ConfigMerge{Format: adapter.ConfigJSON, Entries: hookEntries},
		})
	}
	return outputs, nil
}

func (fixture referenceAdapter) Validate(_ context.Context, request adapter.ValidateRequest) error {
	byPath := make(map[string]adapter.CandidateFile, len(request.Files))
	for _, file := range request.Files {
		byPath[file.Path] = file
	}
	for _, item := range request.Plan.Items {
		file, ok := byPath[item.Target]
		if !ok {
			continue
		}
		switch item.Owner.Kind {
		case adapter.ArtifactRule:
			if err := validateFrontmatter(file.Content); err != nil {
				return fmt.Errorf("malformed_frontmatter: rule %q at %q: %w", item.Owner.ArtifactID, item.Target, err)
			}
		case adapter.ArtifactHook:
			if err := validateNativeEvent(file.Content, item.Owner); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFrontmatter(content []byte) error {
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return fmt.Errorf("missing opening frontmatter marker")
	}
	remainder := text[len("---\n"):]
	closing := strings.Index(remainder, "\n---\n")
	if closing < 0 {
		if strings.HasSuffix(remainder, "\n---") || remainder == "---" {
			closing = len(remainder) - len("\n---")
		} else {
			return fmt.Errorf("missing closing frontmatter marker")
		}
	}
	block := remainder[:closing]
	seen := make(map[string]struct{})
	for _, line := range strings.Split(block, "\n") {
		key, _, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate frontmatter key %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateNativeEvent(content []byte, owner adapter.OwnerRef) error {
	var doc struct {
		Hooks map[string]struct {
			Event string `json:"event"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("invalid_native_event: hook %q: decode rendered config: %w", owner.ArtifactID, err)
	}
	entry, ok := doc.Hooks[owner.ArtifactID]
	if !ok {
		return fmt.Errorf("invalid_native_event: hook %q: rendered config has no entry", owner.ArtifactID)
	}
	want := nativeEventNames[owner.Event]
	if entry.Event != want {
		return fmt.Errorf("invalid_native_event: hook %q: rendered event %q, want %q", owner.ArtifactID, entry.Event, want)
	}
	return nil
}
