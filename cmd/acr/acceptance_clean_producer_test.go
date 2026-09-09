package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Independent acceptance tests for the clean producer mode (issue #117
// deterministic foundation). The fixtures deliberately differ from
// producer_clean_test.go: a package authored at the repository root, a helper
// nested two directories deep, an owner-only executable, a private data file,
// a NOTICE inside the skill tree, a publisher whose only job is publication
// and a foreign reference that shares the package's own skill path.

const acceptanceTarget = "newowner/lantern-acr"

const acceptancePublisherOnly = `name: Publish lantern
on:
  push:
    branches: ["main"]
permissions:
  contents: write
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - name: Check out
        uses: actions/checkout@v4
      - name: Publish
        uses: tesslio/patch-version-publish@v1
        with:
          token: ${{ secrets.TESSL_TOKEN }}
`

const acceptanceIndependentCI = "on: pull_request\njobs:\n  ci:\n    runs-on: ubuntu-latest\n    steps:\n      - run: ./tests/unit.sh\n"

const acceptanceProbeSkill = "---\nname: probe\ndescription: Probe the light.\n---\n# Probe\nRun `skills/probe/run.sh` with `--script=skills/light/scripts/bash/glow.sh`.\nRead [facts](skills/light/data/facts.txt).\nForeign `.tessl/plugins/other-ws/lantern/skills/light/scripts/bash/glow.sh` is not ours.\n"

const acceptanceUserInstructions = "User instructions. Installed via tessl install other-ws/lantern.\n"

const acceptanceNotice = "Lantern notice. Keep verbatim.\n"

const acceptanceNestedRepository = "https://github.com/newowner/telescope-acr"

const acceptanceLegacyFocusHelper = ".tessl/plugins/vendor/telescope/skills/focus/scripts/focus.sh"

const acceptanceNestedPublisher = `name: Publish
on:
  push:
    branches: [main]
permissions:
  contents: write
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: ./tests/lint.sh
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: tesslio/patch-version-publish@v1
        with:
          token: ${{ secrets.TESSL_TOKEN }}
          path: pkgs/telescope
  # Independent tests keep running on main pushes.
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: ./tests/run.sh
`

func acceptancePut(t *testing.T, root, relative, body string, mode os.FileMode) {
	t.Helper()
	reverify2Put(t, root, relative, body, mode)
	if err := os.Chmod(filepath.Join(root, filepath.FromSlash(relative)), mode); err != nil {
		t.Fatal(err)
	}
}

// acceptanceRootPackage authors a Tessl plugin at the repository root.
func acceptanceRootPackage(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	journeyGit(t, root, "init", "-q", "-b", "main")
	acceptancePut(t, root, ".tessl-plugin/plugin.json", `{"name":"old-ws/lantern","version":"0.4.2","description":"Lantern","license":"MIT","author":{"name":"Lantern Author"},"skills":["skills/probe","skills/light"],"rules":["rules/always.md"]}`+"\n", 0o644)
	acceptancePut(t, root, ".tesslignore", "tests/\n", 0o644)
	acceptancePut(t, root, "skills/probe/SKILL.md", acceptanceProbeSkill, 0o644)
	acceptancePut(t, root, "skills/probe/run.sh", "#!/bin/sh\nset -eu\nexec .tessl/plugins/old-ws/lantern/skills/light/scripts/bash/glow.sh \"$@\"\n", 0o700)
	acceptancePut(t, root, "skills/light/SKILL.md", "---\nname: light\ndescription: Glow.\n---\n# Light\nRun `skills/light/scripts/bash/glow.sh`.\n", 0o644)
	acceptancePut(t, root, "skills/light/scripts/bash/glow.sh", "#!/bin/sh\nset -eu\nprintf 'lantern:on\\n'\n", 0o755)
	acceptancePut(t, root, "skills/light/data/facts.txt", "facts\n", 0o600)
	acceptancePut(t, root, "skills/light/NOTICE", acceptanceNotice, 0o644)
	acceptancePut(t, root, "rules/always.md", "---\nalwaysApply: true\n---\n# Always\nUse `skills/light/scripts/bash/glow.sh`.\n", 0o644)
	acceptancePut(t, root, "tests/unit.sh", "#!/bin/sh\nsh skills/light/scripts/bash/glow.sh\n", 0o755)
	acceptancePut(t, root, ".github/workflows/publish.yml", acceptancePublisherOnly, 0o644)
	acceptancePut(t, root, ".github/workflows/ci.yml", acceptanceIndependentCI, 0o644)
	acceptancePut(t, root, "CLAUDE.md", acceptanceUserInstructions, 0o644)
	return root
}

// acceptanceNestedPackage authors a Tessl plugin under pkgs/telescope with a
// three-job publisher whose publish job sits between two independent jobs.
func acceptanceNestedPackage(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	journeyGit(t, root, "init", "-q", "-b", "main")
	acceptancePut(t, root, "pkgs/telescope/.tessl-plugin/plugin.json", `{"name":"vendor/telescope","version":"3.1.4","description":"Telescope","skills":["skills/lens","skills/focus"],"rules":["rules/observe.md"]}`+"\n", 0o644)
	acceptancePut(t, root, "pkgs/telescope/skills/lens/SKILL.md", "---\nname: lens\ndescription: Look through the lens.\n---\n# Lens\nRun `"+acceptanceLegacyFocusHelper+"`.\n", 0o644)
	acceptancePut(t, root, "pkgs/telescope/skills/focus/SKILL.md", "---\nname: focus\ndescription: Focus.\n---\n# Focus\nRun `skills/focus/scripts/focus.sh`.\n", 0o644)
	acceptancePut(t, root, "pkgs/telescope/skills/focus/scripts/focus.sh", "#!/bin/sh\nset -eu\nprintf 'telescope:focused\\n'\n", 0o755)
	acceptancePut(t, root, "pkgs/telescope/rules/observe.md", "---\nalwaysApply: true\n---\n# Observe\n", 0o644)
	acceptancePut(t, root, ".github/workflows/publish.yml", acceptanceNestedPublisher, 0o644)
	acceptancePut(t, root, "AGENTS.md", "Project instructions.\n", 0o644)
	return root
}

func acceptanceCleanArgs(selected, repository string, extra ...string) []string {
	return append([]string{"migrate", "tessl-plugin", selected, "--acr-only", "--repository", repository, "--json"}, extra...)
}

func acceptanceChanges(t *testing.T, result map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := result["changes"].([]any)
	if !ok {
		t.Fatalf("result has no changes list: %#v", result)
	}
	changes := map[string]map[string]any{}
	for _, item := range raw {
		change, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("change is not an object: %#v", item)
		}
		path, _ := change["path"].(string)
		changes[path] = change
	}
	return changes
}

