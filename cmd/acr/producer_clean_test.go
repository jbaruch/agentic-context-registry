package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

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

func TestProducerAgentFlagsReturnExactUsage(t *testing.T) {
	binary := journeyBuiltBinary(t)
	stateHome := t.TempDir()
	for _, tc := range []struct{ target, agent, message string }{
		{"tessl-plugin", "codex", "--agent is no longer supported by acr migrate tessl-plugin: ACR no longer runs agents. Rerun without --agent; files that need manual migration are listed in the refusal."},
		{"tessl-plugin", "claude", "--agent is no longer supported by acr migrate tessl-plugin: ACR no longer runs agents. Rerun without --agent; files that need manual migration are listed in the refusal."},
		{"tessl", "codex", "--agent is not supported by acr migrate tessl: consumer migration selects agents from existing Tessl native output. Rerun without --agent; after migration, select realization agents in agents.yaml or with acr init --agent NAME."},
		{"tessl", "claude", "--agent is not supported by acr migrate tessl: consumer migration selects agents from existing Tessl native output. Rerun without --agent; after migration, select realization agents in agents.yaml or with acr init --agent NAME."},
		{"tessl", "claude-code", "--agent is not supported by acr migrate tessl: consumer migration selects agents from existing Tessl native output. Rerun without --agent; after migration, select realization agents in agents.yaml or with acr init --agent NAME."},
		{"tessl", "anything", "--agent is not supported by acr migrate tessl: consumer migration selects agents from existing Tessl native output. Rerun without --agent; after migration, select realization agents in agents.yaml or with acr init --agent NAME."},
		{"tessl-plugin", "claude-code", "--agent is not supported by migrate; run 'acr migrate --help' for supported options"},
		{"tessl-plugin", "cursor", "--agent is not supported by migrate; run 'acr migrate --help' for supported options"},
		{"tessl-plugin", "unknown", "--agent is not supported by migrate; run 'acr migrate --help' for supported options"},
	} {
		for _, clean := range []bool{false, true} {
			for _, inline := range []bool{false, true} {
				for _, structured := range []bool{false, true} {
					args := []string{"migrate", tc.target}
					if clean {
						args = append(args, "--acr-only")
					}
					if inline {
						args = append(args, "--agent="+tc.agent)
					} else {
						args = append(args, "--agent", tc.agent)
					}
					want := "acr migrate: " + tc.message + "\n"
					if structured {
						args = append(args, "--json")
						encoded, err := json.Marshal(tc.message)
						if err != nil {
							t.Fatal(err)
						}
						want = `{"ok":false,"command":"migrate","error":{"code":"usage","message":` + string(encoded) + `}}` + "\n"
					}
					stdout, stderr, exit := hostileRunBinary(t, binary, stateHome, strings.NewReader(""), args...)
					if exit != 2 || stdout != "" || stderr != want {
						t.Fatalf("%v: exit=%d stdout=%q stderr=%q; want exit=2 stderr=%q", args, exit, stdout, stderr, want)
					}
				}
			}
		}
	}
	stdout, stderr, exit := hostileRunBinary(t, binary, stateHome, strings.NewReader(""), "migrate", "tessl-plugin", "--accept-agent-widening")
	const removed = "acr migrate: unknown flag \"--accept-agent-widening\" for migrate; run 'acr migrate --help' for supported options\n"
	if exit != 2 || stdout != "" || stderr != removed {
		t.Fatalf("removed flag: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	stdout, stderr, exit = hostileRunBinary(t, binary, stateHome, strings.NewReader(""), "migrate", "--help")
	if exit != 0 || stderr != "" || strings.Contains(stdout, "--agent") || strings.Contains(stdout, "--accept-agent-widening") {
		t.Fatalf("migration help advertises removed options: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

func TestProducerRefusalListsEveryFileBeforeWrites(t *testing.T) {
	binary := journeyBuiltBinary(t)
	root := t.TempDir()
	stateHome := t.TempDir()
	journeyGit(t, root, "init", "-q", "-b", "main")
	reverify2Put(t, root, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","skills":["skills/check"]}`+"\n", 0o644)
	reverify2Put(t, root, "skills/check/SKILL.md", "# Check\nRun `tessl login`.\n", 0o644)
	reverify2Put(t, root, "skills/check/config.py", "from pathlib import Path\nstate = Path('tessl.json')\n", 0o644)
	reverify2Put(t, root, "skills/check/install.sh", "#!/bin/sh\ntessl install example/manual\n", 0o755)
	reverify2Put(t, root, ".github/CODEOWNERS", "# tessl install example/manual\n* @example\n", 0o644)
	reverify2Put(t, root, ".github/workflows/review.yml", "on: pull_request\njobs:\n  review:\n    runs-on: ubuntu-latest\n    steps:\n      - run: tessl review run\n", 0o644)
	before := snapshotProjectTree(t, root)
	const first = "Tessl-dependent file outside the recognized standalone publisher requires manual migration"
	const operation = "unknown or custom Tessl command requires manual migration; Tessl command/dependency operation has no deterministic semantic translation"
	const workflow = "Tessl-dependent workflow is not the recognized standalone publisher; its commands and policy require manual migration"
	const config = "Tessl configuration/manifest reference requires manual migration; state, pins and rollback cannot be inferred"
	const headline = "5 file(s) need manual migration; see the list below"
	const want = "acr migrate: " + headline + "\n" +
		"Clean conversion refused before writes:\n" +
		"  .github/CODEOWNERS: " + first + "\n" +
		"  .github/workflows/review.yml: " + workflow + "\n" +
		"  skills/check/SKILL.md: " + operation + "\n" +
		"  skills/check/config.py: " + config + "\n" +
		"  skills/check/install.sh: " + operation + "\n" +
		"Migrate every listed file manually with your own agent in your own sandbox, then rerun deterministic conversion. ACR does not run agents.\n"
	for _, dry := range []bool{false, true} {
		args := []string{"migrate", "tessl-plugin", root, "--acr-only", "--repository", "https://github.com/example/manual"}
		if dry {
			args = append(args, "--dry-run")
		}
		stdout, stderr, exit := hostileRunBinary(t, binary, stateHome, strings.NewReader(""), args...)
		if exit != 1 || stdout != "" || stderr != want {
			t.Fatalf("dry=%t: exit=%d stdout=%q stderr=%q; want stderr=%q", dry, exit, stdout, stderr, want)
		}
		assertTreeUnchanged(t, before, root, "complete deterministic refusal")
		stdout, stderr, exit = hostileRunBinary(t, binary, stateHome, strings.NewReader(""), append(args, "--json")...)
		if exit != 1 || stdout != "" {
			t.Fatalf("JSON refusal: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
		}
		var envelope struct {
			Result map[string]json.RawMessage `json:"result"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
			t.Fatal(err)
		}
		const blockers = `[{"path":".github/CODEOWNERS","reason":"` + first + `"},{"path":".github/workflows/review.yml","reason":"` + workflow + `"},{"path":"skills/check/SKILL.md","reason":"` + operation + `"},{"path":"skills/check/config.py","reason":"` + config + `"},{"path":"skills/check/install.sh","reason":"` + operation + `"}]`
		if string(envelope.Result["blockers"]) != blockers || envelope.Error.Code != "unsupported_semantic_conversion" || envelope.Error.Message != headline || string(envelope.Result["wrote"]) != "false" {
			t.Fatalf("incomplete JSON refusal: %s", stderr)
		}
		for _, removed := range []string{"agentRuns", "policyChanges"} {
			if _, exists := envelope.Result[removed]; exists {
				t.Fatalf("dead report field %s", removed)
			}
		}
		assertTreeUnchanged(t, before, root, "complete JSON refusal")
	}
}

func TestProducerMixedMappingRefusalListsEveryFileBeforeWrites(t *testing.T) {
	binary := journeyBuiltBinary(t)
	for _, tc := range []struct {
		name, field, path, reason string
		secondHook                bool
	}{
		{"native-hook", `"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/stop.sh"]}]}]}},`, "hooks/stop.sh", "nativeHooks for hooks/stop.sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion", false},
		{"two-native-hooks", `"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/stop.sh"]},{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/stop2.sh"]}]}]}},`, "hooks/stop.sh", "nativeHooks for hooks/stop.sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion", true},
		{"private-package", `"private":true,`, ".tessl-plugin/plugin.json", "private: true cannot be published through public GitHub releases; set private to false or drop it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, stateHome := t.TempDir(), t.TempDir()
			journeyGit(t, root, "init", "-q", "-b", "main")
			reverify2Put(t, root, ".tessl-plugin/plugin.json", `{`+tc.field+`"name":"example/manual","version":"1.2.3","skills":["skills/check"]}`+"\n", 0o644)
			reverify2Put(t, root, "hooks/stop.sh", "#!/bin/sh\nexit 0\n", 0o755)
			if tc.secondHook {
				reverify2Put(t, root, "hooks/stop2.sh", "#!/bin/sh\nexit 0\n", 0o755)
			}
			reverify2Put(t, root, "skills/check/SKILL.md", "# Check\nRead `skills/check/paths.md`.\n", 0o644)
			reverify2Put(t, root, "skills/check/config.py", "from pathlib import Path\nstate = Path('tessl.json')\n", 0o644)
			reverify2Put(t, root, "skills/check/paths.md", "Run `skills/check/missing.sh`.\n", 0o644)
			before := snapshotProjectTree(t, root)
			const config = "Tessl configuration/manifest reference requires manual migration; state, pins and rollback cannot be inferred"
			const reference = "owned reference \"skills/check/missing.sh`.\" is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require manual migration"
			findings := []map[string]string{{"path": tc.path, "reason": tc.reason}}
			if tc.secondHook {
				findings = append(findings, map[string]string{"path": "hooks/stop2.sh", "reason": strings.ReplaceAll(tc.reason, "stop.sh", "stop2.sh")})
			}
			findings = append(findings, []map[string]string{
				{"path": "skills/check/config.py", "reason": config},
				{"path": "skills/check/paths.md", "reason": reference},
			}...)
			headline := fmt.Sprintf("%d file(s) need manual migration; see the list below", len(findings))
			want := "acr migrate: " + headline + "\nClean conversion refused before writes:\n"
			for _, finding := range findings {
				want += "  " + finding["path"] + ": " + finding["reason"] + "\n"
			}
			want += "Migrate every listed file manually with your own agent in your own sandbox, then rerun deterministic conversion. ACR does not run agents.\n"
			blockers, err := json.Marshal(findings)
			if err != nil {
				t.Fatal(err)
			}
			for _, dry := range []bool{false, true} {
				for _, structured := range []bool{false, true} {
					args := []string{"migrate", "tessl-plugin", root, "--acr-only", "--repository", "https://github.com/example/manual"}
					if dry {
						args = append(args, "--dry-run")
					}
					if structured {
						args = append(args, "--json")
					}
					stdout, stderr, exit := hostileRunBinary(t, binary, stateHome, strings.NewReader(""), args...)
					if exit != 1 || stdout != "" {
						t.Fatalf("dry=%t json=%t: exit=%d stdout=%q stderr=%q", dry, structured, exit, stdout, stderr)
					}
					if structured {
						var envelope struct {
							Result struct {
								Blockers []map[string]string `json:"blockers"`
								Wrote    bool                `json:"wrote"`
							} `json:"result"`
							Error struct {
								Code, Message string
							} `json:"error"`
						}
						if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
							t.Fatal(err)
						}
						actual, err := json.Marshal(envelope.Result.Blockers)
						if err != nil || !bytes.Equal(actual, blockers) || envelope.Result.Wrote || envelope.Error.Code != "unsupported_semantic_conversion" || envelope.Error.Message != headline {
							t.Fatalf("incomplete mixed JSON refusal: %s (%v)", stderr, err)
						}
					} else if stderr != want {
						t.Fatalf("dry=%t: stderr=%q; want %q", dry, stderr, want)
					}
					assertTreeUnchanged(t, before, root, "mixed mapping and per-file refusal")
				}
			}
		})
	}
}

