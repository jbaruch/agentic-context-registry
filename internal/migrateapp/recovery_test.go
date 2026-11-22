package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

func TestCoverageRecoveryPreservesTheInstallation(t *testing.T) {
	for _, test := range []struct {
		name         string
		inventory    migrate.Report
		code, remedy string
	}{
		{name: "uncovered agent", inventory: migrate.Report{Agents: []migrate.AgentCoverage{{ID: "gemini"}}}, code: "uncovered-agent",
			remedy: "if you no longer use this package, remove its Tessl-generated native output outside the Tessl installation, then rerun acr migrate tessl --finalize; otherwise keep Tessl installed and do not finalize"},
		{name: "unsupported artifact", inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "check", Kind: "hook", Classification: "unsupported"}}}}}, code: "unsupported-artifact",
			remedy: "keep Tessl installed and do not finalize; to finalize, remove this unsupported artifact in the source package, reinstall or update through Tessl, then rerun acr migrate tessl --finalize"},
		{name: "lossy artifact", inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "check", Kind: "rule", Lossy: []string{"description"}}}}}}, code: "lossy-artifact",
			remedy: "keep Tessl installed and do not finalize; to finalize, remove this lossy artifact in the source package, reinstall or update through Tessl, then rerun acr migrate tessl --finalize"},
		{name: "ambiguous artifact", inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "check", Kind: "rule", Classification: "ambiguous"}}}}}, code: "ambiguous-artifact",
			remedy: "resolve the reported ambiguity in the source package or its native output, reinstall or regenerate through Tessl as needed, then rerun acr migrate tessl --finalize"},
	} {
		t.Run(test.name, func(t *testing.T) {
			blockers := coverageBlockers(test.inventory, nil, migrate.AcceptedSet{})
			if len(blockers) != 1 || blockers[0].Code != test.code || blockers[0].Remedy != test.remedy {
				t.Fatalf("blockers=%+v want %s remedy=%q", blockers, test.code, test.remedy)
			}
			if strings.Contains(blockers[0].Remedy, ".tessl/") {
				t.Fatal("remedy targets installed files")
			}
		})
	}
}

func TestNativeEntryRecoveryNamesTheEntry(t *testing.T) {
	const path = ".agents/skills/review"
	const remedy = "remove this native entry or repoint it to a retained skill tree outside the Tessl installation, then rerun acr migrate tessl --finalize"
	_, _, blockers, err := sharedSurfacePlan(nil, migrate.Report{SharedSkills: []migrate.SharedSkillEntry{{Path: path, Disposition: migrate.SharedSkillBlocked}}}, realize.Ledger{})
	if err != nil || len(blockers) != 1 || blockers[0].Remedy != remedy {
		t.Fatalf("blockers=%+v err=%v", blockers, err)
	}
	entry := retainedDependency{path: path, kind: "skill", target: ".tessl/plugins/example/alpha/skills/review"}
	if blocker := danglingBlocker(entry, "the installed tree"); blocker.Remedy != remedy {
		t.Fatalf("dangling recovery=%+v", blocker)
	}
	if blocker := unprovenBlocker(entry, "inspection failed"); blocker.Remedy != "repoint this native entry at an inspectable retained tree outside the Tessl installation without unowned links, or remove that entry, then rerun acr migrate tessl --finalize" {
		t.Fatalf("unproven recovery=%+v", blocker)
	}
}

func TestTesslDirectoryDepthRecovery(t *testing.T) {
	root := t.TempDir()
	directory := ".tessl" + strings.Repeat("/level", 65)
	if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
		t.Fatal(err)
	}
	before := hashTreeWithModes(t, root)
	project, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	_, err = prunedTesslDirectories(project, nil)
	want := fmt.Sprintf("%s nests deeper than 64 directories; shorten the directory structure in the source package and reinstall or update it through Tessl, or remove the affected package through Tessl; then rerun acr migrate tessl --finalize", `"`+directory[:128]+`...[truncated]"`)
	if err == nil || err.Error() != want {
		t.Fatalf("depth recovery=%v want=%q", err, want)
	}
	if !mapsEqual(before, hashTreeWithModes(t, root)) {
		t.Fatal("depth refusal changed installation")
	}
}

