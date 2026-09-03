// Package docsharness holds the negative proofs for the documentation
// harnesses. The positive tests in cmd/acr, internal/cli, and internal/release
// assert that the committed documentation matches the shipped code. They cannot
// assert the converse: that the harness still rejects documentation the code has
// outgrown. A guard narrowed until it accepts everything stays green.
//
// Each case below copies the module's tracked files into a scratch directory,
// injects exactly one defect the tester plan names, and runs only the harness
// test that must reject it. The harness runs as the pipeline runs it, so a
// narrowed guard reds here.
package docsharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// injection is one defect and the harness failure it must produce.
type injection struct {
	name string
	// file is the module-relative path the defect is written to. An absent
	// file is created; create reports whether the injection adds one.
	file    string
	create  bool
	rewrite func(string) string
	pkg     string
	test    string
	// names are substrings the harness failure must contain. Each names the
	// offending block, row, flag, or anchor rather than merely reporting that
	// something is wrong.
	names []string
}

func injections() []injection {
	return []injection{
		{
			name: "wrong exit directive",
			file: "docs/migration-guide.md",
			rewrite: func(content string) string {
				return strings.Replace(content, "# exit: 0\n", "# exit: 3\n", 1)
			},
			pkg:   "./cmd/acr",
			test:  "TestDocumentedCommands",
			names: []string{"migration-guide.md:", "exit = 0, want 3"},
		},
		{
			name: "console fence demoted to text",
			file: "docs/cli.md",
			rewrite: func(content string) string {
				return strings.Replace(content, "```console\n", "```text\n", 1)
			},
			pkg:   "./cmd/acr",
			test:  "TestDocumentedCommands",
			names: []string{"cli.md:", "escapes executable console harness", "(text fence)"},
		},
		{
			name: "console fence tagged non-executable",
			file: "docs/cli.md",
			rewrite: func(content string) string {
				return strings.Replace(content, "```console\n", "```console non-executable\n", 1)
			},
			pkg:   "./cmd/acr",
			test:  "TestDocumentedCommands",
			names: []string{"cli.md:33", "console fence cannot be non-executable"},
		},
		{
			name: "undocumented parsed flag",
			file: "internal/cli/parse.go",
			rewrite: func(content string) string {
				return strings.Replace(content,
					"\t\tcase \"--help\", \"-h\":",
					"\t\tcase \"--verbose\":\n\t\tcase \"--help\", \"-h\":", 1)
			},
			pkg:   "./internal/cli",
			test:  "TestCLIReferenceMatchesCommandSurface",
			names: []string{"does not document parsed flag --verbose"},
		},
		{
			name: "parsed flag hidden by documented prefix",
			file: "internal/cli/parse.go",
			rewrite: func(content string) string {
				return strings.Replace(content,
					"\t\tcase \"--help\", \"-h\":",
					"\t\tcase \"--pi\":\n\t\tcase \"--help\", \"-h\":", 1)
			},
			pkg:   "./internal/cli",
			test:  "TestCLIReferenceMatchesCommandSurface",
			names: []string{"does not document parsed flag --pi"},
		},
		{
			name:   "unregistered machine-readable code",
			file:   "internal/cli/injected_adversarial.go",
			create: true,
			rewrite: func(string) string {
				return "package cli\n\n" +
					"type injectedAdversarial struct{ Code string }\n\n" +
					"var _ = injectedAdversarial{Code: \"surprise_code\"}\n"
			},
			pkg:   "./internal/cli",
			test:  "TestMachineReadableCodeRegistriesMatchSourceAndDocs",
			names: []string{"machine-readable code registry mismatch", "surprise_code"},
		},
		{
			name: "safety row with an empty undo cell",
			file: "docs/safety.md",
			rewrite: func(content string) string {
				return strings.Replace(content,
					"| No undo is needed; the command is read-only |",
					"|  |", 1)
			},
			pkg:   "./internal/cli",
			test:  "TestSafetyMatrixMatchesMutatingCommandSurface",
			names: []string{"acr list", "has no contract"},
		},
		{
			name: "renamed link anchor",
			file: "docs/publishing.md",
			rewrite: func(content string) string {
				return strings.Replace(content, "## Dual-publishing\n", "## Dual-publishing contract\n", 1)
			},
			pkg:   "./internal/release",
			test:  "TestDocumentationRelativeLinksAndAnchorsResolve",
			names: []string{"publishing.md", `absent anchor "dual-publishing"`},
		},
	}
}

func TestHarnessesRejectInjectedDocumentationDefects(t *testing.T) {
	module := copyModule(t)
	for _, defect := range injections() {
		t.Run(defect.name, func(t *testing.T) {
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
		})
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	exitError, ok := err.(*exec.ExitError)
	if ok {
		*target = exitError
	}
	return ok
}

// inject writes one defect and returns the function that restores the file. The
// rewrite must change the file: a rewrite that silently matched nothing would
// make the case pass vacuously when the documentation moves.
func inject(t *testing.T, module string, defect injection) func() {
	t.Helper()
	filename := filepath.Join(module, filepath.FromSlash(defect.file))
	if defect.create {
		if _, err := os.Stat(filename); err == nil {
			t.Fatalf("%s already exists; choose an unused injection path", defect.file)
		}
		if err := os.WriteFile(filename, []byte(defect.rewrite("")), 0o644); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Remove(filename); err != nil {
				t.Errorf("restore %s: %v", defect.file, err)
			}
		}
	}
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	injected := defect.rewrite(string(original))
	if injected == string(original) {
		t.Fatalf("injection %q changed nothing in %s; its anchor text has moved", defect.name, defect.file)
	}
	if err := os.WriteFile(filename, []byte(injected), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(filename, original, info.Mode().Perm()); err != nil {
			t.Errorf("restore %s: %v", defect.file, err)
		}
	}
}

// copyModule reproduces every tracked file in a scratch directory, skipping this
// package so the child test run cannot re-enter it.
func copyModule(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	listing, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("list tracked files: %v", err)
	}
	destination := t.TempDir()
	for _, relative := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		if relative == "" || strings.HasPrefix(relative, "internal/docsharness/") {
			continue
		}
		source := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Stat(source)
		if err != nil {
			t.Fatalf("stat %s: %v", relative, err)
		}
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	return destination
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve adversarial harness path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
