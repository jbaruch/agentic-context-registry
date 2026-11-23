package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func TestOpaqueErrorsThroughProductionMigration(t *testing.T) {
	const retry = "; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
	const publication = "complete producer stage 0: run 'acr migrate tessl-plugin' in the producer repository, publish a stable release with 'acr publish', then re-run 'acr migrate tessl'"
	longTag := strings.Repeat("t", 1<<20)
	tagToken := `"` + strings.Repeat("t", 128) + `...[truncated]"`
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	archive := hostileArchive(t, "example/alpha", map[string]string{
		"agent-plugin.yaml": "schemaVersion: 1\n? \"" + strings.Repeat("k", 1<<20) + "\"\n: true\n",
	})
	tempRoot := regexp.MustCompile(regexp.QuoteMeta(filepath.Join(os.TempDir(), "acr-package-")) + `[0-9]+`)
	for _, test := range []struct {
		name, tag, code, message, remedy string
		transport                        *releaselessGitHub
	}{
		{"megabyte transport tag", longTag, "migrate_failed",
			"resolve requested " + tagToken + ` for "github:example/alpha": resolve release tag ` + tagToken + ` for "github:example/alpha": request GitHub API: dial tcp: connection refused; check network access and retry` + retry, "",
			&releaselessGitHub{failPath: "/repos/example/alpha/releases/tags/" + longTag}},
		{"megabyte YAML key", "v1.0.0", "source_not_a_package",
			`"github:example/alpha" is not an ACR package: validate downloaded github:example/alpha package: decode /fixture/agent-plugin.yaml: yaml: unmarshal errors:\u000a  line 2: field ` + strings.Repeat("k", 16344) + "...[truncated]; fix the package manifest and publish a new release; " + publication, publication,
			&releaselessGitHub{
				statuses: map[string]int{alphaVTag: 200, "/repos/example/alpha/commits/v1.0.0": 200, "/repos/example/alpha/tarball/" + commit: 200},
				bodies:   map[string]string{alphaVTag: `{"id":42,"tag_name":"v1.0.0"}`, "/repos/example/alpha/commits/v1.0.0": `{"sha":"` + commit + `"}`, "/repos/example/alpha/tarball/" + commit: string(archive)},
			}},
	} {
		for _, dryRun := range []bool{true, false} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/dry=%t/json=%t", test.name, dryRun, jsonOutput), func(t *testing.T) {
					root := seedMappedConsumer(t, "1.0.0")
					before := hashTreeWithModes(t, root)
					application := productionMigrateApplication(t, test.transport)
					args := []string{"migrate", "tessl", "--project", root, "--non-interactive", "--map", "example/alpha=github:example/alpha@" + test.tag}
					if dryRun {
						args = append(args, "--dry-run")
					}
					want := "acr migrate: " + test.message + "\n"
					if jsonOutput {
						args = append(args, "--json")
						remedy := ""
						if test.remedy != "" {
							remedy = fmt.Sprintf(",\"remedy\":%q", test.remedy)
						}
						want = fmt.Sprintf("{\"ok\":false,\"command\":\"migrate\",\"error\":{\"code\":%q,\"message\":%q%s}}\n", test.code, test.message, remedy)
					}
					stdout, stderr, exit := runCLI(t, application, args...)
					if test.name == "megabyte YAML key" {
						if len(tempRoot.FindAllString(stderr, -1)) != 1 {
							t.Fatal("expected exactly one generated verification directory")
						}
						stderr = tempRoot.ReplaceAllString(stderr, "/fixture")
					}
					if exit != cli.ExitOperational || stdout != "" || stderr != want {
						t.Fatalf("exact output differs: exit=%d stdout bytes=%d stderr bytes=%d want bytes=%d; stderr prefix=%.250q", exit, len(stdout), len(stderr), len(want), stderr)
					}
					if strings.Contains(stderr, "https://api.github.com") {
						t.Fatal("request URL escaped the producer boundary")
					}
					if !mapsEqual(before, hashTreeWithModes(t, root)) {
						t.Fatal("refusal changed consumer files or the Tessl installation")
					}
				})
			}
		}
	}
}

