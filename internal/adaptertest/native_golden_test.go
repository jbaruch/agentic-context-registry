package adaptertest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestAllAgentsGolden(t *testing.T) {
	runNativeGoldenMatrix(t, "all-agents")
}

func TestFreshnessSessionStartGolden(t *testing.T) {
	runNativeGoldenMatrix(t, "freshness-session-start")
}

func TestSharedFilesImportGolden(t *testing.T) {
	runSharedFilesGolden(t, "shared-files-import", func(t *testing.T, project string) {
		t.Helper()
		sandwichImport(t, filepath.Join(project, "CLAUDE.md"))
		sandwichBlock(t, filepath.Join(project, "AGENTS.md"), "Agents")
	})
}

func TestSharedFilesSeparateGolden(t *testing.T) {
	runSharedFilesGolden(t, "shared-files-separate", func(t *testing.T, project string) {
		t.Helper()
		sandwichBlock(t, filepath.Join(project, "CLAUDE.md"), "Claude")
		sandwichBlock(t, filepath.Join(project, "AGENTS.md"), "Agents")
	})
}

func TestSharedFilesDocumentationMatchesGoldens(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "docs", "shared-files.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := textFences(document)
	if len(blocks) != 13 {
		t.Fatalf("text fence count = %d, want formula plus 12 file examples", len(blocks))
	}

	wants := []string{
		"shared-files-import/project/CLAUDE.md",
		"shared-files-import/project/AGENTS.md",
		"shared-files-import/want/both/sandwiched/files/CLAUDE.md",
		"shared-files-import/want/both/sandwiched/files/AGENTS.md",
		"shared-files-import/want/both/removed/files/CLAUDE.md",
		"shared-files-import/want/both/removed/files/AGENTS.md",
		"shared-files-separate/project/CLAUDE.md",
		"shared-files-separate/project/AGENTS.md",
		"shared-files-separate/want/both/sandwiched/files/CLAUDE.md",
		"shared-files-separate/want/both/sandwiched/files/AGENTS.md",
		"shared-files-separate/want/both/removed/files/CLAUDE.md",
		"shared-files-separate/want/both/removed/files/AGENTS.md",
	}
	for index, relative := range wants {
		relative := relative
		t.Run(strings.ReplaceAll(relative, "/", "_"), func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(relative)))
			if err != nil {
				t.Fatal(err)
			}
			// A closing Markdown fence must begin on a new line. Represent a
			// fixture with no final newline by the same bytes plus that one
			// syntactic newline; the prose and golden tests bind its absence.
			if !bytes.HasSuffix(want, []byte("\n")) {
				want = append(want, '\n')
			}
			if !bytes.Equal(blocks[index+1], want) {
				t.Fatalf("documented block differs from %s\n--- got ---\n%s--- want ---\n%s", relative, blocks[index+1], want)
			}
		})
	}
}

func textFences(document []byte) [][]byte {
	lines := bytes.SplitAfter(document, []byte("\n"))
	var blocks [][]byte
	for index := 0; index < len(lines); index++ {
		if string(bytes.TrimSuffix(lines[index], []byte("\n"))) != "```text" {
			continue
		}
		var block []byte
		for index++; index < len(lines); index++ {
			if string(bytes.TrimSuffix(lines[index], []byte("\n"))) == "```" {
				blocks = append(blocks, block)
				break
			}
			block = append(block, lines[index]...)
		}
	}
	return blocks
}

func runNativeGoldenMatrix(t *testing.T, name string) {
	t.Helper()
	fixture := filepath.Join("testdata", name)
	for _, native := range []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()} {
		native := native
		t.Run(native.Descriptor().ID, func(t *testing.T) {
			RunGoldenFixture(t, fixture, filepath.Join(fixture, "want", native.Descriptor().ID), native, preserve.NewCompiler())
		})
	}
}

