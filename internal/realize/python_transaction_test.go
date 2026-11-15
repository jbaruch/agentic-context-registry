package realize

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAtomicTimestampOnlyExistingPython(t *testing.T) {
	base := time.Unix(946684800, 0)
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
			original, originalStaged := transactionChtimes, transactionStagedModTime
			t.Cleanup(func() {
				transactionChtimes, transactionStagedModTime = original, originalStaged
			})
			transactionStagedModTime = func(os.FileInfo) time.Time { return time.Unix(946684810, 123456789) }
			project := t.TempDir()
			if fixture.existing {
				writeFile(t, project, fixture.name, "before\n")
				if err := os.Chtimes(filepath.Join(project, fixture.name), base, base); err != nil {
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
				if want := time.Unix(946684810, 0); !mtime.Equal(want) || !atime.Equal(want) {
					t.Fatalf("replacement timestamps = %s, %s, want %s", atime, mtime, want)
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
			info, err := root.Stat(fixture.name)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.name == "existing.py" && !info.ModTime().Equal(time.Unix(946684810, 0)) {
				t.Fatalf("stored timestamp = %s, want %s", info.ModTime(), time.Unix(946684810, 0))
			}
			if got := readFile(t, project, fixture.name); got != "after\n" || info.Mode().Perm() != 0o640 {
				t.Fatalf("replacement content=%q, mode=%o", got, info.Mode().Perm())
			}
		})
	}
}