// acceptanceAssertLanded proves every previewed change is exactly what apply
// wrote: bytes, removal and permission bits.
func acceptanceAssertLanded(t *testing.T, root string, changes map[string]map[string]any) {
	t.Helper()
	for path, change := range changes {
		full := filepath.Join(root, filepath.FromSlash(path))
		if change["operation"] == "remove" {
			if _, err := os.Lstat(full); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s should be removed: %v", path, err)
			}
			continue
		}
		body, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != change["after"] {
			t.Fatalf("%s differs from the preview", path)
		}
		mode, _ := change["afterMode"].(float64)
		assertCleanMode(t, root, path, os.FileMode(uint32(mode)))
	}
}

func acceptanceArchiveModes(t *testing.T, github *journeyGitHub, fullName, prefix string) map[string]int64 {
	t.Helper()
	releases := github.Repository(fullName).Releases
	if len(releases) != 1 || releases[0].Draft || len(releases[0].Assets) != 3 {
		t.Fatalf("publication=%+v", releases)
	}
	for _, asset := range releases[0].Assets {
		if !strings.HasSuffix(asset.Name, ".tar.gz") {
			continue
		}
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
			if !strings.HasPrefix(header.Name, prefix) {
				t.Fatalf("unexpected package archive root: %s", header.Name)
			}
			modes[strings.TrimPrefix(header.Name, prefix)] = header.Mode
		}
		if err := zr.Close(); err != nil {
			t.Fatal(err)
		}
		return modes
	}
	t.Fatal("publication did not create a package archive")
	return nil
}

