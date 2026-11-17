package realize

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecoveryBeforeImagesIsReadOnlyAndChecked(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "agents.yaml"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyBefore := treeSnapshot(t, project)
	if images, err := RecoveryBeforeImages(project, "agents.yaml"); err != nil || len(images) != 0 {
		t.Fatalf("no-journal result: %v %v", images, err)
	}
	if !reflect.DeepEqual(emptyBefore, treeSnapshot(t, project)) {
		t.Fatal("inspection created claim paths")
	}
	edits := []FileTransactionEdit{
		{Path: "agents.yaml", Operation: "splice", Before: []byte("before\n"), BeforeMode: 0o644, After: []byte("after\n"), AfterMode: 0o644},
		{Path: ".agents/registry.lock", Operation: "splice", BeforeAbsent: true, After: []byte("lock\n"), AfterMode: 0o644},
	}
	inspected := 0
	inspect := func() {
		before := treeSnapshot(t, project)
		images, err := RecoveryBeforeImages(project, "agents.yaml", ".agents/registry.lock")
		if err != nil || string(images["agents.yaml"]) != "before\n" {
			t.Fatalf("before-image: %v %v", images, err)
		}
		if value, exists := images[".agents/registry.lock"]; !exists || value != nil {
			t.Fatalf("absent before-image: %v", images)
		}
		if !reflect.DeepEqual(before, treeSnapshot(t, project)) {
			t.Fatal("inspection mutated journal/project")
		}
		inspected++
	}
	err := ApplyFileTransactionWithHooks(project, edits, nil, FileTransactionHooks{
		TransactionID: func() (string, error) { return "tx-preview", nil },
		BeforeEdit: func(index int, _ FileTransactionEdit) error {
			if index == 0 {
				inspect()
			}
			return nil
		},
		AfterEdit: func(index int, _ FileTransactionEdit) error {
			if index != 1 {
				return nil
			}
			inspect()
			image := filepath.Join(project, transactionDirectory, "tx-preview", "before", "000000")
			if err := os.WriteFile(image, []byte("corrupt"), 0o600); err != nil {
				return err
			}
			before := treeSnapshot(t, project)
			_, err := RecoveryBeforeImages(project, "agents.yaml")
			var conflict *RecoveryConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("corrupt image accepted: %v", err)
			}
			if !reflect.DeepEqual(before, treeSnapshot(t, project)) {
				t.Fatal("corrupt image inspection wrote files")
			}
			return os.WriteFile(image, []byte("before\n"), 0o600)
		},
	})
	if err != nil || inspected != 2 {
		t.Fatalf("transaction/control: %d %v", inspected, err)
	}
}

func TestRecoveryBeforeImagesProjectErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("project permission tests require a non-root process")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if images, err := RecoveryBeforeImages(missing); images != nil || err != nil {
		t.Fatalf("missing project without journal = %v, %v; want nil success", images, err)
	}
	for _, kind := range []string{"file", "permission", "conflict", "unsupported-version"} {
		t.Run(kind, func(t *testing.T) {
			project := t.TempDir()
			switch kind {
			case "file":
				project = filepath.Join(project, "file")
				if err := os.WriteFile(project, []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "permission":
				t.Cleanup(func() {
					if err := os.Chmod(project, 0o700); err != nil {
						t.Error(err)
					}
				})
				if err := os.Chmod(project, 0); err != nil {
					t.Fatal(err)
				}
			default:
				journal := filepath.Join(project, transactionDirectory, "old")
				if err := os.MkdirAll(journal, 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "conflict" {
					if err := os.Mkdir(filepath.Join(project, transactionDirectory, "other"), 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(journal, journalManifestFilename), []byte(`{"schemaVersion":999,"id":"old","entries":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := RecoveryBeforeImages(project, "agents.yaml")
			prefix := fmt.Sprintf("inspect project transactions before recovery in project %q; restore directory access or verify --project, then retry: ", project)
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || errors.Unwrap(err) == nil {
				t.Fatalf("error = %v, want wrapped inspection failure", err)
			}
			switch kind {
			case "file", "permission":
				var cause *os.PathError
				if !errors.As(err, &cause) || !errors.Is(err, cause.Err) {
					t.Fatalf("lost path cause: %v", err)
				}
				if kind == "permission" && !errors.Is(err, os.ErrPermission) {
					t.Fatalf("lost permission cause: %v", err)
				}
			case "conflict":
				var conflict *RecoveryConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("lost recovery classification: %v", err)
				}
			case "unsupported-version":
				var unsupported *UnsupportedJournalVersionError
				if !errors.As(err, &unsupported) {
					t.Fatalf("lost version classification: %v", err)
				}
			}
		})
	}
}