func TestDiagnosticCause(t *testing.T) {
	for _, test := range []struct{ name, value, want string }{
		{"empty", "", ""},
		{"printable", `path "skills/normal" is invalid; C:\normal café 😀`, `path "skills/normal" is invalid; C:\normal café 😀`},
		{"controls", "\x00\t\r\n\x1b\u2028\u202e\U000e0001", `\u0000\u0009\u000d\u000a\u001b\u2028\u202e\U000e0001`},
		{"invalid UTF-8", "bad\xfftext", "bad�text"},
		{"exact limit", strings.Repeat("é", 64), strings.Repeat("é", 64)},
		{"rune crosses limit", strings.Repeat("x", 127) + "é", strings.Repeat("x", 127) + "é"},
		{"rune at limit", strings.Repeat("x", 124) + "😀!", strings.Repeat("x", 124) + "😀!"},
		{"1MiB", strings.Repeat("x", 1<<20), strings.Repeat("x", 1<<20)},
		{"long composed explanation", strings.Repeat("complete explanation; ", 20) + "check network access and retry", strings.Repeat("complete explanation; ", 20) + "check network access and retry"},
		{"escaped limit", strings.Repeat("\x00", 129), strings.Repeat(`\u0000`, 129)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := diagnostic.Escape(test.value)
			if got != test.want || !utf8.ValidString(got) || strings.ContainsAny(got, "\x00\t\r\n\x1b") {
				t.Fatalf("cause=%q want=%q", got, test.want)
			}
		})
	}
}

func TestMigrateFallbackEscapesCompleteGuidance(t *testing.T) {
	guidance := strings.Repeat("check the published package; ", 700) + "then rerun migration"
	cause := errors.New("refused\n\x1b\u202e")
	err := fmt.Errorf("verify package: %w; %s", cause, guidance)
	got := migrateCLIError(err)
	var cliErr *cli.Error
	want := `verify package: refused\u000a\u001b\u202e; ` + guidance
	if !errors.As(got, &cliErr) || cliErr.Message != want || !errors.Is(got, cause) {
		t.Fatalf("fallback lost guidance or cause: %.200q", got)
	}
}