// TestAcceptanceCleanRootPackageThroughAllAdapters covers criteria 1-5 on a
// root-authored package: deterministic preview, apply equal to preview, inert
// rerun, publication through the production client, installation and
// realization for all three adapters, and direct helper execution with an
// empty PATH. Nothing is edited, chmodded or prefixed between steps.
func TestAcceptanceCleanRootPackageThroughAllAdapters(t *testing.T) {
	binary := journeyBuiltBinary(t)
	github := newJourneyGitHub(t)
	producer := newJourneyProject(t, github)
	root := acceptanceRootPackage(t)
	repository := "https://github.com/" + acceptanceTarget
	args := acceptanceCleanArgs(root, repository)
	dry := acceptanceCleanArgs(root, repository, "--dry-run")
	before := snapshotProjectTree(t, root)
	first := producer.runBinary(binary, 0, dry...)
	second := producer.runBinary(binary, 0, dry...)
	if first.stdout != second.stdout {
		t.Fatal("dry-run output is not deterministic")
	}
	preview := journeyResult(t, first.stdout)
	if preview["package"] != acceptanceTarget || preview["version"] != "0.4.2" || preview["wrote"] != false || preview["current"] != false {
		t.Fatalf("preview=%#v", preview)
	}
	changes := acceptanceChanges(t, preview)
	for path, want := range map[string]string{
		".tessl-plugin/plugin.json":         "remove",
		".tesslignore":                      "remove",
		".github/workflows/publish.yml":     "remove",
		".github/workflows/acr-publish.yml": "create",
		"agent-plugin.yaml":                 "create",
		".acr-producer-migration.json":      "create",
		"skills/probe/run.sh":               "modify",
	} {
		if changes[path]["operation"] != want {
			t.Fatalf("%s operation=%v, want %s", path, changes[path]["operation"], want)
		}
	}
	if changes["skills/probe/run.sh"]["afterMode"] != float64(0o700) || changes[".acr-producer-migration.json"]["afterMode"] != float64(0o600) {
		t.Fatalf("preview modes: %v %v", changes["skills/probe/run.sh"]["afterMode"], changes[".acr-producer-migration.json"]["afterMode"])
	}
	for _, untouched := range []string{"CLAUDE.md", ".github/workflows/ci.yml", "skills/light/NOTICE", "skills/probe/SKILL.md", "skills/light/data/facts.txt", "tests/unit.sh"} {
		if _, planned := changes[untouched]; planned {
			t.Fatalf("preview plans a change to %s", untouched)
		}
	}
	assertTreeUnchanged(t, before, root, "clean root-package dry-run")

	applied := journeyResult(t, producer.runBinary(binary, 0, args...).stdout)
	if applied["wrote"] != true {
		t.Fatalf("apply=%#v", applied)
	}
	acceptanceAssertLanded(t, root, changes)
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if value.Name != acceptanceTarget || value.Version != "0.4.2" || value.Source.TesslIdentity != "" || value.Source.Repository != repository {
		t.Fatalf("manifest=%+v", value)
	}
	skillPaths := map[string]bool{}
	for _, skill := range value.Artifacts.Skills {
		skillPaths[skill.Path] = true
	}
	if !skillPaths["skills/probe"] || !skillPaths["skills/light"] || len(value.Artifacts.Rules) != 1 || value.Artifacts.Rules[0].Path != "rules/always.md" {
		t.Fatalf("artifacts=%+v", value.Artifacts)
	}
	assertFileBody(t, root, "skills/probe/run.sh", "#!/bin/sh\nset -eu\nexec skills/light/scripts/bash/glow.sh \"$@\"\n")
	assertCleanMode(t, root, "skills/probe/run.sh", 0o700)
	assertCleanMode(t, root, "skills/light/data/facts.txt", 0o600)
	assertCleanMode(t, root, ".acr-producer-migration.json", 0o600)
	assertFileBody(t, root, "CLAUDE.md", acceptanceUserInstructions)
	assertFileBody(t, root, ".github/workflows/ci.yml", acceptanceIndependentCI)
	assertFileBody(t, root, "skills/light/NOTICE", acceptanceNotice)
	assertFileBody(t, root, "skills/probe/SKILL.md", acceptanceProbeSkill)
	workflow := readProjectFile(t, &journeyProject{root: root}, ".github/workflows/acr-publish.yml")
	for _, want := range []string{"tags: ['v*']", "publish-package.yml@d3bc96b33b42293aecd1702c04aa94513a3dab1b", "path: .", "acr-version: v0.1.6"} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("tag workflow lacks %q:\n%s", want, workflow)
		}
	}
	rendered := readProjectFile(t, &journeyProject{root: root}, manifest.Filename)
	for _, want := range []string{`# Original license: "MIT"`, `# Original author.name: "Lantern Author"`} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("manifest lost provenance %q:\n%s", want, rendered)
		}
	}
	settled := snapshotProjectTree(t, root)
	rerun := journeyResult(t, producer.runBinary(binary, 0, args...).stdout)
	if rerun["wrote"] != false || rerun["current"] != true {
		t.Fatalf("rerun=%#v", rerun)
	}
	assertTreeUnchanged(t, settled, root, "clean rerun")
	if again := journeyResult(t, producer.runBinary(binary, 0, dry...).stdout); again["current"] != true || again["wrote"] != false {
		t.Fatalf("repeat dry-run=%#v", again)
	}

	// Between conversion and publication only Git runs: no edits, no chmod.
	journeyGit(t, root, "add", "-A")
	journeyGit(t, root, "commit", "-qm", "Convert lantern through the CLI")
	journeyGit(t, root, "tag", "v0.4.2")
	commit := journeyGit(t, root, "rev-parse", "HEAD")
	github.PublishSource(acceptanceTarget, "v0.4.2", commit, journeyGitSourceArchive(t, root, "v0.4.2", commit))
	producer.runSubprocess(0, "publish", root, "--dry-run", "--json")
	producer.runSubprocess(0, "publish", root, "--json")
	modes := acceptanceArchiveModes(t, github, acceptanceTarget, "lantern-acr-0.4.2/")
	for name, want := range map[string]int64{"skills/probe/run.sh": 0o755, "skills/light/scripts/bash/glow.sh": 0o755, "skills/light/data/facts.txt": 0o644, "skills/light/NOTICE": 0o644} {
		if modes[name] != want {
			t.Fatalf("published %s mode=%o, want %o; entries=%v", name, modes[name], want, modes)
		}
	}
	for _, private := range []string{".acr-producer-migration.json", "tests/unit.sh", "CLAUDE.md", ".github/workflows/acr-publish.yml", ".github/workflows/ci.yml"} {
		if _, included := modes[private]; included {
			t.Fatalf("%s was published", private)
		}
	}

	consumer := newJourneyProject(t, github)
	consumer.runSubprocess(0, "init", "--agent", "claude-code", "--agent", "codex", "--agent", "cursor", "--freshness", "none", "--non-interactive")
	consumer.runSubprocess(0, "install", "github:"+acceptanceTarget, "--non-interactive")
	consumer.runSubprocess(0, "realize")
	consumer.runSubprocess(0, "check")
	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		probe := nativeSkillDirectory(agent, acceptanceTarget, "probe")
		light := nativeSkillDirectory(agent, acceptanceTarget, "light")
		body := readProjectFile(t, consumer, probe+"/SKILL.md")
		if !strings.Contains(body, ".tessl/plugins/other-ws/lantern/skills/light/scripts/bash/glow.sh") || strings.Contains(body, ".tessl/plugins/old-ws/lantern") {
			t.Fatalf("%s reference ownership lost: %s", agent, body)
		}
		assertCleanMode(t, consumer.root, probe+"/run.sh", 0o755)
		assertCleanMode(t, consumer.root, light+"/scripts/bash/glow.sh", 0o755)
		assertCleanMode(t, consumer.root, light+"/data/facts.txt", 0o644)
		assertFileBody(t, consumer.root, light+"/NOTICE", acceptanceNotice)
		run := exec.Command(filepath.Join(consumer.root, probe, "run.sh"))
		run.Dir = consumer.root
		run.Env = []string{"PATH="} // No Tessl, no chmod repair, no interpreter prefix.
		output, err := run.CombinedOutput()
		if err != nil || string(output) != "lantern:on\n" {
			t.Fatalf("direct %s helper=%q %v", agent, output, err)
		}
	}
	consumer.runSubprocess(0, "check")
}

