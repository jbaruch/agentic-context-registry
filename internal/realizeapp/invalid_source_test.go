package realizeapp

import (
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

func TestUninstallInvalidSourceIsUsage(t *testing.T) {
	t.Parallel()

	const want = "acr uninstall: invalid source \"software-factory\"; use github:owner/repository or vendor:workspace/package with lowercase canonical names; review the command usage and retry\n"
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil), "uninstall", "software-factory")
	if exitCode != cli.ExitUsage || stdout != "" || stderr != want {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want %q", exitCode, stdout, stderr, want)
	}
	if strings.Contains(stderr, "retry the command, then report the failure") {
		t.Fatalf("usage refusal carries operational guidance: %s", stderr)
	}
}
