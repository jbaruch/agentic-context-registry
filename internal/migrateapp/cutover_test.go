package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// secretSentinel is a value that must never reach any diagnostic. Every
// assertion that scans text or JSON for it proves the same contract: an MCP
// entry can hold a credential, and ACR reports identifiers about one, never
// its content.
const secretSentinel = "s3cr3t-token-do-not-print"

const canonicalMCPJSON = `{
  "mcpServers": {
    "tessl": {
      "type": "stdio",
      "command": "tessl",
      "args": [
        "mcp",
        "start"
      ]
    },
    "notes": {
      "type": "stdio",
      "command": "notes-server",
      "unknownField": {"kept": true}
    }
  }
}
`

const canonicalMCPTOML = `# operator comment above the Tessl table
[mcp_servers.tessl] # operator comment on the header
type = "stdio"
# operator comment between fields
command = "tessl" # operator comment on a field
args = [ "mcp", "start" ]
# operator comment below the Tessl table

[mcp_servers.notes]
command = "notes-server"

[tools]
web_search = true
`

func writeSharedSurfaceConsumer(t *testing.T) string {
	t.Helper()
	root := writeUnmappedConsumer(t)
	linkSharedSkill(t, root, "tessl__review", "../../.tessl/plugins/example/orphan/skills/review")
	return root
}

func linkSharedSkill(t *testing.T, root, name, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ".agents", "skills", name)); err != nil {
		t.Fatal(err)
	}
}

func writeProjectFile(t *testing.T, root, relative, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readProjectFile(t *testing.T, root, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// coexist runs the ordinary coexistence migration that realizes ACR's own
// outputs. Finalization is only meaningful once it has run.
func coexist(t *testing.T, root string) migrate.MigrationReport {
	t.Helper()
	service := newService(vendorPanicRemote{})
	report, err := service.Migrate(context.Background(), root, Options{VendorUnmapped: true})
	if err != nil {
		t.Fatalf("coexistence migration: %v", err)
	}
	return report
}

func finalize(t *testing.T, root string, dryRun bool) (migrate.MigrationReport, error) {
	t.Helper()
	service := newService(vendorPanicRemote{})
	return service.Migrate(context.Background(), root, Options{Finalize: true, DryRun: dryRun})
}

func blockerCodes(report migrate.MigrationReport) []string {
	codes := make([]string, 0, len(report.Blockers))
	for _, blocker := range report.Blockers {
		codes = append(codes, blocker.Code)
	}
	return codes
}

func hasBlocker(report migrate.MigrationReport, code, path string) bool {
	for _, blocker := range report.Blockers {
		if blocker.Code == code && (path == "" || blocker.Path == path) {
			return true
		}
	}
	return false
}

func removedPaths(report migrate.MigrationReport) map[string]migrate.RemovalRecord {
	removed := make(map[string]migrate.RemovalRecord, len(report.Removed))
	for _, record := range report.Removed {
		removed[record.Path] = record
	}
	return removed
}

func retentionReason(report migrate.MigrationReport, path string) string {
	for _, record := range report.Retained {
		if record.Path == path {
			return record.Reason
		}
	}
	return ""
}

// TestFinalizeRetiresSharedLinksAndCanonicalMCPEntries is D1, D10, D11 and
// D14: an ordinary install cuts over, every foreign member survives byte for
// byte, and the TOML table header goes with its fields.
func TestFinalizeRetiresSharedLinksAndCanonicalMCPEntries(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".mcp.json", canonicalMCPJSON)
	writeProjectFile(t, root, ".codex/config.toml", canonicalMCPTOML)
	writeProjectFile(t, root, ".codex/skills/keep.md", "user file\n")
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, false)
	if err != nil {
		t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
	}
	removed := removedPaths(report)
	link, retired := removed[".agents/skills/tessl__review"]
	if !retired {
		t.Fatalf("shared link not retired: %#v", report.Removed)
	}
	if link.Replacement != ".agents/skills/acr__example__orphan__review" {
		t.Fatalf("replacement = %q", link.Replacement)
	}
	if _, err := os.Lstat(filepath.Join(root, ".agents/skills/tessl__review")); !os.IsNotExist(err) {
		t.Fatalf("shared link still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents/skills/acr__example__orphan__review/SKILL.md")); err != nil {
		t.Fatalf("ACR shared entry missing: %v", err)
	}

	jsonAfter := readProjectFile(t, root, ".mcp.json")
	if strings.Contains(jsonAfter, `"tessl"`) {
		t.Fatalf(".mcp.json still declares tessl:\n%s", jsonAfter)
	}
	for _, want := range []string{`"notes"`, `"notes-server"`, `"unknownField"`, `"kept": true`} {
		if !strings.Contains(jsonAfter, want) {
			t.Fatalf(".mcp.json lost %s:\n%s", want, jsonAfter)
		}
	}

	tomlAfter := readProjectFile(t, root, ".codex/config.toml")
	if strings.Contains(tomlAfter, "[mcp_servers.tessl]") {
		t.Fatalf("orphan TOML header survived:\n%s", tomlAfter)
	}
	if strings.Contains(tomlAfter, `args = [ "mcp", "start" ]`) {
		t.Fatalf("tessl table fields survived:\n%s", tomlAfter)
	}
	// Every comment position survives: the canonical object proves ownership of
	// the integration, never of a comment somebody wrote around it.
	for _, want := range []string{
		"# operator comment above the Tessl table",
		"# operator comment on the header",
		"# operator comment between fields",
		"# operator comment on a field",
		"# operator comment below the Tessl table",
		"[mcp_servers.notes]", "[tools]", "web_search = true",
	} {
		if !strings.Contains(tomlAfter, want) {
			t.Fatalf(".codex/config.toml lost %q:\n%s", want, tomlAfter)
		}
	}
}

// TestFinalizeKeepsDifferentlyNamedMCPServer is D22: a user server whose
// command resembles Tessl's is never a retirement candidate.
func TestFinalizeKeepsDifferentlyNamedMCPServer(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	const proxy = `{"mcpServers":{"tessl-proxy":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}` + "\n"
	writeProjectFile(t, root, ".mcp.json", proxy)
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, false)
	if err != nil {
		t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
	}
	if after := readProjectFile(t, root, ".mcp.json"); after != proxy {
		t.Fatalf(".mcp.json changed:\n%s", after)
	}
	if len(report.Blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", blockerCodes(report))
	}
}

// TestFinalizeBlocksOnAmbiguousMCPEntries is D15, D21 and D23: an entry keyed
// tessl that ACR cannot prove is retained, blocks with a remedy, and never
// renders its own content.
func TestFinalizeBlocksOnAmbiguousMCPEntries(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
		reason  string
	}{
		{
			name:    "absolute command path",
			content: `{"mcpServers":{"tessl":{"type":"stdio","command":"/usr/local/bin/tessl","args":["mcp","start"]}}}` + "\n",
			reason:  "command-is-not-the-bare-name",
		},
		{
			name:    "extra argument",
			content: `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start","--verbose"]}}}` + "\n",
			reason:  "args-are-not-mcp-start",
		},
		{
			name:    "environment block",
			content: `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"],"env":{"TOKEN":"` + secretSentinel + `"}}}}` + "\n",
			reason:  "entry-carries-extra-keys",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			writeProjectFile(t, root, ".mcp.json", testCase.content)
			coexist(t, root)
			gitCommitFixture(t, root)

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize succeeded, want a blocked cutover")
			}
			if !hasBlocker(report, blockerMCPAmbiguous, ".mcp.json") {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if after := readProjectFile(t, root, ".mcp.json"); after != testCase.content {
				t.Fatalf(".mcp.json changed:\n%s", after)
			}
			var detail string
			for _, blocker := range report.Blockers {
				if blocker.Code == blockerMCPAmbiguous {
					detail = blocker.Detail
					if blocker.Remedy == "" {
						t.Fatal("ambiguous MCP blocker has no remedy")
					}
				}
			}
			if !strings.Contains(detail, testCase.reason) {
				t.Fatalf("blocker detail = %q, want the differing field named", detail)
			}
			assertNoSecret(t, report)
		})
	}
}

// assertNoSecret proves both rendered forms of the report stay clean: the text
// diagnostic an operator reads and the JSON envelope a CI job captures.
func assertNoSecret(t *testing.T, report migrate.MigrationReport) {
	t.Helper()
	text := migrate.FormatCoexistenceText(report)
	if strings.Contains(text, secretSentinel) {
		t.Fatalf("text report leaked the credential:\n%s", text)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretSentinel) {
		t.Fatalf("JSON report leaked the credential:\n%s", encoded)
	}
	if !strings.Contains(text, "sha256:") {
		t.Fatalf("text report names no entry digest:\n%s", text)
	}
}

// TestFinalizeBlocksOnMalformedMCPConfig is D13.
func TestFinalizeBlocksOnMalformedMCPConfig(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".mcp.json", `{"mcpServers":{"tessl":`)
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, false)
	if err == nil {
		t.Fatal("finalize succeeded on a malformed config")
	}
	if !hasBlocker(report, blockerMCPMalformed, ".mcp.json") {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	if after := readProjectFile(t, root, ".mcp.json"); after != `{"mcpServers":{"tessl":` {
		t.Fatalf("malformed config was rewritten:\n%s", after)
	}
}

// TestFinalizeLeavesUnsupportedAgentMCPAlone is D26: retirement is scoped to
// the supported agents' configs by name, and the unsupported agent keeps
// blocking on its own evidence.
func TestFinalizeLeavesUnsupportedAgentMCPAlone(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	const vscode = `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}` + "\n"
	writeProjectFile(t, root, ".vscode/mcp.json", vscode)
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, false)
	if err == nil {
		t.Fatal("finalize succeeded while vscode is uncovered")
	}
	if !hasBlocker(report, blockerUncoveredAgent, "") {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	if after := readProjectFile(t, root, ".vscode/mcp.json"); after != vscode {
		t.Fatalf(".vscode/mcp.json changed:\n%s", after)
	}
}

