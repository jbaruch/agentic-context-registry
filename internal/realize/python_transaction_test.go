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

func TestPythonReplacementsWithTwoSecondRounding(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		roundUp     int64
		generations []string
		mode        uint32
	}{
		{"ordinary", 0, []string{"B"}, 0o644},
		{"rounded_above_minimum", 1, []string{"B"}, 0o644},
		{"rapid_equal_length", 0, []string{"B", "C"}, 0o644},
		{"mode_only", 0, []string{"A"}, 0o755},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project, "managed.py", "VALUE = 'A'\n")
			writeFile(t, project, "__pycache__/managed.pyc", "retained cache\n")
			filename := filepath.Join(project, "managed.py")
			previous := time.Unix(946684800, 0)
			if err := os.Chtimes(filename, previous, previous); err != nil {
				t.Fatal(err)
			}
			original := transactionChtimes
			t.Cleanup(func() { transactionChtimes = original })
			var minimum time.Time
			calls := 0
			transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
				if calls == 0 {
					minimum = mtime
				}
				calls++
				// Put the first request at an odd second on the simulated
				// two-second clock, regardless of the staged write's host time.
				elapsed := mtime.Unix() - minimum.Unix()
				stored := minimum.Add(time.Duration((elapsed+1+fixture.roundUp)/2*2-1) * time.Second)
				return original(root, name, atime, stored)
			}
			current := testLedger(testTarget("managed.py", "VALUE = 'A'\n", OwnershipGenerated))
			for _, value := range fixture.generations {
				content := "VALUE = '" + value + "'\n"
				intent := testIntent("managed.py", content, OwnershipGenerated)
				intent.Mode = fixture.mode
				plan, err := newPlanner(fakeGitInspector{}).Plan(project, current, []Intent{intent})
				if err != nil {
					t.Fatal(err)
				}
				calls = 0
				if err := applyJournaledTestPlan(project, plan, nil); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(filename)
				if err != nil {
					t.Fatal(err)
				}
				if info.ModTime().Unix() <= previous.Unix() || info.ModTime().Unix() < minimum.Unix() {
					t.Fatalf("stored=%s, predecessor=%s, minimum=%s", info.ModTime(), previous, minimum)
				}
				if want := 2 - int(fixture.roundUp); calls != want {
					t.Fatalf("timestamp attempts=%d, want %d", calls, want)
				}
				if got := readFile(t, project, "managed.py"); got != content || uint32(info.Mode().Perm()) != fixture.mode {
					t.Fatalf("replacement content=%q, mode=%o", got, info.Mode().Perm())
				}
				if got := readFile(t, project, "__pycache__/managed.pyc"); got != "retained cache\n" {
					t.Fatalf("cache changed: %q", got)
				}
				cache, err := os.Stat(filepath.Join(project, "__pycache__/managed.pyc"))
				if err != nil || cache.Mode().Perm() != 0o644 {
					t.Fatalf("cache metadata changed: %v, %v", cache, err)
				}
				previous = info.ModTime()
				current = plan.NextLedger
			}
		})
	}
}

func TestTwoPythonFileRollbackWithTwoSecondRounding(t *testing.T) {
	project := t.TempDir()
	before := time.Unix(946684800, 0)
	var targets []Target
	var intents []Intent
	for _, name := range []string{"a.py", "b.py"} {
		writeFile(t, project, name, "VALUE = 'A'\n")
		if err := os.Chtimes(filepath.Join(project, name), before, before); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, testTarget(name, "VALUE = 'A'\n", OwnershipGenerated))
		intent := testIntent(name, "VALUE = 'B'\n", OwnershipGenerated)
		intent.Mode = 0o755
		intents = append(intents, intent)
	}
	writeFile(t, project, "__pycache__/retained.pyc", "retained cache\n")
	original := transactionChtimes
	t.Cleanup(func() { transactionChtimes = original })
	calls := 0
	var stagedName string
	var minimum time.Time
	transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
		if name != stagedName {
			stagedName, minimum = name, mtime
		}
		calls++
		// Each replacement and restoration starts at an odd second on
		// the simulated clock, so rounding down must trigger a retry.
		elapsed := mtime.Unix() - minimum.Unix()
		stored := minimum.Add(time.Duration((elapsed+1)/2*2-1) * time.Second)
		return original(root, name, atime, stored)
	}
	plan, err := newPlanner(fakeGitInspector{}).Plan(project, testLedger(targets...), intents)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("ledger persistence failed")
	replaced := make(map[string]time.Time)
	err = applyJournaledTestPlan(project, plan, func(Ledger) error {
		for _, target := range targets {
			info, err := os.Stat(filepath.Join(project, target.Path))
			if err != nil {
				t.Fatal(err)
			}
			if info.ModTime().Unix() <= before.Unix() || info.Mode().Perm() != 0o755 || readFile(t, project, target.Path) != "VALUE = 'B'\n" {
				t.Fatalf("replacement %s: mtime=%s, mode=%o", target.Path, info.ModTime(), info.Mode().Perm())
			}
			replaced[target.Path] = info.ModTime()
		}
		return injected
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") || calls != 8 {
		t.Fatalf("rollback=%v, timestamp attempts=%d, want 8", err, calls)
	}
	for _, target := range targets {
		info, err := os.Stat(filepath.Join(project, target.Path))
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Unix() <= replaced[target.Path].Unix() || info.Mode().Perm() != 0o644 || readFile(t, project, target.Path) != "VALUE = 'A'\n" {
			t.Fatalf("restored %s: mtime=%s, predecessor=%s, mode=%o", target.Path, info.ModTime(), replaced[target.Path], info.Mode().Perm())
		}
	}
	if got := readFile(t, project, "__pycache__/retained.pyc"); got != "retained cache\n" {
		t.Fatalf("cache changed: %q", got)
	}
	cache, err := os.Stat(filepath.Join(project, "__pycache__/retained.pyc"))
	if err != nil || cache.Mode().Perm() != 0o644 {
		t.Fatalf("cache metadata changed: %v, %v", cache, err)
	}
	if err := recoverPendingTransaction(project); err != nil {
		t.Fatalf("recovery after completed rollback: %v", err)
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
			calls := 0
			transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
				calls++
				if failure == "set" {
					return injected
				}
				// Emulate a filesystem that accepts requests but never advances.
				stuck := time.Unix(946684800, 0)
				return original(root, name, atime, stuck)
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
			if failure == "verify" && (calls != 4 || !strings.Contains(err.Error(), "after 4 attempts") || !strings.Contains(err.Error(), "use a filesystem supporting advancing timestamps")) {
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
