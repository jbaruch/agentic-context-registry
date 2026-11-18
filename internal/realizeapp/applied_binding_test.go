package realizeapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const bindingTarget = ".codex/skills/acr__example__first__skill0/references/notes.md"

func marshalAppliedLedger(data []byte, ledger realize.Ledger) ([]byte, error) {
	var receipt appliedReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	receipt.Ledger = ledger
	return json.Marshal(receipt)
}

func productTreeWithoutApplied(t *testing.T, project string) map[string]string {
	t.Helper()
	tree := hashProjectTree(t, project)
	delete(tree, appliedPath)
	return tree
}

func TestAppliedReceiptRefusesRestoredHistoricalBytes(t *testing.T) {
	for _, scenario := range []string{"older-cli", "branch-switch", "backup-restore", "copied-checkout"} {
		t.Run(scenario, func(t *testing.T) {
			a, b, _, serviceB, pull := appliedPullFixture(t, true)
			filename := filepath.Join(a, bindingTarget)
			original := readFile(t, filename)
			receipt := readFile(t, filepath.Join(a, appliedPath))
			if scenario == "branch-switch" {
				gitFixtureCommand(t, a, "add", "-f", "--", bindingTarget)
				gitFixtureCommand(t, a, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Record A output")
				gitFixtureCommand(t, a, "checkout", "-qb", "changed-output")
			}
			// An older CLI writes B without maintaining the A receipt.
			writeFixture(t, filename, readFile(t, filepath.Join(b, bindingTarget)), 0o644)
			if scenario == "branch-switch" {
				gitFixtureCommand(t, a, "add", "-f", "--", bindingTarget)
				gitFixtureCommand(t, a, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Record B output")
				gitFixtureCommand(t, a, "checkout", "-q", "-")
				gitFixtureCommand(t, a, "checkout", "-q", "changed-output")
			}
			// Restoring A now is a genuine edit, even though its hash matches
			// historical evidence. Neither the lock nor receipt records this edit.
			writeFixture(t, filename, original, 0o644)
			if scenario == "backup-restore" {
				writeFixture(t, filepath.Join(a, appliedPath), receipt, 0o644)
			}
			if scenario == "copied-checkout" {
				other, _, _, _, _ := appliedPullFixture(t, false)
				writeFixture(t, filepath.Join(a, appliedPath), readFile(t, filepath.Join(other, appliedPath)), 0o644)
			}
			pull()
			for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun} {
				before := hashProjectTree(t, a)
				_, err := serviceB.Run(context.Background(), a, nil, mode)
				if err == nil || !reflect.DeepEqual(before, hashProjectTree(t, a)) {
					t.Fatalf("%s accepted restored historical bytes or wrote files: %v", mode, err)
				}
			}
			assertAppliedConflictUnchanged(t, a, serviceB, false)
		})
	}
}

func TestAppliedReceiptRechecksTargetMetadataBeforeApply(t *testing.T) {
	a, _, _, serviceB, pull := appliedPullFixture(t, true)
	pull()
	filename := filepath.Join(a, bindingTarget)
	before := hashProjectTree(t, a)
	serviceB.loader = appliedChangingLoader{packageLoader: serviceB.loader, change: func() {
		// Compilation observes the same bytes, but the old application is no
		// longer the file's last change. The under-claim recheck must refuse.
		writeFixture(t, filename, readFile(t, filename), 0o644)
	}}
	_, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply)
	const want = "prepare realization state: last-applied file metadata changed while realization was preparing; retry against current checkout state"
	if err == nil || err.Error() != want {
		t.Fatalf("apply error = %v, want %q", err, want)
	}
	if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
		t.Fatal("stale metadata refusal changed product state or the receipt")
	}
}

func TestAppliedReceiptBindingRejectsChangedMetadata(t *testing.T) {
	for _, mutation := range []string{"inode", "mtime", "ctime", "checkout", "missing-binding", "schema-one"} {
		t.Run(mutation, func(t *testing.T) {
			a, _, _, serviceB, pull := appliedPullFixture(t, true)
			filename := filepath.Join(a, bindingTarget)
			info, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "inode":
				// Replace the file while preserving bytes, mode, size and mtime.
				content := readFile(t, filename)
				if err := os.Rename(filename, filename+".saved"); err != nil {
					t.Fatal(err)
				}
				writeFixture(t, filename, content, info.Mode())
				if err := os.Remove(filename + ".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filename, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "mtime":
				if err := os.Chtimes(filename, info.ModTime(), info.ModTime().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "ctime":
				// Restoring mtime does not restore ctime after an in-place edit.
				content := readFile(t, filename)
				writeFixture(t, filename, []byte("B notes\n"), info.Mode())
				writeFixture(t, filename, content, info.Mode())
				if err := os.Chtimes(filename, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			default:
				var receipt appliedReceipt
				if err := json.Unmarshal(readFile(t, filepath.Join(a, appliedPath)), &receipt); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "checkout":
					receipt.Checkout.Inode++
				case "missing-binding":
					receipt.Files = receipt.Files[:len(receipt.Files)-1]
				case "schema-one":
					// Use the exact legacy shape, not schema 2 with a changed number.
					data, err := json.Marshal(struct {
						SchemaVersion int            `json:"schema_version"`
						Ledger        realize.Ledger `json:"ledger"`
					}{1, receipt.Ledger})
					if err != nil {
						t.Fatal(err)
					}
					writeFixture(t, filepath.Join(a, appliedPath), data, 0o644)
				}
				if mutation != "schema-one" {
					data, err := json.Marshal(receipt)
					if err != nil {
						t.Fatal(err)
					}
					writeFixture(t, filepath.Join(a, appliedPath), data, 0o644)
				}
			}
			pull()
			assertAppliedConflictUnchanged(t, a, serviceB, false)
		})
	}
}