func TestPythonReplacementRollbackAdvancesTimestamp(t *testing.T) {
	base := time.Unix(946684800, 0)
	original, originalStaged := transactionChtimes, transactionStagedModTime
	t.Cleanup(func() {
		transactionChtimes, transactionStagedModTime = original, originalStaged
	})
	transactionStagedModTime = func(os.FileInfo) time.Time { return base }
	calls := 0
	transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
		calls++
		want := base.Add(time.Duration(calls) * time.Second)
		if !mtime.Equal(want) || !atime.Equal(want) {
			t.Fatalf("timestamp request %d = %s, %s, want %s", calls, atime, mtime, want)
		}
		return original(root, name, atime, mtime)
	}
	project := t.TempDir()
	writeFile(t, project, "managed.py", "VALUE = 'A'\n")
	filename := filepath.Join(project, "managed.py")
	if err := os.Chtimes(filename, base, base); err != nil {
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
		if !replaced.Equal(base.Add(time.Second)) || info.Mode().Perm() != 0o644 || readFile(t, project, "managed.py") != "VALUE = 'B'\n" {
			t.Fatalf("replacement mtime=%s, mode=%o", replaced, info.Mode().Perm())
		}
		return injected
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("rollback = %v", err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !info.ModTime().Equal(base.Add(2*time.Second)) || info.Mode().Perm() != 0o644 {
		t.Fatalf("timestamps before=%s, replaced=%s, restored=%s, mode=%o, attempts=%d", base, replaced, info.ModTime(), info.Mode().Perm(), calls)
	}
	if got := readFile(t, project, "managed.py"); got != "VALUE = 'A'\n" {
		t.Fatalf("restored source = %q", got)
	}
}

func TestPythonReplacementsWithTwoSecondRounding(t *testing.T) {
	base := time.Unix(946684800, 0)
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
			if err := os.Chtimes(filename, base, base); err != nil {
				t.Fatal(err)
			}
			original, originalStaged := transactionChtimes, transactionStagedModTime
			t.Cleanup(func() {
				transactionChtimes, transactionStagedModTime = original, originalStaged
			})
			transactionStagedModTime = func(os.FileInfo) time.Time { return base }
			var requested, storedSeconds []int64
			transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
				if mtime.Nanosecond() != 0 || !atime.Equal(mtime) {
					t.Fatalf("timestamp request = %s, %s, want equal whole seconds", atime, mtime)
				}
				requested = append(requested, mtime.Unix())
				stored := time.Unix(mtime.Unix()/2*2, 0)
				if fixture.roundUp == 1 {
					stored = time.Unix((mtime.Unix()+1)/2*2, 0)
				}
				if err := original(root, name, atime, stored); err != nil {
					return err
				}
				info, err := root.Stat(name)
				if err != nil {
					return err
				}
				if !info.ModTime().Equal(stored) {
					t.Fatalf("stored timestamp = %s, want %s", info.ModTime(), stored)
				}
				storedSeconds = append(storedSeconds, info.ModTime().Unix())
				return nil
			}
			current := testLedger(testTarget("managed.py", "VALUE = 'A'\n", OwnershipGenerated))
			for generation, value := range fixture.generations {
				content := "VALUE = '" + value + "'\n"
				intent := testIntent("managed.py", content, OwnershipGenerated)
				intent.Mode = fixture.mode
				plan, err := newPlanner(fakeGitInspector{}).Plan(project, current, []Intent{intent})
				if err != nil {
					t.Fatal(err)
				}
				requested, storedSeconds = nil, nil
				if err := applyJournaledTestPlan(project, plan, nil); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(filename)
				if err != nil {
					t.Fatal(err)
				}
				wantSecond := base.Unix() + int64(2*(generation+1))
				previous := base.Add(time.Duration(2*generation) * time.Second)
				if !info.ModTime().Equal(time.Unix(wantSecond, 0)) || info.ModTime().Unix() <= previous.Unix() {
					t.Fatalf("stored=%s, predecessor=%s, want second %d", info.ModTime(), previous, wantSecond)
				}
				wantRequested := []int64{wantSecond - 1, wantSecond}
				wantStored := []int64{wantSecond - 2, wantSecond}
				if fixture.roundUp == 1 {
					wantRequested = []int64{wantSecond - 1}
					wantStored = []int64{wantSecond}
				}
				if !slices.Equal(requested, wantRequested) || !slices.Equal(storedSeconds, wantStored) {
					t.Fatalf("timestamp requests=%v, stored=%v, want requests=%v, stored=%v", requested, storedSeconds, wantRequested, wantStored)
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
	original, originalStaged := transactionChtimes, transactionStagedModTime
	t.Cleanup(func() {
		transactionChtimes, transactionStagedModTime = original, originalStaged
	})
	transactionStagedModTime = func(os.FileInfo) time.Time { return before }
	var requested, storedSeconds []int64
	transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
		if mtime.Nanosecond() != 0 || !atime.Equal(mtime) {
			t.Fatalf("timestamp request = %s, %s, want equal whole seconds", atime, mtime)
		}
		requested = append(requested, mtime.Unix())
		stored := time.Unix(mtime.Unix()/2*2, 0)
		if err := original(root, name, atime, stored); err != nil {
			return err
		}
		info, err := root.Stat(name)
		if err != nil {
			return err
		}
		if !info.ModTime().Equal(stored) {
			t.Fatalf("stored timestamp = %s, want %s", info.ModTime(), stored)
		}
		storedSeconds = append(storedSeconds, info.ModTime().Unix())
		return nil
	}
	plan, err := newPlanner(fakeGitInspector{}).Plan(project, testLedger(targets...), intents)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("ledger persistence failed")
	err = applyJournaledTestPlan(project, plan, func(Ledger) error {
		for _, target := range targets {
			info, err := os.Stat(filepath.Join(project, target.Path))
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(before.Add(2*time.Second)) || info.Mode().Perm() != 0o755 || readFile(t, project, target.Path) != "VALUE = 'B'\n" {
				t.Fatalf("replacement %s: mtime=%s, mode=%o", target.Path, info.ModTime(), info.Mode().Perm())
			}
		}
		return injected
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolled back") || len(requested) != 8 {
		t.Fatalf("rollback=%v, timestamp attempts=%d, want 8", err, len(requested))
	}
	b := before.Unix()
	wantRequested := []int64{b + 1, b + 2, b + 1, b + 2, b + 3, b + 4, b + 3, b + 4}
	wantStored := []int64{b, b + 2, b, b + 2, b + 2, b + 4, b + 2, b + 4}
	if !slices.Equal(requested, wantRequested) || !slices.Equal(storedSeconds, wantStored) {
		t.Fatalf("timestamp requests=%v, stored=%v, want requests=%v, stored=%v", requested, storedSeconds, wantRequested, wantStored)
	}
	for _, target := range targets {
		info, err := os.Stat(filepath.Join(project, target.Path))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(before.Add(4*time.Second)) || info.Mode().Perm() != 0o644 || readFile(t, project, target.Path) != "VALUE = 'A'\n" {
			t.Fatalf("restored %s: mtime=%s, want=%s, mode=%o", target.Path, info.ModTime(), before.Add(4*time.Second), info.Mode().Perm())
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
	base := time.Unix(946684800, 0)
	for _, failure := range []string{"set", "verify"} {
		t.Run(failure, func(t *testing.T) {
			project := t.TempDir()
			writeFile(t, project, "a.txt", "before\n")
			writeFile(t, project, "b.py", "VALUE = 'A'\n")
			if err := os.Chtimes(filepath.Join(project, "b.py"), base, base); err != nil {
				t.Fatal(err)
			}
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
			original, originalStaged := transactionChtimes, transactionStagedModTime
			t.Cleanup(func() {
				transactionChtimes, transactionStagedModTime = original, originalStaged
			})
			transactionStagedModTime = func(os.FileInfo) time.Time { return base }
			injected := errors.New("timestamp metadata unavailable")
			var requested []int64
			transactionChtimes = func(root *os.Root, name string, atime, mtime time.Time) error {
				if mtime.Nanosecond() != 0 || !atime.Equal(mtime) {
					t.Fatalf("timestamp request = %s, %s, want equal whole seconds", atime, mtime)
				}
				requested = append(requested, mtime.Unix())
				if failure == "set" {
					return injected
				}
				// Emulate a filesystem that accepts requests but never advances.
				return original(root, name, atime, base)
			}
			finalized := false
			err = applyJournaledTestPlan(project, plan, func(Ledger) error {
				finalized = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), `"b.py"`) || finalized {
				t.Fatalf("timestamp failure = %v, finalized = %t", err, finalized)
			}
			wantRequested := []int64{base.Unix() + 1}
			if failure == "set" {
				if !errors.Is(err, injected) || !strings.Contains(err.Error(), "set temporary file timestamp") {
					t.Fatalf("lost metadata error: %v", err)
				}
			} else {
				wantRequested = []int64{base.Unix() + 1, base.Unix() + 2, base.Unix() + 4, base.Unix() + 8}
			}
			if !slices.Equal(requested, wantRequested) {
				t.Fatalf("timestamp requests=%v, want %v", requested, wantRequested)
			}
			if failure == "verify" && (!strings.Contains(err.Error(), "after 4 attempts") || !strings.Contains(err.Error(), "use a filesystem supporting advancing timestamps")) {
				t.Fatalf("missing timestamp verification diagnostic: %v", err)
			}
			if got := readFile(t, project, "a.txt"); got != "before\n" {
				t.Fatalf("earlier write not restored: %q", got)
			}
			if got := readFile(t, project, "b.py"); got != "VALUE = 'A'\n" {
				t.Fatalf("source changed after metadata failure: %q", got)
			}
			info, err := os.Stat(filepath.Join(project, "b.py"))
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(base) || info.Mode().Perm() != 0o644 {
				t.Fatalf("source metadata changed: mtime=%s, mode=%o", info.ModTime(), info.Mode().Perm())
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
