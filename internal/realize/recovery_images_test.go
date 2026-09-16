package realize

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
