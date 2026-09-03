package migrateapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

type migrationGitHub struct {
	releases map[string]dependency.Release
	calls    []string
}

type integrationGitHub struct {
	release       dependency.Release
	commit        string
	archive       []byte
	latestCalls   int
	resolveCalls  int
	downloadCalls int
}

func (github *integrationGitHub) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	github.latestCalls++
	return github.release, nil
}

func (github *integrationGitHub) ReleaseByTag(_ context.Context, _ dependency.Repository, tag string) (dependency.Release, error) {
	if tag == github.release.Tag {
		return github.release, nil
	}
	return dependency.Release{}, &dependency.RemoteError{StatusCode: 404, Err: fmt.Errorf("tag %s not found", tag)}
}

func (github *integrationGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	github.resolveCalls++
	return github.commit, nil
}

func (github *integrationGitHub) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	github.downloadCalls++
	return append([]byte(nil), github.archive...), nil
}

func (github *integrationGitHub) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("unexpected release asset call")
}

func (github *migrationGitHub) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	return dependency.Release{}, errors.New("unexpected latest release call")
}

func (github *migrationGitHub) ReleaseByTag(_ context.Context, _ dependency.Repository, tag string) (dependency.Release, error) {
	github.calls = append(github.calls, tag)
	if release, ok := github.releases[tag]; ok {
		return release, nil
	}
	return dependency.Release{}, &dependency.RemoteError{StatusCode: 404, Err: fmt.Errorf("tag %s not found", tag)}
}

func (github *migrationGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	return "", errors.New("unexpected resolve call")
}

func (github *migrationGitHub) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	return nil, errors.New("unexpected download call")
}

func (github *migrationGitHub) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("unexpected asset call")
}

func TestTesslPinResolvesToTag(t *testing.T) {
	for _, test := range []struct {
		name     string
		releases map[string]dependency.Release
		want     string
		code     string
	}{
		{name: "plain", releases: map[string]dependency.Release{"1.2.3": {ID: 1, Tag: "1.2.3"}}, want: "1.2.3"},
		{name: "v-prefixed", releases: map[string]dependency.Release{"v1.2.3": {ID: 2, Tag: "v1.2.3"}}, want: "v1.2.3"},
		{name: "absent", releases: map[string]dependency.Release{}, code: "tessl_version_unavailable"},
		{name: "ambiguous", releases: map[string]dependency.Release{"1.2.3": {ID: 1, Tag: "1.2.3"}, "v1.2.3": {ID: 2, Tag: "v1.2.3"}}, code: "ambiguous_tessl_version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			github := &migrationGitHub{releases: test.releases}
			service := newService(github)
			got, _, _, _, err := service.resolveMapping(context.Background(), dependency.State{}, migrate.Mapping{
				From: "example/pkg", Source: "github:example/pkg", Requested: "1.2.3", TesslVersion: "1.2.3",
			})
			if test.code == "" {
				if err != nil || got != test.want {
					t.Fatalf("resolveMapping() = %q, %v, want %q", got, err, test.want)
				}
			} else {
				var migrationErr *Error
				if !errors.As(err, &migrationErr) || migrationErr.Code != test.code {
					t.Fatalf("error = %#v, want code %s", err, test.code)
				}
			}
			if len(github.calls) != 2 || github.calls[0] != "1.2.3" || github.calls[1] != "v1.2.3" {
				t.Fatalf("tag calls = %#v", github.calls)
			}
		})
	}
}

func TestCompatibleProjectStateRejectsDisagreement(t *testing.T) {
	existing := dependency.State{
		Project: dependency.Project{Agents: []string{"codex"}, Dependencies: []dependency.Declaration{{Source: "github:one/pkg", Requested: "latest"}}},
		Lock:    dependency.Lockfile{Dependencies: []dependency.LockedDependency{{Source: "github:one/pkg", Requested: "latest"}}},
	}
	desired := dependency.State{
		Project: dependency.Project{Agents: []string{"cursor"}, Dependencies: []dependency.Declaration{{Source: "github:two/pkg", Requested: "latest"}}},
	}
	var migrationErr *Error
	if err := compatibleProjectState(existing, desired); !errors.As(err, &migrationErr) || migrationErr.Code != "project_state_conflict" {
		t.Fatalf("error = %#v", err)
	}
}

