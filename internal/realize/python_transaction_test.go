package realize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAtomicTimestampOnlyExistingPython(t *testing.T) {
	original := transactionChtimes
	t.Cleanup(func() { transactionChtimes = original })
	for _, fixture := range []struct {
		name     string
		existing bool
	}{
		{"existing.py", true},
		{"new.py", false},
		{"other.txt", true},
		{"other.PY", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			project := t.TempDir()
			if fixture.existing {
				writeFile(t, project, fixture.name, "before\n")
				past := time.Unix(946684800, 0)
				if err := os.Chtimes(filepath.Join(project, fixture.name), past, past); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(project)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			calls := 0
			transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
				calls++
				staged, err := root.Stat(name)
				if err != nil {
					return err
				}
				if mtime.Unix() != staged.ModTime().Unix() || mtime.Nanosecond() != 0 {
					t.Fatalf("replacement timestamp = %s, want staged write second %s", mtime, staged.ModTime())
				}
				return original(root, name, atime, mtime)
			}
			if err := writeFileAtomic(root, fixture.name, []byte("after\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			want := 0
			if fixture.name == "existing.py" {
				want = 1
			}
			if calls != want {
				t.Fatalf("timestamp changes = %d, want %d", calls, want)
			}
		})
	}
}

func TestPythonReplacementRollbackAdvancesTimestamp(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "managed.py", "VALUE = 'A'\n")
	filename := filepath.Join(project, "managed.py")
	initial, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	before := initial.ModTime().Truncate(time.Second).Add(time.Hour)
	if err := os.Chtimes(filename, before, before); err != nil {
		t.Fatal(err)
	}
	current := testLedger(testTarget("managed.py", "VALUE = 'A'\n", OwnershipGenerated))
	intent := testIntent("managed.py", "VALUE = 'B'\n", OwnershipGenerated)
	plan, err := newPlanner(fakeGitInspector{}).Plan(project, current, []Intent{intent})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("ledger persistence failed")
	var replaced time.Time
	err = applyJournaledTestPlan(project, plan, func(Ledger) error {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		replaced = info.ModTime()
		return injected
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("rollback = %v", err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Unix() != before.Unix()+1 || info.ModTime().Unix() != replaced.Unix()+1 || info.ModTime().Nanosecond() != 0 {
		t.Fatalf("timestamps before=%s, replaced=%s, restored=%s", before, replaced, info.ModTime())
	}
	if got := readFile(t, project, "managed.py"); got != "VALUE = 'A'\n" {
		t.Fatalf("restored source = %q", got)
	}
}

func TestPythonTimestampFailuresRecoverEarlierWrites(t *testing.T) {
	for _, failure := range []string{"set", "verify"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project, "a.txt", "before\n")
			writeFile(t, project, "b.py", "VALUE = 'A'\n")
			current := testLedger(
				testTarget("a.txt", "before\n", OwnershipGenerated),
				testTarget("b.py", "VALUE = 'A'\n", OwnershipGenerated),
			)
			plan, err := newPlanner(fakeGitInspector{}).Plan(project, current, []Intent{
				testIntent("a.txt", "after\n", OwnershipGenerated),
				testIntent("b.py", "VALUE = 'B'\n", OwnershipGenerated),
			})
			if err != nil {
				t.Fatal(err)
			}
			original := transactionChtimes
			t.Cleanup(func() { transactionChtimes = original })
			injected := errors.New("timestamp metadata unavailable")
			transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
				if failure == "set" {
					return injected
				}
				// Emulate a filesystem accepting but rounding the requested
				// timestamp. The writer must verify before publishing it.
				return original(root, name, atime, mtime.Add(-time.Second))
			}
			finalized := false
			err = applyJournaledTestPlan(project, plan, func(Ledger) error {
				finalized = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "rolled back") || finalized {
				t.Fatalf("timestamp failure = %v, finalized = %t", err, finalized)
			}
			if failure == "set" && !errors.Is(err, injected) {
				t.Fatalf("lost metadata error: %v", err)
			}
			if failure == "verify" && !strings.Contains(err.Error(), "use a filesystem supporting whole-second timestamps") {
				t.Fatalf("missing timestamp verification diagnostic: %v", err)
			}
			if got := readFile(t, project, "a.txt"); got != "before\n" {
				t.Fatalf("earlier write not restored: %q", got)
			}
			if got := readFile(t, project, "b.py"); got != "VALUE = 'A'\n" {
				t.Fatalf("source changed after metadata failure: %q", got)
			}
			entries, err := os.ReadDir(project)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".acr-realize-") {
					t.Fatalf("staged file retained after metadata failure: %s", entry.Name())
				}
			}
		})
	}
}
