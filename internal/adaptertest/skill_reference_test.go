package adaptertest

import (
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const skillReferenceFixture = "skill-reference-boundary"

// nativeSkillsRoots is the skills root each shipped adapter realizes into.
// The test states them rather than deriving them, so a renderer that starts
// writing somewhere else fails here instead of following the test.
var nativeSkillsRoots = map[string]string{
	"claude-code": ".claude/skills",
	"codex":       ".codex/skills",
	"cursor":      ".cursor/skills",
}

// unsupportedSkillReferences are the fixture's Step 3 bullets. None of them
// addresses the installed skill tree, so realization must copy every one of
// them byte for byte.
var unsupportedSkillReferences = []string{
	"https://example.com/skills/advocate/scripts/check.sh",
	"vendor/skills/advocate/scripts/check.sh",
	"myskills/advocate/scripts/check.sh",
	"skills/advocate-archive/scripts/check.sh",
	".tessl/plugins/other-workspace/other-plugin/skills/unrelated/check.sh",
	".tessl/plugins/legacy-workspace/skills/advocate/scripts/check.sh",
}

// TestRealizedSkillCommandsExecute is the issue #92 regression. It realizes a
// package whose skills address one helper through both supported reference
// forms, then runs every command the realized skill files instruct an agent
// to run, from the project directory an agent runs it in. The defect it holds
// against produced a path that resolves nowhere, which no assertion over the
// rewriting rule itself would have caught: the rewriting was self-consistent
// and the file it named did not exist.
func TestRealizedSkillCommandsExecute(t *testing.T) {
	t.Parallel()
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			t.Parallel()
			project := realizeSkillReferenceFixture(t, native)
			skillsRoot, ok := nativeSkillsRoots[native.Descriptor().ID]
			if !ok {
				t.Fatalf("no skills root recorded for adapter %q", native.Descriptor().ID)
			}

			commands := instructedCommands(t, filepath.Join(project, filepath.FromSlash(skillsRoot)))
			if len(commands) != 4 {
				t.Fatalf("instructed commands = %d (%v), want the fixture's four helper invocations", len(commands), commands)
			}
			for _, command := range commands {
				fields := strings.Fields(command)
				if len(fields) != 2 {
					t.Fatalf("instructed command %q does not carry a program and one argument", command)
				}
				if !strings.HasPrefix(fields[0], skillsRoot+"/") {
					t.Fatalf("instructed command %q does not address the installed skill tree under %s", command, skillsRoot)
				}
				output, err := runFromProject(project, fields)
				if err != nil {
					t.Fatalf("run %q from the project directory: %v\n%s", command, err, output)
				}
				want := "{\"ok\":true,\"helper\":\"advocate-check\",\"argument\":\"" + fields[1] + "\"}\n"
				if output != want {
					t.Fatalf("run %q stdout = %q, want %q", command, output, want)
				}
			}
		})
	}
}

// TestRealizedSkillFilesPreserveUnsupportedReferences holds the other half of
// the contract: rebasing rewrites the supported forms and leaves every other
// byte where it was.
func TestRealizedSkillFilesPreserveUnsupportedReferences(t *testing.T) {
	t.Parallel()
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			t.Parallel()
			project := realizeSkillReferenceFixture(t, native)
			skillsRoot := nativeSkillsRoots[native.Descriptor().ID]
			advocate := filepath.Join(project, filepath.FromSlash(path.Join(skillsRoot, "acr__example__coexist__advocate", "SKILL.md")))
			realized, err := os.ReadFile(advocate)
			if err != nil {
				t.Fatal(err)
			}
			for _, reference := range unsupportedSkillReferences {
				if !bytes.Contains(realized, []byte(reference)) {
					t.Fatalf("realized %s dropped the unsupported reference %q:\n%s", advocate, reference, realized)
				}
			}
			if bytes.Contains(realized, []byte(".tessl/plugins/legacy-workspace/advocate-plugin/skills/advocate/")) {
				t.Fatalf("realized %s still addresses the Tessl-installed tree:\n%s", advocate, realized)
			}
			if bytes.Contains(realized, []byte("advocate-plugin/"+skillsRoot)) {
				t.Fatalf("realized %s spliced the native root into the Tessl-installed path:\n%s", advocate, realized)
			}

			source, err := os.ReadFile(filepath.Join("testdata", skillReferenceFixture, "package", "skills", "router", "references", "notes.md"))
			if err != nil {
				t.Fatal(err)
			}
			companion := filepath.Join(project, filepath.FromSlash(path.Join(skillsRoot, "acr__example__coexist__router", "references", "notes.md")))
			realizedCompanion, err := os.ReadFile(companion)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(realizedCompanion, source) {
				t.Fatalf("realized %s differs from its package bytes\n--- got ---\n%s--- want ---\n%s", companion, realizedCompanion, source)
			}
		})
	}
}

func realizeSkillReferenceFixture(t *testing.T, native adapter.Adapter) string {
	t.Helper()
	root := filepath.Join("testdata", skillReferenceFixture, "package")
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := adapter.Package{Source: "github:" + loaded.Name, Root: os.DirFS(root), Manifest: loaded}
	project := t.TempDir()
	applyNativePackages(t, project, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, native)
	return project
}

// instructedCommands returns the command each realized SKILL.md tells an
// agent to run: the backquoted text on a line that begins with "Run `".
func instructedCommands(t *testing.T, skillsRoot string) []string {
	t.Helper()
	var commands []string
	err := filepath.WalkDir(skillsRoot, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(content), "\n") {
			const marker = "Run `"
			if !strings.HasPrefix(line, marker) {
				continue
			}
			command, _, found := strings.Cut(line[len(marker):], "`")
			if !found {
				t.Fatalf("%s has an unterminated command on line %q", current, line)
			}
			commands = append(commands, command)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return commands
}

func runFromProject(project string, fields []string) (string, error) {
	command := exec.Command(filepath.Join(project, filepath.FromSlash(fields[0])), fields[1:]...)
	command.Dir = project
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}
