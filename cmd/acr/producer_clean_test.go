package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const cleanPublisher = `name: Publish
on:
  push:
    branches: [main]
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: tesslio/patch-version-publish@v1
        with:
          token: ${{ secrets.TESSL_TOKEN }}
          path: packages/compass
  test:
    runs-on: ubuntu-latest
    steps:
      - run: ./tests/check.sh
`

func cleanProducerFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	journeyGit(t, root, "init", "-q", "-b", "main")
	reverify2Put(t, root, "packages/compass/.tessl-plugin/plugin.json", `{"name":"legacy/compass","version":"1.2.3","repository":"https://github.com/legacy/compass","skills":["skills/router","skills/answer"],"rules":["rules/context.md"],"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/start.sh"]}]}]}}`+"\n", 0o644)
	reverify2Put(t, root, "packages/compass/skills/router/SKILL.md", "---\nname: router\ndescription: Route to the bundled helper.\n---\n# Router\nRun `.tessl/plugins/legacy/compass/skills/router/run.sh`.\nForeign `.tessl/plugins/foreign/unchanged/skills/helper/run.sh`.\n", 0o644)
	reverify2Put(t, root, "packages/compass/skills/router/run.sh", "#!/bin/sh\nset -eu\nexec .tessl/plugins/legacy/compass/skills/answer/check.sh\n", 0o751)
	reverify2Put(t, root, "packages/compass/skills/answer/SKILL.md", "---\nname: answer\ndescription: Report the fixed answer.\n---\n# Answer\nRun `skills/answer/check.sh`.\n", 0o644)
	reverify2Put(t, root, "packages/compass/skills/answer/check.sh", "#!/bin/sh\nset -eu\nprintf 'compass:42\\n'\n", 0o751)
	reverify2Put(t, root, "packages/compass/skills/answer/data.txt", "answer=42\n", 0o640)
	reverify2Put(t, root, "packages/compass/skills/answer/LICENSE", "Original compass author. Retain this notice.\n", 0o644)
	reverify2Put(t, root, "packages/compass/rules/context.md", "---\nalwaysApply: true\n---\n# Context\nRun `skills/answer/check.sh`.\n", 0o644)
	reverify2Put(t, root, "packages/compass/hooks/start.sh", "#!/bin/sh\nset -eu\nprintf 'compass-hook\\n'\n", 0o755)
	reverify2Put(t, root, ".github/workflows/publish.yml", cleanPublisher, 0o644)
	reverify2Put(t, root, "AGENTS.md", "User-owned agent instructions.\n", 0o644)
	reverify2Put(t, root, "tessl.json", `{"dependencies":{"foreign/unchanged":{"version":"4.5.6"}}}`+"\n", 0o644)
	reverify2Put(t, root, ".tessl/plugins/foreign/unchanged/untouched", "foreign bytes\n", 0o644)
	return root
}