func TestFinalizationGateConjunction(t *testing.T) {
	base := migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "rule", Kind: "rule"}}}}}
	if !finalizationReady(base, nil) {
		t.Fatal("empty gate should be ready")
	}
	tests := map[string]struct {
		inventory migrate.Report
		diffs     []migrate.EffectiveDiff
	}{
		"effective-diff":       {inventory: base, diffs: []migrate.EffectiveDiff{{Reason: migrate.DiffBody}}},
		"lossy":                {inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{Lossy: []string{"description"}}}}}}},
		"ambiguous":            {inventory: migrate.Report{Ambiguous: []migrate.PathRecord{{Path: "AGENTS.md"}}}},
		"ambiguous-artifact":   {inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{Classification: "ambiguous"}}}}}},
		"unsupported":          {inventory: migrate.Report{Unsupported: []migrate.PathRecord{{Path: ".mcp.json"}}}},
		"unsupported-artifact": {inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{Classification: "unsupported"}}}}}},
		"uncovered-agent":      {inventory: migrate.Report{Agents: []migrate.AgentCoverage{{ID: "gemini", Covered: false}}}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if finalizationReady(test.inventory, test.diffs) {
				t.Fatalf("%s unexpectedly passed", name)
			}
		})
	}
}

func TestHandEditedTesslNativeBlocksFinalization(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, string)
	}{
		{
			name: "cursor rule remainder",
			edit: func(t *testing.T, project string) {
				source, err := os.ReadFile(filepath.Join(project, ".tessl/plugins/example/alpha/rules/always-rule.md"))
				if err != nil {
					t.Fatal(err)
				}
				native := append([]byte("---\nalwaysApply: true\n---\n\n"), source...)
				native = append(native, []byte("hand-edited\n")...)
				writeFile(t, project, ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc", native, 0o644)
			},
		},
		{
			name: "copied skill",
			edit: func(t *testing.T, project string) {
				native := filepath.Join(project, ".claude/skills/tessl__review-change")
				if err := os.Remove(native); err != nil {
					t.Fatal(err)
				}
				writeFile(t, project, ".claude/skills/tessl__review-change/SKILL.md", []byte("# Hand edited\n"), 0o644)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := seedConsumer(t)
			test.edit(t, project)
			before := hashTree(t, project)
			github := &integrationGitHub{
				release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("f", 40), archive: migrationPackageArchive(t),
			}
			mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
			if err != nil {
				t.Fatal(err)
			}
			service := newService(github)
			report, err := service.Migrate(context.Background(), project, Options{DryRun: true, CLIMappings: mappings})
			if err != nil {
				t.Fatal(err)
			}
			if report.FinalizationReady || len(report.EffectiveDiffs) != 0 || !hasNote(report.Notes, "ambiguous") {
				t.Fatalf("report = %#v, want an artifact-level ambiguity without an effective diff", report)
			}

			application := &Application{service: service, fallback: cli.UnavailableApplication{}}
			_, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--finalize", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest")
			if exitCode != cli.ExitConflict || !strings.Contains(stderr, `"code":"finalization_blocked"`) {
				t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
			}
			if after := hashTree(t, project); !mapsEqual(before, after) {
				t.Fatalf("blocked finalization changed hand-edited Tessl bytes\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func hasNote(notes []migrate.CoexistenceNote, code string) bool {
	for _, note := range notes {
		if note.Code == code {
			return true
		}
	}
	return false
}

func TestCoexistenceKeepsTesslBytes(t *testing.T) {
	project := seedConsumer(t)
	before := tesslSurface(t, project)
	commit := strings.Repeat("a", 40)
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: commit,
		archive: migrationPackageArchive(t),
	}
	mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
	if err != nil {
		t.Fatal(err)
	}
	service := newService(github)
	report, err := service.Migrate(context.Background(), project, Options{CLIMappings: mappings})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Wrote || report.DryRun || len(report.EffectiveDiffs) != 0 {
		t.Fatalf("first report = %#v", report)
	}
	foundNative := false
	for _, operation := range report.Plan.Operations {
		if strings.Contains(operation.Path, "acr__") {
			foundNative = true
		}
	}
	if !foundNative {
		t.Fatalf("apply report has no ACR native: %#v", report.Plan.Operations)
	}
	after := tesslSurface(t, project)
	if !mapsEqual(before, after) {
		t.Fatalf("Tessl surface changed\nbefore=%v\nafter=%v", before, after)
	}
	projectBytes, err := os.ReadFile(filepath.Join(project, dependency.ProjectFilename))
	if err != nil {
		t.Fatal(err)
	}
	lockBytes, err := os.ReadFile(filepath.Join(project, dependency.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	latestCalls, resolveCalls := github.latestCalls, github.resolveCalls
	second, err := service.Migrate(context.Background(), project, Options{CLIMappings: mappings})
	if err != nil {
		t.Fatal(err)
	}
	if second.Wrote || github.latestCalls != latestCalls || github.resolveCalls != resolveCalls {
		t.Fatalf("second apply = %#v, mutable resolution calls latest=%d resolve=%d", second, github.latestCalls-latestCalls, github.resolveCalls-resolveCalls)
	}
	assertFileBytes(t, filepath.Join(project, dependency.ProjectFilename), projectBytes)
	assertFileBytes(t, filepath.Join(project, dependency.LockFilename), lockBytes)
}

func TestAcrCodexTableAppendsAfterTesslTrustIndices(t *testing.T) {
	project := seedConsumer(t)
	configPath := filepath.Join(project, ".codex", "config.toml")
	trustKey := filepath.ToSlash(configPath) + ":session_start:0:0"
	tessl := "[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"tessl hook run --plugin-path='.tessl/plugins/example/alpha' --event='SessionStart' --agent=codex --schema-version=1\"\n"
	writeFile(t, project, ".codex/config.toml", []byte(tessl+"\n[hooks.state.\""+trustKey+"\"]\nlast_run = 123\n"), 0o644)

	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newService(github).Migrate(context.Background(), project, Options{CLIMappings: mappings}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	trust := strings.Index(text, "[hooks.state.\""+trustKey+"\"]")
	acr := strings.Index(text, "acr__")
	if trust < 0 || acr <= trust || strings.Count(text, "[[hooks.SessionStart]]") < 2 {
		t.Fatalf("Codex hook order = %s", text)
	}
	if !strings.Contains(text, "[hooks.state.\""+trustKey+"\"]\nlast_run = 123") {
		t.Fatalf("Tessl trust index changed: %s", text)
	}
}

func TestTesslOwnershipIsReinventoriedPerRun(t *testing.T) {
	project := seedConsumer(t)
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40), archive: migrationPackageArchive(t),
	}
	mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
	if err != nil {
		t.Fatal(err)
	}
	service := newService(github)
	first, err := service.Migrate(context.Background(), project, Options{CLIMappings: mappings})
	if err != nil {
		t.Fatal(err)
	}
	newNative := ".claude/skills/tessl__new-skill"
	if ownershipNames(first.TesslOwned, newNative) {
		t.Fatalf("first inventory already contains %s", newNative)
	}

	writeJSON(t, project, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{
		"name": "example/alpha", "version": "1.0.0",
		"rules":  []string{"rules/always-rule.md"},
		"skills": []string{"skills/review-change", "skills/new-skill"},
		"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
		}}}}},
	})
	writeFile(t, project, ".tessl/plugins/example/alpha/skills/new-skill/SKILL.md", []byte("# New\n"), 0o644)
	target := filepath.Join(project, ".tessl/plugins/example/alpha/skills/new-skill")
	link := filepath.Join(project, filepath.FromSlash(newNative))
	relative, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, link); err != nil {
		t.Fatal(err)
	}

	second, err := service.Migrate(context.Background(), project, Options{DryRun: true, CLIMappings: mappings})
	if err != nil {
		t.Fatal(err)
	}
	if !ownershipNames(second.TesslOwned, newNative) || ownershipNames(first.TesslOwned, newNative) {
		t.Fatalf("ownership was not reinventoried: first=%#v second=%#v", first.TesslOwned, second.TesslOwned)
	}
}