func TestProducerPerFileFirstReasonBeforeWrites(t *testing.T) {
	binary := journeyBuiltBinary(t)
	// Let the newly built executable complete its first platform startup before
	// applying the short timeout to each migration regression.
	if stdout, stderr, exit := hostileRunBinary(t, binary, t.TempDir(), strings.NewReader(""), "--version"); exit != 0 || stdout == "" || stderr != "" {
		t.Fatalf("start built binary: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	for _, name := range []string{"unicode-emphasis", "unicode-token", "workflow-dependencies", "matcher-hooks", "nested-combined"} {
		t.Run(name, func(t *testing.T) {
			root, stateHome := t.TempDir(), t.TempDir()
			journeyGit(t, root, "init", "-q", "-b", "main")
			selected := root
			var findings []map[string]string
			switch name {
			case "unicode-emphasis", "unicode-token":
				reverify2Put(t, root, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","skills":["é/check"]}`+"\n", 0o644)
				source := "Read **é/check/missing.sh**.\n"
				reason := `owned reference "é/check/missing.sh**." is in an unsupported context; the file requires manual migration`
				if name == "unicode-token" {
					source = "Read `é/check/missing.sh`.\n"
					reason = "owned reference \"é/check/missing.sh`.\" is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require manual migration"
				}
				reverify2Put(t, root, "é/check/SKILL.md", "# Check\n"+source+"Read `é/check/another.sh`.\n", 0o644)
				reverify2Put(t, root, "é/check/config.py", "from pathlib import Path\nstate = Path('tessl.json')\n", 0o644)
				findings = []map[string]string{
					{"path": "é/check/SKILL.md", "reason": reason},
					{"path": "é/check/config.py", "reason": "Tessl configuration/manifest reference requires manual migration; state, pins and rollback cannot be inferred"},
				}
			case "workflow-dependencies":
				reverify2Put(t, root, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","skills":["skills/check"]}`+"\n", 0o644)
				reverify2Put(t, root, "skills/check/SKILL.md", "# Check\n", 0o644)
				publisher := strings.Replace(cleanPublisher, "path: packages/compass", "path: .", 1)
				publisher = strings.Replace(publisher, "  publish:\n", "  publish:\n    name: 123\n", 1)
				publisher = strings.Replace(publisher, "  test:\n", "  test:\n    needs: publish\n", 1)
				publisher += "  second:\n    needs: publish\n    runs-on: ubuntu-latest\n    steps: []\n"
				reverify2Put(t, root, ".github/workflows/publish.yml", publisher, 0o644)
				reverify2Put(t, root, ".github/workflows/review.yml", "name: Review\njobs:\n  review:\n    steps:\n      - run: tessl review\n", 0o644)
				findings = []map[string]string{
					{"path": ".github/workflows/publish.yml", "reason": "unsupported Tessl workflow/review policy: job test has dependencies; the file requires manual migration to preserve its policy"},
					{"path": ".github/workflows/review.yml", "reason": "Tessl-dependent workflow is not the recognized standalone publisher; its commands and policy require manual migration"},
				}
			case "matcher-hooks":
				reverify2Put(t, root, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","skills":["skills/check"],"nativeHooks":{"claude-code":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/one.sh","tessl.json"]},{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/two.sh","tessl.json"]}]}],"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/three.sh","--one"]}]}]},"codex":{"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/three.sh","--two"]}]}]}}}`+"\n", 0o644)
				reverify2Put(t, root, "skills/check/SKILL.md", "# Check\n", 0o644)
				for _, hook := range []string{"one", "two", "three"} {
					reverify2Put(t, root, "hooks/"+hook+".sh", "#!/bin/sh\nexit 0\n", 0o755)
				}
				findings = []map[string]string{
					{"path": ".tessl-plugin/plugin.json", "reason": `hook matcher "Bash" has no agent-plugin.yaml field; move the restriction into the hook script or drop the matcher`},
					{"path": "hooks/one.sh", "reason": "nativeHooks for hooks/one.sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion"},
					{"path": "hooks/two.sh", "reason": "nativeHooks for hooks/two.sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion"},
					{"path": "hooks/three.sh", "reason": "hook command bodies for hooks/three.sh on stop diverge; keep one command form or move the consensus into hooks"},
				}
			case "nested-combined":
				selected = filepath.Join(root, "plugins/orbit")
				reverify2Put(t, selected, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","private":true,"rules":["rules/one.md","rules/two.md","rules/scoped.md"],"skills":["skills/check"],"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/one.sh","tessl.json"]},{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/two.sh"]}]}]}}}`+"\n", 0o644)
				reverify2Put(t, selected, "skills/check/SKILL.md", "# Check\nRun **skills/check/missing.sh**.\n", 0o644)
				reverify2Put(t, selected, "rules/scoped.md", "---\nalwaysApply: false\napplyTo: tessl.json — Tessl config\n---\n# Scoped\n", 0o644)
				findings = []map[string]string{
					{"path": "plugins/orbit/.tessl-plugin/plugin.json", "reason": "private: true cannot be published through public GitHub releases; set private to false or drop it"},
					{"path": "plugins/orbit/skills/check/SKILL.md", "reason": `owned reference "skills/check/missing.sh**." is in an unsupported context; the file requires manual migration`},
					{"path": "plugins/orbit/rules/scoped.md", "reason": "Tessl configuration/manifest reference requires manual migration; state, pins and rollback cannot be inferred"},
				}
				for _, file := range []string{"one", "two"} {
					reverify2Put(t, selected, "rules/"+file+".md", "# Rule\n", 0o644)
					reverify2Put(t, selected, "hooks/"+file+".sh", "#!/bin/sh\nexit 0\n", 0o755)
					findings = append(findings,
						map[string]string{"path": "plugins/orbit/rules/" + file + ".md", "reason": "rule rules/" + file + ".md has no YAML frontmatter; set alwaysApply and convert again"},
						map[string]string{"path": "plugins/orbit/hooks/" + file + ".sh", "reason": "nativeHooks for hooks/" + file + ".sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion"})
				}
				for _, file := range []string{"NOTICE", "plugins/orbit/LICENSE"} {
					reverify2Put(t, root, file, "Preserve this notice.\n", 0o644)
					findings = append(findings, map[string]string{"path": file, "reason": "required license/notice file is outside the published inventory; place it at a supported root license path or inside a declared artifact before clean conversion"})
				}
			}
			sort.Slice(findings, func(i, j int) bool { return findings[i]["path"] < findings[j]["path"] })
			headline := fmt.Sprintf("%d file(s) need manual migration; see the list below", len(findings))
			want := "acr migrate: " + headline + "\nClean conversion refused before writes:\n"
			for _, finding := range findings {
				want += "  " + finding["path"] + ": " + finding["reason"] + "\n"
			}
			want += "Migrate every listed file manually with your own agent in your own sandbox, then rerun deterministic conversion. ACR does not run agents.\n"
			blockers, err := json.Marshal(findings)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotProjectTree(t, root)
			for _, dry := range []bool{false, true} {
				for _, structured := range []bool{false, true} {
					args := []string{"migrate", "tessl-plugin", selected, "--acr-only", "--repository", "https://github.com/example/manual"}
					if dry {
						args = append(args, "--dry-run")
					}
					if structured {
						args = append(args, "--json")
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					command := exec.CommandContext(ctx, binary, args...)
					command.Env = environmentWith("ACR_STATE_HOME", stateHome)
					command.Stdin = strings.NewReader("")
					stdout, stderr, exit := hostileRunCommand(t, command)
					contextErr := ctx.Err()
					cancel()
					if contextErr != nil || exit != 1 || stdout != "" {
						t.Fatalf("dry=%t json=%t: context=%v exit=%d stdout=%q stderr=%q", dry, structured, contextErr, exit, stdout, stderr)
					}
					if structured {
						var envelope struct {
							Result struct {
								Blockers []map[string]string `json:"blockers"`
								Wrote    bool                `json:"wrote"`
							} `json:"result"`
							Error struct{ Code, Message string } `json:"error"`
						}
						if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
							t.Fatal(err)
						}
						actual, err := json.Marshal(envelope.Result.Blockers)
						if err != nil || !bytes.Equal(actual, blockers) || envelope.Result.Wrote || envelope.Error.Code != "unsupported_semantic_conversion" || envelope.Error.Message != headline {
							t.Fatalf("per-file JSON refusal: %s (%v); want %s", stderr, err, blockers)
						}
					} else if stderr != want {
						t.Fatalf("dry=%t: stderr=%q; want %q", dry, stderr, want)
					}
					assertTreeUnchanged(t, before, root, "first reason per file refusal")
				}
			}
		})
	}
}

func TestProducerLicenseInventoryCollisionRemainsInvalidPath(t *testing.T) {
	root, stateHome := t.TempDir(), t.TempDir()
	reverify2Put(t, root, "LICENSE", "MIT\n", 0o644)
	if _, err := os.Stat(filepath.Join(root, "license")); err == nil {
		t.Skip("LICENSE and license/pkg require a case-sensitive volume; the MapFS regression runs on every host")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	binary := journeyBuiltBinary(t)
	journeyGit(t, root, "init", "-q", "-b", "main")
	packageRoot := filepath.Join(root, "license/pkg")
	reverify2Put(t, packageRoot, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","skills":["skills/check"]}`+"\n", 0o644)
	reverify2Put(t, packageRoot, "skills/check/SKILL.md", "# Check\n", 0o644)
	before := snapshotProjectTree(t, root)
	// Exact text and error fields captured from base f9e92b73.
	const message = `manifest validation failed: invalid_path at license/pkg/skills/check/SKILL.md: distribution path "license/pkg/skills/check/SKILL.md" occupies "license", which conflicts with "LICENSE" under canonical normalization and Unicode full case folding (including file/directory occupancy); rename the conflicting or unsafe source path and update its declarations before retrying; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists`
	for _, dry := range []bool{false, true} {
		for _, structured := range []bool{false, true} {
			args := []string{"migrate", "tessl-plugin", packageRoot, "--acr-only", "--repository", "https://github.com/example/manual"}
			if dry {
				args = append(args, "--dry-run")
			}
			if structured {
				args = append(args, "--json")
			}
			stdout, stderr, exit := hostileRunBinary(t, binary, stateHome, strings.NewReader(""), args...)
			if exit != 1 || stdout != "" {
				t.Fatalf("dry=%t json=%t: exit=%d stdout=%q stderr=%q", dry, structured, exit, stdout, stderr)
			}
			if structured {
				var envelope struct {
					Result struct {
						Blockers []json.RawMessage
						Wrote    bool
					}
					Error struct{ Code, Message, Field string }
				}
				if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Error.Code != "invalid_path" || envelope.Error.Message != message || envelope.Error.Field != "license/pkg/skills/check/SKILL.md" || len(envelope.Result.Blockers) != 0 || envelope.Result.Wrote {
					t.Fatalf("inventory failure became a semantic refusal: %s", stderr)
				}
			} else if stderr != "acr migrate: "+message+"\n" {
				t.Fatalf("dry=%t: stderr=%q", dry, stderr)
			}
			assertTreeUnchanged(t, before, root, "invalid distribution inventory")
		}
	}
}

