package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

type documentedCommand struct {
	file       string
	line       int
	command    string
	fixture    string
	exitCode   int
	wantStdout string
}

type commandFixture struct {
	root   string
	remote *dependencytest.Remote
}

func TestDocumentedCommands(t *testing.T) {
	root := commandDocsRoot(t)
	commands := parseDocumentedCommands(t, root)
	if len(commands) < 10 {
		t.Fatalf("found %d executable documentation blocks, want at least 10", len(commands))
	}

	originalVersion, originalCommit := version, commit
	version, commit = "1.0.0", ""
	t.Cleanup(func() {
		version, commit = originalVersion, originalCommit
	})
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())

	for _, example := range commands {
		example := example
		t.Run(fmt.Sprintf("%s:%d", filepath.Base(example.file), example.line), func(t *testing.T) {
			fixture := newCommandFixture(t, example.fixture)
			before := snapshotCommandTree(t, fixture.root)
			arguments := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(example.command, "$ acr "), "$ ./acr "))

			workingDirectory, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(fixture.root); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chdir(workingDirectory); err != nil {
					t.Errorf("restore working directory: %v", err)
				}
			})

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWith(fixture.remote, strings.NewReader(""), &stdout, &stderr, arguments)
			if exitCode != example.exitCode {
				t.Fatalf("%s exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", example.command, exitCode, example.exitCode, stdout.String(), stderr.String())
			}
			compareDocumentedStdout(t, normalizeCommandOutput(stdout.String(), fixture.root), example.wantStdout)
			if strings.Contains(example.command, "--dry-run") {
				after := snapshotCommandTree(t, fixture.root)
				if !reflect.DeepEqual(after, before) {
					t.Errorf("dry-run changed fixture tree\nbefore=%v\nafter=%v", before, after)
				}
			}
		})
	}
}

func TestRunDelegatesToInjectableRemote(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(commandDocsRoot(t), "cmd", "acr", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, seam := range []string{
		"return runWith(dependency.NewGitHubClient(), stdin, stdout, stderr, args)",
		"func runWith(remote dependency.Remote, stdin io.Reader, stdout, stderr io.Writer, args []string) int",
	} {
		if !strings.Contains(text, seam) {
			t.Errorf("main command does not preserve injectable remote seam %q", seam)
		}
	}
}

func TestMigrationGuideCoversFreshConflictAndIgnoredState(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	remote, _, _ := docsRemote(t)
	mapping := "example/alpha=github:example/alpha@v1.0.0"

	t.Run("fresh consumer synthesizes state", func(t *testing.T) {
		root := t.TempDir()
		seedDocsTesslConsumer(t, root, true)
		stdout, _, exitCode := runDocsCLI(remote, "migrate", "tessl", "--project", root, "--dry-run", "--json", "--map", mapping)
		if exitCode != 0 {
			t.Fatalf("exit = %d, stdout = %s", exitCode, stdout)
		}
		var envelope struct {
			Result struct {
				Wrote   bool `json:"wrote"`
				Project struct {
					Agents []string `json:"agents"`
				} `json:"project"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.Wrote || len(envelope.Result.Project.Agents) == 0 {
			t.Fatalf("fresh dry-run result = %+v", envelope.Result)
		}
		if _, err := os.Stat(filepath.Join(root, dependency.ProjectFilename)); !os.IsNotExist(err) {
			t.Fatalf("fresh dry-run wrote %s: %v", dependency.ProjectFilename, err)
		}
	})

	t.Run("conflicting state refuses without writes", func(t *testing.T) {
		fixture := newCommandFixture(t, "tessl-conflict")
		before := snapshotCommandTree(t, fixture.root)
		_, stderr, exitCode := runDocsCLI(remote, "migrate", "tessl", "--project", fixture.root, "--dry-run", "--json", "--map", mapping)
		if exitCode != 1 || !strings.Contains(stderr, `"code":"project_state_conflict"`) {
			t.Fatalf("exit = %d, stderr = %s", exitCode, stderr)
		}
		if after := snapshotCommandTree(t, fixture.root); !reflect.DeepEqual(after, before) {
			t.Fatalf("conflict changed fixture\nbefore=%v\nafter=%v", before, after)
		}
	})

	t.Run("ignored state carries source line", func(t *testing.T) {
		root := t.TempDir()
		seedDocsTesslConsumer(t, root, true)
		writeDocsFile(t, root, ".gitignore", []byte("agents.yaml\n.agents/\n"), 0o644)
		runDocsGit(t, root, "init", "-q")
		stdout, stderr, exitCode := runDocsCLI(remote, "migrate", "tessl", "--project", root, "--dry-run", "--json", "--map", mapping)
		if exitCode != 0 || stderr != "" {
			t.Fatalf("exit = %d, stderr = %s", exitCode, stderr)
		}
		var envelope struct {
			Result struct {
				Notes []struct {
					Code      string `json:"code"`
					Path      string `json:"path"`
					IgnoredBy string `json:"ignoredBy"`
				} `json:"notes"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, note := range envelope.Result.Notes {
			if note.Code == "gitignored_state" && note.IgnoredBy != "" {
				seen[note.Path] = true
			}
		}
		for _, path := range []string{"agents.yaml", ".agents/registry.lock"} {
			if !seen[path] {
				t.Errorf("no gitignored_state note with source evidence for %s: %+v", path, envelope.Result.Notes)
			}
		}
	})
}

func runDocsCLI(remote dependency.Remote, arguments ...string) (string, string, int) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWith(remote, strings.NewReader(""), &stdout, &stderr, arguments)
	return stdout.String(), stderr.String(), exitCode
}

func parseDocumentedCommands(t *testing.T, root string) []documentedCommand {
	t.Helper()
	files := []string{filepath.Join(root, "README.md")}
	docFiles, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(docFiles)
	files = append(files, docFiles...)

	acrLine := regexp.MustCompile(`^\s*\$?\s*(?:\./)?acr(?:\s|$)`)
	var commands []documentedCommand
	for _, filename := range files {
		content, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(string(content), "\ufeff"), "\r\n", "\n"), "\n")
		for index := 0; index < len(lines); index++ {
			if !strings.HasPrefix(lines[index], "```") {
				continue
			}
			language := strings.TrimSpace(strings.TrimPrefix(lines[index], "```"))
			start := index + 1
			for index++; index < len(lines) && lines[index] != "```"; index++ {
			}
			if index == len(lines) {
				t.Fatalf("unterminated fence at %s:%d", filename, start)
			}
			body := lines[start:index]
			hasCommand := false
			for _, line := range body {
				hasCommand = hasCommand || acrLine.MatchString(line)
			}
			if !hasCommand {
				continue
			}
			if language != "console" {
				t.Errorf("acr command escapes executable console harness at %s:%d (%s fence)", filename, start+1, language)
				continue
			}
			commands = append(commands, parseConsoleCommand(t, filename, start+1, body))
		}
	}
	return commands
}

