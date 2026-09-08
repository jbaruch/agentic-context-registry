package realizeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Seed the released 0.1.4 serialization through the preservation compiler and
// engine, so the upgrade uses genuine recorded ownership, not a patched hash.
// Only hook serialization and the descriptor differ from the current adapter.
type claude014Adapter struct{ claudecode.Adapter }

func (claude014Adapter) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: "claude-code", Version: "1.0.2", Boundary: adapter.CurrentBoundaryVersion}
}

func (old claude014Adapter) Plan(ctx context.Context, request adapter.PlanRequest) (adapter.NativePlan, error) {
	plan, err := old.Adapter.Plan(ctx, request)
	plan.Adapter = old.Descriptor()
	return plan, err
}

func (old claude014Adapter) Render(ctx context.Context, request adapter.RenderRequest) ([]adapter.Output, error) {
	outputs, err := old.Adapter.Render(ctx, request)
	if err != nil {
		return nil, err
	}
	for _, output := range outputs {
		if output.Config == nil {
			continue
		}
		for index := range output.Config.Entries {
			entry := &output.Config.Entries[index]
			name, err := adapter.NativeArtifactName(entry.Owner.Source, entry.Owner.ArtifactID)
			if err != nil {
				return nil, err
			}
			target := ".claude/hooks/" + name + "/" + adapter.SourceBasename(entry.Owner.SourcePath)
			escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(target)
			command := map[string]any{"type": "command", "command": "\"${CLAUDE_PROJECT_DIR}/" + escaped + "\""}
			for _, pkg := range request.Packages {
				for _, hook := range pkg.Manifest.Artifacts.Hooks {
					if pkg.Source == entry.Owner.Source && hook.ID == entry.Owner.ArtifactID && len(hook.Args) != 0 {
						command["args"] = hook.Args
					}
				}
			}
			entry.EncodedValue, err = json.Marshal(map[string]any{"hooks": []any{command}})
			if err != nil {
				return nil, err
			}
		}
	}
	return outputs, nil
}

func (claude014Adapter) Validate(context.Context, adapter.ValidateRequest) error {
	// Historical fixture only: the new validator must reject this old shape.
	return nil
}

