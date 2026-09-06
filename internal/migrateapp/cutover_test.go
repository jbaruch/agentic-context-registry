package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
[mcp_servers.tessl]
type = "stdio"
command = "tessl"
args = [ "mcp", "start" ]

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
	for _, want := range []string{"# operator comment above the Tessl table", "[mcp_servers.notes]", "[tools]", "web_search = true"} {
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

// TestFinalizeRollsBackSharedAndMCPEdits is D16: a failure between the link
// removal and the config splice restores link targets and config bytes.
func TestFinalizeRollsBackSharedAndMCPEdits(t *testing.T) {
	root := writeSharedSurfaceConsumer(t)
	writeProjectFile(t, root, ".mcp.json", canonicalMCPJSON)
	coexist(t, root)
	gitCommitFixture(t, root)
	before := hashTree(t, root)
	linkBefore, err := os.Readlink(filepath.Join(root, ".agents/skills/tessl__review"))
	if err != nil {
		t.Fatal(err)
	}

	original := applyFinalizationFileTransaction
	applyFinalizationFileTransaction = func(string, []realize.FileTransactionEdit, func() error) error {
		return errors.New("injected transaction failure")
	}
	defer func() { applyFinalizationFileTransaction = original }()

	if _, err := finalize(t, root, false); err == nil {
		t.Fatal("finalize succeeded despite an injected transaction failure")
	}
	if after := hashTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("rolled-back finalization changed the project: before=%v after=%v", before, after)
	}
	linkAfter, err := os.Readlink(filepath.Join(root, ".agents/skills/tessl__review"))
	if err != nil || linkAfter != linkBefore {
		t.Fatalf("link target = %q, %v; want %q restored", linkAfter, err, linkBefore)
	}
	if got := readProjectFile(t, root, ".mcp.json"); got != canonicalMCPJSON {
		t.Fatalf(".mcp.json was not restored:\n%s", got)
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