// TestAcceptanceCleanRefusalsWriteNothing covers the refusal boundaries of
// criteria 1, 3 and 4 through the shipped binary. Every refusal names the
// offending path, happens in both preview and apply, and writes nothing.
func TestAcceptanceCleanRefusalsWriteNothing(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	cases := []struct {
		name, code, path, repository, selected string
		prepare                                func(t *testing.T, root string)
	}{
		{name: "license outside the skill tree", code: "unsupported_semantic_conversion", path: "pkgs/telescope/LICENSE", prepare: func(t *testing.T, root string) {
			acceptancePut(t, root, "pkgs/telescope/LICENSE", "Apache License\n", 0o644)
		}},
		{name: "existing root manifest", code: "unsupported_semantic_conversion", path: "agent-plugin.yaml", prepare: func(t *testing.T, root string) {
			acceptancePut(t, root, "agent-plugin.yaml", "existing: manifest\n", 0o644)
		}},
		{name: "existing tag workflow", code: "unsupported_semantic_conversion", path: ".github/workflows/acr-publish.yml", prepare: func(t *testing.T, root string) {
			acceptancePut(t, root, ".github/workflows/acr-publish.yml", "existing: workflow\n", 0o644)
		}},
		{name: "dependent independent job", code: "unsupported_semantic_conversion", path: ".github/workflows/publish.yml", prepare: func(t *testing.T, root string) {
			acceptancePut(t, root, ".github/workflows/publish.yml", strings.Replace(acceptanceNestedPublisher, "  test:\n", "  test:\n    needs: publish\n", 1), 0o644)
		}},
		{name: "second authored package", code: "unsupported_semantic_conversion", path: "other/.tessl-plugin/plugin.json", prepare: func(t *testing.T, root string) {
			acceptancePut(t, root, "other/.tessl-plugin/plugin.json", `{"name":"other/one","version":"1.0.0"}`, 0o644)
		}},
		{name: "interrupted transaction", code: "transaction_conflict", path: ".acr-producer-transaction", prepare: func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, ".acr-producer-transaction"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "installed consumer selected", code: "unsafe_path", path: ".tessl/plugins/other/telescope", selected: ".tessl/plugins/other/telescope", prepare: func(t *testing.T, root string) {
			acceptancePut(t, root, ".tessl/plugins/other/telescope/skills/x/SKILL.md", "# x\n", 0o644)
		}},
		{name: "trailing slash repository", code: "invalid_package_name", path: "name", repository: acceptanceNestedRepository + "/"},
		{name: "uppercase repository", code: "invalid_package_name", path: "name", repository: "https://github.com/NewOwner/Telescope-ACR"},
		{name: "non-github repository", code: "invalid_source", path: "source.repository", repository: "https://gitlab.com/newowner/telescope-acr"},
		{name: "scheme-less repository", code: "invalid_source", path: "source.repository", repository: "github.com/newowner/telescope-acr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := acceptanceNestedPackage(t)
			if tc.prepare != nil {
				tc.prepare(t, root)
			}
			selected, repository := "pkgs/telescope", acceptanceNestedRepository
			if tc.selected != "" {
				selected = tc.selected
			}
			if tc.repository != "" {
				repository = tc.repository
			}
			before := snapshotProjectTree(t, root)
			for _, dry := range []bool{true, false} {
				args := acceptanceCleanArgs(filepath.Join(root, filepath.FromSlash(selected)), repository)
				if dry {
					args = append(args, "--dry-run")
				}
				result := project.runBinary(binary, 1, args...)
				failure := journeyError(t, result.stderr)
				if failure["code"] != tc.code {
					t.Fatalf("code=%v, want %s: %s", failure["code"], tc.code, result.stderr)
				}
				if !strings.Contains(result.stderr, tc.path) {
					t.Fatalf("refusal does not name %s: %s", tc.path, result.stderr)
				}
				assertTreeUnchanged(t, before, root, tc.name)
			}
			if _, err := os.Lstat(filepath.Join(root, ".acr-producer-migration.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal left a receipt: %v", err)
			}
		})
	}
}