func TestCLIUpgradesClaude014HooksToNativeExec(t *testing.T) {
	t.Parallel()
	base, packageRoot, state, value := realizationFixture(t)
	project := filepath.Join(base, "consumer with spaces")
	const probe = "#!/bin/bash\nset -euo pipefail\nprintf 'package hook ran\\n' >&2\n"
	writeFixture(t, filepath.Join(packageRoot, "hooks", "session start.sh"), []byte(probe), 0o755)
	value.Artifacts.Hooks = []manifest.HookArtifact{{ID: "session-start", Event: manifest.HookSessionStart, Path: "hooks/session start.sh"}}
	state.Project.Agents = []string{"claude-code"}
	state.Project.Freshness = "outdated"
	const settingsPath = ".claude/settings.json"
	const user = `{"matcher":"startup","hooks":[{"type":"command","command":"user-command"}]}`
	const tessl = `{"hooks":[{"type":"command","command":"tessl-command","args":[]}]}`
	const permissions = `"permissions": {"allow": ["Read"]}`
	writeFixture(t, filepath.Join(project, settingsPath), []byte("{\n  "+permissions+",\n  \"hooks\": {\"SessionStart\": ["+user+","+tessl+"], \"Stop\": ["+user+"]}\n}\n"), 0o600)
	writeFixture(t, filepath.Join(project, "CLAUDE.md"), []byte("User guidance\n<!-- tessl-managed -->\nTessl guidance\n"), 0o644)
	writeFixture(t, filepath.Join(project, ".tessl", "plugins", "example", "sibling", "hook.sh"), []byte("Tessl payload\n"), 0o755)
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	builtin, ok := freshness.HookPackage(freshness.PolicyOutdated)
	if !ok {
		t.Fatal("missing built-in freshness hook")
	}
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), claude014Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	previous := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(project)), []adapter.Package{
		{Source: "github:example/all-agents", Root: os.DirFS(packageRoot), Manifest: value}, builtin,
	}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realize.NewEngine().Run(project, previous, intents, realize.ModeApply, func(next realize.Ledger) error {
		encoded, err := realize.EncodeLedger(next)
		if err != nil {
			return err
		}
		state.Lock.Realization = encoded
		return dependency.WriteState(project, state)
	}); err != nil {
		t.Fatal(err)
	}

	oldSettings := readProjectFile(t, project, settingsPath)
	oldHooks := ownedClaudeHooks(t, oldSettings)
	if len(oldHooks) != 2 || oldHooks[0].Args != nil || len(oldHooks[1].Args) != 2 {
		t.Fatalf("historical fixture must cover omitted and nonempty args: %#v", oldHooks)
	}
	// Demonstrate the observed ENOENT with the emitted quoted executable.
	broken := exec.Command(strings.ReplaceAll(oldHooks[1].Command, "${CLAUDE_PROJECT_DIR}", project), oldHooks[1].Args...)
	if err := broken.Run(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old freshness spawn = %v, want nonexistent quoted executable", err)
	}
	before := snapshotClaudeUpgrade(t, project)
	application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
	for _, args := range [][]string{{"check"}, {"realize", "--dry-run"}} {
		stdout, stderr, code := runCLI(t, application, append(args, "--project", project, "--json")...)
		if args[0] == "check" {
			if code != cli.ExitChanges || !strings.Contains(stderr, `"code":"realization_changes"`) {
				t.Fatalf("old check: exit %d, stdout %s, stderr %s", code, stdout, stderr)
			}
		} else if code != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"ledgerChanged":true`) {
			t.Fatalf("upgrade dry-run: exit %d, stdout %s, stderr %s", code, stdout, stderr)
		}
		if got := snapshotClaudeUpgrade(t, project); !reflect.DeepEqual(got, before) {
			t.Fatalf("%v wrote project files", args)
		}
	}
	stdout, stderr, code := runCLI(t, application, "realize", "--project", project, "--json")
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("upgrade: exit %d, stdout %s, stderr %s", code, stdout, stderr)
	}
	after := snapshotClaudeUpgrade(t, project)
	for _, changed := range []string{settingsPath, dependency.LockFilename} {
		delete(before, changed)
		delete(after, changed)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("upgrade changed prompts, source payloads, modes, dependencies or sibling files")
	}
	settings := readProjectFile(t, project, settingsPath)
	for _, sibling := range []string{user, tessl, permissions} {
		if bytes.Count(settings, []byte(sibling)) != bytes.Count(oldSettings, []byte(sibling)) {
			t.Fatalf("upgrade changed foreign settings %s: %s", sibling, settings)
		}
	}
	info, err := os.Stat(filepath.Join(project, settingsPath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode changed: %v, %v", info, err)
	}
	hooks := ownedClaudeHooks(t, settings)
	if len(hooks) != 2 {
		t.Fatalf("upgrade must replace both handlers without duplicates: %#v", hooks)
	}
	fakeACR := filepath.Join(t.TempDir(), "fake acr")
	callPath := filepath.Join(t.TempDir(), "argv")
	writeFixture(t, fakeACR, []byte("#!/bin/bash\nset -euo pipefail\nprintf '%s\\0' \"$@\" >\"$ACR_TEST_CALL\"\nprintf 'freshness diagnostic\\n' >&2\nexit 4\n"), 0o755)
	for index, hook := range hooks {
		if hook.Args == nil {
			t.Fatalf("hook %d did not select exec form", index)
		}
		process := exec.Command(strings.ReplaceAll(hook.Command, "${CLAUDE_PROJECT_DIR}", project), hook.Args...)
		process.Dir = t.TempDir()
		process.Env = append(os.Environ(), "ACR_BIN="+fakeACR, "ACR_TEST_CALL="+callPath, "CURSOR_VERSION=")
		var stderr bytes.Buffer
		process.Stderr = &stderr
		output, err := process.Output()
		if err != nil {
			t.Fatalf("upgraded hook %d: %v, stderr %s", index, err, stderr.String())
		}
		if index == 0 {
			if len(output) != 0 || stderr.String() != "package hook ran\n" {
				t.Fatalf("package hook streams changed: stdout %q stderr %q", output, stderr.String())
			}
		} else {
			var result struct {
				HookSpecificOutput struct{ HookEventName, AdditionalContext string }
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
			if result.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(result.HookSpecificOutput.AdditionalContext, "freshness diagnostic") || !strings.Contains(result.HookSpecificOutput.AdditionalContext, "failed open") || stderr.Len() != 0 {
				t.Fatalf("freshness status lost: %s, stderr %s", output, stderr.String())
			}
			physicalProject, err := filepath.EvalSymlinks(project)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(readFile(t, callPath)), "freshness\x00run\x00--project\x00"+physicalProject+"\x00--policy\x00outdated\x00"; got != want {
				t.Fatalf("freshness argv = %q, want %q", got, want)
			}
		}
	}
	current := snapshotClaudeUpgrade(t, project)
	for _, command := range []string{"check", "realize"} {
		stdout, stderr, code := runCLI(t, application, command, "--project", project, "--json")
		if code != cli.ExitSuccess || stderr != "" {
			t.Fatalf("%s after upgrade: exit %d, stdout %s, stderr %s", command, code, stdout, stderr)
		}
		if got := snapshotClaudeUpgrade(t, project); !reflect.DeepEqual(got, current) {
			t.Fatalf("%s after upgrade was not idempotent", command)
		}
	}
	loaded, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Lock.Dependencies, state.Lock.Dependencies) {
		t.Fatal("upgrade changed locked dependencies")
	}
	for _, target := range loadLedger(t, project).Targets {
		for _, entry := range target.Entries {
			if entry.Adapter == "claude-code" && entry.AdapterVersion != claudecode.New().Descriptor().Version {
				t.Fatalf("stale adapter version in ledger: %#v", entry)
			}
		}
	}
}

type claudeExecHook struct {
	Command string
	Args    []string
}

func ownedClaudeHooks(t *testing.T, settings []byte) []claudeExecHook {
	t.Helper()
	var document struct {
		Hooks map[string][]struct{ Hooks []claudeExecHook }
	}
	if err := json.Unmarshal(settings, &document); err != nil {
		t.Fatal(err)
	}
	var result []claudeExecHook
	for _, group := range document.Hooks["SessionStart"] {
		for _, hook := range group.Hooks {
			if strings.Contains(hook.Command, "/.claude/hooks/acr__") {
				result = append(result, hook)
			}
		}
	}
	slices.SortFunc(result, func(a, b claudeExecHook) int { return strings.Compare(a.Command, b.Command) })
	return result
}

func snapshotClaudeUpgrade(t *testing.T, root string) map[string]struct {
	Content string
	Mode    os.FileMode
} {
	t.Helper()
	result := make(map[string]struct {
		Content string
		Mode    os.FileMode
	})
	if err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		result[relative] = struct {
			Content string
			Mode    os.FileMode
		}{string(readFile(t, filename)), info.Mode()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}
