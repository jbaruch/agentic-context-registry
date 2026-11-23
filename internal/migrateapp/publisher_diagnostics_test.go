package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	operandRetry   = "; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
	operandCommit  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hostileOperand = "bad\nFAKE-GUIDANCE\x1b[2J\u202e"
	// shownOperand is how a token displays hostileOperand.
	shownOperand = `bad\nFAKE-GUIDANCE\x1b[2J\u202e`
)

// truncatedToken is the token of hostileOperand followed by a long run of fill.
func truncatedToken(fill string) string {
	return `"` + shownOperand + strings.Repeat(fill, 128-len(hostileOperand)) + `...[truncated]"`
}

// hostileRelease serves one inert release of example/alpha as the latest one.
func hostileRelease(t *testing.T, tag string, files map[string]string, metadata map[string]any) *releaselessGitHub {
	t.Helper()
	encode := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	release := map[string]any{"id": 42, "tag_name": tag}
	commitPath := "/repos/example/alpha/commits/" + tag
	archivePath := "/repos/example/alpha/tarball/" + operandCommit
	transport := &releaselessGitHub{
		statuses: map[string]int{alphaLatest: 200, commitPath: 200, archivePath: 200},
		bodies: map[string]string{
			commitPath:  `{"sha":"` + operandCommit + `"}`,
			archivePath: string(hostileArchive(t, "example/alpha", files)),
		},
	}
	if metadata != nil {
		release["assets"] = []dependency.ReleaseAsset{{ID: 7, Name: dependency.ReleaseMetadataAssetName, URL: "https://api.github.com/assets/7"}}
		transport.statuses["/assets/7"] = 200
		transport.bodies["/assets/7"] = encode(metadata)
	}
	transport.bodies[alphaLatest] = encode(release)
	return transport
}

// runDiagnostic runs the CLI and returns the message exactly as the selected
// output mode printed it, with the error the application handed to the runner.
func runDiagnostic(t *testing.T, app cli.Application, jsonOutput bool, args ...string) (string, error) {
	t.Helper()
	var cause error
	capture := cli.ApplicationFunc(func(ctx context.Context, invocation cli.Invocation) (cli.Result, error) {
		result, err := app.Execute(ctx, invocation)
		cause = err
		return result, err
	})
	if jsonOutput {
		args = append(args, "--json")
	}
	stdout, stderr, exit := runCLI(t, capture, args...)
	if exit != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit=%d stdout bytes=%d stderr=%.300q", exit, len(stdout), stderr)
	}
	if len(stderr) > 4096 {
		t.Fatalf("diagnostic is unbounded: %d bytes, prefix=%.300q", len(stderr), stderr)
	}
	line, terminated := strings.CutSuffix(stderr, "\n")
	if !terminated || strings.ContainsAny(line, "\n\r\x1b\u202e") {
		t.Fatalf("diagnostic is not one escaped line: %q", stderr)
	}
	if !jsonOutput {
		return strings.TrimPrefix(line, "acr "+args[0]+": "), cause
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(envelope.Error.Message+envelope.Error.Field, "\n\r\x1b\u202e") {
		t.Fatalf("decoded diagnostic carries controls: %q", envelope.Error)
	}
	return envelope.Error.Message, cause
}

