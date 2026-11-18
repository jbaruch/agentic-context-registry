package migrateapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

func TestMigrateMappingFileFailureOutput(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing file", "invalid YAML"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			message := fmt.Sprintf("read migration mapping file \"mapping.yaml\": %s; create or correct the mapping file and ensure it is readable, then rerun with --mapping-file argument value \"mapping.yaml\"", migrate.DiagnosticToken("open "+filepath.Join(root, "mapping.yaml")+": no such file or directory"))
			if name == "invalid YAML" {
				writeMapping(t, root, "schemaVersion: [\n")
				message = `"decode migration mapping file: yaml: line 1: did not find expected node content; use schemaVersion: 1 with a packages list"`
			}
			before := hashTreeWithModes(t, root)
			for _, dryRun := range []bool{false, true} {
				for _, format := range []string{"text", "json"} {
					app := NewApplication(dependencytest.NewRemote(), "test")
					args := []string{"migrate", "tessl", "--project", root, "--mapping-file", "mapping.yaml", "--non-interactive"}
					if dryRun {
						args = append(args, "--dry-run")
					}
					want := "acr migrate: " + message + "\n"
					if format == "json" {
						args = append(args, "--json")
						want = fmt.Sprintf("{\"ok\":false,\"command\":\"migrate\",\"error\":{\"code\":\"mapping_file_invalid\",\"message\":%q}}\n", message)
					}
					stdout, stderr, exitCode := runCLI(t, app, args...)
					if stdout != "" || exitCode != cli.ExitOperational || stderr != want {
						t.Fatalf("dryRun=%t %s: exit=%d stdout=%q stderr=%q, want exit=%d stderr=%q", dryRun, format, exitCode, stdout, stderr, cli.ExitOperational, want)
					}
				}
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatal("mapping-file refusal changed project files")
			}
		})
	}
}

func TestMigrateInvalidSourceIsUsage(t *testing.T) {
	t.Parallel()

	usage := "review the command usage and retry"

	t.Run("map bare name", func(t *testing.T) {
		t.Parallel()
		app := NewApplication(dependencytest.NewRemote(), "test")
		root := t.TempDir()
		const want = "acr migrate: invalid mapping source \"software-factory\"; use github:owner/repository with lowercase canonical names; review the command usage and retry\n"
		stdout, stderr, exitCode := runCLI(t, app, "migrate", "tessl", "--map", "example/pkg=software-factory", "--project", root)
		assertUsageDiagnostic(t, stdout, stderr, exitCode, want)
		if !strings.Contains(stderr, usage) {
			t.Fatalf("stderr = %q, want usage guidance", stderr)
		}
	})

	t.Run("mapping file bare name", func(t *testing.T) {
		t.Parallel()
		app := NewApplication(dependencytest.NewRemote(), "test")
		root := t.TempDir()
		writeMapping(t, root, "schemaVersion: 1\npackages:\n  - from: example/pkg\n    source: software-factory\n")
		const want = "acr migrate: invalid mapping source \"software-factory\"; use github:owner/repository with lowercase canonical names; review the command usage and retry\n"
		stdout, stderr, exitCode := runCLI(t, app, "migrate", "tessl", "--mapping-file", "mapping.yaml", "--project", root)
		assertUsageDiagnostic(t, stdout, stderr, exitCode, want)
	})

	t.Run("mapping file malformed target", func(t *testing.T) {
		t.Parallel()
		app := NewApplication(dependencytest.NewRemote(), "test")
		root := t.TempDir()
		writeMapping(t, root, "schemaVersion: 1\npackages:\n  - from: example/pkg\n    source: github:owner/repo@\n")
		const want = "acr migrate: invalid mapping target \"github:owner/repo@\"; use github:owner/repository[@REQUESTED]; review the command usage and retry\n"
		stdout, stderr, exitCode := runCLI(t, app, "migrate", "tessl", "--mapping-file", "mapping.yaml", "--project", root)
		assertUsageDiagnostic(t, stdout, stderr, exitCode, want)
	})

	for _, input := range []string{"map", "mapping-file"} {
		t.Run(input+" uppercase owner", func(t *testing.T) {
			t.Parallel()
			app := NewApplication(dependencytest.NewRemote(), "test")
			root := seedConsumer(t)
			const diagnostic = "invalid source \"github:Owner/Plugin\"; use github:owner/repository with lowercase canonical names; review the command usage and retry"
			args := []string{"migrate", "tessl", "--project", root, "--dry-run"}
			if input == "map" {
				args = append(args, "--map", "example/alpha=github:Owner/Plugin")
			} else {
				writeMapping(t, root, "schemaVersion: 1\npackages:\n  - from: example/alpha\n    source: github:Owner/Plugin\n")
				args = append(args, "--mapping-file", "mapping.yaml")
			}
			const want = "acr migrate: " + diagnostic + "\n"
			stdout, stderr, exitCode := runCLI(t, app, args...)
			assertUsageDiagnostic(t, stdout, stderr, exitCode, want)

			stdout, stderr, exitCode = runCLI(t, app, append(args, "--json")...)
			var envelope migrateEnvelope
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("decode stderr %q: %v", stderr, err)
			}
			if exitCode != cli.ExitUsage || stdout != "" || envelope.OK || envelope.Error.Code != "usage" || envelope.Error.Message != diagnostic {
				t.Fatalf("exit = %d, stdout = %q, stderr = %s, want usage diagnostic %q", exitCode, stdout, stderr, diagnostic)
			}
		})
	}

	t.Run("mapping file bad package stays mapping_file_invalid", func(t *testing.T) {
		t.Parallel()
		app := NewApplication(dependencytest.NewRemote(), "test")
		root := t.TempDir()
		writeMapping(t, root, "schemaVersion: 1\npackages:\n  - from: not-a-package\n    source: github:owner/repo\n")
		const want = "acr migrate: \"invalid mapping-file mapping package \\\"not-a-package\\\"; use a workspace/package identity\"\n"
		stdout, stderr, exitCode := runCLI(t, app, "migrate", "tessl", "--mapping-file", "mapping.yaml", "--project", root)
		if exitCode != cli.ExitOperational || stdout != "" || stderr != want {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q, want %q", exitCode, stdout, stderr, want)
		}
		stdout, stderr, exitCode = runCLI(t, app, "migrate", "tessl", "--mapping-file", "mapping.yaml", "--project", root, "--json")
		assertJSONCode(t, stdout, stderr, exitCode, "mapping_file_invalid")
	})
}

