package migrateapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
)

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

	t.Run("map uppercase owner", func(t *testing.T) {
		t.Parallel()
		app := NewApplication(dependencytest.NewRemote(), "test")
		root := seedConsumer(t)
		const want = "acr migrate: invalid source \"github:Owner/Plugin\"; use github:owner/repository or vendor:workspace/package with lowercase canonical names; review the command usage and retry\n"
		stdout, stderr, exitCode := runCLI(t, app, "migrate", "tessl", "--map", "example/alpha=github:Owner/Plugin", "--project", root, "--dry-run")
		assertUsageDiagnostic(t, stdout, stderr, exitCode, want)
	})
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