func ownershipNames(records []migrate.OwnershipRecord, path string) bool {
	for _, record := range records {
		if record.Path == path {
			return true
		}
	}
	return false
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	project := seedConsumer(t)
	before := hashTree(t, project)
	github := &integrationGitHub{release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("c", 40), archive: migrationPackageArchive(t)}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--dry-run", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"dryRun":true`) || !strings.Contains(stdout, `"wrote":false`) {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("dry-run changed tree\nbefore=%v\nafter=%v", before, after)
	}
}

func TestFinalizeWhenEquivalentIsNotImplemented(t *testing.T) {
	project := seedConsumer(t)
	github := &integrationGitHub{release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("d", 40), archive: migrationPackageArchive(t)}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	_, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--finalize", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest")
	if exitCode != cli.ExitOperational || !strings.Contains(stderr, `"code":"not_implemented"`) {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
	}
	if _, err := os.Stat(filepath.Join(project, dependency.ProjectFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finalize wrote project state: %v", err)
	}
}

func TestEffectiveConfigDiffBlocksFinalization(t *testing.T) {
	project := seedConsumer(t)
	github := &integrationGitHub{release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("e", 40), archive: migrationPackageArchiveWithRule(t, "# Different\n")}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--dry-run", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest")
	if exitCode != cli.ExitSuccess || stderr != "" || !strings.Contains(stdout, `"reason":"body"`) || !strings.Contains(stdout, `"finalizationReady":false`) {
		t.Fatalf("dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	before := hashTree(t, project)
	_, stderr, exitCode = runCLI(t, application, "migrate", "tessl", "--finalize", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@latest")
	if exitCode != cli.ExitConflict || !strings.Contains(stderr, `"code":"finalization_blocked"`) {
		t.Fatalf("finalize exit = %d, stderr = %q", exitCode, stderr)
	}
	if after := hashTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("blocked finalize changed tree\nbefore=%v\nafter=%v", before, after)
	}
}

func TestOneUnmappedPackageBlocksWholeMigration(t *testing.T) {
	project := seedConsumer(t)
	before := hashTree(t, project)
	github := &integrationGitHub{}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	_, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--dry-run", "--json", "--project", project)
	if exitCode != cli.ExitOperational || !strings.Contains(stderr, `"code":"unmapped_package"`) || !strings.Contains(stderr, "example/alpha") {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
	}
	if github.latestCalls != 0 || github.resolveCalls != 0 || github.downloadCalls != 0 {
		t.Fatalf("unmapped package reached GitHub: %#v", github)
	}
	if after := hashTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("unmapped migration changed tree\nbefore=%v\nafter=%v", before, after)
	}
}