func TestRecoveryNaturalDiagnostics(t *testing.T) {
	const remedy = "fix the reported problem in the source package, ask its publisher if needed, reinstall or update through Tessl, then rerun migration; alternatively, map the package to a published ACR source with --map FROM=github:owner/repository[@TAG]"
	for _, test := range []struct {
		name, want string
		cause      error
	}{
		{"missing path", `declared plugin "skill" path "skills/normal" does not exist`, &tesslplugin.DeclaredPathError{Kind: "skill", Path: "skills/normal"}},
		{"validation", `manifest validation failed: invalid_path at skills[0].path: path "skills/normal" is invalid`, &manifest.ValidationErrors{Issues: []manifest.ValidationError{{Code: manifest.CodeInvalidPath, Field: "skills[0].path", Message: `path "skills/normal" is invalid`}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := classifyVendorError(test.cause)
			var diagnostic *Error
			want := test.want + "; " + remedy
			if !errors.As(err, &diagnostic) || !errors.Is(err, test.cause) || diagnostic.Message != want || diagnostic.Remedy != remedy {
				t.Fatalf("diagnostic=%v want=%q", err, want)
			}
			app := cli.ApplicationFunc(func(context.Context, cli.Invocation) (cli.Result, error) {
				return cli.Result{}, migrateCLIError(err)
			})
			for _, jsonOutput := range []bool{false, true} {
				args := []string{"migrate", "tessl", "--project", t.TempDir()}
				if jsonOutput {
					args = append(args, "--json")
				}
				stdout, stderr, exit := runCLI(t, app, args...)
				message := strings.TrimSuffix(strings.TrimPrefix(stderr, "acr migrate: "), "\n")
				if jsonOutput {
					var envelope migrateEnvelope
					if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
						t.Fatal(err)
					}
					message = envelope.Error.Message
				}
				if exit != cli.ExitOperational || stdout != "" || message != want || strings.Contains(message, `\`) {
					t.Fatalf("exit=%d message=%q want=%q", exit, message, want)
				}
			}
		})
	}
}

func TestRecoveryDiagnosticTokens(t *testing.T) {
	for _, input := range []struct{ name, value string }{
		{"controls", "review\nFAKE-TRUSTED-GUIDANCE\x1b[2J\u202e"},
		{"1MiB", strings.Repeat("x", 1<<20)},
	} {
		t.Run(input.name, func(t *testing.T) {
			validation := manifest.ValidateArtifacts(fstest.MapFS{}, manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{ID: "hook", Event: manifest.HookSessionStart, Path: input.value}}}})
			for _, cause := range []error{
				&tesslplugin.DeclaredPathError{Kind: "skill", Path: input.value},
				validation,
			} {
				err := classifyVendorError(cause)
				var diagnostic *Error
				if !errors.As(err, &diagnostic) || !errors.Is(err, cause) || len(diagnostic.Message) > 1024 || strings.ContainsAny(diagnostic.Message, "\n\x1b\u202e") {
					t.Fatal("vendor diagnostic was not bounded/escaped or lost its cause")
				}
			}
			root := t.TempDir()
			_, err := readMappingFile(root, input.value)
			var pathErr *os.PathError
			var diagnostic *Error
			if !errors.As(err, &pathErr) || !errors.As(err, &diagnostic) || diagnostic.Code != "mapping_file_invalid" || !strings.Contains(diagnostic.Message, "then rerun with the same --mapping-file value") || len(diagnostic.Message) > 2048 || strings.ContainsAny(diagnostic.Message, "\n\x1b\u202e") {
				t.Fatal("mapping read diagnostic was not bounded/escaped or lost its path error")
			}
			content, err := json.Marshal(map[string]any{"schemaVersion": 1, "packages": []map[string]string{{"from": input.value, "source": "github:example/alpha"}}})
			if err != nil {
				t.Fatal(err)
			}
			writeMapping(t, root, string(content))
			_, err = readMappingFile(root, "mapping.yaml")
			if !errors.As(err, &diagnostic) || diagnostic.Code != "mapping_file_invalid" || !errors.Is(err, diagnostic.Cause) || diagnostic.Message != "invalid mapping-file mapping package "+migrate.DiagnosticToken(input.value)+"; use a workspace/package identity; correct the mapping file to use schemaVersion: 1 with a packages list, workspace/package identities, and valid github:owner/repository sources, then rerun with the same --mapping-file value" || len(diagnostic.Message) > 1024 || strings.ContainsAny(diagnostic.Message, "\n\x1b\u202e") {
				t.Fatal("mapping decode diagnostic was not bounded/escaped or lost its cause")
			}
			for _, withMapping := range []bool{false, true} {
				source := "github:example/alpha"
				existing := []dependency.Declaration{{Source: source, Requested: input.value}}
				desired := []dependency.Declaration{{Source: source, Requested: "latest"}}
				var mappings []migrate.Mapping
				if withMapping {
					mappings = []migrate.Mapping{{From: input.value, Source: source, Requested: "latest"}}
				}
				err := declarationConflict(existing, desired, mappings)
				if !errors.As(err, &diagnostic) || !strings.Contains(diagnostic.Message, "requested "+migrate.DiagnosticToken(input.value)) || len(diagnostic.Message) > 2048 || strings.ContainsAny(diagnostic.Message, "\n\x1b\u202e") {
					t.Fatal("declaration conflict diagnostic was not bounded/escaped")
				}
				if withMapping && diagnostic.Remedy != "to keep the existing request, rerun acr migrate tessl with a --map value using the original Tessl package identity, the complete source from the conflicting agents.yaml dependency, and that dependency’s existing requested value; keep the other original migration options" {
					t.Fatal("mapping override must refer to original values")
				}
			}
		})
	}
}

func TestReleaseFailureDiagnosticTokens(t *testing.T) {
	for _, input := range []struct{ name, value string }{
		{"controls", "review\nFAKE-TRUSTED-GUIDANCE\x1b[2J\u202e"},
		{"1MiB", strings.Repeat("x", 1<<20)},
	} {
		for _, stable := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stable-%t/explicit-%t", input.name, stable, explicit), func(t *testing.T) {
					transport := &releaselessGitHub{statuses: map[string]int{alphaLatest: 404, alphaRepo: 200}, bodies: map[string]string{alphaRepo: alphaReadable}}
					if stable {
						transport.statuses[alphaLatest] = 200
						transport.bodies[alphaLatest] = alphaStable
					}
					app := productionMigrateApplication(t, transport)
					mapping := migrate.Mapping{From: input.value, Source: "github:example/alpha", Explicit: explicit}
					cause := &dependency.RemoteError{StatusCode: 404, Err: errors.New("missing release")}
					err := app.service.classifyReleaseFailure(context.Background(), mapping, input.value, "migrate_failed", "lookup failed", cause)
					var diagnostic *Error
					if !errors.As(err, &diagnostic) || !errors.Is(err, cause) || len(diagnostic.Message) > 4096 || strings.ContainsAny(diagnostic.Message, "\n\x1b\u202e") {
						t.Fatal("release diagnostic was not bounded/escaped or lost its cause")
					}
					if stable {
						want := "select an available release tag, then rerun acr migrate tessl with a --map value for this Tessl package using the same original FROM and SOURCE values and that tag"
						if diagnostic.Remedy != want {
							t.Fatal("release selection must refer to original values")
						}
					} else if diagnostic.Code != cli.CodeSourceNotPublished || !strings.Contains(diagnostic.Message, migrate.DiagnosticToken(input.value)) || (!explicit && !strings.Contains(diagnostic.Message, migrate.DiagnosticToken("v"+input.value))) {
						t.Fatal("requested/prefixed tags were not rendered individually")
					}
				})
			}
		}
	}
}

// Long operands pass the actual identity, source and requested-tag parsers;
// the recovery instructions must refer back to them without copying display tokens.
func TestLongAcceptedOperandsUseReferenceRecovery(t *testing.T) {
	from := "workspace/" + strings.Repeat("package", 40)
	source := "github:example/" + strings.Repeat("repository", 30)
	tag := "release-" + strings.Repeat("tag", 100)
	mapping, err := migrate.ParseInlineMapping(from + "=" + source + "@" + tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dependency.ParseSource(mapping.Source); err != nil {
		t.Fatal(err)
	}
	repositoryPath := "/repos/example/" + strings.Repeat("repository", 30)
	const selection = "select an available release tag, then rerun acr migrate tessl with a --map value for this Tessl package using the same original FROM and SOURCE values and that tag"
	const keepRequest = "to keep the existing request, rerun acr migrate tessl with a --map value using the original Tessl package identity, the complete source from the conflicting agents.yaml dependency, and that dependency’s existing requested value; keep the other original migration options"
	const adoptMapping = "to adopt the mapping, edit the conflicting agents.yaml dependency: set requested to the original requested value selected by the Tessl mapping and remove its hold; remove only that source's dependency entry from .agents/registry.lock, then rerun 'acr migrate tessl' with the same options"
	const uninstall = "if removing this dependency is intended, run acr uninstall using the complete source value from the conflicting agents.yaml dependency, then retry the migration with the same options"
	transport := &releaselessGitHub{statuses: map[string]int{repositoryPath + "/releases/tags/" + tag: 404, repositoryPath + "/releases/latest": 200}, bodies: map[string]string{repositoryPath + "/releases/latest": alphaStable}}
	app := productionMigrateApplication(t, transport)
	resolver := dependency.NewResolver(app.service.github)
	_, cause := resolver.Candidate(context.Background(), dependency.Declaration{Source: source, Requested: tag})
	if !dependency.IsGitHubStatus(cause, 404) {
		t.Fatalf("long requested tag was not accepted: %v", cause)
	}
	explanation := diagnostic.Escape(cause.Error())
	var diagnostic *Error
	err = app.service.classifyReleaseFailure(context.Background(), mapping, tag, "migrate_failed", explanation, cause)
	if !errors.As(err, &diagnostic) || !errors.Is(err, cause) || diagnostic.Remedy != selection || !strings.Contains(diagnostic.Message, "...[truncated]") || len(diagnostic.Message) > 1500 {
		t.Fatal("stable-release recovery lost its cause, bounded evidence or original-value instruction")
	}
	// Both exact tags exist; this also exercises a manifest-derived, tagless mapping.
	body, err := json.Marshal(map[string]any{"id": 42, "tag_name": tag})
	if err != nil {
		t.Fatal(err)
	}
	prefixedBody, err := json.Marshal(map[string]any{"id": 43, "tag_name": "v" + tag})
	if err != nil {
		t.Fatal(err)
	}
	transport.statuses[repositoryPath+"/releases/tags/"+tag] = 200
	transport.statuses[repositoryPath+"/releases/tags/v"+tag] = 200
	transport.bodies[repositoryPath+"/releases/tags/"+tag] = string(body)
	transport.bodies[repositoryPath+"/releases/tags/v"+tag] = string(prefixedBody)
	mapping.Explicit, mapping.TesslVersion = false, tag
	_, _, _, _, err = app.service.resolveMapping(context.Background(), dependency.State{}, mapping)
	if !errors.As(err, &diagnostic) || diagnostic.Code != cli.CodeAmbiguousTesslVersion || diagnostic.Remedy != selection || len(diagnostic.Message) > 1000 {
		t.Fatal("ambiguous-release recovery copied or lost long operands")
	}
	existing := []dependency.Declaration{{Source: source, Requested: tag, Hold: &dependency.Hold{Pin: "v1.0.0"}}}
	desired := []dependency.Declaration{{Source: source, Requested: tag + "-new"}}
	for _, test := range []struct {
		name     string
		desired  []dependency.Declaration
		mappings []migrate.Mapping
		want     string
	}{
		{"keep request", desired, []migrate.Mapping{mapping}, keepRequest},
		{"edit request", desired, nil, adoptMapping},
		{"uninstall", nil, nil, uninstall},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := declarationConflict(existing, test.desired, test.mappings)
			if !errors.As(err, &diagnostic) || diagnostic.Remedy != test.want || strings.Contains(diagnostic.Remedy, "...[truncated]") || strings.Contains(diagnostic.Remedy, tag) || len(diagnostic.Message) > 1400 {
				t.Fatal("declaration remedy must reference original operands with bounded display evidence")
			}
		})
	}
}

func TestHostileRemotePayloadPreservesFixedRecovery(t *testing.T) {
	for _, payload := range []struct{ name, value, token string }{
		{"controls", "remote\nforged\x1b[2J\u202e", `"remote\nforged\x1b[2J\u202e"`},
		{"long", strings.Repeat("x", 1024), `"` + strings.Repeat("x", 128) + `...[truncated]"`},
	} {
		for _, status := range []int{401, 403, 502} {
			t.Run(fmt.Sprintf("%s/%d", payload.name, status), func(t *testing.T) {
				body, err := json.Marshal(map[string]string{"message": payload.value})
				if err != nil {
					t.Fatal(err)
				}
				transport := &releaselessGitHub{statuses: map[string]int{alphaLatest: status}, bodies: map[string]string{alphaLatest: string(body)}}
				app := productionMigrateApplication(t, transport)
				resolver := dependency.NewResolver(app.service.github)
				_, cause := resolver.Candidate(context.Background(), dependency.Declaration{Source: "github:example/alpha", Requested: "latest"})
				want := `resolve latest release for "github:example/alpha": GitHub access denied: ` + payload.token + `; run 'gh auth login' or configure Git HTTPS credentials and retry`
				if status == 502 {
					want = `resolve latest release for "github:example/alpha": GitHub API returned "Bad Gateway": ` + payload.token + `; retry the request`
				}
				if cause == nil || cause.Error() != want || !dependency.IsGitHubStatus(cause, status) || strings.ContainsAny(want, "\n\x1b\u202e") {
					t.Fatalf("remote diagnostic = %v, want %s", cause, want)
				}
			})
		}
	}
}

func TestDownloadedLongPackageNameKeepsIdentityGuidance(t *testing.T) {
	name := "upstream/" + strings.Repeat("package", 40)
	remote := &integrationGitHub{archive: hostileArchive(t, name, map[string]string{"rules/always.md": "---\nalwaysApply: true\n---\n# Always\n"})}
	resolver := dependency.NewResolver(remote)
	_, _, cause := resolver.MaterializeLocked(context.Background(), dependency.LockedDependency{Source: "github:example/alpha", Commit: strings.Repeat("a", 40)})
	wantCause := `downloaded package identity "upstream/` + strings.Repeat("package", 17) + `...[truncated]" does not match "github:example/alpha"; fix agent-plugin.yaml and publish a new release`
	if cause == nil || cause.Error() != wantCause {
		t.Fatalf("downloaded identity diagnostic = %v, want %s", cause, wantCause)
	}
	err := classifyResolutionError("github:example/alpha", cause)
	var diagnostic *Error
	want := `"github:example/alpha" is not an ACR package: ` + wantCause + `; complete producer stage 0: run 'acr migrate tessl-plugin' in the producer repository, publish a stable release with 'acr publish', then re-run 'acr migrate tessl'`
	if !errors.As(err, &diagnostic) || !errors.Is(err, cause) || diagnostic.Message != want {
		t.Fatal("identity explanation or source-package recovery was cut off")
	}
}