func TestManyInvalidDeclarationsThroughProductionMigration(t *testing.T) {
	var document strings.Builder
	document.WriteString("schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n")
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&document, "    - id: INVALID%d\n      path: rules/a.md\n      activation:\n        mode: always\n", i)
	}
	archive := hostileArchive(t, "example/alpha", map[string]string{"agent-plugin.yaml": document.String(), "rules/a.md": "Always.\n"})
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const summary = "manifest validation failed: invalid_artifact_id at artifacts.rules[0].id: use a lowercase kebab-case artifact ID; and 9999 more"
	const message = `"github:example/alpha" is not an ACR package: validate downloaded github:example/alpha package: ` + summary + "; fix the package manifest and publish a new release; " + sourceNotPublishedRemedy
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry=%t", dryRun), func(t *testing.T) {
			root := seedMappedConsumer(t, "1.0.0")
			before := hashTreeWithModes(t, root)
			transport := &releaselessGitHub{
				statuses: map[string]int{alphaVTag: 200, "/repos/example/alpha/commits/v1.0.0": 200, "/repos/example/alpha/tarball/" + commit: 200},
				bodies:   map[string]string{alphaVTag: `{"id":42,"tag_name":"v1.0.0"}`, "/repos/example/alpha/commits/v1.0.0": `{"sha":"` + commit + `"}`, "/repos/example/alpha/tarball/" + commit: string(archive)},
			}
			app := productionMigrateApplication(t, transport)
			mappings, err := migrate.ParseInlineMappings([]string{"example/alpha=github:example/alpha@v1.0.0"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = app.service.Migrate(context.Background(), root, Options{DryRun: dryRun, CLIMappings: mappings})
			var validation *manifest.ValidationErrors
			if !errors.As(err, &validation) || len(validation.Issues) != 10000 {
				t.Fatal("downloaded validation lost the complete typed issues")
			}
			for i, issue := range validation.Issues {
				if issue.Code != manifest.CodeInvalidArtifactID || issue.Field != fmt.Sprintf("artifacts.rules[%d].id", i) || issue.Message != "use a lowercase kebab-case artifact ID" {
					t.Fatalf("downloaded issue %d changed", i)
				}
			}
			args := []string{"migrate", "tessl", "--project", root, "--non-interactive", "--map", "example/alpha=github:example/alpha@v1.0.0"}
			if dryRun {
				args = append(args, "--dry-run")
			}
			assertMigrationFailureOutput(t, app, args, "source_not_a_package", message, sourceNotPublishedRemedy, cli.ExitOperational)
			if !mapsEqual(before, hashTreeWithModes(t, root)) {
				t.Fatal("invalid downloaded declarations changed consumer or Tessl files")
			}
		})
	}
}

