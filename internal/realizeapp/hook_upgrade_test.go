package realizeapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Reproduce hook serialization at 3200628be47650f0fe02e5966a03e52030c423ec,
// including descriptor versions and absence of adapter-owned translators.
// Other artifacts use the current renderer, as in the Claude historical fixture.
type historicalHookAdapter struct{ adapter.Adapter }

func (old historicalHookAdapter) Descriptor() adapter.Descriptor {
	descriptor := old.Adapter.Descriptor()
	descriptor.Version = map[string]string{"codex": "1.0.4", "cursor": "1.0.2"}[descriptor.ID]
	return descriptor
}

func (old historicalHookAdapter) Plan(ctx context.Context, request adapter.PlanRequest) (adapter.NativePlan, error) {
	plan, err := old.Adapter.Plan(ctx, request)
	plan.Adapter = old.Descriptor()
	plan.Items = slices.DeleteFunc(plan.Items, func(item adapter.PlanItem) bool {
		return item.Owner == adapter.HookTranslatorOwner
	})
	return plan, err
}

func (old historicalHookAdapter) Render(ctx context.Context, request adapter.RenderRequest) ([]adapter.Output, error) {
	outputs, err := old.Adapter.Render(ctx, request)
	if err != nil {
		return nil, err
	}
	outputs = slices.DeleteFunc(outputs, func(output adapter.Output) bool {
		return output.File != nil && output.File.Owner == adapter.HookTranslatorOwner
	})
	for _, output := range outputs {
		if output.Config == nil {
			continue
		}
		for i := range output.Config.Entries {
			entry := &output.Config.Entries[i]
			if entry.Owner.Event == "" {
				continue
			}
			name, err := adapter.NativeArtifactName(entry.Owner.Source, entry.Owner.ArtifactID)
			if err != nil {
				return nil, err
			}
			target := "." + old.Descriptor().ID + "/hooks/" + name + "/" + adapter.SourceBasename(entry.Owner.SourcePath)
			var args []string
			for _, pkg := range request.Packages {
				for _, hook := range pkg.Manifest.Artifacts.Hooks {
					if pkg.Source == entry.Owner.Source && hook.ID == entry.Owner.ArtifactID {
						args = hook.Args
					}
				}
			}
			if old.Descriptor().ID == "codex" {
				escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(target)
				command := historicalCodexLocator + "exec \"$acr_root/" + escaped + "\""
				entry.EncodedValue = adapter.TOMLCommandHookValue(adapter.ShellJoin(command, args))
			} else {
				entry.EncodedValue, err = json.Marshal(map[string]string{"command": adapter.ShellJoin(adapter.ShellQuote(target), args)})
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return outputs, nil
}

func (historicalHookAdapter) Validate(context.Context, adapter.ValidateRequest) error {
	// Current validation deliberately rejects historical command shapes.
	return nil
}

const historicalCodexLocator = "acr_root=$PWD\n" +
	"while [ ! -f \"$acr_root/agents.yaml\" ]; do\n" +
	"  if [ \"$acr_root\" = / ]; then\n" +
	"    printf '%s\\n' 'ACR hook: no agents.yaml above session cwd; launch inside the ACR project.' >&2\n" +
	"    exit 1\n" +
	"  fi\n" +
	"  acr_root=${acr_root%/*}\n" +
	"  if [ -z \"$acr_root\" ]; then acr_root=/; fi\n" +
	"done\n"

func TestHookAdapterRollbackBeforeRestoringLock(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run(map[bool]string{false: "supported-order", true: "historical-lock-first-conflicts"}[reversed], func(t *testing.T) {
			project, packageRoot, state, value := realizationFixture(t)
			state.Project.Agents = []string{"codex", "cursor"}
			state.Project.Freshness = "outdated"
			value.Artifacts.Hooks = []manifest.HookArtifact{{ID: "authored", Event: manifest.HookPreToolUse, Path: "hooks/check.sh", Args: []string{"two words"}}}
			writeFixture(t, filepath.Join(packageRoot, "hooks/check.sh"), []byte("#!/bin/sh\nprintf '{}'\n"), 0o755)
			if err := dependency.WriteState(project, state); err != nil {
				t.Fatal(err)
			}
			const codexUser = "[model]\nname = \"user-model\"\n"
			const cursorUser = `"user_setting":{"keep":true}`
			writeFixture(t, filepath.Join(project, ".codex/config.toml"), []byte(codexUser), 0o600)
			writeFixture(t, filepath.Join(project, ".cursor/hooks.json"), []byte("{"+cursorUser+"}\n"), 0o600)
			assertUser := func() {
				t.Helper()
				for name, content := range map[string]string{".codex/config.toml": codexUser, ".cursor/hooks.json": cursorUser} {
					info, err := os.Stat(filepath.Join(project, name))
					if err != nil || info.Mode().Perm() != 0o600 || !strings.Contains(string(readProjectFile(t, project, name)), content) {
						t.Fatalf("user configuration changed: %s (%v)", name, err)
					}
				}
			}
			builtin, ok := freshness.HookPackage(freshness.PolicyOutdated)
			if !ok {
				t.Fatal("missing freshness hooks")
			}
			packages := []adapter.Package{{Source: state.Lock.Dependencies[0].Source, Root: os.DirFS(packageRoot), Manifest: value}, builtin}
			coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), historicalHookAdapter{codex.New()}, historicalHookAdapter{cursor.New()})
			if err != nil {
				t.Fatal(err)
			}
			// Run the historical adapters through the real compiler and engine,
			// preserving committed-ledger ownership and the local receipt boundary.
			older := func(mode realize.Mode) {
				t.Helper()
				live, err := dependency.LoadState(project)
				if err != nil {
					t.Fatal(err)
				}
				previous := loadLedger(t, project)
				intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(project)), packages, previous)
				if err != nil {
					t.Fatal(err)
				}
				_, err = realize.NewEngine().Run(project, previous, intents, mode, func(next realize.Ledger) error {
					encoded, err := realize.EncodeLedger(next)
					if err != nil {
						return err
					}
					live.Lock.Realization = encoded
					return dependency.WriteState(project, live)
				})
				if err != nil {
					t.Fatalf("historical %s: %v", mode, err)
				}
				if mode == realize.ModeApply {
					receipt, err := encodeApplied(project, generatedLedger(loadLedger(t, project)))
					if err != nil {
						t.Fatal(err)
					}
					writeFixture(t, filepath.Join(project, appliedPath), receipt, 0o644)
				}
				assertUser()
			}
			application := &Application{service: NewService(fixtureLoader{root: packageRoot, manifest: value}), fallback: cli.UnavailableApplication{}}
			current := func(command string) {
				t.Helper()
				out, stderr, code := runCLI(t, application, command, "--project", project, "--json")
				if code != cli.ExitSuccess || stderr != "" {
					t.Fatalf("current %s: exit %d; %s %s", command, code, out, stderr)
				}
				assertUser()
			}
			older(realize.ModeApply)
			older(realize.ModeCheck)
			historical := map[string][]byte{}
			for _, name := range []string{dependency.LockFilename, ".codex/config.toml", ".cursor/hooks.json"} {
				historical[name] = readProjectFile(t, project, name)
			}
			restore := func() {
				for name, content := range historical {
					mode := os.FileMode(0o600)
					if name == dependency.LockFilename {
						mode = 0o644
					}
					writeFixture(t, filepath.Join(project, name), content, mode)
				}
			}
			current("realize")
			current("check")
			for _, id := range state.Project.Agents {
				name := "." + id + "/hooks/acr-hook.py"
				if _, err := os.Stat(filepath.Join(project, name)); err != nil {
					t.Fatalf("upgrade did not create %s: %v", name, err)
				}
			}
			if reversed {
				restore()
				// Restoring the old lock does not grant receipt-only deletion.
				older(realize.ModeApply)
				older(realize.ModeCheck)
				for _, id := range state.Project.Agents {
					name := "." + id + "/hooks/acr-hook.py"
					if _, err := os.Stat(filepath.Join(project, name)); err != nil {
						t.Fatalf("receipt-only translator was removed: %v", err)
					}
				}
				before := snapshotClaudeUpgrade(t, project)
				out, stderr, code := runCLI(t, application, "realize", "--project", project, "--json")
				if code != cli.ExitConflict || !strings.Contains(stderr, "realization_conflict") {
					t.Fatalf("unsupported re-upgrade: %d; %s %s", code, out, stderr)
				}
				if !reflect.DeepEqual(before, snapshotClaudeUpgrade(t, project)) {
					t.Fatal("conflicting re-upgrade wrote files")
				}
				return
			}
			older(realize.ModeApply) // The upgraded lock still owns both translators.
			older(realize.ModeCheck)
			for _, id := range state.Project.Agents {
				name := "." + id + "/hooks/acr-hook.py"
				if _, err := os.Stat(filepath.Join(project, name)); !os.IsNotExist(err) {
					t.Fatalf("rollback retained translator %s: %v", name, err)
				}
				for _, ledger := range []realize.Ledger{loadLedger(t, project), readApplied(t, project)} {
					for _, target := range ledger.Targets {
						if target.Path == name {
							t.Fatalf("rollback retained translator ownership %s", name)
						}
					}
				}
			}
			restore()
			older(realize.ModeApply)
			older(realize.ModeCheck)
			current("realize")
			current("check")
			before := snapshotClaudeUpgrade(t, project)
			current("realize")
			if !reflect.DeepEqual(before, snapshotClaudeUpgrade(t, project)) {
				t.Fatal("repeated candidate realization changed bytes or modes")
			}
		})
	}
}
