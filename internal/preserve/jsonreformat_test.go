package preserve

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// coexistenceSettings mirrors a Claude settings file that Tessl and ACR both
// write: Tessl owns the other hooks and re-indents the file when it installs.
func coexistenceSettings(t *testing.T) (adapter.ConfigEntry, adapter.SharedCompilation) {
	t.Helper()
	entry := testConfigEntry(adapter.ConfigJSON, []string{"hooks", "SessionStart"}, adapter.ConfigElement, "session-start",
		`{"hooks":[{"type":"command","command":"${CLAUDE_PROJECT_DIR}/.claude/hooks/acr/session-start.sh","args":["--policy","outdated"]}]}`)
	host := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"tessl hook run session-start"}]}]},"tessl":{"managed":true}}` + "\n")
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: ptrObserved("settings.json", host)},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Candidate.Ownership != realize.OwnershipShared || !bytes.Contains(compiled.Candidate.Content, entry.EncodedValue) {
		t.Fatalf("initial coexistence compilation = %#v", compiled)
	}
	return entry, compiled
}

func reindent(t *testing.T, content []byte) []byte {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(content), "", "  "); err != nil {
		t.Fatal(err)
	}
	return append(pretty.Bytes(), '\n')
}

// Observed (#171): Tessl re-indented .claude/settings.json after ACR realized
// it, so ACR's unchanged hook entry no longer matched its ledger hash byte for
// byte and every later realize refused.
func TestSharedJSONEntryReindentedByAnotherToolStaysOwnedAndKeepsItsLayout(t *testing.T) {
	t.Parallel()
	entry, initial := coexistenceSettings(t)
	reindented := reindent(t, initial.Candidate.Content)
	previous := configTarget("settings.json", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: ptrObserved("settings.json", reindented), Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatalf("realize after another tool re-indented the shared file: %v", err)
	}
	if !bytes.Equal(compiled.Candidate.Content, reindented) {
		t.Fatalf("realize rewrote the other tool's layout:\n%s\nwant:\n%s", compiled.Candidate.Content, reindented)
	}
	if len(compiled.Managed) != 1 || compiled.Managed[0].ManagedHash == initial.Managed[0].ManagedHash {
		t.Fatalf("managed = %#v, want the re-indented entry recorded", compiled.Managed)
	}

	again := configTarget("settings.json", compiled.Candidate.Content, realize.OwnershipShared, compiled.Managed)
	repeated, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: ptrObserved("settings.json", reindented), Previous: &again},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil || !bytes.Equal(repeated.Candidate.Content, reindented) || repeated.Managed[0].ManagedHash != compiled.Managed[0].ManagedHash {
		t.Fatalf("second realize = %#v, %v; want an unchanged no-op", repeated, err)
	}

	updated := entry
	updated.EncodedValue = []byte(`{"hooks":[{"type":"command","command":"${CLAUDE_PROJECT_DIR}/.claude/hooks/acr/session-start.sh","args":["--policy","install"]}]}`)
	upgraded, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: ptrObserved("settings.json", reindented), Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{updated},
	})
	if err != nil || !bytes.Contains(upgraded.Candidate.Content, updated.EncodedValue) || bytes.Contains(upgraded.Candidate.Content, []byte(`"outdated"`)) {
		t.Fatalf("changed entry after re-indent = %s, %v", upgraded.Candidate.Content, err)
	}
	if !bytes.Contains(upgraded.Candidate.Content, []byte(`"command": "tessl hook run session-start"`)) {
		t.Fatalf("changed entry damaged the other tool's entry:\n%s", upgraded.Candidate.Content)
	}
}

func TestSharedJSONEntryWithAChangedValueStillConflictsAndSaysWhatACRExpects(t *testing.T) {
	t.Parallel()
	entry, initial := coexistenceSettings(t)
	edited := bytes.Replace(reindent(t, initial.Candidate.Content), []byte(`"outdated"`), []byte(`"none"`), 1)
	previous := configTarget("settings.json", initial.Candidate.Content, realize.OwnershipShared, initial.Managed)
	_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.json", Observed: ptrObserved("settings.json", edited), Previous: &previous},
		Format: adapter.ConfigJSON, Desired: []adapter.ConfigEntry{entry},
	})
	if err == nil {
		t.Fatal("an edited owned entry realized without a conflict")
	}
	for _, want := range []string{"was edited or removed", "even ignoring JSON whitespace", "in hooks.SessionStart", string(entry.EncodedValue)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("conflict = %v, want it to mention %q", err, want)
		}
	}
}