func TestPublisherOperandsThroughInstallAndMigrate(t *testing.T) {
	long := hostileOperand + strings.Repeat("z", 1<<20)
	longToken := truncatedToken("z")
	computedHash := regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	packageFiles := func(extra map[string]string) map[string]string {
		files := map[string]string{"rules/a.md": "Always.\n"}
		for name, content := range extra {
			files[name] = content
		}
		return files
	}
	for _, test := range []struct {
		name, want   string
		transport    func() *releaselessGitHub
		notAPackage  bool
		preservedErr error
	}{
		{
			name: "package version",
			want: `release tag "v1.0.0" does not match package version "1.0.0+` + strings.Repeat("v", 122) + `...[truncated]"; publish matching agent-plugin.yaml metadata and retry`,
			transport: func() *releaselessGitHub {
				document := "schemaVersion: 1\nname: example/alpha\nversion: 1.0.0+" + strings.Repeat("v", 1<<20) + "\nsource:\n  repository: https://github.com/example/alpha\n" +
					"artifacts:\n  rules:\n    - id: a\n      path: rules/a.md\n      activation:\n        mode: always\n"
				return hostileRelease(t, "v1.0.0", packageFiles(map[string]string{"agent-plugin.yaml": document}), nil)
			},
			notAPackage: true,
		},
		{
			name:        "release tag",
			want:        "release tag " + longToken + ` does not match package version "1.0.0"; publish matching agent-plugin.yaml metadata and retry`,
			transport:   func() *releaselessGitHub { return hostileRelease(t, long, packageFiles(nil), nil) },
			notAPackage: true,
		},
		{
			name: "release tag at commit lookup",
			want: "resolve commit " + longToken + " for github:example/alpha: request GitHub API: dial tcp: connection refused; check network access and retry",
			transport: func() *releaselessGitHub {
				transport := hostileRelease(t, long, packageFiles(nil), nil)
				transport.failPath = "/repos/example/alpha/commits/" + long
				return transport
			},
		},
		{
			name: "resolved commit",
			want: "GitHub returned invalid commit " + longToken + " for github:example/alpha; retry or report the repository response",
			transport: func() *releaselessGitHub {
				transport := hostileRelease(t, "v1.0.0", packageFiles(nil), nil)
				encoded, err := json.Marshal(map[string]string{"sha": long})
				if err != nil {
					t.Fatal(err)
				}
				transport.bodies["/repos/example/alpha/commits/v1.0.0"] = string(encoded)
				return transport
			},
		},
		{
			name: "metadata commit",
			want: `release metadata commit mismatch for github:example/alpha tag "v1.0.0": metadata records ` + longToken + ", resolved tag points to " + operandCommit + "; do not use the package and restore the immutable tag",
			transport: func() *releaselessGitHub {
				return hostileRelease(t, "v1.0.0", packageFiles(nil), map[string]any{"metadataVersion": 1, "commit": long})
			},
		},
		{
			name: "metadata content hash",
			want: `release metadata content hash mismatch for github:example/alpha tag "v1.0.0": metadata records ` + longToken + ", source tree verifies as sha256:<computed>; do not use the package and inspect the release",
			transport: func() *releaselessGitHub {
				return hostileRelease(t, "v1.0.0", packageFiles(nil), map[string]any{"metadataVersion": 1, "commit": operandCommit, "contentHash": long})
			},
		},
		{
			name: "archive file name",
			want: "create package file " + longToken + ": file name too long; retry with a valid archive",
			transport: func() *releaselessGitHub {
				name := hostileOperand + strings.Repeat("z", 1000)
				return hostileRelease(t, "v1.0.0", packageFiles(map[string]string{name: "inert\n"}), nil)
			},
			preservedErr: syscall.ENAMETOOLONG,
		},
	} {
		for _, command := range []string{"install", "migrate"} {
			for _, dryRun := range []bool{false, true} {
				for _, jsonOutput := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/dry=%t/json=%t", test.name, command, dryRun, jsonOutput), func(t *testing.T) {
						root, args := t.TempDir(), []string{"install", "github:example/alpha"}
						want := "resolve github:example/alpha@latest: " + test.want
						if command == "migrate" {
							root, args = seedMappedConsumer(t, "latest"), []string{"migrate", "tessl", "--map", "example/alpha=github:example/alpha"}
							want = test.want
							if test.notAPackage {
								want = `"github:example/alpha" is not an ACR package: ` + want + "; " + sourceNotPublishedRemedy
							}
						}
						if command == "install" || !test.notAPackage {
							want += operandRetry
						}
						args = append(args, "--project", root, "--non-interactive")
						if dryRun {
							args = append(args, "--dry-run")
						}
						before := hashTreeWithModes(t, root)
						message, cause := runDiagnostic(t, productionMigrateApplication(t, test.transport()), jsonOutput, args...)
						if message = computedHash.ReplaceAllString(message, "sha256:<computed>"); message != want {
							t.Fatalf("message = %q\nwant      %q", message, want)
						}
						if test.preservedErr != nil {
							var pathErr *fs.PathError
							if !errors.Is(cause, test.preservedErr) || !errors.As(cause, &pathErr) || !strings.Contains(pathErr.Path, "FAKE-GUIDANCE") {
								t.Fatalf("original cause was lost: %v", cause)
							}
						}
						if !mapsEqual(before, hashTreeWithModes(t, root)) {
							t.Fatal("refusal changed the consumer project")
						}
					})
				}
			}
		}
	}
}

