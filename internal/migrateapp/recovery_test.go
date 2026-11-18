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
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
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
			remedy: "if you no longer use \"gemini\", remove its Tessl-generated native output outside the Tessl installation, then rerun acr migrate tessl --finalize; otherwise keep Tessl installed and do not finalize"},
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
	const remedy = "remove the native entry \".agents/skills/review\" or repoint it to a retained skill tree outside the Tessl installation, then rerun acr migrate tessl --finalize"
	_, _, blockers, err := sharedSurfacePlan(nil, migrate.Report{SharedSkills: []migrate.SharedSkillEntry{{Path: path, Disposition: migrate.SharedSkillBlocked}}}, realize.Ledger{})
	if err != nil || len(blockers) != 1 || blockers[0].Remedy != remedy {
		t.Fatalf("blockers=%+v err=%v", blockers, err)
	}
	entry := retainedDependency{path: path, kind: "skill", target: ".tessl/plugins/example/alpha/skills/review"}
	if blocker := danglingBlocker(entry, "the installed tree"); blocker.Remedy != remedy {
		t.Fatalf("dangling recovery=%+v", blocker)
	}
	if blocker := unprovenBlocker(entry, "inspection failed"); blocker.Remedy != "repoint the native entry \".agents/skills/review\" at an inspectable retained tree outside the Tessl installation without unowned links, or remove that entry, then rerun acr migrate tessl --finalize" {
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
		{"rune crosses limit", strings.Repeat("x", 127) + "é", strings.Repeat("x", 127) + "...[truncated]"},
		{"rune at limit", strings.Repeat("x", 124) + "😀!", strings.Repeat("x", 124) + "😀...[truncated]"},
		{"1MiB", strings.Repeat("x", 1<<20), strings.Repeat("x", 128) + "...[truncated]"},
		{"escaped limit", strings.Repeat("\x00", 129), strings.Repeat(`\u0000`, 128) + "...[truncated]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := diagnosticCause(test.value)
			if got != test.want || !utf8.ValidString(got) || strings.ContainsAny(got, "\x00\t\r\n\x1b") {
				t.Fatalf("cause=%q want=%q", got, test.want)
			}
		})
	}
}

func TestRecoveryNaturalDiagnostics(t *testing.T) {
	const remedy = "fix the reported problem in the source package, ask its publisher if needed, reinstall or update through Tessl, then rerun migration; alternatively, map the package to a published ACR source with --map FROM=github:owner/repository[@TAG]"
	for _, test := range []struct {
		name, want string
		cause      error
	}{
		{"missing path", `declared plugin skill path "skills/normal" does not exist`, &tesslplugin.DeclaredPathError{Kind: "skill", Path: "skills/normal"}},
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
		{"controls", "review\nFAKE-TRUSTED-GUIDANCE\x1b[2J"},
		{"1MiB", strings.Repeat("x", 1<<20)},
	} {
		t.Run(input.name, func(t *testing.T) {
			for _, cause := range []error{
				&tesslplugin.DeclaredPathError{Kind: "skill", Path: input.value},
				&manifest.ValidationErrors{Issues: []manifest.ValidationError{{Code: manifest.CodeInvalidPath, Field: input.value, Message: input.value}}},
			} {
				err := classifyVendorError(cause)
				var diagnostic *Error
				if !errors.As(err, &diagnostic) || !errors.Is(err, cause) || len(diagnostic.Message) > 1024 || strings.ContainsAny(diagnostic.Message, "\n\x1b") {
					t.Fatal("vendor diagnostic was not bounded/escaped or lost its cause")
				}
			}
			root := t.TempDir()
			_, err := readMappingFile(root, input.value)
			var pathErr *os.PathError
			var diagnostic *Error
			if !errors.As(err, &pathErr) || !errors.As(err, &diagnostic) || diagnostic.Code != "mapping_file_invalid" || !strings.Contains(diagnostic.Message, "--mapping-file argument value "+migrate.DiagnosticToken(input.value)) || len(diagnostic.Message) > 2048 || strings.ContainsAny(diagnostic.Message, "\n\x1b") {
				t.Fatal("mapping read diagnostic was not bounded/escaped or lost its path error")
			}
			content, err := json.Marshal(map[string]any{"schemaVersion": 1, "packages": []map[string]string{{"from": input.value, "source": "github:example/alpha"}}})
			if err != nil {
				t.Fatal(err)
			}
			writeMapping(t, root, string(content))
			_, err = readMappingFile(root, "mapping.yaml")
			if !errors.As(err, &diagnostic) || diagnostic.Code != "mapping_file_invalid" || !errors.Is(err, diagnostic.Cause) || diagnostic.Message != diagnosticCause(diagnostic.Cause.Error()) || len(diagnostic.Message) > 528 || strings.ContainsAny(diagnostic.Message, "\n\x1b") {
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
				if !errors.As(err, &diagnostic) || !strings.Contains(diagnostic.Message, "requested "+migrate.DiagnosticToken(input.value)) || len(diagnostic.Message) > 2048 || strings.ContainsAny(diagnostic.Message, "\n\x1b") {
					t.Fatal("declaration conflict diagnostic was not bounded/escaped")
				}
				if withMapping && !strings.Contains(diagnostic.Remedy, "--map argument value "+migrate.DiagnosticToken(input.value+"="+source+"@"+input.value)) {
					t.Fatal("mapping override was not composed before rendering")
				}
			}
		})
	}
}

func TestReleaseFailureDiagnosticTokens(t *testing.T) {
	for _, input := range []struct{ name, value string }{
		{"controls", "review\nFAKE-TRUSTED-GUIDANCE\x1b[2J"},
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
					if !errors.As(err, &diagnostic) || !errors.Is(err, cause) || len(diagnostic.Message) > 4096 || strings.ContainsAny(diagnostic.Message, "\n\x1b") {
						t.Fatal("release diagnostic was not bounded/escaped or lost its cause")
					}
					if stable {
						want := "select an available release explicitly with --map argument value " + migrate.DiagnosticToken(input.value+"=github:example/alpha@TAG")
						if diagnostic.Remedy != want {
							t.Fatal("release selection mapping was not composed before rendering")
						}
					} else if diagnostic.Code != cli.CodeSourceNotPublished || !strings.Contains(diagnostic.Message, migrate.DiagnosticToken(input.value)) || (!explicit && !strings.Contains(diagnostic.Message, migrate.DiagnosticToken("v"+input.value))) {
						t.Fatal("requested/prefixed tags were not rendered individually")
					}
				})
			}
		}
	}
}
