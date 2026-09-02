package preserve

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestTOMLUserHookTimeoutSurvivesNextToOwnedEntry(t *testing.T) {
	t.Parallel()

	userStop := []byte("[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"user-command\"\ntimeout = 30\n")
	entry := testConfigEntry(adapter.ConfigTOML, []string{"hooks", "SessionStart"}, adapter.ConfigElement, "owner-key", `{ hooks = [{ type = "command", command = "echo ready" }] }`)
	entry.Representation = adapter.ConfigEntryTOMLHookTables
	observed := observedFile("config.toml", userStop)
	compiled, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "config.toml", Observed: &observed},
		Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := compiled.Candidate.Content
	if !bytes.Contains(got, userStop) {
		t.Fatalf("user Stop table with timeout was re-encoded or dropped:\n%s", got)
	}
	if bytes.Count(got, userStop) != 1 {
		t.Fatalf("user Stop table did not survive as one exact span:\n%s", got)
	}
}

func TestTOMLOwnedHookStoredExtraFieldIsRejected(t *testing.T) {
	t.Parallel()

	entry := testConfigEntry(adapter.ConfigTOML, []string{"hooks", "SessionStart"}, adapter.ConfigElement, "owner-key", `{ hooks = [{ type = "command", command = "echo ready" }] }`)
	entry.Representation = adapter.ConfigEntryTOMLHookTables
	initial := compileMissingConfig(t, adapter.ConfigTOML, entry)
	stored := append([]byte(nil), initial.Candidate.Content...)
	mutated := bytes.Replace(stored, []byte("command = \"echo ready\"\n"), []byte("command = \"echo ready\"\ntimeout = 30\n"), 1)
	if bytes.Equal(mutated, stored) {
		t.Fatalf("could not inject timeout into owned hook table: %s", stored)
	}
	previous := configTarget("settings.toml", stored, realize.OwnershipGenerated, initial.Managed)
	observed := observedFile("settings.toml", mutated)
	_, err := NewCompiler().CompileConfig(context.Background(), adapter.ConfigCompileRequest{
		Target: adapter.SharedTarget{Path: "settings.toml", Observed: &observed, Previous: &previous},
		Format: adapter.ConfigTOML, Desired: []adapter.ConfigEntry{entry},
	})
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.Code != CodeConfigConflict {
		t.Fatalf("CompileConfig() error = %v, want typed %s rejecting the extra stored field", err, CodeConfigConflict)
	}
}