// TestAcceptanceCleanReceiptBindsOutputAndOptions covers criterion 4's receipt
// contract: an inert rerun survives a Git commit of the converted checkout,
// while edited output, changed options and a receipt whose private mode was
// lost each refuse with receipt_conflict and write nothing.
func TestAcceptanceCleanReceiptBindsOutputAndOptions(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	convert := func(t *testing.T) (string, []string) {
		t.Helper()
		root := acceptanceNestedPackage(t)
		args := acceptanceCleanArgs(filepath.Join(root, "pkgs/telescope"), acceptanceNestedRepository, "--package-version", "4.0.0")
		if journeyResult(t, project.runBinary(binary, 0, args...).stdout)["version"] != "4.0.0" {
			t.Fatal("version override ignored")
		}
		return root, args
	}
	t.Run("inert after commit", func(t *testing.T) {
		root, args := convert(t)
		journeyGit(t, root, "add", "-A")
		journeyGit(t, root, "commit", "-qm", "Convert")
		settled := snapshotProjectTree(t, root)
		rerun := journeyResult(t, project.runBinary(binary, 0, args...).stdout)
		if rerun["current"] != true || rerun["wrote"] != false {
			t.Fatalf("rerun=%#v", rerun)
		}
		assertTreeUnchanged(t, settled, root, "inert rerun after commit")
	})
	for name, tamper := range map[string]func(t *testing.T, root string, args []string) []string{
		"edited output": func(t *testing.T, root string, args []string) []string {
			acceptancePut(t, root, "agent-plugin.yaml", "edited: true\n", 0o644)
			return args
		},
		"changed options": func(t *testing.T, root string, args []string) []string {
			return acceptanceCleanArgs(filepath.Join(root, "pkgs/telescope"), acceptanceNestedRepository, "--package-version", "4.0.1")
		},
		"receipt mode lost": func(t *testing.T, root string, args []string) []string {
			if err := os.Chmod(filepath.Join(root, ".acr-producer-migration.json"), 0o644); err != nil {
				t.Fatal(err)
			}
			return args
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, args := convert(t)
			args = tamper(t, root, args)
			before := snapshotProjectTree(t, root)
			for _, dry := range []bool{true, false} {
				run := args
				if dry {
					run = append(append([]string{}, args...), "--dry-run")
				}
				result := project.runBinary(binary, 1, run...)
				if journeyError(t, result.stderr)["code"] != "receipt_conflict" {
					t.Fatalf("refusal=%s", result.stderr)
				}
				assertTreeUnchanged(t, before, root, name)
			}
		})
	}
}