// TestCleanProducerCLIPublishRoundtrip starts with the shipped binary. Network
// operations use the existing composed-subprocess HTTP lane with the production
// GitHub client; publication creates the actual assets installed by consumers.
func TestCleanProducerCLIPublishRoundtrip(t *testing.T) {
	const target = "example/observatory"
	binary := journeyBuiltBinary(t)
	github := newJourneyGitHub(t)
	producer := newJourneyProject(t, github)
	root := cleanProducerFixture(t)
	selected := filepath.Join(root, "packages/compass")
	args := []string{"migrate", "tessl-plugin", selected, "--acr-only", "--repository", "https://github.com/" + target, "--package-version", "5.6.7", "--json"}
	before := snapshotProjectTree(t, root)
	dry := producer.runBinary(binary, 0, append(append([]string{}, args...), "--dry-run")...)
	report := journeyResult(t, dry.stdout)
	if report["package"] != target || report["version"] != "5.6.7" || report["wrote"] != false {
		t.Fatalf("preview=%#v", report)
	}
	if !strings.Contains(dry.stdout, "packages/compass/skills/router") || !strings.Contains(dry.stdout, "beforeMode") || !strings.Contains(dry.stdout, "publishedFiles") {
		t.Fatal("preview omits reviewable delta")
	}
	assertTreeUnchanged(t, before, root, "clean migration dry-run")
	applied := producer.runBinary(binary, 0, args...)
	if journeyResult(t, applied.stdout)["wrote"] != true {
		t.Fatal("apply wrote nothing")
	}
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if value.Name != target || value.Version != "5.6.7" || value.Source.TesslIdentity != "" {
		t.Fatalf("manifest=%+v", value)
	}
	if _, err := os.Lstat(filepath.Join(root, "packages/compass/.tessl-plugin/plugin.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("selected manifest was not retired")
	}
	assertCleanMode(t, root, "packages/compass/skills/answer/check.sh", 0o751)
	assertCleanMode(t, root, "packages/compass/skills/answer/data.txt", 0o640)
	assertFileBody(t, root, "AGENTS.md", "User-owned agent instructions.\n")
	assertFileBody(t, root, ".tessl/plugins/foreign/unchanged/untouched", "foreign bytes\n")
	assertFileBody(t, root, "tessl.json", `{"dependencies":{"foreign/unchanged":{"version":"4.5.6"}}}`+"\n")
	if body, err := os.ReadFile(filepath.Join(root, ".github/workflows/publish.yml")); err != nil || !strings.Contains(string(body), "      - run: ./tests/check.sh\n") {
		t.Fatal("independent tests changed")
	}
	settled := snapshotProjectTree(t, root)
	repeated := producer.runBinary(binary, 0, args...)
	if journeyResult(t, repeated.stdout)["wrote"] != false || journeyResult(t, repeated.stdout)["current"] != true {
		t.Fatal("rerun not current")
	}
	assertTreeUnchanged(t, settled, root, "clean migration rerun")

	// No edits, chmod, fixture patching or replacement files occur between
	// conversion and publication. Git performs its documented mode normalization.
	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Convert fixture through the CLI")
	journeyGit(t, root, "tag", "v5.6.7")
	commit := journeyGit(t, root, "rev-parse", "HEAD")
	github.PublishSource(target, "v5.6.7", commit, journeyGitSourceArchive(t, root, "v5.6.7", commit))
	producer.runSubprocess(0, "publish", root, "--dry-run", "--json")
	producer.runSubprocess(0, "publish", root, "--json")
	releases := github.Repository(target).Releases
	if len(releases) != 1 || releases[0].Draft || len(releases[0].Assets) != 3 {
		t.Fatalf("publication=%+v", releases)
	}
	foundArchive := false
	for _, asset := range releases[0].Assets {
		if !strings.HasSuffix(asset.Name, ".tar.gz") {
			continue
		}
		foundArchive = true
		zr, err := gzip.NewReader(bytes.NewReader(asset.Bytes))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(zr)
		modes := map[string]int64{}
		for {
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(header.Name, "observatory-5.6.7/") {
				t.Fatalf("unexpected package archive root: %s", header.Name)
			}
			modes[strings.TrimPrefix(header.Name, "observatory-5.6.7/")] = header.Mode
		}
		if err := zr.Close(); err != nil {
			t.Fatal(err)
		}
		for name, mode := range map[string]int64{"packages/compass/skills/answer/check.sh": 0o755, "packages/compass/skills/answer/data.txt": 0o644} {
			if modes[name] != mode {
				t.Fatalf("published %s mode=%o, want %o; entries=%v", name, modes[name], mode, modes)
			}
		}
		if _, included := modes[".acr-producer-migration.json"]; included {
			t.Fatal("private receipt was published")
		}
	}
	if !foundArchive {
		t.Fatal("publication did not create a package archive")
	}

	consumer := newJourneyProject(t, github)
	consumer.runSubprocess(0, "init", "--agent", "claude-code", "--agent", "codex", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	consumer.runSubprocess(0, "install", "github:"+target, "--non-interactive")
	consumer.runSubprocess(0, "realize")
	consumer.runSubprocess(0, "check")
	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		router := nativeSkillDirectory(agent, target, "router")
		answer := nativeSkillDirectory(agent, target, "answer")
		body := readProjectFile(t, consumer, router+"/SKILL.md")
		if !strings.Contains(body, ".tessl/plugins/foreign/unchanged/skills/helper/run.sh") || strings.Contains(body, ".tessl/plugins/legacy/compass") {
			t.Fatalf("%s reference ownership lost: %s", agent, body)
		}
		assertCleanMode(t, consumer.root, router+"/run.sh", 0o755)
		assertCleanMode(t, consumer.root, answer+"/check.sh", 0o755)
		assertCleanMode(t, consumer.root, answer+"/data.txt", 0o644)
		assertFileBody(t, consumer.root, answer+"/LICENSE", "Original compass author. Retain this notice.\n")
		run := exec.Command(filepath.Join(consumer.root, router, "run.sh"))
		run.Dir = consumer.root
		run.Env = []string{"PATH="} // No Tessl, chmod repair, or interpreter prefix.
		output, err := run.CombinedOutput()
		if err != nil || string(output) != "compass:42\n" {
			t.Fatalf("direct %s helper=%q %v", agent, output, err)
		}
	}
	consumer.runSubprocess(0, "check")
}

func assertCleanMode(t *testing.T, root, name string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode=%o, want %o", name, info.Mode().Perm(), want)
	}
}

