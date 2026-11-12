package publish

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func skillManifestYAML(rulePath string) string {
	return "schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  skills:\n    - id: advocate\n      path: skills/advocate\n  rules:\n    - id: guidance\n      path: " + rulePath + "\n      activation:\n        mode: always\n"
}

func TestPackageFilterLimitsSkillDirectoriesToGitsView(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeHostileFile(t, root, manifest.Filename, skillManifestYAML("guidance.md"))
	writeHostileFile(t, root, "guidance.md", "# Guidance\n")
	writeHostileFile(t, root, "skills/advocate/SKILL.md", "# Skill\n")
	writeHostileFile(t, root, "skills/advocate/runtime/worker.py", "print()\n")
	writeHostileFile(t, root, "skills/advocate/runtime/__pycache__/worker.cpython-311.pyc", "bytecode")
	writeHostileFile(t, root, "skills/advocate/untracked.md", "not ignored\n")
	source := &fakeGitSource{inside: true, visible: []string{
		"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md", "skills/advocate/runtime/worker.py", "skills/advocate/untracked.md",
	}}
	filter, err := packageFilter(context.Background(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.PackageFilesFiltered(root, mustLoad(t, root), filter)
	if err != nil {
		t.Fatal(err)
	}
	want := "agent-plugin.yaml guidance.md skills/advocate/SKILL.md skills/advocate/runtime/worker.py skills/advocate/untracked.md"
	if got := strings.Join(files, " "); got != want {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestPackageFilterIsAbsentOutsideAGitWorkTree(t *testing.T) {
	t.Parallel()

	filter, err := packageFilter(context.Background(), t.TempDir(), &fakeGitSource{})
	if err != nil || filter != nil {
		t.Fatalf("packageFilter() = %v, %v; want nil filter without a Git work tree", filter != nil, err)
	}
}

func TestBuilderIgnoresGitIgnoredSkillFilesAndRefusesTagTreeMismatchWithAccurateAdvice(t *testing.T) {
	t.Parallel()

	manifestContent := skillManifestYAML("guidance.md")
	commit := strings.Repeat("a", 40)
	tree := map[string]File{
		"agent-plugin.yaml":        {Path: "agent-plugin.yaml", Mode: 0o644, Content: []byte(manifestContent)},
		"guidance.md":              {Path: "guidance.md", Mode: 0o644, Content: []byte("# Guidance\n")},
		"skills/advocate/SKILL.md": {Path: "skills/advocate/SKILL.md", Mode: 0o644, Content: []byte("---\nname: advocate\ndescription: Advocate.\n---\n# Skill\n")},
	}
	build := func(t *testing.T, tree map[string]File, ignored map[string]bool) error {
		root := t.TempDir()
		writeHostileFile(t, root, manifest.Filename, manifestContent)
		writeHostileFile(t, root, "guidance.md", "# Guidance\n")
		writeHostileFile(t, root, "skills/advocate/SKILL.md", "# Skill\n")
		writeHostileFile(t, root, "skills/advocate/__pycache__/worker.cpython-311.pyc", "bytecode")
		source := &fakeGitSource{
			clean: true, head: commit, tags: []string{"v1.2.3"}, files: tree, ignored: ignored, inside: true,
			visible: []string{"agent-plugin.yaml", "guidance.md", "skills/advocate/SKILL.md"},
		}
		_, err := newBuilder(source, NewGate(), "acr test").Prepare(context.Background(), root)
		return err
	}

	t.Run("ignored bytecode never reaches the tag tree lookup", func(t *testing.T) {
		t.Parallel()
		if err := build(t, tree, nil); err != nil {
			t.Fatalf("ignored bytecode blocked preparation: %v", err)
		}
	})

	t.Run("tag tree lacks a declared file", func(t *testing.T) {
		t.Parallel()
		missing := map[string]File{"agent-plugin.yaml": tree["agent-plugin.yaml"], "skills/advocate/SKILL.md": tree["skills/advocate/SKILL.md"]}
		err := build(t, missing, nil)
		assertUnpublishable(t, err, "does not contain \"guidance.md\"", "commit the declared file")
		if strings.Contains(err.Error(), "commit every declared package file") {
			t.Fatalf("old advice survived: %v", err)
		}
	})

	t.Run("tag tree lacks a declared file that Git ignores", func(t *testing.T) {
		t.Parallel()
		missing := map[string]File{"agent-plugin.yaml": tree["agent-plugin.yaml"], "skills/advocate/SKILL.md": tree["skills/advocate/SKILL.md"]}
		err := build(t, missing, map[string]bool{"guidance.md": true})
		assertUnpublishable(t, err, "Git ignores \"guidance.md\"", "stop declaring it")
		if strings.Contains(err.Error(), "commit the declared file") || strings.Contains(err.Error(), "commit every") {
			t.Fatalf("advice tells the operator to commit an ignored file: %v", err)
		}
	})
}

func assertUnpublishable(t *testing.T, err error, wants ...string) {
	t.Helper()
	var refusal *Error
	if err == nil || !errors.As(err, &refusal) || refusal.Code != CodeUnpublishable {
		t.Fatalf("error = %v, want %s", err, CodeUnpublishable)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func mustLoad(t *testing.T, root string) manifest.Manifest {
	t.Helper()
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
