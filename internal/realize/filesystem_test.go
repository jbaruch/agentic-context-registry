package realize

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWrapProjectAccessError(t *testing.T) {
	project := filepath.Join("workspace", "project")
	for _, test := range []struct {
		name       string
		operation  string
		cause      error
		underlying error
	}{
		{name: "open-root", operation: "open project directory", cause: os.ErrNotExist, underlying: os.ErrNotExist},
		{name: "inspect-state", operation: `inspect transactional state path ".agents/.acr-applied.json"`, cause: os.ErrPermission, underlying: os.ErrPermission},
		{name: "read-state", operation: `read transactional state path ".agents/.acr-applied.json"`, cause: &os.PathError{Op: "open", Path: ".agents/.acr-applied.json", Err: os.ErrPermission}, underlying: os.ErrPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := WrapProjectAccessError(project, test.operation, test.cause)
			for _, want := range []string{project, test.operation, "restore directory access or verify --project, then retry"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("access error %q lacks %q", err, want)
				}
			}
			if errors.Unwrap(err) != test.cause || !errors.Is(err, test.cause) || !errors.Is(err, test.underlying) {
				t.Fatalf("access error did not preserve its cause: %v", err)
			}
			var pathError *os.PathError
			wantPathError, hasPathError := test.cause.(*os.PathError)
			if errors.As(err, &pathError) != hasPathError || pathError != wantPathError {
				t.Fatalf("access error changed its path cause: %v", err)
			}
		})
	}
}

func TestComparisonLedgerRootOpenDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name      string
		cause     error
		causeText string
		setup     func(*testing.T, string)
	}{
		{name: "missing-directory", cause: os.ErrNotExist},
		{name: "regular-file", causeText: "not a directory", setup: func(t *testing.T, project string) {
			if err := os.WriteFile(project, []byte("not a directory\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "inaccessible-ancestor", cause: os.ErrPermission, setup: func(t *testing.T, project string) {
			if os.Geteuid() == 0 {
				t.Skip("permission denial requires a nonprivileged process")
			}
			if err := os.Mkdir(project, 0o755); err != nil {
				t.Fatal(err)
			}
			ancestor := filepath.Dir(project)
			t.Cleanup(func() {
				if err := os.Chmod(ancestor, 0o755); err != nil {
					t.Error(err)
				}
			})
			if err := os.Chmod(ancestor, 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "project")
			if test.setup != nil {
				test.setup(t, project)
			}
			applied := Ledger{SchemaVersion: 1, Targets: []Target{{Path: ".codex/config.toml", Ownership: OwnershipGenerated}}}
			comparison, eligible, err := ComparisonLedger(project, Ledger{SchemaVersion: 1}, applied)
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("root-open error = %v, want wrapped %v", err, test.cause)
			}
			var pathError *os.PathError
			if !errors.As(err, &pathError) {
				t.Fatalf("root-open error lost its path cause: %v", err)
			}
			if errors.Unwrap(err) != pathError || !errors.Is(err, pathError.Err) || pathError.Path != project || (test.causeText != "" && pathError.Err.Error() != test.causeText) {
				t.Fatalf("root-open error did not preserve its cause: %v", err)
			}
			for _, want := range []string{project, "open project directory for ledger comparison", "restore directory access or verify --project, then retry"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("root-open error %q lacks %q", err, want)
				}
			}
			if !reflect.DeepEqual(comparison, Ledger{}) || !reflect.DeepEqual(eligible, Ledger{}) {
				t.Fatalf("root-open failure returned comparison = %#v, eligible = %#v", comparison, eligible)
			}
		})
	}
}

func TestReadStateFileRootOpenDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name      string
		cause     error
		causeText string
		setup     func(*testing.T, string)
	}{
		{name: "missing-directory", cause: os.ErrNotExist},
		{name: "regular-file", causeText: "not a directory", setup: func(t *testing.T, project string) {
			if err := os.WriteFile(project, []byte("not a directory\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "inaccessible-ancestor", cause: os.ErrPermission, setup: func(t *testing.T, project string) {
			if os.Geteuid() == 0 {
				t.Skip("permission denial requires a nonprivileged process")
			}
			if err := os.Mkdir(project, 0o755); err != nil {
				t.Fatal(err)
			}
			ancestor := filepath.Dir(project)
			t.Cleanup(func() {
				if err := os.Chmod(ancestor, 0o755); err != nil {
					t.Error(err)
				}
			})
			if err := os.Chmod(ancestor, 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "project")
			if test.setup != nil {
				test.setup(t, project)
			}
			state, exists, err := ReadStateFile(project, ".agents/.acr-applied.json")
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("root-open error = %v, want wrapped %v", err, test.cause)
			}
			var pathError *os.PathError
			if !errors.As(err, &pathError) {
				t.Fatalf("root-open error lost its path cause: %v", err)
			}
			if errors.Unwrap(err) != pathError || !errors.Is(err, pathError.Err) || pathError.Path != project || (test.causeText != "" && pathError.Err.Error() != test.causeText) {
				t.Fatalf("root-open error did not preserve its cause: %v", err)
			}
			for _, want := range []string{project, "open project directory for transactional state", "restore directory access or verify --project, then retry"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("root-open error %q lacks %q", err, want)
				}
			}
			if exists || !reflect.DeepEqual(state, StateFile{}) {
				t.Fatalf("root-open failure returned state = %#v, exists = %t", state, exists)
			}
		})
	}
}

func TestReadStateFileSizeBoundary(t *testing.T) {
	project := t.TempDir()
	const name = ".agents/.acr-applied.json"
	writeFile(t, project, name, strings.Repeat(" ", maxTargetBytes))
	state, exists, err := ReadStateFile(project, name)
	if err != nil || !exists || len(state.Content) != 32<<20 {
		t.Fatalf("exact 32 MiB boundary: exists=%t size=%d error=%v", exists, len(state.Content), err)
	}
	if err := os.Truncate(filepath.Join(project, name), (32<<20)+1); err != nil {
		t.Fatal(err)
	}
	_, _, err = ReadStateFile(project, name)
	var oversized *FileTooLargeError
	if !errors.As(err, &oversized) || oversized.Path != name || !strings.Contains(err.Error(), "exceeds 32 MiB") {
		t.Fatalf("32 MiB+1 boundary error = %v", err)
	}
}

func TestReadStateFileRefusesZeroModeBeforeOpen(t *testing.T) {
	project := t.TempDir()
	const name = ".agents/.acr-applied.json"
	writeFile(t, project, name, "before\n")
	filename := filepath.Join(project, name)
	if err := os.Chmod(filename, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filename, 0o644); err != nil {
			t.Error(err)
		}
	})
	if os.Geteuid() != 0 {
		file, err := os.Open(filename)
		if err == nil {
			file.Close()
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("non-root ordinary open = %v, want permission denial", err)
		}
		t.Logf("verified EACCES at effective UID %d", os.Geteuid())
	}
	_, _, err := ReadStateFile(project, name)
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "mode 000; deliberately restore readable permissions") {
		t.Fatalf("zero-mode refusal = %v", err)
	}
}
