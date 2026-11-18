package publishapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestValidateListsEveryInvalidDeclaration(t *testing.T) {
	root := t.TempDir()
	content := "schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n"
	var issues []string
	for i := 0; i < 3; i++ {
		content += fmt.Sprintf("    - id: INVALID%d\n      path: a.md\n      activation:\n        mode: always\n", i)
		issues = append(issues, fmt.Sprintf("invalid_artifact_id at artifacts.rules[%d].id: use a lowercase kebab-case artifact ID", i))
	}
	for filename, data := range map[string]string{"agent-plugin.yaml": content, "a.md": "Always.\n"} {
		if err := os.WriteFile(filepath.Join(root, filename), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := "validate package: manifest validation failed: " + strings.Join(issues, "; ")
	_, err := validatePackage(context.Background(), root)
	var validation *manifest.ValidationErrors
	if !errors.As(err, &validation) || len(validation.Issues) != 3 || err.Error() != want {
		t.Fatalf("validate lost author diagnostics: %v", err)
	}
	const retry = "; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
	for _, jsonOutput := range []bool{false, true} {
		args := []string{"validate", "--project", root}
		expected := "acr validate: " + want + retry + "\n"
		if jsonOutput {
			args = append(args, "--json")
			expected = fmt.Sprintf("{\"ok\":false,\"command\":\"validate\",\"error\":{\"code\":\"invalid_artifact_id\",\"message\":%q,\"field\":\"artifacts.rules[0].id\"}}\n", want+retry)
		}
		var stdout, stderr bytes.Buffer
		exit := cli.New(&stdout, &stderr, newApplication(nil, nil), cli.Build{Version: "test"}).Run(context.Background(), args)
		if exit != cli.ExitOperational || stdout.Len() != 0 || stderr.String() != expected {
			t.Fatalf("validate output differs: exit=%d stdout=%q stderr=%q", exit, &stdout, &stderr)
		}
	}
}