func TestMappingConflictWritesNothing(t *testing.T) {
	project := seedConsumer(t)
	writeFile(t, project, "mapping.yaml", []byte("schemaVersion: 1\npackages:\n  - from: example/alpha\n    source: github:one/alpha\n  - from: example/alpha\n    source: github:two/alpha\n"), 0o644)
	before := hashTree(t, project)
	github := &integrationGitHub{}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	_, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--dry-run", "--json", "--project", project, "--mapping-file", "mapping.yaml")
	if exitCode != cli.ExitOperational || !strings.Contains(stderr, `"code":"mapping_conflict"`) {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
	}
	if github.latestCalls != 0 || github.resolveCalls != 0 || github.downloadCalls != 0 {
		t.Fatalf("mapping conflict reached GitHub: %#v", github)
	}
	if after := hashTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("mapping conflict changed tree\nbefore=%v\nafter=%v", before, after)
	}
}

func TestDuplicateEffectWarningNamesBothCommands(t *testing.T) {
	report := migrate.MigrationReport{DryRun: true, Notes: []migrate.CoexistenceNote{{
		Code: "duplicate-effect", Event: "session-start", Tessl: "tessl hook run --event=session-start", ACR: ".claude/hooks/acr__example__alpha__session-start/session-start.sh",
	}}}
	text := migrate.FormatCoexistenceText(report)
	want := "WARNING duplicate-effect session-start: tessl hook run --event=session-start + .claude/hooks/acr__example__alpha__session-start/session-start.sh"
	if !strings.Contains(text, want) {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

func TestTesslPluginPathNativeHookIsTesslOwned(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, ".tessl/plugins/example/alpha/hooks/stop.sh", []byte("#!/bin/sh\n"), 0o755)
	inventory := migrate.Report{Packages: []migrate.PackageReport{{TesslIdentity: "example/alpha"}}}
	owned := []byte(`{"hooks":{"Stop":[{"command":"bash","args":["${CLAUDE_PROJECT_DIR}/.tessl/plugins/example/alpha/hooks/stop.sh"]}]}}`)
	if !pluginPathHook(owned, project, inventory) {
		t.Fatal("live plugin path hook was not claimed")
	}
	codex := []byte("[[hooks.Stop]]\ncommand = 'bash \".tessl/plugins/example/alpha/hooks/stop.sh\"'\n")
	if !pluginPathHook(codex, project, inventory) {
		t.Fatal("Codex plugin path hook was not claimed")
	}
	user := []byte(`{"hooks":{"Stop":[{"command":"./scripts/notify.sh --state .tessl/state.json"}]}}`)
	if pluginPathHook(user, project, inventory) {
		t.Fatal("unrelated user hook was claimed")
	}
	pluginArgument := []byte(`{"hooks":{"Stop":[{"command":"./scripts/notify.sh","args":["--reference",".tessl/plugins/example/alpha/hooks/stop.sh"]}]}}`)
	if pluginPathHook(pluginArgument, project, inventory) {
		t.Fatal("user hook argument naming a plugin script was claimed as the executable")
	}
}

func TestPluginTreeWithoutManifestIsNotTesslOwned(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, ".tessl/plugins/example/alpha/hooks/stop.sh", []byte("#!/bin/sh\n"), 0o755)
	content := []byte(`{"command":"bash","args":[".tessl/plugins/example/alpha/hooks/stop.sh"]}`)
	if pluginPathHook(content, project, migrate.Report{}) {
		t.Fatal("plugin tree without live tessl.json inventory was claimed")
	}
	_, err := newService(&migrationGitHub{}).Migrate(context.Background(), project, Options{})
	var migrationErr *Error
	if !errors.As(err, &migrationErr) || migrationErr.Code != "tessl_manifest_absent" {
		t.Fatalf("error = %#v", err)
	}
}