func parseConsoleCommand(t *testing.T, filename string, line int, body []string) documentedCommand {
	t.Helper()
	if len(body) == 0 || (!strings.HasPrefix(body[0], "$ acr ") && !strings.HasPrefix(body[0], "$ ./acr ")) {
		t.Fatalf("console block at %s:%d must start with '$ acr' or '$ ./acr'", filename, line)
	}
	example := documentedCommand{file: filename, line: line, command: body[0]}
	outputStart := 1
	for ; outputStart < len(body) && strings.HasPrefix(body[outputStart], "# "); outputStart++ {
		directive := body[outputStart]
		switch {
		case strings.HasPrefix(directive, "# fixture: "):
			example.fixture = strings.TrimSpace(strings.TrimPrefix(directive, "# fixture: "))
		case strings.HasPrefix(directive, "# exit: "):
			if _, err := fmt.Sscanf(directive, "# exit: %d", &example.exitCode); err != nil {
				t.Fatalf("invalid exit directive at %s:%d: %v", filename, line+outputStart, err)
			}
		case strings.HasPrefix(directive, "# stub:"):
			t.Fatalf("stubbed command is forbidden at %s:%d", filename, line+outputStart)
		default:
			t.Fatalf("unknown command directive at %s:%d: %s", filename, line+outputStart, directive)
		}
	}
	if example.fixture == "" {
		t.Fatalf("console block at %s:%d has no fixture", filename, line)
	}
	example.wantStdout = strings.TrimSpace(strings.Join(body[outputStart:], "\n"))
	return example
}

