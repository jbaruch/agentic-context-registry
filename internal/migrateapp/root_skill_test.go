package migrateapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func seedRootSkillConsumer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	fixture := filepath.Join("testdata", "root-skill")
	if err := os.CopyFS(filepath.Join(root, ".tessl", "plugins"), os.DirFS(filepath.Join(fixture, "plugins"))); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(fixture, "tessl.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "tessl.json", content, 0o644)
	natives := filepath.Join(root, ".codex", "skills")
	if err := os.MkdirAll(natives, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, target := range map[string]string{
		"codex-review": "../../.tessl/plugins/finsi/codex-review",
		"normal":       "../../.tessl/plugins/zeta/normal/skills/normal",
	} {
		if err := os.Symlink(target, filepath.Join(natives, "tessl__"+id)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRootSkillCompleteFailureOutput(t *testing.T) {
	for _, resolution := range []string{"mapped", "vendor"} {
		for _, dryRun := range []bool{true, false} {
			for _, format := range []string{"text", "json"} {
				name := fmt.Sprintf("dry-run-%t-%s", dryRun, format)
				if resolution == "vendor" {
					name = "vendor-" + name
				}
				t.Run(name, func(t *testing.T) {
					root := seedRootSkillConsumer(t)
					before := hashTreeWithModes(t, root)
					remote := newMultiRepoGitHub(map[string][]byte{
						"zeta/normal": hostileArchive(t, "zeta/normal", map[string]string{"skills/normal/SKILL.md": "# Normal\n"}),
					})
					args := []string{"migrate", "tessl", "--project", root, "--non-interactive"}
					if resolution == "vendor" {
						args = append(args, "--vendor-unmapped")
					} else {
						args = append(args, "--map", "zeta/normal=github:zeta/normal@v1.0.0")
					}
					if dryRun {
						args = append(args, "--dry-run")
					}
					if format == "json" {
						args = append(args, "--json")
					}
					app := &Application{service: newService(remote)}
					stdout, stderr, exit := runCLI(t, app, args...)
					if exit != cli.ExitOperational || stdout != "" {
						t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
					}
					want, err := os.ReadFile(filepath.Join("testdata", "root-skill", name+".golden"))
					if err != nil {
						t.Fatal(err)
					}
					wantOutput := string(want)
					if format == "text" {
						// Quote text goldens to preserve significant output spaces
						// without trailing whitespace in the fixture files.
						if err := json.Unmarshal(want, &wantOutput); err != nil {
							t.Fatal(err)
						}
					}
					if stderr != wantOutput {
						t.Fatalf("unexpected %s output:\n%s", name, stderr)
					}
					after := hashTreeWithModes(t, root)
					if dryRun {
						if !reflect.DeepEqual(before, after) {
							t.Fatal("dry-run changed files")
						}
					} else {
						content, err := os.ReadFile(filepath.Join(root, ".codex/skills/acr__zeta__normal__normal/SKILL.md"))
						if err != nil || string(content) != "# Normal\n" {
							t.Fatalf("normal skill was not realized: %q, %v", content, err)
						}
					}
					for name, hash := range before {
						if strings.HasPrefix(name, ".tessl/") || strings.Contains(name, "tessl__") {
							if after[name] != hash {
								t.Errorf("Tessl path changed: %s", name)
							}
						}
					}
					if resolution == "vendor" && len(remote.calls) != 0 {
						t.Fatal("vendoring contacted GitHub")
					}
				})
			}
		}
	}
}

func TestRootSkillDiagnosticTokens(t *testing.T) {
	for _, payload := range []struct {
		name, key, artifact string
	}{
		{"newline", "review\nFAKE-TRUSTED-GUIDANCE", `"review\nFAKE-TRUSTED-GUIDANCE"`},
		{"escape", "review\x1b[2J", `"review\x1b[2J"`},
		{"1MiB", strings.Repeat("x", 1<<20), `"` + strings.Repeat("x", 128) + `...[truncated]"`},
	} {
		for _, resolution := range []string{"mapped", "vendor"} {
			for _, dryRun := range []bool{true, false} {
				for _, format := range []string{"text", "json"} {
					t.Run(fmt.Sprintf("%s/%s/dry-run-%t/%s", payload.name, resolution, dryRun, format), func(t *testing.T) {
						root := seedRootSkillConsumer(t)
						tilePath := ".tessl/plugins/finsi/codex-review/tile.json"
						content, err := os.ReadFile(filepath.Join(root, tilePath))
						if err != nil {
							t.Fatal(err)
						}
						var tile map[string]any
						if err := json.Unmarshal(content, &tile); err != nil {
							t.Fatal(err)
						}
						identity := "finsi/" + payload.key
						tile["name"] = identity
						tile["skills"] = map[string]any{payload.key: map[string]string{"path": "SKILL.md"}}
						content, err = json.Marshal(tile)
						if err != nil {
							t.Fatal(err)
						}
						writeFile(t, root, tilePath, content, 0o644)
						remote := newMultiRepoGitHub(map[string][]byte{
							"zeta/normal": hostileArchive(t, "zeta/normal", map[string]string{"skills/normal/SKILL.md": "# Normal\n"}),
						})
						args := []string{"migrate", "tessl", "--project", root, "--non-interactive"}
						if resolution == "vendor" {
							args = append(args, "--vendor-unmapped")
						} else {
							args = append(args, "--map", "zeta/normal=github:zeta/normal@v1.0.0")
						}
						if dryRun {
							args = append(args, "--dry-run")
						}
						if format == "json" {
							args = append(args, "--json")
						}
						stdout, stderr, exit := runCLI(t, &Application{service: newService(remote)}, args...)
						if exit != cli.ExitOperational || stdout != "" {
							t.Fatalf("exit=%d stdout bytes=%d stderr bytes=%d", exit, len(stdout), len(stderr))
						}
						wantRemedy := "in the source package, move SKILL.md and its required scripts, references, and assets into a dedicated directory under skills/ and update the skill declaration; keep package metadata and unrelated artifacts outside that directory; ask the publisher if needed, reinstall or update through Tessl, then rerun migration; alternatively, uninstall this package through Tessl, migrate the remaining packages, and add the skill to the project by hand"
						wantMessage := "Tessl package " + migrate.DiagnosticToken(identity) + " requires manual migration; see Package inventory for the complete inventory and guidance"
						blockerPath := ".tessl/plugins/" + identity
						if format == "text" {
							if len(stderr) > 64<<10 || strings.Contains(stderr, "\nFAKE-TRUSTED-GUIDANCE") || strings.ContainsRune(stderr, '\x1b') || strings.Count(stderr, wantRemedy) != 2 {
								t.Fatalf("unsafe coexistence diagnostic (%d bytes)", len(stderr))
							}
							for _, want := range []string{wantMessage, "skill " + payload.artifact + "  unsupported", migrate.ReasonRootSkill + "  " + migrate.DiagnosticToken(blockerPath)} {
								if !strings.Contains(stderr, want) {
									t.Errorf("coexistence diagnostic missing %q", want)
								}
							}
							return
						}
						var envelope struct {
							Error  struct{ Code, Message, Remedy string }
							Result migrate.MigrationReport
						}
						if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
							t.Fatal(err)
						}
						if envelope.Error.Code != migrate.ReasonRootSkill || envelope.Error.Message != wantMessage || envelope.Error.Remedy != wantRemedy {
							t.Fatal("unexpected decoded error diagnostic")
						}
						if len(envelope.Result.Packages) != 2 || envelope.Result.Packages[0].Remedy != wantRemedy || len(envelope.Result.Blockers) != 1 || envelope.Result.Blockers[0].Remedy != wantRemedy || envelope.Result.Blockers[0].Detail != "ACR v1 cannot represent a package-root skill tree" {
							t.Fatal("unexpected decoded package/blocker diagnostic")
						}
						if len(envelope.Result.Packages[0].Artifacts) != 1 || envelope.Result.Packages[0].Artifacts[0].ID != payload.key {
							t.Fatal("raw artifact ID did not round-trip unchanged")
						}
						if envelope.Result.Packages[0].TesslIdentity != identity || envelope.Result.Blockers[0].Path != blockerPath {
							t.Fatal("raw package identity or blocker path did not round-trip unchanged")
						}
						if strings.ContainsAny(wantRemedy, "\n\x1b") || len(wantRemedy) > 1024 {
							t.Fatal("unsafe decoded remedy")
						}
					})
				}
			}
		}
	}
}

func TestRootSkillContinuesOtherPackages(t *testing.T) {
	for _, resolution := range []string{"mapped", "vendor", "unpublished", "unmapped"} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry-run-%t", resolution, dryRun), func(t *testing.T) {
				root := seedRootSkillConsumer(t)
				before := hashTreeWithModes(t, root)
				remote := newMultiRepoGitHub(map[string][]byte{
					"zeta/normal": hostileArchive(t, "zeta/normal", map[string]string{"skills/normal/SKILL.md": "# Normal\n"}),
				})
				app := &Application{service: newService(remote)}
				var transport *releaselessGitHub
				if resolution == "unpublished" {
					transport = &releaselessGitHub{statuses: map[string]int{
						"/repos/zeta/normal/releases/tags/v1.0.0": 404,
						"/repos/zeta/normal/releases/latest":      404,
						"/repos/zeta/normal":                      200,
					}, bodies: map[string]string{"/repos/zeta/normal": `{"id":7,"full_name":"zeta/normal"}`}}
					app = productionMigrateApplication(t, transport)
				}
				args := []string{"migrate", "tessl", "--project", root, "--non-interactive", "--json"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				if resolution == "vendor" {
					args = append(args, "--vendor-unmapped")
				} else if resolution != "unmapped" {
					args = append(args, "--map", "finsi/codex-review=github:finsi/codex-review", "--map", "zeta/normal=github:zeta/normal@v1.0.0")
				}
				stdout, stderr, exit := runCLI(t, app, args...)
				if exit != cli.ExitOperational || stdout != "" {
					t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
				}
				var envelope struct {
					Error  struct{ Code string }   `json:"error"`
					Result migrate.MigrationReport `json:"result"`
				}
				if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
					t.Fatal(err)
				}
				report := envelope.Result
				if len(report.Packages) != 2 || report.Packages[0].Reason != migrate.ReasonRootSkill || report.Packages[0].Remedy == "" || report.Packages[1].TesslIdentity != "zeta/normal" || report.FinalizationReady {
					t.Fatalf("incomplete package report: %+v", report.Packages)
				}
				if resolution == "unmapped" {
					if envelope.Error.Code != cli.CodeUnmappedPackage || len(remote.calls) != 0 || !reflect.DeepEqual(before, hashTreeWithModes(t, root)) || strings.Contains(stderr, "retry the command, then report the failure") {
						t.Fatalf("unmapped failure lost inventory, recovery guidance, or changed files: %s", stderr)
					}
					return
				}
				if resolution == "unpublished" {
					if envelope.Error.Code != cli.CodeSourceNotPublished || len(transport.requests) != 3 || !reflect.DeepEqual(before, hashTreeWithModes(t, root)) {
						t.Fatalf("later failure lost inventory or changed files: %s", stderr)
					}
					return
				}
				if envelope.Error.Code != migrate.ReasonRootSkill || len(report.Mappings) != 1 || report.Mappings[0].From != "zeta/normal" || report.Wrote == dryRun {
					t.Fatalf("other package was not migrated: %s", stderr)
				}
				if resolution == "vendor" && len(remote.calls) != 0 {
					t.Fatal("vendoring contacted GitHub")
				}
				if resolution == "mapped" && remote.calls["download"] == 0 {
					t.Fatal("mapped package was not materialized")
				}
				if dryRun {
					if !reflect.DeepEqual(before, hashTreeWithModes(t, root)) {
						t.Fatal("dry-run changed files")
					}
				} else {
					content, err := os.ReadFile(filepath.Join(root, ".codex/skills/acr__zeta__normal__normal/SKILL.md"))
					if err != nil || string(content) != "# Normal\n" {
						t.Fatalf("normal skill was not realized: %q, %v", content, err)
					}
					if _, err := os.Stat(filepath.Join(root, ".agents/vendor/finsi/codex-review")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("root package was vendored: %v", err)
					}
					applied := hashTreeWithModes(t, root)
					_, stderr, exit = runCLI(t, app, args...)
					if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
						t.Fatal(err)
					}
					if exit != cli.ExitOperational || envelope.Error.Code != migrate.ReasonRootSkill || envelope.Result.Wrote || !reflect.DeepEqual(applied, hashTreeWithModes(t, root)) {
						t.Fatalf("repeated migration was not inert: %s", stderr)
					}
					_, stderr, exit = runCLI(t, app, append(args, "--finalize")...)
					if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
						t.Fatal(err)
					}
					if exit != cli.ExitConflict || envelope.Error.Code != "finalization_blocked" || envelope.Result.Wrote || len(envelope.Result.Packages) != 2 || !reflect.DeepEqual(applied, hashTreeWithModes(t, root)) {
						t.Fatalf("finalization did not retain the root package: %s", stderr)
					}
				}
				after := hashTreeWithModes(t, root)
				for name, hash := range before {
					if strings.HasPrefix(name, ".tessl/") || strings.Contains(name, "tessl__") {
						if after[name] != hash {
							t.Errorf("Tessl path changed: %s", name)
						}
					}
				}
			})
		}
	}
}