func TestGitignoredLockfileIsReported(t *testing.T) {
	project := t.TempDir()
	command := exec.Command("git", "init", "-q")
	command.Dir = project
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	writeFile(t, project, ".gitignore", []byte(".agents/\n"), 0o644)
	report := migrate.MigrationReport{}
	if err := addGitignoreNotes(project, &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range report.Notes {
		if note.Code == "gitignored_state" && note.Path == dependency.LockFilename && strings.HasSuffix(note.IgnoredBy, ".gitignore:1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %#v", report.Notes)
	}
}

func TestCoexistenceReportingReturnsUnexpectedReadErrors(t *testing.T) {
	t.Run("Tessl host", func(t *testing.T) {
		project := t.TempDir()
		if err := os.Mkdir(filepath.Join(project, "AGENTS.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := addTesslHostOwnership(project, migrate.Report{}, &migrate.MigrationReport{})
		if err == nil || !strings.Contains(err.Error(), "AGENTS.md") {
			t.Fatalf("error = %v, want AGENTS.md read failure", err)
		}
	})

	t.Run("Git check-ignore", func(t *testing.T) {
		project := t.TempDir()
		if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := addGitignoreNotes(project, &migrate.MigrationReport{})
		if err == nil || !strings.Contains(err.Error(), "Git ignore status") {
			t.Fatalf("error = %v, want git check-ignore failure", err)
		}
	})
}

func TestSourceNotAPackageIsNamed(t *testing.T) {
	cause := errors.New("validate downloaded github:example/pkg package: open agent-plugin.yaml: file does not exist")
	var migrationErr *Error
	err := classifyResolutionError("github:example/pkg", cause)
	if !errors.As(err, &migrationErr) || migrationErr.Code != "source_not_a_package" || !strings.Contains(migrationErr.Message, "#11") || !strings.Contains(migrationErr.Message, "#9") {
		t.Fatalf("error = %#v", err)
	}
}

func TestUnclaimedNativeSiblingsAreNeverPlannedOrWritten(t *testing.T) {
	project := seedConsumer(t)
	writeFile(t, project, ".claude/skills/operator-skill/SKILL.md", []byte("# Operator\n"), 0o640)
	writeFile(t, project, ".cursor/rules/operator.mdc", []byte("operator\n"), 0o600)
	operatorSkill := filepath.Join(project, ".claude/skills/operator-skill/SKILL.md")
	operatorRule := filepath.Join(project, ".cursor/rules/operator.mdc")
	beforeSkill, err := os.ReadFile(operatorSkill)
	if err != nil {
		t.Fatal(err)
	}
	beforeRule, err := os.ReadFile(operatorRule)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	github := &integrationGitHub{release: dependency.Release{ID: 7, Tag: "v1.0.0"}, commit: commit, archive: migrationPackageArchive(t)}
	mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@latest"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := newService(github).Migrate(context.Background(), project, Options{CLIMappings: mappings})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range report.Plan.Operations {
		if operation.Path == ".claude/skills/operator-skill/SKILL.md" || operation.Path == ".cursor/rules/operator.mdc" {
			t.Fatalf("unclaimed sibling was planned: %#v", operation)
		}
	}
	assertFileBytes(t, operatorSkill, beforeSkill)
	assertFileBytes(t, operatorRule, beforeRule)
}

func TestReportOwnershipPartitionIsExhaustive(t *testing.T) {
	report := migrate.MigrationReport{
		ToolOwned:  []migrate.OwnershipRecord{{Path: "AGENTS.md", Kind: "managed-span", ID: "acr"}},
		TesslOwned: []migrate.OwnershipRecord{{Path: "AGENTS.md", Kind: "managed-span", ID: "tessl-managed"}},
		Unmanaged:  []migrate.OwnershipRecord{{Path: "AGENTS.md", Kind: "fragment", ID: "prefix"}},
	}
	if err := validateOwnershipPartition(report); err != nil {
		t.Fatal(err)
	}
	report.Unmanaged = append(report.Unmanaged, report.ToolOwned[0])
	if err := validateOwnershipPartition(report); err == nil {
		t.Fatal("duplicate surface member was accepted in two ownership buckets")
	}
}

func migrationPackageArchive(t *testing.T) []byte {
	return migrationPackageArchiveWithRule(t, "# Always\n")
}

func migrationPackageArchiveWithRule(t *testing.T, ruleBody string) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n    - id: always-rule\n      path: rules/always-rule.md\n      activation:\n        mode: always\n  skills:\n    - id: review-change\n      path: skills/review-change\n  hooks:\n    - id: session-start\n      event: session-start\n      path: hooks/session-start.sh\n"
	files := map[string]struct {
		content string
		mode    int64
	}{
		"agent-plugin.yaml":             {manifest, 0o644},
		"rules/always-rule.md":          {"---\nalwaysApply: true\n---\n" + ruleBody, 0o644},
		"skills/review-change/SKILL.md": {"# Review\n", 0o644},
		"hooks/session-start.sh":        {"#!/bin/sh\necho start\n", 0o755},
	}
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, file := range files {
		data := []byte(file.content)
		header := &tar.Header{Name: "example-alpha-commit/" + name, Mode: file.mode, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func tesslSurface(t *testing.T, root string) map[string]string {
	t.Helper()
	all := hashTree(t, root)
	result := make(map[string]string)
	for name, digest := range all {
		if name == "tessl.json" || strings.HasPrefix(name, ".tessl/") || strings.Contains(name, "/tessl__") {
			result[name] = digest
		}
	}
	return result
}

func assertFileBytes(t *testing.T, filename string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed\ngot=%q\nwant=%q", filename, got, want)
	}
}