// TestAcceptanceCleanIdentityRejectsCloneSuffix pins criterion 1's identity
// derivation: the target identity is OWNER/REPO. The canonical clone URL ends
// in .git, which is not part of the repository name, so it must be refused or
// normalized rather than becoming part of the published identity.
func TestAcceptanceCleanIdentityRejectsCloneSuffix(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	root := acceptanceNestedPackage(t)
	before := snapshotProjectTree(t, root)
	args := append(acceptanceCleanArgs(filepath.Join(root, "pkgs/telescope"), acceptanceNestedRepository+".git", "--dry-run"), "--project", project.root)
	stdout, stderr, exit := hostileRunBinary(t, binary, project.stateHome, strings.NewReader(""), args...)
	assertTreeUnchanged(t, before, root, "clone-suffix dry-run")
	if exit != 0 {
		if journeyError(t, stderr)["code"] == "" {
			t.Fatalf("refusal without a code: %s", stderr)
		}
		return
	}
	if result := journeyResult(t, stdout); result["package"] != "newowner/telescope-acr" {
		t.Fatalf("a .git clone URL became package identity %q; OWNER/REPO must not carry the clone suffix", result["package"])
	}
}

// TestAcceptanceCleanOwnedLegacyPathNeverSurvivesSilently pins criterion 2's
// "unsupported owned paths refuse" boundary for published text. Once the
// clean output drops source.tesslIdentity, an owned `.tessl/plugins/<source>/`
// runtime path that the scanner did not rewrite is dead: the conversion must
// refuse naming the file, or the planned output must no longer carry it.
func TestAcceptanceCleanOwnedLegacyPathNeverSurvivesSilently(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	for name, body := range map[string]string{
		"bold markdown":  "---\nname: lens\ndescription: L.\n---\n# Lens\nRun **" + acceptanceLegacyFocusHelper + "** now.\n",
		"structured key": "---\nname: lens\ndescription: L.\n---\n# Lens\nhelper:" + acceptanceLegacyFocusHelper + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := acceptanceNestedPackage(t)
			acceptancePut(t, root, "pkgs/telescope/skills/lens/SKILL.md", body, 0o644)
			before := snapshotProjectTree(t, root)
			args := append(acceptanceCleanArgs(filepath.Join(root, "pkgs/telescope"), acceptanceNestedRepository, "--dry-run"), "--project", project.root)
			stdout, stderr, exit := hostileRunBinary(t, binary, project.stateHome, strings.NewReader(""), args...)
			assertTreeUnchanged(t, before, root, name)
			if exit != 0 {
				if !strings.Contains(stderr, "pkgs/telescope/skills/lens/SKILL.md") {
					t.Fatalf("refusal does not name the file: %s", stderr)
				}
				return
			}
			after, _ := acceptanceChanges(t, journeyResult(t, stdout))["pkgs/telescope/skills/lens/SKILL.md"]["after"].(string)
			if after == "" {
				after = body
			}
			if strings.Contains(after, ".tessl/plugins/vendor/telescope/") {
				t.Fatalf("%s: clean output still carries the owned legacy path after dropping source.tesslIdentity:\n%s", name, after)
			}
		})
	}
}