func TestCleanProducerCLIUsageAndRefusal(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	for _, args := range [][]string{
		{"migrate", "tessl-plugin", "--acr-only"},
		{"migrate", "tessl-plugin", "--package-version", "1.2.3"},
		{"migrate", "tessl", "--acr-only"},
		{"migrate", "tessl", "--package-version", "1.2.3"},
		{"publish", "--acr-only"},
		{"install", "--package-version", "1.2.3"},
		{"migrate", "tessl-plugin", "--acr-only=false"},
	} {
		result := project.runBinary(binary, 2, append(args, "--json")...)
		if journeyError(t, result.stderr)["code"] != "usage" {
			t.Fatalf("usage=%s", result.stderr)
		}
	}
	root := cleanProducerFixture(t)
	reverify2Put(t, root, "packages/compass/skills/router/config.py", "open('tessl.json', 'w').write('{}')\n", 0o644)
	before := snapshotProjectTree(t, root)
	for _, dry := range []bool{true, false} {
		args := []string{"migrate", "tessl-plugin", filepath.Join(root, "packages/compass"), "--acr-only", "--repository", "https://github.com/example/refused", "--json"}
		if dry {
			args = append(args, "--dry-run")
		}
		result := project.runBinary(binary, 1, args...)
		if journeyError(t, result.stderr)["code"] != "unsupported_semantic_conversion" || !strings.Contains(result.stderr, "config.py") {
			t.Fatalf("refusal=%s", result.stderr)
		}
		assertTreeUnchanged(t, before, root, "unsupported CLI conversion")
	}
}

// Fixture bytes are adopted unchanged from the lead's immutable mode-000 probe.
func TestCorrection14CleanCLINativeModeRefusal(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getuid() == 0 {
		t.Skip("actual read ACL requires nonroot macOS")
	}
	binary := journeyBuiltBinary(t)
	root := t.TempDir()
	reverify2Put(t, root, ".tessl-plugin/plugin.json", `{"name": "origin/demo", "version": "1.2.3", "skills": ["skills/check"]}`+"\n", 0644)
	reverify2Put(t, root, "skills/check/SKILL.md", "# Check\nRead `.tessl/plugins/origin/demo/skills/check/reference.md`.\n", 0644)
	reverify2Put(t, root, "skills/check/reference.md", "Ordinary support.\n", 0644)
	filename := filepath.Join(root, "skills/check/SKILL.md")
	user, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chmod", "+a", "user:"+strings.TrimSpace(string(user))+" allow read", filename).CombinedOutput(); err != nil {
		t.Fatalf("ACL setup: %v %s", err, out)
	}
	if err := os.Chmod(filename, 0); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotProjectTree(t, root)
	acl, err := exec.Command("ls", "-lde", filename).Output()
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	for _, phase := range []string{"preview", "apply-request", "repeat"} {
		t.Run(phase, func(t *testing.T) {
			args := []string{"migrate", "tessl-plugin", "--acr-only", "--repository", "https://github.com/destination/demo", "--json", "--project", root}
			if phase == "preview" {
				args = append(args, "--dry-run")
			}
			command := exec.Command(binary, args...)
			command.Env = append(os.Environ(), "TMPDIR="+stage)
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("refusal exit: %v %s %s", err, &stdout, &stderr)
			}
			var envelope struct {
				Error  struct{ Code, Message, Field string }
				Result struct{ Wrote, Current bool }
			}
			if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "unsupported_file_mode" || envelope.Error.Field != "skills/check/SKILL.md" || envelope.Result.Wrote || envelope.Result.Current {
				t.Fatalf("refusal: %s", &stderr)
			}
			assertTreeUnchanged(t, before, root, "mode000 refusal")
			current, err := os.Stat(filename)
			if err != nil || !os.SameFile(original, current) || current.Mode().Perm() != 0 {
				t.Fatal("original inode/mode changed")
			}
			currentACL, err := exec.Command("ls", "-lde", filename).Output()
			if err != nil || !bytes.Equal(acl, currentACL) {
				t.Fatal("original ACL changed")
			}
			entries, err := os.ReadDir(stage)
			if err != nil || len(entries) != 0 {
				t.Fatal("unexpected validation stage")
			}
			t.Logf("uid=%d: %s typed refusal, full inventory/inode/ACL intact", os.Getuid(), phase)
		})
	}
}
