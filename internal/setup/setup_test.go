package setup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func projectWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func detectIn(t *testing.T, root string) []string {
	t.Helper()
	snapshot, err := adapter.NewRootSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	detected, err := Detect(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return detected
}

func TestDetectReportsEveryAgentInUse(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		files map[string]string
		want  []string
	}{
		"empty project": {files: map[string]string{}},
		"codex only":    {files: map[string]string{"AGENTS.md": "# Agents\n"}, want: []string{"codex"}},
		"claude only":   {files: map[string]string{"CLAUDE.md": "# Claude\n"}, want: []string{"claude-code"}},
		"cursor rules":  {files: map[string]string{".cursor/rules/user.mdc": "---\n---\n"}, want: []string{"cursor"}},
		"all three": {files: map[string]string{
			"AGENTS.md": "# Agents\n", ".claude/settings.json": "{}\n", ".cursor/hooks.json": "{}\n",
		}, want: []string{"claude-code", "codex", "cursor"}},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := projectWith(t, test.files)
			before := treeNames(t, root)

			got := detectIn(t, root)

			if len(got) != len(test.want) || (len(got) != 0 && !reflect.DeepEqual(got, test.want)) {
				t.Fatalf("Detect() = %#v, want %#v", got, test.want)
			}
			if after := treeNames(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("Detect() changed the project tree:\n before %#v\n after  %#v", before, after)
			}
		})
	}
}

func TestConfiguredReportsOnlyARegularProjectFile(t *testing.T) {
	t.Parallel()

	empty := t.TempDir()
	configured, err := Configured(empty)
	if err != nil || configured {
		t.Fatalf("Configured(empty) = %t, %v", configured, err)
	}

	root := projectWith(t, map[string]string{dependency.ProjectFilename: "schemaVersion: 2\nagents: []\n"})
	configured, err = Configured(root)
	if err != nil || !configured {
		t.Fatalf("Configured(project) = %t, %v", configured, err)
	}
}

func TestApplyWritesOnceAndPreservesTheRestOfTheProject(t *testing.T) {
	t.Parallel()

	root := projectWith(t, map[string]string{
		dependency.ProjectFilename: "schemaVersion: 2\nagents:\n  - codex\nexperimental:\n  owner: someone\n",
	})
	selection := Selection{Agents: []string{"cursor", "claude-code"}, Freshness: "install"}

	dry, err := Apply(root, selection, true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.Changed || !reflect.DeepEqual(dry.Agents, []string{"claude-code", "cursor"}) {
		t.Fatalf("Apply(dry-run) = %#v", dry)
	}
	if stored, err := Stored(root); err != nil || !reflect.DeepEqual(stored.Agents, []string{"codex"}) {
		t.Fatalf("Apply(dry-run) wrote %#v, %v", stored, err)
	}

	applied, err := Apply(root, selection, false)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Changed || applied.Freshness != "install" {
		t.Fatalf("Apply() = %#v", applied)
	}
	written, err := os.ReadFile(filepath.Join(root, dependency.ProjectFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "owner: someone") {
		t.Fatalf("Apply() dropped an unknown top-level field: %s", written)
	}

	again, err := Apply(root, selection, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Fatalf("Apply() is not idempotent: %#v", again)
	}
}

func treeNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	if err := filepath.Walk(root, func(name string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return relErr
		}
		names = append(names, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return names
}