func runSharedFilesGolden(t *testing.T, name string, addCustomContent func(*testing.T, string)) {
	t.Helper()
	fixture := filepath.Join("testdata", name)
	want := filepath.Join(fixture, "want", "both")
	natives := []adapter.Adapter{claudecode.New(), codex.New()}
	compiler := preserve.NewCompiler()

	RunGoldenFixtureAdapters(t, fixture, filepath.Join(want, "realized"), compiler, natives...)

	loaded, err := manifest.Load(filepath.Join(fixture, "package"))
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := copyTree(filepath.Join(fixture, "project"), project); err != nil {
		t.Fatal(err)
	}
	pkg := adapter.Package{
		Source: "github:" + loaded.Name, Root: os.DirFS(filepath.Join(fixture, "package")), Manifest: loaded,
	}

	_, firstLedger := applyNativePackages(t, project, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}, natives...)
	addCustomContent(t, project)
	_, sandwichLedger := applyNativePackages(t, project, []adapter.Package{pkg}, firstLedger, natives...)
	assertOrUpdateGoldenTree(t, filepath.Join(want, "sandwiched"), project)

	idempotent, _ := planNativePackages(t, project, []adapter.Package{pkg}, sandwichLedger, natives...)
	if idempotent.HasChanges() {
		t.Fatalf("re-realize after anchoring custom content has changes: %#v", idempotent)
	}

	removalPlan, removalLedger := applyNativePackages(t, project, nil, sandwichLedger, natives...)
	assertRemovalLeavesLedger(t, removalPlan, removalLedger, sandwichLedger)
	assertOrUpdateGoldenTree(t, filepath.Join(want, "removed"), project)
}

// assertRemovalLeavesLedger covers issue #55: the engine applies the final
// removal, every shared target the ledger owned transitions to unmanaged, and
// nothing is left owned.
func assertRemovalLeavesLedger(t *testing.T, plan realize.Plan, removed, previous realize.Ledger) {
	t.Helper()
	if len(previous.Targets) == 0 {
		t.Fatal("removal fixture owned no targets before removal")
	}
	for _, operation := range plan.Operations {
		if operation.Kind == realize.OperationConflict {
			t.Fatalf("removal conflict on %s: %s", operation.Path, operation.Reason)
		}
		if operation.Kind == realize.OperationRemove && operation.OwnershipAfter != realize.OwnershipUnmanaged {
			t.Fatalf("removal of %s left ownership %q, want unmanaged", operation.Path, operation.OwnershipAfter)
		}
	}
	if len(removed.Targets) != 0 {
		t.Fatalf("ledger still owns %#v after removal", removed.Targets)
	}
}

func applyNativePackages(t *testing.T, project string, packages []adapter.Package, previous realize.Ledger, natives ...adapter.Adapter) (realize.Plan, realize.Ledger) {
	t.Helper()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), natives...)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(project)), packages, previous)
	if err != nil {
		t.Fatal(err)
	}
	next := previous
	plan, err := realize.NewEngine().Run(project, previous, intents, realize.ModeApply, func(ledger realize.Ledger) error {
		next = ledger
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan, next
}

func planNativePackages(t *testing.T, project string, packages []adapter.Package, previous realize.Ledger, natives ...adapter.Adapter) (realize.Plan, realize.Ledger) {
	t.Helper()
	coordinator, err := adapter.NewCoordinator(preserve.NewCompiler(), natives...)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(project)), packages, previous)
	if err != nil {
		t.Fatal(err)
	}
	next := previous
	plan, err := realize.NewEngine().Run(project, previous, intents, realize.ModeDryRun, func(ledger realize.Ledger) error {
		next = ledger
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan, next
}

func sandwichImport(t *testing.T, filename string) {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	const include = "@AGENTS.md"
	if bytes.Count(content, []byte(include)) != 1 {
		t.Fatalf("%s does not contain exactly one %s", filename, include)
	}
	content = bytes.Replace(content, []byte(include), []byte("Custom Claude content above the import.\n@AGENTS.md\nCustom Claude content below the import."), 1)
	if err := os.WriteFile(filename, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sandwichBlock(t *testing.T, filename, label string) {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(content, []byte("<!-- acr:begin "))
	endMarker := bytes.Index(content, []byte("<!-- acr:end "))
	if start < 0 || endMarker < start {
		t.Fatalf("%s has no complete ACR block", filename)
	}
	end := bytes.IndexByte(content[endMarker:], '\n')
	if end < 0 {
		end = len(content)
	} else {
		end += endMarker + 1
	}
	above := []byte(fmt.Sprintf("Custom %s content added above the block.\n", label))
	below := []byte(fmt.Sprintf("Custom %s content added below the block.", label))
	next := make([]byte, 0, len(content)+len(above)+len(below))
	next = append(next, content[:start]...)
	next = append(next, above...)
	next = append(next, content[start:end]...)
	next = append(next, below...)
	if err := os.WriteFile(filename, next, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertOrUpdateGoldenTree(t *testing.T, wantDir, project string) {
	t.Helper()
	if *update {
		if err := os.RemoveAll(wantDir); err != nil {
			t.Fatal(err)
		}
		if err := copyTree(project, filepath.Join(wantDir, "files")); err != nil {
			t.Fatal(err)
		}
		return
	}
	assertGoldenFiles(t, wantDir, project)
}
