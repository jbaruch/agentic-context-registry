package migrateapp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func TestRemigrationAfterUninstallNamesRemainingRequestConflict(t *testing.T) {
	for _, uninstall := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-uninstall", true: "after-uninstall"}[uninstall], func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			writeJSON(t, root, "tessl.json", map[string]any{"dependencies": map[string]any{
				"example/orphan": map[string]string{"version": "legacy"}, "example/alpha": map[string]string{"version": "latest"},
			}})
			writeJSON(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{
				"name": "example/alpha", "version": "1.0.0", "repository": "https://github.com/example/alpha", "rules": []string{"rules/always-rule.md"},
			})
			writeFile(t, root, ".tessl/plugins/example/alpha/rules/always-rule.md", []byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
			remote := remigrationRemote(t)
			retained := seedRetainedState(t, root, remote)
			app := NewApplication(remote, "test")
			args := []string{"migrate", "tessl", "--vendor-unmapped", "--non-interactive", "--json", "--project", root}
			pinned := append(append([]string(nil), args...), "--map", "example/alpha=github:example/alpha@v1.0.0")
			requireMigrationSuccess(t, app, pinned...)
			if uninstall {
				requireMigrationSuccess(t, app, "uninstall", "vendor:example/orphan", "--json", "--project", root)
				if _, ok := declarationBySource(loadRemigrationState(t, root).Project.Dependencies, "vendor:example/orphan"); ok {
					t.Fatal("uninstall retained vendor declaration")
				}
			}
			before := hashTreeWithModes(t, root)
			for _, dryRun := range []bool{true, false} {
				attempt := append([]string(nil), args...)
				if dryRun {
					attempt = append(attempt, "--dry-run")
				}
				stdout, stderr, exit := runCLI(t, app, attempt...)
				t.Logf("dryRun=%t exit=%d stdout=%s stderr=%s", dryRun, exit, stdout, stderr)
				if exit != cli.ExitOperational || stdout != "" {
					t.Fatalf("refusal: %d %s %s", exit, stdout, stderr)
				}
				var envelope struct {
					Error struct{ Code, Message, Remedy string } `json:"error"`
				}
				if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Error.Code != cli.CodeProjectStateConflict {
					t.Fatalf("wrong error: %s", stderr)
				}
				diagnostic := envelope.Error.Message + " " + envelope.Error.Remedy
				for _, want := range []string{"github:example/alpha", "v1.0.0", "latest", "agents.yaml", "--map 'example/alpha=github:example/alpha@v1.0.0'"} {
					if !strings.Contains(diagnostic, want) {
						t.Errorf("diagnostic missing %q: %s", want, diagnostic)
					}
				}
				if !mapsEqual(before, hashTreeWithModes(t, root)) {
					t.Fatal("refusal changed project bytes or modes")
				}
			}
			// Keeping the original explicit mapping restores the removed vendor while
			// leaving the remaining pin and unrelated packages intact.
			requireMigrationSuccess(t, app, pinned...)
			assertRetained(t, retained, root)
			requireMigrationSuccess(t, app, pinned...)
		})
	}
}

// A declaration-only vendor cannot be verified for automatic superseding.
// The diagnostic must suggest the supported uninstall, never vendor install.
func TestRemigrationDroppedVendorRecoveryUsesSupportedCommand(t *testing.T) {
	root := writeUnmappedConsumer(t)
	app := NewApplication(remigrationRemote(t), "test")
	requireMigrationSuccess(t, app, "migrate", "tessl", "--vendor-unmapped", "--project", root)
	state := loadRemigrationState(t, root)
	state.Lock.Dependencies = nil
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	remote := dependencytest.NewRemote()
	source := "github:example/orphan"
	remote.Latest[source] = dependency.Release{ID: 1, Tag: "v1.0.0"}
	remote.Commits[source+"@v1.0.0"] = strings.Repeat("a", 40)
	remote.Archives[source+"@"+strings.Repeat("a", 40)] = orphanPackageArchive(t)
	app = NewApplication(remote, "test")
	args := []string{"migrate", "tessl", "--project", root, "--map", "example/orphan=github:example/orphan@v1.0.0"}
	before := hashTreeWithModes(t, root)
	for _, jsonOutput := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			attempt := append([]string(nil), args...)
			if jsonOutput {
				attempt = append(attempt, "--json")
			}
			if dryRun {
				attempt = append(attempt, "--dry-run")
			}
			stdout, stderr, exit := runCLI(t, app, attempt...)
			t.Logf("dropped vendor: exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			if exit != cli.ExitOperational || stdout != "" {
				t.Fatalf("wrong refusal: %d %s %s", exit, stdout, stderr)
			}
			for _, want := range []string{"vendor:example/orphan", "vendored", "github:example/orphan", "v1.0.0", "acr uninstall vendor:example/orphan"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("missing %q: %s", want, stderr)
				}
			}
			if strings.Contains(stderr, "acr install vendor:") {
				t.Errorf("unsupported recovery: %s", stderr)
			}
			if !mapsEqual(before, hashTreeWithModes(t, root)) {
				t.Fatal("refusal changed project bytes or modes")
			}
		}
	}
	requireMigrationSuccess(t, app, "uninstall", "vendor:example/orphan", "--project", root)
	requireMigrationSuccess(t, app, args...)
}

func TestRemigrationDeclarationConflictFallback(t *testing.T) {
	existing := []dependency.Declaration{{Source: "github:example/alpha", Requested: "latest", Hold: &dependency.Hold{Pin: "v1.0.0", Rejected: "v2.0.0"}}}
	desired := []dependency.Declaration{{Source: "github:example/alpha", Requested: "v1.0.0"}}
	var conflict *Error
	err := declarationConflict(existing, desired, nil)
	if !errors.As(err, &conflict) || conflict.Code != cli.CodeProjectStateConflict {
		t.Fatalf("wrong conflict: %v", err)
	}
	for _, want := range []string{`set requested to "v1.0.0"`, "remove its hold", "remove only that source's dependency entry", "acr migrate tessl"} {
		if !strings.Contains(conflict.Remedy, want) {
			t.Errorf("missing %q: %s", want, conflict.Remedy)
		}
	}
}

func TestRemigrationRecoveryQuotesMappedReleaseTag(t *testing.T) {
	const requested = "release'$(echo unexpected)"
	existing := []dependency.Declaration{{Source: "github:upstream/package", Requested: requested}}
	desired := []dependency.Declaration{{Source: "github:upstream/package", Requested: "latest"}}
	mappings := []migrate.Mapping{{From: "local/alias", Source: "github:upstream/package", Requested: "latest"}}
	var conflict *Error
	if err := declarationConflict(existing, desired, mappings); !errors.As(err, &conflict) {
		t.Fatalf("wrong error: %v", err)
	}
	want := `--map 'local/alias=github:upstream/package@release'"'"'$(echo unexpected)'`
	if !strings.Contains(conflict.Remedy, want) {
		t.Fatalf("recovery must preserve the mapping's original identity and quote its request: %s", conflict.Remedy)
	}
}
