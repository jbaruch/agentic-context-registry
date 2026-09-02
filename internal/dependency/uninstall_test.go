package dependency

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

func TestResumeAndUpdateRefuseUndeclaredAsUsage(t *testing.T) {
	t.Parallel()

	for _, command := range []string{"resume", "update"} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			args := []string{command, "github:owner/missing", "--project", t.TempDir(), "--json"}
			exitCode := cli.New(&stdout, &stderr, NewApplication(&fakeGitHub{}), cli.Build{Version: "test"}).Run(context.Background(), args)

			if exitCode != cli.ExitUsage {
				t.Fatalf("Run(%s) exit = %d, want %d", command, exitCode, cli.ExitUsage)
			}
			if stdout.Len() != 0 {
				t.Fatalf("Run(%s) stdout = %q, want empty", command, stdout.String())
			}
			if !strings.Contains(stderr.String(), `"code":"dependency_not_declared"`) || !strings.Contains(stderr.String(), "acr list") {
				t.Fatalf("Run(%s) stderr = %q, want a dependency_not_declared refusal naming acr list", command, stderr.String())
			}
		})
	}
}