func TestProducerOperationalErrorsPreserveBaseGuidance(t *testing.T) {
	binary := journeyBuiltBinary(t)
	for _, tc := range []struct{ code, path, content, reason string }{
		{"transaction_conflict", ".acr-producer-transaction", "occupied", "another migration or interrupted transaction exists; inspect its backups before retrying"},
		{"receipt_conflict", ".acr-producer-migration.json", "{", "invalid receipt; restore the original receipt or source checkout: unexpected EOF"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			root, stateHome := t.TempDir(), t.TempDir()
			journeyGit(t, root, "init", "-q", "-b", "main")
			reverify2Put(t, root, ".tessl-plugin/plugin.json", `{"name":"example/manual","version":"1.2.3","skills":["skills/check"]}`+"\n", 0o644)
			reverify2Put(t, root, "skills/check/SKILL.md", "# Check\n", 0o644)
			reverify2Put(t, root, tc.path, tc.content, 0o644)
			before := snapshotProjectTree(t, root)
			want := tc.reason + "; retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
			for _, dry := range []bool{false, true} {
				for _, structured := range []bool{false, true} {
					args := []string{"migrate", "tessl-plugin", root, "--acr-only", "--repository", "https://github.com/example/manual"}
					if dry {
						args = append(args, "--dry-run")
					}
					if structured {
						args = append(args, "--json")
					}
					stdout, stderr, exit := hostileRunBinary(t, binary, stateHome, strings.NewReader(""), args...)
					if exit != 1 || stdout != "" {
						t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
					}
					if structured {
						var envelope struct {
							Result struct {
								Blockers []json.RawMessage
								Wrote    bool
							}
							Error struct{ Code, Message, Field string }
						}
						if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
							t.Fatal(err)
						}
						if envelope.Error.Code != tc.code || envelope.Error.Message != want || envelope.Error.Field != tc.path || len(envelope.Result.Blockers) != 0 || envelope.Result.Wrote {
							t.Fatalf("ordinary failure changed: %s", stderr)
						}
					} else if stderr != "acr migrate: "+want+"\n" {
						t.Fatalf("stderr=%q", stderr)
					}
					assertTreeUnchanged(t, before, root, "ordinary producer error")
				}
			}
		})
	}
}