func newCommandFixture(t *testing.T, name string) commandFixture {
	t.Helper()
	root := t.TempDir()
	remote, archive, contentHash := docsRemote(t)
	fixture := commandFixture{root: root, remote: remote}
	switch name {
	case "bare":
	case "initialized":
		writeDocsState(t, root, dependency.State{
			Project: dependency.Project{SchemaVersion: dependency.BaselineSchemaVersion, Agents: []string{"codex"}, Freshness: "none"},
			Lock:    dependency.Lockfile{SchemaVersion: dependency.BaselineSchemaVersion},
		})
	case "github-installed":
		writeDocsState(t, root, docsInstalledState(contentHash, false))
	case "github-held":
		writeDocsState(t, root, docsInstalledState(contentHash, true))
	case "github-newer":
		state := docsInstalledState(contentHash, false)
		state.Lock.Dependencies[0].ReleaseID = 43
		state.Lock.Dependencies[0].Tag = "v1.0.1"
		state.Lock.Dependencies[0].Commit = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		state.Lock.Dependencies[0].PackageVersion = "1.0.1"
		writeDocsState(t, root, state)
	case "producer":
		writeDocsFile(t, root, ".tessl-plugin/plugin.json", []byte(`{"name":"example/alpha","version":"1.0.0","description":"alpha plugin","repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n"), 0o644)
		writeDocsFile(t, root, "rules/always.md", []byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	case "tessl-consumer", "tessl-conflict":
		seedDocsTesslConsumer(t, root, true)
		if name == "tessl-conflict" {
			writeDocsState(t, root, dependency.State{
				Project: dependency.Project{SchemaVersion: dependency.BaselineSchemaVersion, Agents: []string{"cursor"}},
				Lock:    dependency.Lockfile{SchemaVersion: dependency.BaselineSchemaVersion},
			})
		}
	case "tessl-unmapped":
		seedDocsTesslConsumer(t, root, false)
	case "tessl-ready":
		seedDocsTesslConsumer(t, root, true)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := runWith(remote, strings.NewReader(""), &stdout, &stderr, []string{
			"migrate", "tessl", "--project", root, "--map", "example/alpha=github:example/alpha@v1.0.0",
		})
		if exitCode != 0 {
			t.Fatalf("prepare finalization fixture: exit=%d\nstdout:\n%s\nstderr:\n%s", exitCode, stdout.String(), stderr.String())
		}
		runDocsGit(t, root, "init", "-q")
		runDocsGit(t, root, "config", "user.name", "ACR Docs")
		runDocsGit(t, root, "config", "user.email", "acr-docs@example.invalid")
		runDocsGit(t, root, "add", "-A")
		runDocsGit(t, root, "commit", "-q", "-m", "record coexistence")
	case "publisher":
		seedDocsPublisher(t, root)
		commit := strings.TrimSpace(runDocsGit(t, root, "rev-parse", "HEAD"))
		remote.TagCommits["github:example/alpha@v1.0.0"] = commit
	default:
		t.Fatalf("unknown documentation fixture %q", name)
	}
	_ = archive
	return fixture
}

func docsRemote(t *testing.T) (*dependencytest.Remote, []byte, string) {
	t.Helper()
	const commit = "ffffffffffffffffffffffffffffffffffffffff"
	archive := docsPackageArchive(t)
	extracted := t.TempDir()
	if err := dependency.ExtractPackageArchive(archive, extracted); err != nil {
		t.Fatal(err)
	}
	value, err := manifest.Load(extracted)
	if err != nil {
		t.Fatal(err)
	}
	contentHash, err := dependency.HashPackageFiles(extracted, value)
	if err != nil {
		t.Fatal(err)
	}
	remote := dependencytest.NewRemote()
	release := dependency.Release{ID: 42, Tag: "v1.0.0"}
	remote.Latest["github:example/alpha"] = release
	remote.Releases["github:example/alpha@v1.0.0"] = release
	remote.Commits["github:example/alpha@v1.0.0"] = commit
	remote.Archives["github:example/alpha@"+commit] = archive
	return remote, archive, contentHash
}

func docsInstalledState(contentHash string, held bool) dependency.State {
	declaration := dependency.Declaration{Source: "github:example/alpha", Requested: "latest"}
	locked := dependency.LockedDependency{
		Source: "github:example/alpha", Requested: "latest", Kind: dependency.ResolutionRelease,
		ReleaseID: 42, Tag: "v1.0.0", Commit: "ffffffffffffffffffffffffffffffffffffffff",
		PackageVersion: "1.0.0", ContentHash: contentHash,
	}
	if held {
		declaration.Hold = &dependency.Hold{Pin: "v1.0.0", Rejected: "v1.0.1", Reason: "compatibility"}
		locked.Hold = &dependency.LockHold{RejectedTag: "v1.0.1", RejectedReleaseID: 43, RejectedCommit: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	}
	return dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.BaselineSchemaVersion, Agents: []string{"codex"}, Freshness: "none", Dependencies: []dependency.Declaration{declaration}},
		Lock:    dependency.Lockfile{SchemaVersion: dependency.BaselineSchemaVersion, Dependencies: []dependency.LockedDependency{locked}},
	}
}

func seedDocsTesslConsumer(t *testing.T, root string, repository bool) {
	t.Helper()
	writeDocsFile(t, root, "tessl.json", []byte(`{"name":"consumer","mode":"vendored","dependencies":{"example/alpha":{"version":"1.0.0"}}}`+"\n"), 0o644)
	repositoryField := ""
	if repository {
		repositoryField = `,"repository":"https://github.com/example/alpha"`
	}
	plugin := []byte(`{"name":"example/alpha","version":"1.0.0"` + repositoryField + `,"rules":["rules/always.md"]}` + "\n")
	tile := []byte(`{"name":"example/alpha","version":"1.0.0","rules":{"always":{"rules":"rules/always.md"}}}` + "\n")
	writeDocsFile(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", plugin, 0o644)
	writeDocsFile(t, root, ".tessl/plugins/example/alpha/tile.json", tile, 0o644)
	writeDocsFile(t, root, ".tessl/plugins/example/alpha/rules/always.md", []byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeDocsFile(t, root, ".tessl/RULES.md", []byte("# Agent Rules\n\n@plugins/example/alpha/rules/always.md\n"), 0o644)
	writeDocsFile(t, root, ".codex/skills/operator/SKILL.md", []byte("# Operator skill\n"), 0o644)
}

func seedDocsPublisher(t *testing.T, root string) {
	t.Helper()
	manifest := "schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n"
	writeDocsFile(t, root, "agent-plugin.yaml", []byte(manifest), 0o644)
	writeDocsFile(t, root, "guidance.md", []byte("# Guidance\n"), 0o644)
	runDocsGit(t, root, "init", "-q")
	runDocsGit(t, root, "config", "user.name", "ACR Docs")
	runDocsGit(t, root, "config", "user.email", "acr-docs@example.invalid")
	runDocsGit(t, root, "add", "agent-plugin.yaml", "guidance.md")
	runDocsGit(t, root, "commit", "-q", "-m", "seed package")
	runDocsGit(t, root, "tag", "v1.0.0")
}

func writeDocsState(t *testing.T, root string, state dependency.State) {
	t.Helper()
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
}

func writeDocsFile(t *testing.T, root, relative string, content []byte, mode fs.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, mode); err != nil {
		t.Fatal(err)
	}
}

func docsPackageArchive(t *testing.T) []byte {
	t.Helper()
	files := map[string]struct {
		content string
		mode    int64
	}{
		"agent-plugin.yaml": {"schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\nartifacts:\n  rules:\n    - id: always\n      path: rules/always.md\n      activation:\n        mode: always\n", 0o644},
		"rules/always.md":   {"---\nalwaysApply: true\n---\n# Always\n", 0o644},
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range names {
		file := files[name]
		contents := []byte(file.content)
		header := &tar.Header{Name: "example-alpha-commit/" + name, Mode: file.mode, Size: int64(len(contents)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func runDocsGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func snapshotCommandTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		result[filepath.ToSlash(relative)] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func normalizeCommandOutput(output, root string) string {
	absolute, _ := filepath.Abs(root)
	resolved, _ := filepath.EvalSymlinks(absolute)
	for _, spelling := range []string{root, absolute, resolved} {
		if spelling != "" {
			output = strings.ReplaceAll(output, spelling, "PROJECT")
		}
	}
	return strings.TrimSpace(output)
}

func compareDocumentedStdout(t *testing.T, actual, expected string) {
	t.Helper()
	actual = trimTranscriptLineEnds(actual)
	expected = trimTranscriptLineEnds(expected)
	var actualJSON, expectedJSON any
	if json.Unmarshal([]byte(actual), &actualJSON) == nil && json.Unmarshal([]byte(expected), &expectedJSON) == nil {
		if !reflect.DeepEqual(actualJSON, expectedJSON) {
			t.Errorf("stdout JSON mismatch\ngot:  %s\nwant: %s", actual, expected)
		}
		return
	}
	if actual != expected {
		t.Errorf("stdout mismatch\ngot:\n%s\nwant:\n%s", actual, expected)
	}
}

func trimTranscriptLineEnds(value string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " \t")
	}
	return strings.Join(lines, "\n")
}

func commandDocsRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve command docs test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