func TestMigrationManifestSummaryBoundsOnlyFirstIssue(t *testing.T) {
	const prefix = "invalid_artifact_id at artifacts.rules[0].id: "
	const migrationRetryRemedy = "retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
	for _, size := range []int{16384, 16385} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			original := &manifest.ValidationErrors{Issues: []manifest.ValidationError{
				{Code: manifest.CodeInvalidArtifactID, Field: "artifacts.rules[0].id", Message: strings.Repeat("x", size-len(prefix))},
				{Code: manifest.CodeInvalidArtifactID, Field: "artifacts.rules[1].id", Message: "second failure"},
			}}
			first := original.Issues[0]
			want := "manifest validation failed: " + prefix + strings.Repeat("x", 16384-len(prefix))
			if size > 16384 {
				want += "...[truncated]"
			}
			want += "; and 1 more"
			vendorRemedy := migrate.SourcePackageRepairRemedy + "; alternatively, map the package to a published ACR source with --map FROM=github:owner/repository[@TAG]"
			consumerErr := manifest.ConsumerError(original)
			vendor := classifyVendorError(fmt.Errorf("validate vendored package: %w", consumerErr))
			resolution := classifyResolutionError("github:example/alpha", fmt.Errorf("package manifest: %w; fix the package manifest and publish a new release", consumerErr))
			for _, test := range []struct {
				err  error
				want string
			}{
				{migrateError(original), want},
				{vendor, "validate vendored package: " + want + "; " + vendorRemedy},
				{resolution, `"github:example/alpha" is not an ACR package: package manifest: ` + want + "; fix the package manifest and publish a new release; " + sourceNotPublishedRemedy},
			} {
				if test.err.Error() != test.want || !errors.Is(test.err, original) {
					t.Fatal("migration summary lost its boundary, count, guidance or original cause")
				}
			}
			if len(original.Issues) != 2 || original.Issues[0] != first || !original.Has(manifest.CodeInvalidArtifactID) || !strings.Contains(original.Error(), "second failure") {
				t.Fatal("migration rendering changed complete author diagnostics")
			}
			app := cli.ApplicationFunc(func(context.Context, cli.Invocation) (cli.Result, error) { return cli.Result{}, migrateError(original) })
			for _, jsonOutput := range []bool{false, true} {
				args := []string{"migrate", "tessl-plugin", "--project", t.TempDir()}
				expected := "acr migrate: " + want + "; " + migrationRetryRemedy + "\n"
				if jsonOutput {
					args = append(args, "--json")
					expected = fmt.Sprintf("{\"ok\":false,\"command\":\"migrate\",\"error\":{\"code\":\"invalid_artifact_id\",\"message\":%q,\"field\":\"artifacts.rules[0].id\"}}\n", want+"; "+migrationRetryRemedy)
				}
				stdout, stderr, exit := runCLI(t, app, args...)
				if exit != cli.ExitOperational || stdout != "" || stderr != expected {
					t.Fatalf("direct conversion output differs: exit=%d stdout bytes=%d stderr bytes=%d want bytes=%d; prefix=%.180q", exit, len(stdout), len(stderr), len(expected), stderr)
				}
			}
		})
	}
}

func assertMigrationFailureOutput(t *testing.T, app cli.Application, args []string, code, message, remedy string, exitCode int) {
	t.Helper()
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOutput), func(t *testing.T) {
			invocation := append([]string(nil), args...)
			want := "acr migrate: " + message + "\n"
			if jsonOutput {
				invocation = append(invocation, "--json")
				remedyField := ""
				if remedy != "" {
					remedyField = fmt.Sprintf(",\"remedy\":%q", remedy)
				}
				want = fmt.Sprintf("{\"ok\":false,\"command\":\"migrate\",\"error\":{\"code\":%q,\"message\":%q%s}}\n", code, message, remedyField)
			}
			stdout, stderr, exit := runCLI(t, app, invocation...)
			if exit != exitCode || stdout != "" || stderr != want {
				t.Fatalf("exact output differs: exit=%d stdout bytes=%d stderr bytes=%d want bytes=%d; prefix=%.250q", exit, len(stdout), len(stderr), len(want), stderr)
			}
		})
	}
}

func TestOpaqueDetailsEscapeThroughMigrationConsumer(t *testing.T) {
	original := errors.New("decoder\n\x1b\u202e: " + strings.Repeat("detail ", 30))
	cause := fmt.Errorf("decode package: %w; repair the source package", diagnostic.OpaqueError(original))
	err := classifyResolutionError("github:example/alpha", fmt.Errorf("agent-plugin.yaml: %w", cause))
	want := `"github:example/alpha" is not an ACR package: agent-plugin.yaml: decode package: decoder\u000a\u001b\u202e: ` + strings.Repeat("detail ", 30) + "; repair the source package; complete producer stage 0: run 'acr migrate tessl-plugin' in the producer repository, publish a stable release with 'acr publish', then re-run 'acr migrate tessl'"
	if !errors.Is(err, original) || err.Error() != want {
		t.Fatal("escaped consumer message differs or original cause lost")
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
		if exit != cli.ExitOperational || stdout != "" || message != want || strings.ContainsAny(message, "\n\x1b\u202e") {
			t.Fatal("CLI changed or failed to escape the complete diagnostic")
		}
	}
}