func TestValidationListIsCappedForConsumersAndCompleteForAuthors(t *testing.T) {
	const count = 2000
	const firstIssue = "manifest validation failed: invalid_artifact_id at artifacts.rules[0].id: use a lowercase kebab-case artifact ID"
	var document strings.Builder
	document.WriteString("schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n")
	for index := range count {
		fmt.Fprintf(&document, "    - id: INVALID%d\n      path: rules/a.md\n      activation:\n        mode: always\n", index)
	}
	files := map[string]string{"agent-plugin.yaml": document.String(), "rules/a.md": "Always.\n"}

	for _, jsonOutput := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("install/dry=%t/json=%t", dryRun, jsonOutput), func(t *testing.T) {
				args := []string{"install", "github:example/alpha", "--project", t.TempDir(), "--non-interactive"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				message, cause := runDiagnostic(t, productionMigrateApplication(t, hostileRelease(t, "v1.0.0", files, nil)), jsonOutput, args...)
				want := "resolve github:example/alpha@latest: validate downloaded github:example/alpha package: " + firstIssue +
					fmt.Sprintf("; and %d more; fix the package manifest and publish a new release", count-1) + operandRetry
				var validation *manifest.ValidationErrors
				if message != want || !errors.As(cause, &validation) || len(validation.Issues) != count {
					t.Fatalf("message = %q, want %q with all %d typed issues", message, want, count)
				}
			})
			t.Run(fmt.Sprintf("migrate/dry=%t/json=%t", dryRun, jsonOutput), func(t *testing.T) {
				args := []string{"migrate", "tessl", "--map", "example/alpha=github:example/alpha", "--project", seedMappedConsumer(t, "latest"), "--non-interactive"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				message, cause := runDiagnostic(t, productionMigrateApplication(t, hostileRelease(t, "v1.0.0", files, nil)), jsonOutput, args...)
				want := `"github:example/alpha" is not an ACR package: validate downloaded github:example/alpha package: ` + firstIssue +
					fmt.Sprintf("; and %d more; fix the package manifest and publish a new release; ", count-1) + sourceNotPublishedRemedy
				var validation *manifest.ValidationErrors
				if message != want || !errors.As(cause, &validation) || len(validation.Issues) != count {
					t.Fatalf("message = %q, want %q with all %d typed issues", message, want, count)
				}
			})
		}

		t.Run(fmt.Sprintf("local install/json=%t", jsonOutput), func(t *testing.T) {
			t.Setenv("ACR_STATE_HOME", t.TempDir())
			root := t.TempDir()
			for name, content := range files {
				writeFile(t, filepath.Join(root, "package"), name, []byte(content), 0o644)
			}
			message, _ := runDiagnostic(t, productionMigrateApplication(t, &releaselessGitHub{}), jsonOutput,
				"install", "./package", "--project", root, "--non-interactive")
			if !strings.Contains(message, firstIssue+fmt.Sprintf("; and %d more", count-1)) {
				t.Fatalf("local install was not capped: %q", message)
			}
		})

		t.Run(fmt.Sprintf("validate/json=%t", jsonOutput), func(t *testing.T) {
			root := t.TempDir()
			for name, content := range files {
				writeFile(t, root, name, []byte(content), 0o644)
			}
			args := []string{"validate", "--project", root}
			if jsonOutput {
				args = append(args, "--json")
			}
			stdout, stderr, exit := runCLI(t, productionMigrateApplication(t, &releaselessGitHub{}), args...)
			if exit != cli.ExitOperational || stdout != "" || strings.Count(stderr, "invalid_artifact_id at") != count ||
				!strings.Contains(stderr, fmt.Sprintf("artifacts.rules[%d].id", count-1)) || strings.Contains(stderr, " more;") {
				t.Fatalf("author validation lost issues: exit=%d, issues=%d", exit, strings.Count(stderr, "invalid_artifact_id at"))
			}
		})
	}
}