// TestFinalizeBlocksOnOpenHandsSkills is D25: an OpenHands user never loses
// their skill links silently.
func TestFinalizeBlocksOnOpenHandsSkills(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	if err := os.MkdirAll(filepath.Join(root, ".openhands", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../.tessl/plugins/example/orphan/skills/review", filepath.Join(root, ".openhands/skills/tessl-review")); err != nil {
		t.Fatal(err)
	}
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, false)
	if err == nil {
		t.Fatal("finalize succeeded while OpenHands skills are present")
	}
	named := false
	for _, blocker := range report.Blockers {
		if blocker.Code == blockerUncoveredAgent && blocker.ID == "openhands" && blocker.Remedy != "" {
			named = true
		}
	}
	if !named {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	if _, err := os.Lstat(filepath.Join(root, ".openhands/skills/tessl-review")); err != nil {
		t.Fatalf("OpenHands link was removed: %v", err)
	}
}

// TestSharedSurfaceRetentionAndBlocking covers the per-entry removal
// predicate: D2 (empty surface), D3 (hand-written entries), D4 (a real
// directory), D5 (an escaping link), D6 and D24 (an orphan naming .tessl
// state this run deletes).
func TestSharedSurfaceRetentionAndBlocking(t *testing.T) {
	t.Run("empty surface is not a blocker", func(t *testing.T) {
		root := writeUnmappedConsumer(t)
		if err := os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, false)
		if err != nil {
			t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
		}
	})

	t.Run("hand-written entries survive byte for byte", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		writeProjectFile(t, root, ".agents/skills/my-own-skill/SKILL.md", "# Mine\n")
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, false)
		if err != nil {
			t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
		}
		if got := readProjectFile(t, root, ".agents/skills/my-own-skill/SKILL.md"); got != "# Mine\n" {
			t.Fatalf("user skill changed: %q", got)
		}
	})

	t.Run("a real directory is retained, never removed", func(t *testing.T) {
		root := writeUnmappedConsumer(t)
		// Byte-identical to the plugin tree, so the artifact stays migratable
		// and this case isolates the one fact under test: the entry is a real
		// directory, not a link ACR can prove.
		writeProjectFile(t, root, ".agents/skills/tessl__review/SKILL.md", "# Review\n")
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, false)
		if err != nil {
			t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
		}
		if reason := retentionReason(report, ".agents/skills/tessl__review"); reason != "non-symlink-shared-entry" {
			t.Fatalf("retention reason = %q", reason)
		}
		if got := readProjectFile(t, root, ".agents/skills/tessl__review/SKILL.md"); got != "# Review\n" {
			t.Fatalf("directory entry changed: %q", got)
		}
	})

	t.Run("a link escaping the project root is retained", func(t *testing.T) {
		root := writeUnmappedConsumer(t)
		linkSharedSkill(t, root, "tessl__review", "../../../outside/skills/review")
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, false)
		if err != nil {
			t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
		}
		if reason := retentionReason(report, ".agents/skills/tessl__review"); reason != "skill-tree-escape" {
			t.Fatalf("retention reason = %q", reason)
		}
		target, err := os.Readlink(filepath.Join(root, ".agents/skills/tessl__review"))
		if err != nil || target != "../../../outside/skills/review" {
			t.Fatalf("escaping link = %q, %v", target, err)
		}
	})

	t.Run("an orphan naming deleted Tessl state blocks", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		linkSharedSkill(t, root, "tessl__gone", "../../.tessl/plugins/example/orphan/skills/gone")
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, false)
		if err == nil {
			t.Fatal("finalize succeeded with an orphan shared link")
		}
		if !hasBlocker(report, blockerSharedOrphan, ".agents/skills/tessl__gone") {
			t.Fatalf("blockers = %#v", report.Blockers)
		}
		if _, err := os.Lstat(filepath.Join(root, ".agents/skills/tessl__gone")); err != nil {
			t.Fatalf("orphan link was removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".tessl/plugins/example/orphan/skills/review/SKILL.md")); err != nil {
			t.Fatalf("blocked finalization deleted Tessl state: %v", err)
		}
	})
}

// TestBlockedFinalizationReportsEveryBlocker is D19: the blocked run still
// produces the detailed report, in JSON and in text, naming each blocker and
// its remedy.
func TestBlockedFinalizationReportsEveryBlocker(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	linkSharedSkill(t, root, "tessl__gone", "../../.tessl/plugins/example/orphan/skills/gone")
	writeProjectFile(t, root, ".vscode/mcp.json", `{"mcpServers":{}}`+"\n")
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, true)
	if err == nil {
		t.Fatal("blocked dry run reported success")
	}
	if report.SchemaVersion == 0 {
		t.Fatal("blocked run returned an empty report")
	}
	if !hasBlocker(report, blockerSharedOrphan, ".agents/skills/tessl__gone") || !hasBlocker(report, blockerUncoveredAgent, "") {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	for _, blocker := range report.Blockers {
		if blocker.Remedy == "" {
			t.Fatalf("blocker %+v has no remedy", blocker)
		}
	}
	text := migrate.FormatCoexistenceText(report)
	for _, want := range []string{"Finalization blockers", blockerSharedOrphan, "remedy:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text report missing %q:\n%s", want, text)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"blockers"`) {
		t.Fatalf("JSON report has no blockers: %s", encoded)
	}
}

// TestFinalizeRollsBackAfterALiveMutation is D16 and R7. The failure is
// injected through the transaction's own AfterEdit hook, so the edit under
// test has actually landed on disk before the run fails: the assertions run
// against a project that really was changed and then restored.
func TestFinalizeRollsBackAfterALiveMutation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// mutated names the edit the run is allowed to complete before failing.
		mutated string
		// verify proves that edit is live at the moment the failure is raised.
		verify func(t *testing.T, root string)
	}{
		{
			name:    "after a shared link is removed",
			mutated: ".agents/skills/tessl__review",
			verify: func(t *testing.T, root string) {
				if _, err := os.Lstat(filepath.Join(root, ".agents/skills/tessl__review")); !os.IsNotExist(err) {
					t.Fatalf("the shared link was still present when the failure was injected: %v", err)
				}
			},
		},
		{
			name:    "after the MCP entry is spliced",
			mutated: ".mcp.json",
			verify: func(t *testing.T, root string) {
				content, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(content), `"tessl"`) {
					t.Fatalf("the MCP splice had not landed when the failure was injected:\n%s", content)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			writeProjectFile(t, root, ".mcp.json", canonicalMCPJSON)
			coexist(t, root)
			gitCommitFixture(t, root)
			before := hashTreeWithModes(t, root)
			linkBefore, err := os.Readlink(filepath.Join(root, ".agents/skills/tessl__review"))
			if err != nil {
				t.Fatal(err)
			}
			stateBefore, err := dependency.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}

			injected := errors.New("injected failure after a live edit")
			mutated := false
			original := applyFinalizationFileTransaction
			applyFinalizationFileTransaction = func(projectDirectory string, edits []realize.FileTransactionEdit, finalize func() error) error {
				return realize.ApplyFileTransactionWithHooks(projectDirectory, edits, finalize, realize.FileTransactionHooks{
					AfterEdit: func(_ int, edit realize.FileTransactionEdit) error {
						if edit.Path != testCase.mutated {
							return nil
						}
						mutated = true
						testCase.verify(t, projectDirectory)
						return injected
					},
				})
			}
			defer func() { applyFinalizationFileTransaction = original }()

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize succeeded despite an injected failure")
			}
			if !mutated {
				t.Fatalf("the transaction never reached %s; the test proves nothing", testCase.mutated)
			}

			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("rolled-back finalization changed the project: before=%v after=%v", before, after)
			}
			linkAfter, err := os.Readlink(filepath.Join(root, ".agents/skills/tessl__review"))
			if err != nil || linkAfter != linkBefore {
				t.Fatalf("link target = %q, %v; want %q restored", linkAfter, err, linkBefore)
			}
			if got := readProjectFile(t, root, ".mcp.json"); got != canonicalMCPJSON {
				t.Fatalf(".mcp.json was not restored:\n%s", got)
			}
			stateAfter, err := dependency.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stateBefore, stateAfter) {
				t.Fatal("dependency state was not restored")
			}

			// R6: a restored project must not be reported as a finalized one.
			if report.FinalizationReady {
				t.Fatal("a failed finalization reported readiness")
			}
			if report.DryRun {
				t.Fatal("an apply reported itself as a dry run")
			}
			if report.Wrote {
				t.Fatal("a rolled-back run reported that it wrote")
			}
			if report.Mode != "finalize" {
				t.Fatalf("mode = %q, want the unfinished finalize mode", report.Mode)
			}
			if len(report.Removed) != 0 || len(report.Reanchored) != 0 {
				t.Fatalf("a rolled-back run claimed removals %#v and re-anchors %#v", report.Removed, report.Reanchored)
			}
			blocker, found := blockerFor(report.Blockers, blockerFinalizationFailed, "")
			if !found {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if blocker.Remedy == "" {
				t.Fatal("the failure blocker has no remedy")
			}
		})
	}
}

// TestRepeatFinalizeStaysCleanAndIdempotent is D17: a second finalize is a
// no-op, and the shared surface stays realized and current afterwards.
func TestRepeatFinalizeStaysCleanAndIdempotent(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".mcp.json", canonicalMCPJSON)
	coexist(t, root)
	gitCommitFixture(t, root)
	if _, err := finalize(t, root, false); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	before := hashTree(t, root)

	report, err := finalize(t, root, false)
	if err != nil {
		t.Fatalf("second finalize: %v", err)
	}
	if report.Wrote || len(report.Removed) != 0 {
		t.Fatalf("second finalize = %#v", report)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("second finalize changed the project: before=%v after=%v", before, after)
	}

	realizer := newService(vendorPanicRemote{}).realizer
	if _, err := realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("acr check after finalize: %v", err)
	}
	result, err := realizer.Run(context.Background(), root, nil, realize.ModeApply)
	if err != nil {
		t.Fatalf("acr realize after finalize: %v", err)
	}
	if result.Plan.HasChanges() {
		t.Fatalf("realize after finalize planned changes: %#v", result.Plan.Operations)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("realize after finalize changed the project: before=%v after=%v", before, after)
	}
}

// TestFinalizeLeavesRealizationCurrentForAWhollyOwnedConfig is the D17
// contract at the point it is easiest to break: retiring the MCP entry can
// remove the last content ACR does not own from a shared config, and a ledger
// still recording shared ownership makes every later check and realize refuse
// the merge for want of unmanaged content to preserve.
func TestFinalizeLeavesRealizationCurrentForAWhollyOwnedConfig(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	// ACR's own Codex session-start hook plus the Tessl MCP table, and nothing
	// else: the config is shared before finalization and wholly ACR-owned after.
	writeProjectFile(t, root, ".codex/config.toml", "[mcp_servers.tessl]\ntype = \"stdio\"\ncommand = \"tessl\"\nargs = [ \"mcp\", \"start\" ]\n")
	coexist(t, root)
	gitCommitFixture(t, root)

	shared := false
	for _, target := range finalizeLedger(t, root).Targets {
		if target.Path == ".codex/config.toml" && target.Ownership == realize.OwnershipShared {
			shared = true
		}
	}
	if !shared {
		t.Fatal("fixture does not produce a shared .codex/config.toml; the regression it guards cannot occur")
	}

	report, err := finalize(t, root, false)
	if err != nil {
		t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
	}
	demoted := false
	for _, record := range report.Reanchored {
		if record.Path == ".codex/config.toml" && record.OwnershipAfter == string(realize.OwnershipGenerated) {
			demoted = true
		}
	}
	if !demoted {
		t.Fatalf("reanchored = %#v, want the demotion reported", report.Reanchored)
	}

	realizer := newService(vendorPanicRemote{}).realizer
	if _, err := realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
		t.Fatalf("acr check after finalize: %v", err)
	}
	result, err := realizer.Run(context.Background(), root, nil, realize.ModeApply)
	if err != nil {
		t.Fatalf("acr realize after finalize: %v", err)
	}
	if result.Plan.HasChanges() {
		t.Fatalf("realize after finalize planned changes: %#v", result.Plan.Operations)
	}
}

func finalizeLedger(t *testing.T, root string) realize.Ledger {
	t.Helper()
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

// TestFinalizeBlocksOnTrailingContentAfterTheMCPDocument is R5: a config whose
// first value parses but whose file does not end there is unreadable to the
// client, so it must not pass the gate — even with no tessl member in that
// first object.
func TestFinalizeBlocksOnTrailingContentAfterTheMCPDocument(t *testing.T) {
	const trailing = `{"mcpServers":{}} garbage` + "\n"
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".mcp.json", trailing)
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, true)
	if err == nil {
		t.Fatal("finalize dry-run accepted a config with trailing content")
	}
	if !hasBlocker(report, blockerMCPMalformed, ".mcp.json") {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	if report.FinalizationReady {
		t.Fatal("blocked run reported readiness")
	}
	if after := readProjectFile(t, root, ".mcp.json"); after != trailing {
		t.Fatalf(".mcp.json changed:\n%s", after)
	}
}

// TestFinalizeBlocksOnAMalformedConfigAddedAfterCoexistence is R5's second
// case. .codex/config.toml is also a hook host, so a malformed one used to
// fail the inventory — and then the preservation compiler — before the blocker
// report could be built at all.
func TestFinalizeBlocksOnAMalformedConfigAddedAfterCoexistence(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".codex/config.toml", "# user config\n")
	coexist(t, root)
	broken := readProjectFile(t, root, ".codex/config.toml") + "\n[mcp_servers.tessl]\ncommand = \"planted-token\" BROKEN\n"
	writeProjectFile(t, root, ".codex/config.toml", broken)
	gitCommitFixture(t, root)
	before := hashTree(t, root)

	report, err := finalize(t, root, true)
	if err == nil {
		t.Fatal("finalize dry-run accepted a malformed Codex config")
	}
	if report.FinalizationReady {
		t.Fatal("blocked run reported readiness")
	}
	if !hasBlocker(report, blockerMCPMalformed, ".codex/config.toml") {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	positioned := false
	for _, blocker := range report.Blockers {
		if blocker.Code != blockerMCPMalformed {
			continue
		}
		if strings.Contains(blocker.Detail, "line") && strings.Contains(blocker.Detail, "column") {
			positioned = true
		}
		if blocker.Remedy == "" {
			t.Fatal("malformed-config blocker has no remedy")
		}
	}
	if !positioned {
		t.Fatalf("blockers keep no parse coordinate: %#v", report.Blockers)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
	}
	assertNoPlantedToken(t, report, "planted-token")
	if got := readProjectFile(t, root, ".codex/config.toml"); got != broken {
		t.Fatalf(".codex/config.toml changed:\n%s", got)
	}
}

// assertNoPlantedToken scans both rendered forms of a report for a value that
// must never leave the file it came from.
func assertNoPlantedToken(t *testing.T, report migrate.MigrationReport, token string) {
	t.Helper()
	if text := migrate.FormatCoexistenceText(report); strings.Contains(text, token) {
		t.Fatalf("text report leaked config content:\n%s", text)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatalf("JSON report leaked config content:\n%s", encoded)
	}
}

// TestFinalizeBlocksOnGitHubMCPOnlyEvidence is R8: `.github/mcp.json` alone is
// enough GitHub evidence to keep the uncovered-agent guard, and the file itself
// stays outside the three supported mutation paths.
func TestFinalizeBlocksOnGitHubMCPOnlyEvidence(t *testing.T) {
	const github = `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}` + "\n"
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".github/mcp.json", github)
	coexist(t, root)
	gitCommitFixture(t, root)
	before := hashTree(t, root)

	report, err := finalize(t, root, false)
	if err == nil {
		t.Fatal("finalize succeeded with a GitHub MCP integration left behind")
	}
	named := false
	for _, blocker := range report.Blockers {
		if blocker.Code == blockerUncoveredAgent && blocker.ID == "github" && blocker.Remedy != "" {
			named = true
		}
	}
	if !named {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	if after := readProjectFile(t, root, ".github/mcp.json"); after != github {
		t.Fatalf(".github/mcp.json changed:\n%s", after)
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
	}
}

// projectInventory and projectLedger give a test the two inputs finalization
// planning consumes, so a probe can change the tree between them exactly the
// way a user or another process can.
func projectInventory(t *testing.T, root string) migrate.Report {
	t.Helper()
	inventory, err := newService(vendorPanicRemote{}).Inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}

func projectLedger(t *testing.T, root string) realize.Ledger {
	t.Helper()
	return finalizeLedger(t, root)
}

func planPaths(plan migrate.FinalizePlan) map[string]migrate.FinalizeEdit {
	edits := make(map[string]migrate.FinalizeEdit, len(plan.Edits))
	for _, edit := range plan.Edits {
		edits[edit.Path] = edit
	}
	return edits
}

func blockerFor(blockers []migrate.Blocker, code, path string) (migrate.Blocker, bool) {
	for _, blocker := range blockers {
		if blocker.Code == code && blocker.Path == path {
			return blocker, true
		}
	}
	return migrate.Blocker{}, false
}

// TestPlanningRebindsRetirementOwnershipToTheBytesItReads is R1. Inventory and
// planning read the project separately, and planning's read is what the
// transaction accepts as its before-image. Ownership is re-proved against
// those bytes, so evidence that changed in between refuses instead of
// authorizing a deletion it never covered.
func TestPlanningRebindsRetirementOwnershipToTheBytesItReads(t *testing.T) {
	t.Run("an MCP object swapped for a user server", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		writeProjectFile(t, root, ".mcp.json", canonicalMCPJSON)
		coexist(t, root)
		gitCommitFixture(t, root)
		inventory := projectInventory(t, root)
		ledger := projectLedger(t, root)

		const swapped = `{"mcpServers":{"tessl":{"type":"stdio","command":"user-server","args":["run"],"env":{"TOKEN":"` + secretSentinel + `"}}}}` + "\n"
		writeProjectFile(t, root, ".mcp.json", swapped)

		plan, blockers, err := planFinalization(root, inventory, ledger)
		if err != nil {
			t.Fatal(err)
		}
		blocker, found := blockerFor(blockers, blockerMCPOwnership, ".mcp.json")
		if !found {
			t.Fatalf("blockers = %#v", blockers)
		}
		if blocker.Remedy == "" {
			t.Fatal("ownership blocker has no remedy")
		}
		if strings.Contains(blocker.Detail, secretSentinel) || strings.Contains(blocker.Detail, "user-server") {
			t.Fatalf("blocker detail leaked entry content: %q", blocker.Detail)
		}
		if edit, planned := planPaths(plan)[".mcp.json"]; planned {
			t.Fatalf(".mcp.json was still spliced: %q", edit.After)
		}
	})

	t.Run("a shared link repointed at a user tree", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		coexist(t, root)
		gitCommitFixture(t, root)
		inventory := projectInventory(t, root)
		ledger := projectLedger(t, root)

		writeProjectFile(t, root, "team/skills/review/SKILL.md", "# Team\n")
		link := filepath.Join(root, ".agents", "skills", "tessl__review")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../team/skills/review", link); err != nil {
			t.Fatal(err)
		}

		plan, blockers, err := planFinalization(root, inventory, ledger)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := blockerFor(blockers, blockerSharedOwnership, ".agents/skills/tessl__review"); !found {
			t.Fatalf("blockers = %#v", blockers)
		}
		if edit, planned := planPaths(plan)[".agents/skills/tessl__review"]; planned {
			t.Fatalf("repointed link was still scheduled for deletion: %+v", edit)
		}
	})

	t.Run("a combined Codex host re-read for its hooks", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		const combined = `[mcp_servers.tessl]