func TestMigrateManifestSourceStaysOperational(t *testing.T) {
	t.Parallel()

	app := NewApplication(dependencytest.NewRemote(), "test")
	root := seedConsumer(t)
	writeJSON(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{
		"name":       "example/alpha",
		"version":    "1.0.0",
		"repository": "https://github.com/Owner/Plugin",
		"rules":      []string{"rules/always-rule.md"},
		"skills":     []string{"skills/review-change"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
			}}}},
		},
	})
	const diagnostic = "invalid source \"github:Owner/Plugin\"; use github:owner/repository or vendor:workspace/package with lowercase canonical names"
	const guidance = "retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
	const want = "acr migrate: " + diagnostic + "; " + guidance + "\n"

	stdout, stderr, exitCode := runCLI(t, app, "migrate", "tessl", "--dry-run", "--project", root)
	if exitCode != cli.ExitOperational || stdout != "" || stderr != want {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want %q", exitCode, stdout, stderr, want)
	}
	if strings.Contains(stderr, "review the command usage") {
		t.Fatalf("manifest source carries usage guidance: %s", stderr)
	}

	stdout, stderr, exitCode = runCLI(t, app, "migrate", "tessl", "--dry-run", "--json", "--project", root)
	envelope := assertJSONCode(t, stdout, stderr, exitCode, "migrate_failed")
	if !strings.Contains(envelope.Error.Message, diagnostic) || !strings.Contains(envelope.Error.Message, guidance) || strings.Contains(envelope.Error.Message, "review the command usage") {
		t.Fatalf("json envelope = %s", stderr)
	}
}

func writeMapping(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "mapping.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertUsageDiagnostic(t *testing.T, stdout, stderr string, exitCode int, want string) {
	t.Helper()
	if exitCode != cli.ExitUsage || stdout != "" || stderr != want {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want %q", exitCode, stdout, stderr, want)
	}
	if strings.Contains(stderr, "retry the command, then report the failure") {
		t.Fatalf("usage refusal carries operational guidance: %s", stderr)
	}
}

func assertJSONCode(t *testing.T, stdout, stderr string, exitCode int, code string) migrateEnvelope {
	t.Helper()
	var envelope migrateEnvelope
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("decode stderr %q: %v", stderr, err)
	}
	if exitCode != cli.ExitOperational || stdout != "" || envelope.OK || envelope.Error.Code != code {
		t.Fatalf("exit = %d, stdout = %q, stderr = %s, want code %s", exitCode, stdout, stderr, code)
	}
	if strings.Contains(envelope.Error.Message, "review the command usage") {
		t.Fatalf("operational refusal carries usage guidance: %s", stderr)
	}
	return envelope
}

type migrateEnvelope struct {
	OK    bool `json:"ok"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