func TestInstalledPackageOperandsThroughMigrate(t *testing.T) {
	long := hostileOperand + strings.Repeat("q", 1024)
	longToken := truncatedToken("q")
	for _, test := range []struct {
		name   string
		plugin map[string]any
		want   string
	}{
		{"hook event", map[string]any{"hooks": map[string]any{long: []any{}}}, "hook event " + longToken + " is outside the v1 vocabulary"},
		{"hook type", map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": long}}}}}}, "hook type " + longToken + " is not supported"},
		{"hook command", map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": long}}}}}}, "hook command " + longToken + " is outside the closed Tessl grammar"},
		{"invalid declared path", map[string]any{"rules": []string{"/" + long}}, `declared rule path "/` + shownOperand},
		{"unreadable declared path", map[string]any{"skills": []string{long}}, "...[truncated]\": file name too long"},
		{"repository", map[string]any{"repository": "https://github.com/Example/" + strings.Repeat("q", 1024)}, `invalid source "github:Example/qqq`},
		{"unmapped name", map[string]any{"name": long}, "add --map FROM=github:owner/repository or declare it in --mapping-file"},
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", test.name, jsonOutput), func(t *testing.T) {
				root := writeUnmappedConsumer(t)
				plugin := map[string]any{"name": "example/orphan", "version": "legacy", "rules": []string{"rules"}, "skills": []string{"skills"}}
				for key, value := range test.plugin {
					plugin[key] = value
				}
				writeJSON(t, root, ".tessl/plugins/example/orphan/.tessl-plugin/plugin.json", plugin)
				args := []string{"migrate", "tessl", "--project", root, "--non-interactive"}
				if test.name != "unmapped name" {
					args = append(args, "--vendor-unmapped")
				}
				message, _ := runDiagnostic(t, productionMigrateApplication(t, &releaselessGitHub{}), jsonOutput, args...)
				if !strings.Contains(message, test.want) {
					t.Fatalf("message = %q, want it to contain %q", message, test.want)
				}
			})
		}
	}
}

// A valid artifact ID becomes a native file name, so a release can reach the
// realization transaction with a name the filesystem refuses.
func TestNativeTargetNamesThroughMigrate(t *testing.T) {
	id := strings.Repeat("q", 1024)
	const head = "schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n"
	for _, test := range []struct{ name, artifacts, target string }{
		{"skill", "  skills:\n    - id: " + id + "\n      path: skills/a\n", `read ".claude/skills/acr__example__alpha__q`},
		{"hook", "  hooks:\n    - id: " + id + "\n      event: session-start\n      path: rules/a.md\n", `create parents for ".claude/hooks/acr__example__alpha__q`},
		{"rule", "  rules:\n    - id: " + id + "\n      path: rules/a.md\n      activation:\n        mode: always\n", `apply create ".cursor/rules/acr__example__alpha__q`},
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", test.name, jsonOutput), func(t *testing.T) {
				root := seedMappedConsumer(t, "latest")
				if test.name == "rule" {
					// Only Cursor gives each rule a file of its own.
					if err := os.MkdirAll(filepath.Join(root, ".cursor/skills"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("../../.tessl/plugins/example/alpha/skills/review-change", filepath.Join(root, ".cursor/skills/tessl__review-change")); err != nil {
						t.Fatal(err)
					}
				}
				transport := hostileRelease(t, "v1.0.0", map[string]string{"agent-plugin.yaml": head + test.artifacts, "rules/a.md": "Always.\n", "skills/a/SKILL.md": "# A\n"}, nil)
				message, cause := runDiagnostic(t, productionMigrateApplication(t, transport), jsonOutput,
					"migrate", "tessl", "--project", root, "--non-interactive", "--map", "example/alpha=github:example/alpha")
				if !strings.Contains(message, test.target) || !strings.Contains(message, "file name too long") || !strings.HasSuffix(message, operandRetry) || !errors.Is(cause, syscall.ENAMETOOLONG) {
					t.Fatalf("message = %q, cause retained = %t", message, errors.Is(cause, syscall.ENAMETOOLONG))
				}
			})
		}
	}
}
