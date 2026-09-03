package docsharness

import (
	"os/exec"
	"strings"
	"testing"
)

// The executable-console guard has two halves. Tagging a console fence
// non-executable is now rejected outright, but a `text non-executable` fence
// still leaves the executable set silently, so a whole page can lose every
// block without one console fence being tagged. The per-file minimum is the
// only thing that reds then, and nothing proved it red until this case.
func TestExecutableMinimumRejectsDeExecutedPage(t *testing.T) {
	assertHarnessRejects(t, injection{
		name: "every console fence on one page de-executed",
		file: "docs/cli.md",
		rewrite: func(content string) string {
			return strings.ReplaceAll(content, "```console\n", "```text non-executable\n")
		},
		pkg:   "./cmd/acr",
		test:  "TestDocumentedCommands",
		names: []string{"docs/cli.md has 0 executable console blocks, want at least 12"},
	})
}

// assertHarnessRejects injects one defect into a scratch copy of the module and
// requires the named harness to fail naming the offending file, row, or count.
func assertHarnessRejects(t *testing.T, defect injection) {
	t.Helper()
	module := copyModule(t)
	restore := inject(t, module, defect)
	defer restore()

	command := exec.Command("go", "test", defect.pkg, "-run", "^"+defect.test+"$", "-count=1")
	command.Dir = module
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("%s accepted %s\n%s", defect.test, defect.name, output)
	}
	var exitError *exec.ExitError
	if !asExitError(err, &exitError) {
		t.Fatalf("run %s: %v\n%s", defect.test, err, output)
	}
	for _, name := range defect.names {
		if !strings.Contains(string(output), name) {
			t.Errorf("%s failure does not name %q\n%s", defect.test, name, output)
		}
	}
}