type = "stdio"
command = "tessl"
args = [ "mcp", "start" ]
[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = "tessl hook run --event=\"SessionStart\" --agent=codex --schema-version=1"
`
		writeProjectFile(t, root, ".codex/config.toml", combined)
		coexist(t, root)
		gitCommitFixture(t, root)
		inventory := projectInventory(t, root)
		ledger := projectLedger(t, root)

		swapped := strings.Replace(combined, `command = "tessl"`, `command = "/usr/local/bin/tessl"`, 1)
		writeProjectFile(t, root, ".codex/config.toml", swapped)

		plan, blockers, err := planFinalization(root, inventory, ledger)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := blockerFor(blockers, blockerMCPOwnership, ".codex/config.toml"); !found {
			t.Fatalf("blockers = %#v", blockers)
		}
		// The hook dispatchers may still splice — the run is blocked either
		// way — but the changed MCP table keeps every byte.
		if edit, planned := planPaths(plan)[".codex/config.toml"]; planned {
			for _, want := range []string{"[mcp_servers.tessl]", `command = "/usr/local/bin/tessl"`, `args = [ "mcp", "start" ]`} {
				if !strings.Contains(string(edit.After), want) {
					t.Fatalf("the changed MCP table lost %q:\n%s", want, edit.After)
				}
			}
		}
	})
}

// TestFinalizeBlocksARetainedLinkWhoseTargetItRemoves is R2. A link ACR keeps
// is only safe while its target survives, and the plan removes files well
// outside .tessl. An absolute pathname can name a file inside this very
// project, so it is placed against the project root rather than treated as an
// escape.
func TestFinalizeBlocksARetainedLinkWhoseTargetItRemoves(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// target is relative to .agents/skills; absolute reports the same
		// dependency written as an absolute pathname inside the project.
		target   string
		absolute bool
		// shortcut, when set, is a user symlink at the project root created
		// before coexistence, so the alias reaches its target through a link
		// ACR does not own.
		shortcut string
		code     string
	}{
		{name: "user alias into the Tessl plugin tree", target: "../../.tessl/plugins/example/orphan/skills/review"},
		{name: "user alias onto a per-agent Tessl native", target: "../../.claude/skills/tessl__review"},
		{name: "absolute alias into the Tessl plugin tree", target: ".tessl/plugins/example/orphan/skills/review", absolute: true},
		{name: "absolute alias onto a per-agent Tessl native", target: ".claude/skills/tessl__review", absolute: true},
		// A Tessl skill tree is a symlink and the plan deletes that one link,
		// not each path beneath it, so these lose an ancestor rather than
		// their own target.
		{name: "user alias below a per-agent Tessl native", target: "../../.claude/skills/tessl__review/nested"},
		{name: "user alias onto a file below a Tessl native", target: "../../.claude/skills/tessl__review/SKILL.md"},
		{name: "absolute alias below a per-agent Tessl native", target: ".claude/skills/tessl__review/nested", absolute: true},
		// The lexical target overlaps nothing the plan removes; the link it
		// reaches through does.
		{
			name:   "user alias through an unowned shortcut into the plugin tree",
			target: "../../shortcut/skills/review", shortcut: ".tessl/plugins/example/orphan",
			code: blockerSharedUnproven,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			// A real, readable nested skill under the Tessl tree, so every
			// alias below is live before the run rather than broken already.
			writeProjectFile(t, root, ".tessl/plugins/example/orphan/skills/review/nested/SKILL.md", "# Preserved custom nested skill\n")
			if testCase.shortcut != "" {
				if err := os.Symlink(testCase.shortcut, filepath.Join(root, "shortcut")); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.absolute {
				testCase.target = filepath.Join(root, filepath.FromSlash(testCase.target))
			}
			linkSharedSkill(t, root, "my-alias", testCase.target)
			alias := filepath.Join(root, ".agents", "skills", "my-alias")
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("the fixture's alias is not live before the run: %v", err)
			}
			coexist(t, root)
			gitCommitFixture(t, root)
			before := hashTreeWithModes(t, root)

			code := testCase.code
			if code == "" {
				code = blockerSharedDangling
			}
			preview, err := finalize(t, root, true)
			if err == nil {
				t.Fatal("finalize dry-run reported a cutover that would strand the alias")
			}
			if _, found := blockerFor(preview.Blockers, code, ".agents/skills/my-alias"); !found {
				t.Fatalf("dry-run blockers = %#v", preview.Blockers)
			}
			if preview.FinalizationReady || !preview.DryRun {
				t.Fatalf("dry-run report = %+v", struct{ Ready, DryRun bool }{preview.FinalizationReady, preview.DryRun})
			}
			if text := migrate.FormatCoexistenceText(preview); !strings.Contains(text, code) {
				t.Fatalf("dry-run text does not name the blocker:\n%s", text)
			}

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize succeeded and left a dangling user alias")
			}
			blocker, found := blockerFor(report.Blockers, code, ".agents/skills/my-alias")
			if !found {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if blocker.Remedy == "" {
				t.Fatal("dependency blocker has no remedy")
			}
			if report.FinalizationReady || report.DryRun || report.Wrote {
				t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
			}

			link, err := os.Readlink(alias)
			if err != nil || link != testCase.target {
				t.Fatalf("alias = %q, %v; want the user's link untouched", link, err)
			}
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("the alias target the run refused to strand is gone: %v", err)
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
			}
		})
	}
}

// TestFinalizeBlocksARetainedLinkWhoseInteriorDotDotHidesASymlink is the R2
// case lexical Clean misses. The stored target walks through an unowned
// shortcut then `..`. os.Stat follows the shortcut into the plugin tree;
// path.Clean drops it and substitutes a surviving user directory. A
// successful finalization must not leave that formerly live alias dangling.
// Safe actionable refusal is the accepted outcome.
func TestFinalizeBlocksARetainedLinkWhoseInteriorDotDotHidesASymlink(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		absolute bool
	}{
		{name: "relative alias through shortcut then .."},
		{name: "absolute alias through shortcut then ..", absolute: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			writeProjectFile(t, root, "team/skills/review/SKILL.md", "# Lexical team skill\n")
			writeProjectFile(t, root, ".tessl/plugins/example/team/skills/review/SKILL.md", "# Actual via shortcut parent\n")
			if err := os.Symlink(".tessl/plugins/example/orphan", filepath.Join(root, "shortcut")); err != nil {
				t.Fatal(err)
			}
			target := "../../shortcut/../team/skills/review"
			if testCase.absolute {
				// filepath.Join Cleans, which is the bug under test. Keep the
				// stored `shortcut/..` bytes the kernel will follow.
				target = filepath.Join(root, "shortcut") + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Join("team", "skills", "review")
			}
			linkSharedSkill(t, root, "dotdot-alias", target)
			alias := filepath.Join(root, ".agents", "skills", "dotdot-alias")
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("interior-dotdot alias is not live before the run: %v", err)
			}
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil {
				t.Fatalf("eval alias: %v", err)
			}
			if !strings.Contains(filepath.ToSlash(resolved), ".tessl/plugins/example/team/skills/review") {
				t.Fatalf("kernel resolved lexical team path instead of following shortcut: %s", resolved)
			}
			coexist(t, root)
			gitCommitFixture(t, root)
			before := hashTreeWithModes(t, root)

			preview, err := finalize(t, root, true)
			if err == nil {
				t.Fatal("finalize dry-run reported a cutover that would strand the alias")
			}
			if _, found := blockerFor(preview.Blockers, blockerSharedUnproven, ".agents/skills/dotdot-alias"); !found {
				t.Fatalf("dry-run blockers = %#v", preview.Blockers)
			}
			if preview.FinalizationReady || !preview.DryRun {
				t.Fatalf("dry-run report = %+v", struct{ Ready, DryRun bool }{preview.FinalizationReady, preview.DryRun})
			}
			if text := migrate.FormatCoexistenceText(preview); !strings.Contains(text, blockerSharedUnproven) {
				t.Fatalf("dry-run text does not name the blocker:\n%s", text)
			}

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize succeeded and left a dangling user alias")
			}
			blocker, found := blockerFor(report.Blockers, blockerSharedUnproven, ".agents/skills/dotdot-alias")
			if !found {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if blocker.Remedy == "" {
				t.Fatal("dependency blocker has no remedy")
			}
			if report.FinalizationReady || report.DryRun || report.Wrote {
				t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
			}

			link, err := os.Readlink(alias)
			if err != nil || link != target {
				t.Fatalf("alias = %q, %v; want the user's link untouched", link, err)
			}
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("the alias target the run refused to strand is gone: %v", err)
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
			}
		})
	}
}

// TestFinalizeBlocksARetainedLinkThatLeavesAndReentersTheProject is the rest
// of R2. Leaving the project, or failing a raw prefix match, is not proof
// the target is outside. These aliases are live before the run, every
// component is a real directory, and the kernel lands in .tessl state this
// finalization deletes.
func TestFinalizeBlocksARetainedLinkThatLeavesAndReentersTheProject(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		target func(*testing.T, string) string
	}{
		{
			name: "relative escape and re-entry",
			target: func(_ *testing.T, root string) string {
				return "../../../" + filepath.Base(root) + "/.tessl/plugins/example/orphan/skills/review"
			},
		},
		{
			name: "absolute escape and re-entry",
			target: func(_ *testing.T, root string) string {
				return root + "/../" + filepath.Base(root) + "/.tessl/plugins/example/orphan/skills/review"
			},
		},
		{
			name: "absolute escape through the evaluated project path",
			target: func(t *testing.T, root string) string {
				physical, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				return physical + "/../" + filepath.Base(physical) + "/.tessl/plugins/example/orphan/skills/review"
			},
		},
		{
			name: "absolute prefix with a dot segment before the project",
			target: func(_ *testing.T, root string) string {
				return filepath.Dir(root) + "/./" + filepath.Base(root) + "/.tessl/plugins/example/orphan/skills/review"
			},
		},
		{
			name: "absolute prefix with a doubled separator before the project",
			target: func(_ *testing.T, root string) string {
				return filepath.Dir(root) + "//" + filepath.Base(root) + "/.tessl/plugins/example/orphan/skills/review"
			},
		},
		{
			name: "absolute target through a symlink spelling of the project",
			target: func(t *testing.T, root string) string {
				alt := filepath.Join(t.TempDir(), "project")
				if err := os.Symlink(root, alt); err != nil {
					t.Fatal(err)
				}
				return alt + "/.tessl/plugins/example/orphan/skills/review"
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			target := testCase.target(t, root)
			linkSharedSkill(t, root, "reentry-alias", target)
			alias := filepath.Join(root, ".agents", "skills", "reentry-alias")
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("re-entry alias is not live before the run: %v", err)
			}
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil {
				t.Fatalf("eval alias: %v", err)
			}
			if !strings.Contains(filepath.ToSlash(resolved), "/.tessl/") {
				t.Fatalf("fixture does not resolve into .tessl: %s", resolved)
			}
			coexist(t, root)
			gitCommitFixture(t, root)
			before := hashTreeWithModes(t, root)

			preview, err := finalize(t, root, true)
			if err == nil {
				t.Fatal("finalize dry-run reported a cutover that would strand the alias")
			}
			if _, found := blockerFor(preview.Blockers, blockerSharedDangling, ".agents/skills/reentry-alias"); !found {
				t.Fatalf("dry-run blockers = %#v", preview.Blockers)
			}
			if preview.FinalizationReady || !preview.DryRun {
				t.Fatalf("dry-run report = %+v", struct{ Ready, DryRun bool }{preview.FinalizationReady, preview.DryRun})
			}
			if text := migrate.FormatCoexistenceText(preview); !strings.Contains(text, blockerSharedDangling) {
				t.Fatalf("dry-run text does not name the blocker:\n%s", text)
			}

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize succeeded and left a dangling user alias")
			}
			blocker, found := blockerFor(report.Blockers, blockerSharedDangling, ".agents/skills/reentry-alias")
			if !found {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if blocker.Remedy == "" {
				t.Fatal("dependency blocker has no remedy")
			}
			if report.FinalizationReady || report.DryRun || report.Wrote {
				t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
			}
			if text := migrate.FormatCoexistenceText(report); strings.Contains(text, "Tessl finalization applied.") {
				t.Fatalf("a refused apply claimed it applied:\n%s", text)
			}

			link, err := os.Readlink(alias)
			if err != nil || link != target {
				t.Fatalf("alias = %q, %v; want the user's link untouched", link, err)
			}
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("the alias target the run refused to strand is gone: %v", err)
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
			}
		})
	}
}

// TestFinalizeHandlesARelativeAliasThatLeavesThroughASiblingName is the
// remaining R2 shape: a relative target that leaves through a sibling whose
// name is not this project. A sibling symlink back into .tessl is not proven
// outside. A genuine external directory reached the same way still survives.
func TestFinalizeHandlesARelativeAliasThatLeavesThroughASiblingName(t *testing.T) {
	const (
		reviewSkill    = "# Review\n"
		externalSkill  = "# EXTERNAL SURVIVES\n"
		pluginSuffix   = "/.tessl/plugins/example/orphan/skills/review"
		externalSuffix = "/skills/review"
	)
	for _, testCase := range []struct {
		name     string
		absolute bool
		symlink  bool
		refuse   bool
		skill    string
		suffix   string
	}{
		{name: "relative sibling symlink back into .tessl", symlink: true, refuse: true, skill: reviewSkill, suffix: pluginSuffix},
		{name: "absolute sibling symlink back into .tessl", absolute: true, symlink: true, refuse: true, skill: reviewSkill, suffix: pluginSuffix},
		{name: "relative real external directory", skill: externalSkill, suffix: externalSuffix},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			outside := filepath.Join(filepath.Dir(root), "sibling-outside")
			if testCase.symlink {
				if err := os.Symlink(root, outside); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(outside, "skills", "review"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, "skills", "review", "SKILL.md"), []byte(externalSkill), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { os.RemoveAll(outside) })
			target := "../../../" + filepath.Base(outside) + testCase.suffix
			if testCase.absolute {
				target = outside + testCase.suffix
			}
			linkSharedSkill(t, root, "user-alias", target)
			alias := filepath.Join(root, ".agents", "skills", "user-alias")
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("sibling alias is not live before the run: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(alias, "SKILL.md"))
			if err != nil {
				t.Fatalf("read skill through alias: %v", err)
			}
			if string(got) != testCase.skill {
				t.Fatalf("alias opened %q, want %q", got, testCase.skill)
			}

			coexist(t, root)
			gitCommitFixture(t, root)
			before := hashTreeWithModes(t, root)
			beforeLink, err := os.Readlink(alias)
			if err != nil {
				t.Fatal(err)
			}

			preview, err := finalize(t, root, true)
			if testCase.refuse {
				if err == nil {
					t.Fatal("finalize dry-run reported a cutover that would strand the alias")
				}
				if _, found := blockerFor(preview.Blockers, blockerSharedDangling, ".agents/skills/user-alias"); !found {
					t.Fatalf("dry-run blockers = %#v", preview.Blockers)
				}
				if preview.FinalizationReady || preview.Wrote || !preview.DryRun {
					t.Fatalf("dry-run report = %+v", struct{ Ready, Wrote, DryRun bool }{preview.FinalizationReady, preview.Wrote, preview.DryRun})
				}
				if text := migrate.FormatCoexistenceText(preview); !strings.Contains(text, blockerSharedDangling) || strings.Contains(text, "Tessl finalization applied.") {
					t.Fatalf("dry-run text = %s", text)
				}
			} else if err != nil {
				t.Fatalf("finalize dry-run: %v (blockers %v)", err, blockerCodes(preview))
			} else if !preview.FinalizationReady || preview.Wrote || !preview.DryRun {
				t.Fatalf("dry-run report = %+v", struct{ Ready, Wrote, DryRun bool }{preview.FinalizationReady, preview.Wrote, preview.DryRun})
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("dry-run changed the project: before=%v after=%v", before, after)
			}

			report, err := finalize(t, root, false)
			link, readErr := os.Readlink(alias)
			if readErr != nil || link != beforeLink || link != target {
				t.Fatalf("alias = %q, %v; want the user's link untouched (%q)", link, readErr, target)
			}
			if testCase.refuse {
				if err == nil {
					t.Fatal("finalize succeeded and left a dangling user alias")
				}
				blocker, found := blockerFor(report.Blockers, blockerSharedDangling, ".agents/skills/user-alias")
				if !found {
					t.Fatalf("blockers = %#v", report.Blockers)
				}
				if blocker.Remedy == "" {
					t.Fatal("dependency blocker has no remedy")
				}
				if report.FinalizationReady || report.DryRun || report.Wrote {
					t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
				}
				if text := migrate.FormatCoexistenceText(report); strings.Contains(text, "Tessl finalization applied.") {
					t.Fatalf("a refused apply claimed it applied:\n%s", text)
				}
				if _, err := os.Stat(alias); err != nil {
					t.Fatalf("the alias target the run refused to strand is gone: %v", err)
				}
				if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
					t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
				}
			} else {
				if err != nil {
					t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
				}
				if !report.FinalizationReady || report.DryRun || !report.Wrote {
					t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
				}
				if text := migrate.FormatCoexistenceText(report); !strings.Contains(text, "Tessl finalization applied.") {
					t.Fatalf("successful apply text = %s", text)
				}
				if _, err := os.Stat(alias); err != nil {
					t.Fatalf("external alias no longer resolves: %v", err)
				}
				got, err = os.ReadFile(filepath.Join(alias, "SKILL.md"))
				if err != nil {
					t.Fatalf("read external skill after finalize: %v", err)
				}
				if string(got) != externalSkill {
					t.Fatalf("external skill changed: %q", got)
				}
				realizer := newService(vendorPanicRemote{}).realizer
				if _, err := realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
					t.Fatalf("acr check after finalize: %v", err)
				}
			}
			got, err = os.ReadFile(filepath.Join(alias, "SKILL.md"))
			if err != nil {
				t.Fatalf("read skill through alias after the run: %v", err)
			}
			if string(got) != testCase.skill {
				t.Fatalf("alias no longer opens %q: %q", testCase.skill, got)
			}
		})
	}
}

// TestFinalizeInspectsTheEntireExternalRetainedRoute exercises the real
// coexistence/finalization services: leaving the project is not a survival
// proof when a later stored component can lead back to a planned removal.
func TestFinalizeInspectsTheEntireExternalRetainedRoute(t *testing.T) {
	const pluginSuffix = "/.tessl/plugins/example/orphan/skills/review"
	for _, shape := range []string{
		"parent-alias", "directory-bridge", "deep-directory-bridge",
		"bridge-before-dotdot", "direct-project-alias", "real-directory",
		"real-directory-dotdot", "real-directory-reentry", "missing-component",
		"inspection-failure", "identity-failure",
	} {
		for _, absolute := range []bool{false, true} {
			spelling := "relative"
			if absolute {
				spelling = "absolute"
			}
			t.Run(shape+"/"+spelling, func(t *testing.T) {
				root := writeSharedSurfaceConsumer(t)
				outside := filepath.Join(filepath.Dir(root), "outside-route")
				if err := os.Mkdir(outside, 0o750); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.RemoveAll(outside); err != nil {
						t.Error(err)
					}
				})
				writeProjectFile(t, outside, "keep.txt", "outside bytes stay unchanged\n")
				if err := os.Chmod(filepath.Join(outside, "keep.txt"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeProjectFile(t, root, "team/SKILL.md", "# Project neighbour\n")
				suffix, skill, code := "", "# Review\n", blockerSharedUnproven
				link := func(name, target string) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(filepath.Join(outside, name)), 0o750); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, filepath.Join(outside, name)); err != nil {
						t.Fatal(err)
					}
				}
				switch shape {
				case "parent-alias":
					if err := os.RemoveAll(outside); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Dir(root), outside); err != nil {
						t.Fatal(err)
					}
					suffix = "/" + filepath.Base(root) + pluginSuffix
					if absolute {
						code = blockerSharedDangling // established absolute ancestor placement
					}
				case "directory-bridge", "deep-directory-bridge":
					bridge := "bridge"
					if shape == "deep-directory-bridge" {
						bridge = "one/two/bridge"
					}
					link(bridge, root)
					suffix = "/" + bridge + pluginSuffix
					if absolute {
						code = blockerSharedDangling // established absolute project placement
					}
				case "bridge-before-dotdot":
					link("one/two/bridge", filepath.Join(root, ".tessl/plugins/example/orphan"))
					writeProjectFile(t, root, ".tessl/plugins/example/team/SKILL.md", "# Through bridge parent\n")
					suffix, skill = "/one/two/bridge/../team", "# Through bridge parent\n"
				case "direct-project-alias":
					// The sibling itself is a direct project alias, as in FIX6.
					if err := os.RemoveAll(outside); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(root, outside); err != nil {
						t.Fatal(err)
					}
					suffix, code = pluginSuffix, blockerSharedDangling
				case "real-directory", "real-directory-dotdot":
					writeProjectFile(t, outside, "one/two/skills/review/SKILL.md", "# External survives\n")
					suffix, skill, code = "/one/two/skills/review", "# External survives\n", ""
					if shape == "real-directory-dotdot" {
						suffix = "/one/./two/../two/skills/review"
					}
				case "real-directory-reentry":
					if err := os.MkdirAll(filepath.Join(outside, "one/two"), 0o750); err != nil {
						t.Fatal(err)
					}
					suffix, code = "/one/two/../../../"+filepath.Base(root)+pluginSuffix, blockerSharedDangling
				case "missing-component":
					suffix, skill, code = "/missing/../"+filepath.Base(root)+pluginSuffix, "", ""
				case "inspection-failure":
					// ENAMETOOLONG is an inspection failure, not proof of absence.
					suffix, skill = "/"+strings.Repeat("x", 300)+"/skills/review", ""
				case "identity-failure":
					link("loop", "loop")
					suffix, skill = "/loop/skills/review", ""
				}
				target := "../../../" + filepath.Base(outside) + suffix
				if absolute {
					target = outside + suffix // preserve each stored component, including ..
				}
				linkSharedSkill(t, root, "user-route", target)
				alias := filepath.Join(root, ".agents/skills/user-route")
				assertSkill := func() {
					t.Helper()
					got, err := os.ReadFile(filepath.Join(alias, "SKILL.md"))
					if skill != "" && (err != nil || string(got) != skill) {
						t.Fatalf("retained skill = %q, %v; want %q", got, err, skill)
					}
					if skill == "" && err == nil {
						t.Fatal("broken/uninspectable fixture unexpectedly opened a skill")
					}
				}
				assertSkill()
				coexist(t, root)
				gitCommitFixture(t, root)
				beforeProject := snapshotRetainedRoute(t, root)
				beforeOutside := snapshotRetainedRoute(t, outside)
				for _, dryRun := range []bool{true, false} {
					report, err := finalize(t, root, dryRun)
					if code != "" {
						if err == nil || report.FinalizationReady || report.Wrote || report.DryRun != dryRun {
							t.Errorf("refusal: error=%v ready=%t wrote=%t dryRun=%t", err, report.FinalizationReady, report.Wrote, report.DryRun)
						}
						blocker, found := blockerFor(report.Blockers, code, ".agents/skills/user-route")
						if !found || blocker.Detail == "" || !strings.Contains(blocker.Remedy, ".agents/skills/user-route") {
							t.Errorf("missing named actionable blocker %s: %#v", code, report.Blockers)
						}
						text := migrate.FormatCoexistenceText(report)
						if !strings.Contains(text, code) || strings.Contains(text, "Tessl finalization applied.") {
							t.Errorf("refusal text = %s", text)
						}
					} else if err != nil || !report.FinalizationReady || report.Wrote == dryRun || report.DryRun != dryRun {
						t.Errorf("safe route: error=%v ready=%t wrote=%t dryRun=%t", err, report.FinalizationReady, report.Wrote, report.DryRun)
					}
					if dryRun || code != "" {
						if after := snapshotRetainedRoute(t, root); !reflect.DeepEqual(beforeProject, after) {
							t.Error("finalization changed project paths/bytes/modes/links")
						}
					}
					if after := snapshotRetainedRoute(t, outside); !reflect.DeepEqual(beforeOutside, after) {
						t.Error("finalization changed outside paths/bytes/modes/links")
					}
					got, readErr := os.Readlink(alias)
					if readErr != nil || got != target {
						t.Errorf("raw user link = %q, %v; want %q", got, readErr, target)
					}
					assertSkill()
				}
				if code == "" {
					if _, err := newService(vendorPanicRemote{}).realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
						t.Fatalf("acr check after finalize: %v", err)
					}
				}
			})
		}
	}
}

// Include .git and symlink modes as well as every path and exact content.
// WalkDir does not follow links, including when the fixture root is a link.
func snapshotRetainedRoute(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			value += " link " + target
		} else if info.Mode().IsRegular() {
			content, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			value += " bytes " + string(content)
		}
		result[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestFinalizeRefusesRetirementThroughAnUnownedShortcutDotDot is R9. A
// tessl__ link whose stored shortcut/.. the kernel follows into a distinct
// user skill is not the declared package skill, so finalization must not
// delete it and claim the ACR replacement is equivalent.
func TestFinalizeRefusesRetirementThroughAnUnownedShortcutDotDot(t *testing.T) {
	const userSkill = "# USER OWNED DISTINCT SKILL\n"
	for _, testCase := range []struct {
		name     string
		alias    string
		absolute bool
	}{
		{name: "exact tessl__review name", alias: "tessl__review"},
		{name: "custom tessl__ name", alias: "tessl__user-custom"},
		{name: "absolute exact name", alias: "tessl__review", absolute: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			if err := os.MkdirAll(filepath.Join(root, "team", "stage"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeProjectFile(t, root, "team/.tessl/plugins/example/orphan/skills/review/SKILL.md", userSkill)
			if err := os.Symlink("team/stage", filepath.Join(root, "shortcut")); err != nil {
				t.Fatal(err)
			}
			target := "../../shortcut/../.tessl/plugins/example/orphan/skills/review"
			if testCase.absolute {
				target = filepath.Join(root, "shortcut") + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.FromSlash(".tessl/plugins/example/orphan/skills/review")
			}
			if testCase.alias == "tessl__review" {
				if err := os.Remove(filepath.Join(root, ".agents", "skills", "tessl__review")); err != nil {
					t.Fatal(err)
				}
			}
			linkSharedSkill(t, root, testCase.alias, target)
			alias := filepath.Join(root, ".agents", "skills", testCase.alias)
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("retirement alias is not live before the run: %v", err)
			}
			skill := filepath.Join(alias, "SKILL.md")
			got, err := os.ReadFile(skill)
			if err != nil {
				t.Fatalf("read user skill through alias: %v", err)
			}
			if string(got) != userSkill {
				t.Fatalf("alias opened %q, want the distinct user skill", got)
			}
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil {
				t.Fatalf("eval alias: %v", err)
			}
			if !strings.Contains(filepath.ToSlash(resolved), "/team/.tessl/") {
				t.Fatalf("kernel did not follow shortcut into the user skill: %s", resolved)
			}

			coexist(t, root)
			gitCommitFixture(t, root)
			before := hashTreeWithModes(t, root)
			beforeSkill := readProjectFile(t, root, "team/.tessl/plugins/example/orphan/skills/review/SKILL.md")

			preview, err := finalize(t, root, true)
			if err == nil {
				t.Fatal("finalize dry-run reported a cutover that would retire the user alias")
			}
			if preview.FinalizationReady || !preview.DryRun {
				t.Fatalf("dry-run report = %+v", struct{ Ready, DryRun bool }{preview.FinalizationReady, preview.DryRun})
			}
			if text := migrate.FormatCoexistenceText(preview); strings.Contains(text, "Tessl finalization applied.") {
				t.Fatalf("a refused dry-run claimed it applied:\n%s", text)
			}

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize deleted a tessl__ alias that opened a distinct user skill")
			}
			if report.FinalizationReady || report.DryRun || report.Wrote {
				t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
			}
			if text := migrate.FormatCoexistenceText(report); strings.Contains(text, "Tessl finalization applied.") {
				t.Fatalf("a refused apply claimed it applied:\n%s", text)
			}
			if _, found := blockerFor(report.Blockers, blockerSharedUnproven, ".agents/skills/"+testCase.alias); !found {
				if _, found = blockerFor(preview.Blockers, blockerSharedUnproven, ".agents/skills/"+testCase.alias); !found {
					t.Fatalf("blockers = %#v (preview %#v)", report.Blockers, preview.Blockers)
				}
			}

			link, err := os.Readlink(alias)
			if err != nil || link != target {
				t.Fatalf("alias = %q, %v; want the user's link untouched", link, err)
			}
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("the alias was removed: %v", err)
			}
			if after := readProjectFile(t, root, "team/.tessl/plugins/example/orphan/skills/review/SKILL.md"); after != beforeSkill {
				t.Fatalf("user skill bytes changed: %q", after)
			}
			got, err = os.ReadFile(skill)
			if err != nil {
				t.Fatalf("read user skill through alias after refusal: %v", err)
			}
			if string(got) != userSkill {
				t.Fatalf("alias no longer opens the user skill: %q", got)
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("blocked finalization changed the project: before=%v after=%v", before, after)
			}
		})
	}
}

// TestFinalizeKeepsAUserAliasThatDependsOnNothingRemoved holds the other side:
// inspecting a link grants no deletion ownership, and an alias whose target
// survives is neither blocked nor touched.
func TestFinalizeKeepsAUserAliasThatDependsOnNothingRemoved(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, "team/skills/review/SKILL.md", "# Team\n")
	// A neighbour whose name merely starts with a removed native's name. The
	// comparison is on path components, so it is not mistaken for one.
	writeProjectFile(t, root, ".claude/skills/tessl__review-mine/SKILL.md", "# Mine\n")
	linkSharedSkill(t, root, "my-alias", "../../team/skills/review")
	linkSharedSkill(t, root, "outside-alias", "../../../outside/skills/review")
	linkSharedSkill(t, root, "absolute-alias", filepath.Join(root, "team", "skills", "review"))
	linkSharedSkill(t, root, "absolute-outside-alias", filepath.Join(filepath.Dir(root), "elsewhere"))
	linkSharedSkill(t, root, "neighbour-alias", "../../.claude/skills/tessl__review-mine")
	linkSharedSkill(t, root, "reentry-surviving-alias", "../../../"+filepath.Base(root)+"/team/skills/review")
	// Real-directory dot segments are ordinary path motion. Cleaning them is
	// fine; refusing them as if they were unowned links is not.
	linkSharedSkill(t, root, "dot-alias", "../../team/./skills/review")
	linkSharedSkill(t, root, "dotdot-real-alias", "../../team/skills/../skills/review")
	linkSharedSkill(t, root, "absolute-dot-alias", filepath.Join(root, "team")+string(filepath.Separator)+"."+string(filepath.Separator)+filepath.Join("skills", "review"))
	linkSharedSkill(t, root, "absolute-dotdot-real-alias", filepath.Join(root, "team", "skills")+string(filepath.Separator)+".."+string(filepath.Separator)+filepath.Join("skills", "review"))
	// A pre-existing broken alias is not this run's to repair or to refuse.
	linkSharedSkill(t, root, "already-broken", "../../team/skills/does-not-exist")
	for _, name := range []string{"my-alias", "dot-alias", "dotdot-real-alias", "absolute-alias", "absolute-dot-alias", "absolute-dotdot-real-alias", "neighbour-alias", "reentry-surviving-alias"} {
		if _, err := os.Stat(filepath.Join(root, ".agents", "skills", name)); err != nil {
			t.Fatalf("%s is not live before the run: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "skills", "already-broken")); err == nil {
		t.Fatal("already-broken alias resolved; the fixture is not a missing-component case")
	}
	coexist(t, root)
	gitCommitFixture(t, root)

	report, err := finalize(t, root, false)
	if err != nil {
		t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
	}
	for _, name := range []string{"my-alias", "outside-alias", "absolute-alias", "absolute-outside-alias", "neighbour-alias", "dot-alias", "dotdot-real-alias", "absolute-dot-alias", "absolute-dotdot-real-alias", "reentry-surviving-alias", "already-broken"} {
		if _, err := os.Lstat(filepath.Join(root, ".agents", "skills", name)); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
	if got := readProjectFile(t, root, "team/skills/review/SKILL.md"); got != "# Team\n" {
		t.Fatalf("user skill changed: %q", got)
	}
	// Every alias the run kept is still usable, not merely still a link.
	for _, name := range []string{"my-alias", "absolute-alias", "neighbour-alias", "dot-alias", "dotdot-real-alias", "absolute-dot-alias", "absolute-dotdot-real-alias", "reentry-surviving-alias"} {
		if _, err := os.Stat(filepath.Join(root, ".agents", "skills", name)); err != nil {
			t.Fatalf("%s no longer resolves after finalization: %v", name, err)
		}
	}
	broken, err := os.Readlink(filepath.Join(root, ".agents", "skills", "already-broken"))
	if err != nil || broken != "../../team/skills/does-not-exist" {
		t.Fatalf("already-broken alias = %q, %v", broken, err)
	}
}

// TestFinalizeDemotesAGeneratedMarkdownHostItEmpties is R4. The host is built
// the ordinary way — ACR generates it through coexistence, a later Tessl
// install appends its managed span, a second coexistence pass records shared
// ownership — so no ledger or block is fabricated to reach the boundary.
func TestFinalizeDemotesAGeneratedMarkdownHostItEmpties(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		prose       string
		wantDemoted bool
	}{
		{name: "no user prose at all", prose: "", wantDemoted: true},
		{name: "real user prose", prose: "# User\n\nKeep real user prose.\n\n", wantDemoted: false},
		{name: "whitespace only", prose: "\n", wantDemoted: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			// Tessl's own rule index, which its managed span includes.
			writeProjectFile(t, root, ".tessl/RULES.md", "# Agent Rules\n\n@plugins/example/orphan/rules/always.md\n")
			if testCase.prose != "" {
				writeProjectFile(t, root, "CLAUDE.md", testCase.prose)
			}
			coexist(t, root)
			generated := readProjectFile(t, root, "CLAUDE.md")
			if !strings.Contains(generated, "<!-- acr:begin ") {
				t.Fatalf("coexistence generated no ACR block:\n%s", generated)
			}
			writeProjectFile(t, root, "CLAUDE.md", generated+"## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n")
			coexist(t, root)
			gitCommitFixture(t, root)

			shared := false
			for _, target := range finalizeLedger(t, root).Targets {
				if target.Path == "CLAUDE.md" && target.Ownership == realize.OwnershipShared {
					shared = true
				}
			}
			if !shared {
				t.Fatal("fixture does not produce a shared CLAUDE.md; the boundary it guards cannot occur")
			}

			report, err := finalize(t, root, false)
			if err != nil {
				t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
			}
			demoted := false
			for _, record := range report.Reanchored {
				if record.Path == "CLAUDE.md" && record.OwnershipAfter == string(realize.OwnershipGenerated) {
					demoted = true
				}
			}
			if demoted != testCase.wantDemoted {
				t.Fatalf("demoted = %v, want %v; reanchored = %#v", demoted, testCase.wantDemoted, report.Reanchored)
			}

			after := readProjectFile(t, root, "CLAUDE.md")
			if strings.Contains(after, "tessl-managed") {
				t.Fatalf("the Tessl span survived:\n%s", after)
			}
			if !strings.Contains(after, "<!-- acr:begin ") || !strings.Contains(after, "<!-- acr:end ") {
				t.Fatalf("ACR's own block was damaged:\n%s", after)
			}
			if testCase.prose != "" && !strings.HasPrefix(after, testCase.prose) {
				t.Fatalf("user prose changed:\n%q", after)
			}

			realizer := newService(vendorPanicRemote{}).realizer
			if _, err := realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
				t.Fatalf("acr check after finalize: %v", err)
			}
			result, err := realizer.Run(context.Background(), root, nil, realize.ModeApply)
			if err != nil {
				t.Fatalf("acr realize after finalize: %v", err)
			}
			if result.Plan.HasChanges() {
				t.Fatalf("realize after finalize planned changes: %#v", result.Plan.Operations)
			}
		})
	}
}

// writeCodexRuleConsumer is a Tessl-first, rule-bearing Codex consumer: Tessl
// wrote its managed heading into AGENTS.md before ACR existed in this project,
// so ACR's block lands inside that heading's span.
func writeCodexRuleConsumer(t *testing.T, prose string) string {
	t.Helper()
	root := writeUnmappedConsumer(t)
	if err := os.MkdirAll(filepath.Join(root, ".codex", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../.tessl/plugins/example/orphan/skills/review", filepath.Join(root, ".codex/skills/tessl__review")); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ".tessl/RULES.md", "# Agent Rules\n\n@plugins/example/orphan/rules/always.md\n")
	writeProjectFile(t, root, "AGENTS.md", prose+"## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md follow the [instructions](.tessl/RULES.md)\n")
	return root
}

// TestTesslFirstCodexHostFinalizes is F-T1. A Tessl heading span runs to the
// next same-or-higher heading or to EOF, so a consumer that installed Tessl
// first ends up with ACR's block inside that span. It read as extra content in
// a Tessl-owned span, the host became ambiguous, and the only way out was
// editing AGENTS.md by hand.
func TestTesslFirstCodexHostFinalizes(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		prose string
	}{
		{name: "no user prose", prose: ""},
		{name: "user prose before the Tessl heading", prose: "# User\n\nPrefix prose.\n\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeCodexRuleConsumer(t, testCase.prose)
			coexist(t, root)
			host := readProjectFile(t, root, "AGENTS.md")
			if !strings.Contains(host, "tessl-managed") || !strings.Contains(host, "<!-- acr:begin ") {
				t.Fatalf("fixture does not place an ACR block inside the Tessl span:\n%s", host)
			}
			block := host[strings.Index(host, "<!-- acr:begin "):]
			gitCommitFixture(t, root)

			report, err := finalize(t, root, false)
			if err != nil {
				t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
			}
			after := readProjectFile(t, root, "AGENTS.md")
			if strings.Contains(after, "tessl-managed") || strings.Contains(after, "@.tessl/RULES.md") {
				t.Fatalf("Tessl bytes survived:\n%s", after)
			}
			if after != testCase.prose+block {
				t.Fatalf("host = %q, want the user prefix followed by ACR's own block unchanged", after)
			}

			realizer := newService(vendorPanicRemote{}).realizer
			if _, err := realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
				t.Fatalf("acr check after finalize: %v", err)
			}
			result, err := realizer.Run(context.Background(), root, nil, realize.ModeApply)
			if err != nil {
				t.Fatalf("acr realize after finalize: %v", err)
			}
			if result.Plan.HasChanges() {
				t.Fatalf("realize after finalize planned changes: %#v", result.Plan.Operations)
			}
			repeat, err := finalize(t, root, false)
			if err != nil {
				t.Fatalf("repeat finalize: %v (blockers %v)", err, blockerCodes(repeat))
			}
			if repeat.Wrote {
				t.Fatalf("repeat finalize wrote: %#v", repeat)
			}
			if got := readProjectFile(t, root, "AGENTS.md"); got != after {
				t.Fatalf("repeat finalize changed the host:\n%s", got)
			}
		})
	}
}

// TestTesslSpanBoundaryNeedsProvenACROwnership keeps the boundary honest: a
// line that merely looks like an ACR marker establishes nothing, so the span
// stays foreign and the host still refuses.
func TestTesslSpanBoundaryNeedsProvenACROwnership(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		extra string
	}{
		{name: "marker-shaped user line", extra: "<!-- acr:begin id=deadbeef -->\nUser text.\n"},
		{name: "opening marker with no close", extra: "<!-- acr:begin id=" + strings.Repeat("a", 64) + " source=vendor:example/orphan artifact=always adapter=codex prefix=none -->\nUser text.\n"},
		{name: "plain user prose inside the span", extra: "Some user note inside the Tessl span.\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeUnmappedConsumer(t)
			writeProjectFile(t, root, ".tessl/RULES.md", "# Agent Rules\n\n@plugins/example/orphan/rules/always.md\n")
			host := "## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n" + testCase.extra
			writeProjectFile(t, root, "AGENTS.md", host)
			coexist(t, root)
			gitCommitFixture(t, root)

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatalf("finalize accepted an unproven span boundary; host now:\n%s", readProjectFile(t, root, "AGENTS.md"))
			}
			if !hasBlocker(report, blockerAmbiguousPath, "AGENTS.md") {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if got := readProjectFile(t, root, "AGENTS.md"); !strings.Contains(got, testCase.extra) {
				t.Fatalf("user content changed:\n%s", got)
			}
		})
	}
}

// TestFinalizeRefusalsReportThemselves is R6 for the two gates that ran after
// planning and returned the pre-refusal report untouched: readiness stayed
// true and blockers[] stayed empty while the envelope failed.
func TestFinalizeRefusalsReportThemselves(t *testing.T) {
	t.Run("untracked manifest", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		gitInitFixture(t, root)
		coexist(t, root)
		gitAddAllExcept(t, root, "tessl.json")
		before := hashTree(t, root)

		report, err := finalize(t, root, true)
		if err == nil {
			t.Fatal("finalize accepted an untracked manifest")
		}
		if report.FinalizationReady {
			t.Fatal("the tracking refusal reported readiness")
		}
		blocker, found := blockerFor(report.Blockers, blockerUntrackedState, "")
		if !found {
			t.Fatalf("blockers = %#v", report.Blockers)
		}
		if !strings.Contains(blocker.Remedy, "git add tessl.json") {
			t.Fatalf("remedy = %q", blocker.Remedy)
		}
		if after := hashTree(t, root); !mapsEqual(before, after) {
			t.Fatalf("the refusal changed the project: before=%v after=%v", before, after)
		}
	})

	t.Run("pending coexistence", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		coexist(t, root)
		gitCommitFixture(t, root)
		// A realized output removed after coexistence leaves the plan unapplied.
		if err := os.Remove(filepath.Join(root, ".claude/skills/acr__example__orphan__review/SKILL.md")); err != nil {
			t.Fatal(err)
		}
		before := hashTree(t, root)

		report, err := finalize(t, root, true)
		if err == nil {
			t.Fatal("finalize accepted a project with pending coexistence changes")
		}
		if report.FinalizationReady {
			t.Fatal("the pending-coexistence refusal reported readiness")
		}
		blocker, found := blockerFor(report.Blockers, blockerPendingCoexistence, "")
		if !found {
			t.Fatalf("blockers = %#v", report.Blockers)
		}
		if blocker.Remedy == "" {
			t.Fatal("the pending-coexistence blocker has no remedy")
		}
		if after := hashTree(t, root); !mapsEqual(before, after) {
			t.Fatalf("the refusal changed the project: before=%v after=%v", before, after)
		}
	})

	t.Run("a dry run keeps its own mode", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		coexist(t, root)
		gitCommitFixture(t, root)

		preview, err := finalize(t, root, true)
		if err != nil {
			t.Fatalf("finalize dry-run: %v", err)
		}
		if !preview.DryRun || preview.Wrote {
			t.Fatalf("dry run = %+v", struct {
				DryRun bool
				Wrote  bool
			}{preview.DryRun, preview.Wrote})
		}
		applied, err := finalize(t, root, false)
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if applied.DryRun {
			t.Fatal("an apply reported itself as a dry run")
		}
	})
}

func gitInitFixture(t *testing.T, root string) {
	t.Helper()
	runGitFixture(t, root, "init", "-q")
	runGitFixture(t, root, "commit", "-q", "--allow-empty", "-m", "empty")
}

func gitAddAllExcept(t *testing.T, root, exclude string) {
	t.Helper()
	runGitFixture(t, root, "add", "-A")
	runGitFixture(t, root, "rm", "--cached", "-q", exclude)
	runGitFixture(t, root, "commit", "-q", "-m", "fixture")
}

func runGitFixture(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", fixtureGitArguments(arguments)...)
	command.Dir = root
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=ACR Test", "GIT_AUTHOR_EMAIL=acr@example.invalid",
		"GIT_COMMITTER_NAME=ACR Test", "GIT_COMMITTER_EMAIL=acr@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

// TestFinalizeTextNeverClaimsAnApplyThatDidNotHappen is R6(b). The headline
// was read from the dry-run flag alone, so every refused or rolled-back apply
// printed "Tessl finalization applied." with zero removals.
func TestFinalizeTextNeverClaimsAnApplyThatDidNotHappen(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "a dangling user alias",
			setup: func(t *testing.T, root string) {
				linkSharedSkill(t, root, "my-alias", "../../.tessl/plugins/example/orphan/skills/review")
			},
		},
		{
			name: "an unreadable MCP config",
			setup: func(t *testing.T, root string) {
				writeProjectFile(t, root, ".mcp.json", `{"mcpServers":{"tessl":`)
			},
		},
		{
			name: "GitHub MCP evidence",
			setup: func(t *testing.T, root string) {
				writeProjectFile(t, root, ".github/mcp.json", `{"mcpServers":{"tessl":{"type":"stdio","command":"tessl","args":["mcp","start"]}}}`+"\n")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			testCase.setup(t, root)
			coexist(t, root)
			gitCommitFixture(t, root)

			report, err := finalize(t, root, false)
			if err == nil {
				t.Fatal("finalize succeeded, so this case proves nothing")
			}
			text := migrate.FormatCoexistenceText(report)
			if strings.Contains(text, "Tessl finalization applied.") {
				t.Fatalf("a refused apply claimed it applied:\n%s", text)
			}
			if !strings.Contains(text, "Tessl finalization refused.") {
				t.Fatalf("text does not name the refusal:\n%s", text)
			}
			if len(report.Removed) != 0 {
				t.Fatalf("a refused apply reported removals: %#v", report.Removed)
			}
		})
	}

	t.Run("a completed apply still says applied", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, false)
		if err != nil {
			t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
		}
		if !strings.Contains(migrate.FormatCoexistenceText(report), "Tessl finalization applied.") {
			t.Fatalf("a completed apply did not say applied:\n%s", migrate.FormatCoexistenceText(report))
		}
	})

	t.Run("a dry run says dry-run", func(t *testing.T) {
		root := writeSharedSurfaceConsumer(t)
		coexist(t, root)
		gitCommitFixture(t, root)
		report, err := finalize(t, root, true)
		if err != nil {
			t.Fatalf("finalize dry-run: %v", err)
		}
		if !strings.Contains(migrate.FormatCoexistenceText(report), "Tessl finalization dry-run.") {
			t.Fatalf("a dry run did not say dry-run:\n%s", migrate.FormatCoexistenceText(report))
		}
	})
}

// TestFinalizeRefusesAnUnaddressableMCPLayout is R6(a). The same canonical
// object written as an inline table is just as positively identified, but has
// no header and no per-field location to splice. Planning used to fail with
// that, returning the coexistence report it had built before the finalize
// branch initialized anything.
func TestFinalizeRefusesAnUnaddressableMCPLayout(t *testing.T) {
	const inline = "[mcp_servers]\ntessl = { type = \"stdio\", command = \"tessl\", args = [\"mcp\", \"start\"] }\n[tools]\nweb_search = true\n"
	for _, dryRun := range []bool{true, false} {
		name := "apply"
		if dryRun {
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			root := writeSharedSurfaceConsumer(t)
			writeProjectFile(t, root, ".codex/config.toml", inline)
			coexist(t, root)
			gitCommitFixture(t, root)
			// Coexistence appends ACR's own hook table, so the untouched
			// bytes are whatever the host holds now.
			host := readProjectFile(t, root, ".codex/config.toml")
			before := hashTreeWithModes(t, root)

			report, err := finalize(t, root, dryRun)
			if err == nil {
				t.Fatal("finalize accepted an entry it cannot address")
			}
			if report.FinalizationReady {
				t.Fatal("the refusal reported readiness")
			}
			if report.Mode != "finalize" {
				t.Fatalf("mode = %q, want the finalize mode", report.Mode)
			}
			if report.DryRun != dryRun {
				t.Fatalf("dryRun = %v, want the invocation's own mode", report.DryRun)
			}
			blocker, found := blockerFor(report.Blockers, blockerMCPLayout, ".codex/config.toml")
			if !found {
				t.Fatalf("blockers = %#v", report.Blockers)
			}
			if blocker.Remedy == "" {
				t.Fatal("the layout blocker has no remedy")
			}
			if got := readProjectFile(t, root, ".codex/config.toml"); got != host {
				t.Fatalf(".codex/config.toml changed:\n%s", got)
			}
			if !strings.Contains(host, `tessl = { type = "stdio"`) {
				t.Fatalf("the fixture lost its inline entry before finalizing:\n%s", host)
			}
			if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
				t.Fatalf("the refusal changed the project: before=%v after=%v", before, after)
			}
		})
	}
}

// TestFailedRecoveryIsNotCertifiedComplete is R6(c). A concurrent write to a
// file the transaction had already changed leaves recovery unable to finish.
// It correctly refuses to overwrite that content and preserves its journal;
// the report must say so instead of certifying a complete restore.
func TestFailedRecoveryIsNotCertifiedComplete(t *testing.T) {
	const concurrent = `{"mcpServers":{"notes":{"type":"stdio","command":"notes-server"}}}` + "\n"
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".mcp.json", canonicalMCPJSON)
	coexist(t, root)
	gitCommitFixture(t, root)

	injected := errors.New("injected failure after a live edit")
	spliced := false
	original := applyFinalizationFileTransaction
	applyFinalizationFileTransaction = func(projectDirectory string, edits []realize.FileTransactionEdit, finalize func() error) error {
		return realize.ApplyFileTransactionWithHooks(projectDirectory, edits, finalize, realize.FileTransactionHooks{
			AfterEdit: func(_ int, edit realize.FileTransactionEdit) error {
				if edit.Path != ".mcp.json" {
					return nil
				}
				content, err := os.ReadFile(filepath.Join(projectDirectory, ".mcp.json"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(content), `"tessl"`) {
					t.Fatalf("the splice had not landed when the conflict was introduced:\n%s", content)
				}
				spliced = true
				// A different, valid concurrent write: the file now matches
				// neither the planned after-state nor the recorded before-image.
				if err := os.WriteFile(filepath.Join(projectDirectory, ".mcp.json"), []byte(concurrent), 0o644); err != nil {
					t.Fatal(err)
				}
				return injected
			},
		})
	}
	defer func() { applyFinalizationFileTransaction = original }()

	report, err := finalize(t, root, false)
	if err == nil {
		t.Fatal("finalize succeeded despite an injected failure")
	}
	if !spliced {
		t.Fatal("the transaction never spliced .mcp.json; the test proves nothing")
	}
	if !strings.Contains(err.Error(), "automatic recovery failed") {
		t.Fatalf("error = %v, want the incomplete recovery named", err)
	}
	if got := readProjectFile(t, root, ".mcp.json"); got != concurrent {
		t.Fatalf("recovery overwrote the concurrent write:\n%s", got)
	}
	entries, readErr := os.ReadDir(filepath.Join(root, ".agents", ".acr-transactions"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	journals := 0
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			journals++
		}
	}
	if journals == 0 {
		t.Fatal("the journal was not preserved for reconciliation")
	}

	blocker, found := blockerFor(report.Blockers, blockerRecoveryConflict, "")
	if !found {
		t.Fatalf("blockers = %#v", report.Blockers)
	}
	if strings.Contains(blocker.Remedy, "restored every file") {
		t.Fatalf("an incomplete recovery was certified complete: %q", blocker.Remedy)
	}
	if !strings.Contains(blocker.Remedy, "journal is preserved at") {
		t.Fatalf("remedy names no journal to reconcile against: %q", blocker.Remedy)
	}
	if report.FinalizationReady || report.DryRun || report.Wrote {
		t.Fatalf("report = %+v", struct{ Ready, DryRun, Wrote bool }{report.FinalizationReady, report.DryRun, report.Wrote})
	}
	if strings.Contains(migrate.FormatCoexistenceText(report), "Tessl finalization applied.") {
		t.Fatal("a failed recovery claimed the finalization applied")
	}
}

// TestFinalizeSynchronizesGitExclusionForADemotedHost is R4. Changing a
// target's ownership moves it into the local Git exclusion block, and leaving
// that to the next realization made a successful finalization hand back a
// project with pending work: check exited 3 and realize wrote the repair.
func TestFinalizeSynchronizesGitExclusionForADemotedHost(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		prose string
		track bool
	}{
		{name: "untracked generated host", prose: ""},
		{name: "tracked generated host", prose: "", track: true},
		{name: "one blank line", prose: "\n"},
		{name: "real user prose", prose: "# User\n\nPrefix prose.\n\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := writeCodexRuleConsumerWithGit(t, testCase.prose)
			// ACR generates the host first, then a later Tessl install appends
			// its span, then ordinary coexistence records shared ownership.
			coexist(t, root)
			generated := readProjectFile(t, root, "AGENTS.md")
			if !strings.Contains(generated, "<!-- acr:begin ") {
				t.Fatalf("coexistence generated no ACR block:\n%s", generated)
			}
			writeProjectFile(t, root, "AGENTS.md", generated+"## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md follow the [instructions](.tessl/RULES.md)\n")
			coexist(t, root)

			// Commit the recovery inputs finalization requires, and only those:
			// the generated host stays untracked unless this case tracks it.
			runGitFixture(t, root, "add", "tessl.json", ".tessl", ".agents", "agents.yaml")
			if testCase.track {
				runGitFixture(t, root, "add", "AGENTS.md")
			}
			runGitFixture(t, root, "commit", "-qm", "recovery inputs")

			block := acrBlockOf(t, readProjectFile(t, root, "AGENTS.md"))

			preview, err := finalize(t, root, true)
			if err != nil {
				t.Fatalf("finalize dry-run: %v (blockers %v)", err, blockerCodes(preview))
			}
			report, err := finalize(t, root, false)
			if err != nil {
				t.Fatalf("finalize: %v (blockers %v)", err, blockerCodes(report))
			}
			if after := readProjectFile(t, root, "AGENTS.md"); after != testCase.prose+block {
				t.Fatalf("host = %q, want the user prefix followed by ACR's own block unchanged", after)
			}

			// The promised post-finalization state: clean immediately, with no
			// repair realization in between.
			realizer := newService(vendorPanicRemote{}).realizer
			if _, err := realizer.Run(context.Background(), root, nil, realize.ModeCheck); err != nil {
				t.Fatalf("acr check immediately after finalize: %v", err)
			}
			result, err := realizer.Run(context.Background(), root, nil, realize.ModeApply)
			if err != nil {
				t.Fatalf("acr realize after finalize: %v", err)
			}
			if result.Plan.HasChanges() {
				t.Fatalf("realize after finalize planned changes: %#v", result.Plan.Operations)
			}

			// Only a host the splice leaves wholly ACR-owned is demoted, and
			// only an untracked generated-only target belongs in the exclusion
			// block. A host that keeps user content stays shared, and a shared
			// target is never excluded.
			demoted := false
			for _, record := range report.Reanchored {
				if record.Path == "AGENTS.md" && record.OwnershipAfter == string(realize.OwnershipGenerated) {
					demoted = true
				}
			}
			wantExcluded := demoted && !testCase.track
			excluded := map[string]bool{}
			for _, target := range finalizeLedger(t, root).Targets {
				excluded[target.Path] = target.Excluded
			}
			if excluded["AGENTS.md"] != wantExcluded {
				t.Fatalf("AGENTS.md excluded = %v, want %v (demoted %v, tracked %v)", excluded["AGENTS.md"], wantExcluded, demoted, testCase.track)
			}
			exclude := readProjectFile(t, root, ".git/info/exclude")
			if strings.Contains(exclude, "AGENTS.md") != wantExcluded {
				t.Fatalf(".git/info/exclude names AGENTS.md = %v, want %v:\n%s", strings.Contains(exclude, "AGENTS.md"), wantExcluded, exclude)
			}
			if wantExcluded && !strings.Contains(migrate.FormatCoexistenceText(preview), "git-exclusion") {
				t.Fatalf("the dry run did not name the exclusion edit:\n%s", migrate.FormatCoexistenceText(preview))
			}
		})
	}
}

// acrBlockOf returns exactly ACR's own managed block, from its opening marker
// through its closing one.
func acrBlockOf(t *testing.T, host string) string {
	t.Helper()
	start := strings.Index(host, "<!-- acr:begin ")
	closing := strings.Index(host, "<!-- acr:end ")
	if start < 0 || closing < 0 {
		t.Fatalf("host carries no ACR block:\n%s", host)
	}
	end := strings.Index(host[closing:], " -->")
	if end < 0 {
		t.Fatalf("host has an unterminated ACR block:\n%s", host)
	}
	return host[start : closing+end+len(" -->")+1]
}

// writeCodexRuleConsumerWithGit is the rule-bearing Codex consumer with Git
// initialized before the first coexistence, so generated outputs are untracked
// exactly as an ordinary consumer leaves them.
func writeCodexRuleConsumerWithGit(t *testing.T, prose string) string {
	t.Helper()
	root := writeUnmappedConsumer(t)
	runGitFixture(t, root, "init", "-q")
	if err := os.MkdirAll(filepath.Join(root, ".codex", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../.tessl/plugins/example/orphan/skills/review", filepath.Join(root, ".codex/skills/tessl__review")); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ".tessl/RULES.md", "# Agent Rules\n\n@plugins/example/orphan/rules/always.md\n")
	if prose != "" {
		writeProjectFile(t, root, "AGENTS.md", prose)
	}
	return root
}

// TestFixtureGitStateIsDeterministicAndProtected is T-1. The route snapshot
// enumerates .git on purpose, so the fixture's Git subprocesses run with
// automatic maintenance and garbage collection disabled for that process
// alone. The snapshot keeps its teeth: a controlled change to protected Git
// data still fails the oracle.
func TestFixtureGitStateIsDeterministicAndProtected(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	coexist(t, root)
	gitCommitFixture(t, root)

	for _, setting := range []struct{ key, want string }{
		{key: "maintenance.auto", want: "false"},
		{key: "gc.auto", want: "0"},
	} {
		command := exec.Command("git", fixtureGitArguments([]string{"config", "--get", setting.key})...)
		command.Dir = root
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		output, err := command.Output()
		if err != nil || strings.TrimSpace(string(output)) != setting.want {
			t.Fatalf("fixture git %s = %q, %v; want %q", setting.key, output, err, setting.want)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", "gc.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("automatic maintenance reported into the fixture repository: %v", err)
	}

	before := snapshotRetainedRoute(t, root)
	if again := snapshotRetainedRoute(t, root); !reflect.DeepEqual(before, again) {
		t.Fatal("two consecutive snapshots of an idle fixture disagree")
	}
	for _, relative := range []string{".git/config", ".git/info/exclude", ".git/HEAD", ".git/index"} {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		original, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if err := os.WriteFile(filename, append(append([]byte(nil), original...), '\n'), 0o644); err != nil {
			t.Fatalf("mutate %s: %v", relative, err)
		}
		if mutated := snapshotRetainedRoute(t, root); reflect.DeepEqual(before, mutated) {
			t.Fatalf("the snapshot did not notice a change to %s", relative)
		}
		if err := os.WriteFile(filename, original, 0o644); err != nil {
			t.Fatalf("restore %s: %v", relative, err)
		}
		if restored := snapshotRetainedRoute(t, root); !reflect.DeepEqual(before, restored) {
			t.Fatalf("restoring %s did not restore the snapshot", relative)
		}
	}
}
