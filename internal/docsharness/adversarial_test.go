// Package docsharness holds the negative proofs for the documentation
// harnesses. The positive tests in cmd/acr, internal/cli, and internal/release
// assert that the committed documentation matches the shipped code. They cannot
// assert the converse: that the harness still rejects documentation the code has
// outgrown. A guard narrowed until it accepts everything stays green.
//
// Each case below copies working-tree contents of tracked and nonignored new files into scratch space,
// injects exactly one defect the tester plan names, and runs only the harness
// test that must reject it. The harness runs as the pipeline runs it, so a
// narrowed guard reds here.
package docsharness

import (
	"fmt"
	"io/fs"
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
	// fault locates the injected expression in the resulting source.
	fault string
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
			names: []string{"cli.md:34", "console fence cannot be non-executable"},
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
			name: "deferred capability without issue URL",
			file: "README.md",
			rewrite: func(content string) string {
				return strings.Replace(content,
					"\n## CLI\n",
					"\n- Browser integrations remain deferred.\n\n## CLI\n", 1)
			},
			pkg:   "./internal/migrate",
			test:  "TestReadmeNamesEveryUncoveredTesslTree",
			names: []string{"deferred capability bullet has no issue URL: - Browser integrations"},
			fault: "- Browser integrations remain deferred.",
		},
		{
			name: "undocumented machine-readable code",
			file: "internal/cli/codes.go",
			rewrite: func(content string) string {
				return strings.Replace(content,
					"\t\"usage\",\n",
					"\t\"surprise_code\",\n\t\"usage\",\n", 1)
			},
			pkg:   "./internal/cli",
			test:  "TestMachineReadableCodeRegistriesMatchDocs",
			names: []string{"documented refusal codes mismatch", "surprise_code"},
		},
		{
			name: "misspelled named error code",
			file: "internal/migrateapp/service.go",
			rewrite: func(content string) string {
				return strings.Replace(content, `namedError("vendor_collision",`, `namedError("vendor_collison",`, 1)
			},
			pkg:   "./internal/cli",
			test:  "TestMachineReadableCodeRegistriesMatchDocs",
			names: []string{`refusal: unregistered code "vendor_collison"`},
			fault: `"vendor_collison"`,
		},
		{
			name: "computed code local",
			file: "internal/migrateapp/application.go",
			rewrite: func(content string) string {
				return strings.Replace(content, `code := "migrate_failed"`, `code := fmt.Sprint("arbitrary", "_code")`, 1)
			},
			pkg:   "./internal/cli",
			test:  "TestMachineReadableCodeRegistriesMatchDocs",
			names: []string{"refusal: cannot prove code expression"},
			fault: `fmt.Sprint("arbitrary", "_code")`,
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
	module := copyModule(t, moduleRoot(t))
	for _, defect := range injections() {
		t.Run(defect.name, func(t *testing.T) {
			checkInjection(t, module, defect)
			if defect.fault != "" {
				t.Run("comment moves diagnostic", func(t *testing.T) {
					originalRewrite := defect.rewrite
					defect.rewrite = func(content string) string {
						changed := originalRewrite(content)
						if changed == content {
							return content
						}
						return "// Harmless source comment.\n" + changed
					}
					checkInjection(t, module, defect)
				})
			}
		})
	}
}

func checkInjection(t *testing.T, module string, defect injection) {
	t.Helper()
	restore := inject(t, module, defect)
	defer restore()
	names := append([]string(nil), defect.names...)
	if defect.fault != "" {
		content, err := os.ReadFile(filepath.Join(module, defect.file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(content), defect.fault) != 1 {
			t.Fatalf("fault %q must occur exactly once", defect.fault)
		}
		offset := strings.Index(string(content), defect.fault)
		location := fmt.Sprintf("%s:%d: ", defect.file, 1+strings.Count(string(content[:offset]), "\n"))
		for i := range names {
			names[i] = location + names[i]
		}
	}
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
	for _, name := range names {
		if !strings.Contains(string(output), name) {
			t.Errorf("%s failure does not name %q\n%s", defect.test, name, output)
		}
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

// copyModule reproduces indexed and non-ignored untracked working-tree files,
// skipping this package so the child test run cannot re-enter it. New packages
// must be available to child builds before the candidate is staged or committed.
func copyModule(t *testing.T, root string) string {
	t.Helper()
	listing, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		t.Fatalf("list working-tree files: %v", err)
	}
	destination := t.TempDir()
	for _, relative := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		if relative == "" || strings.HasPrefix(relative, "internal/docsharness/") {
			continue
		}
		source := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Stat(source)
		if os.IsNotExist(err) {
			// Unstaged deletions are part of the candidate under test.
			continue
		}
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

func TestCopyModuleUsesNonIgnoredWorkingTreeFiles(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, output)
		}
	}
	write := func(name, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	indexed := map[string]string{
		"go.mod":                "module fixture.example/docs\n\ngo 1.25\n",
		"internal/cli/parse.go": "package cli\n",
		"docs/cli reference.md": "indexed documentation\n",
		".gitignore":            "*.ignored\n",
	}
	for name, content := range indexed {
		write(name, content)
	}
	write("internal/docsharness/adversarial_test.go", "package docsharness\n")
	git("add", "--all")
	// Copy current bytes of indexed filenames, rather than the index blobs.
	indexed["docs/cli reference.md"] = "working-tree documentation\n"
	write("docs/cli reference.md", indexed["docs/cli reference.md"])
	write("internal/cli/untracked.go", "malformed Go source\n")
	indexed["internal/cli/untracked.go"] = "malformed Go source\n"
	write("internal/diagnostic/error.go", "package diagnostic\n")
	indexed["internal/diagnostic/error.go"] = "package diagnostic\n"
	write("local.ignored", "ignored input\n")
	destination := copyModule(t, root)
	copied := map[string]string{}
	if err := filepath.WalkDir(destination, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(destination, filename)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		copied[filepath.ToSlash(relative)] = string(content)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(copied) != len(indexed) {
		t.Fatalf("copied %d files, want exactly %d non-ignored non-harness inputs", len(copied), len(indexed))
	}
	for name, content := range indexed {
		if copied[name] != content {
			t.Fatalf("copied %s = %q, want current working-tree file %q", name, copied[name], content)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve adversarial harness path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
