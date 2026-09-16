package dependency

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStateRejectsConcurrentProjectChange(t *testing.T) {
	project, source, service, _ := localFixture(t)
	if _, err := service.InstallLocal(context.Background(), project, source, false); err != nil {
		t.Fatal(err)
	}
	expected, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	desired := cloneState(expected)
	desired.Project.Freshness = "none"
	concurrent := readTestFile(t, filepath.Join(project, ProjectFilename)) + "ownerSetting: keep\n"
	if err := os.WriteFile(filepath.Join(project, ProjectFilename), []byte(concurrent), 0o644); err != nil {
		t.Fatal(err)
	}
	err = writeExpectedState(project, expected, desired)
	if err == nil || !strings.Contains(err.Error(), "concurrently") {
		t.Fatalf("concurrent write accepted: %v", err)
	}
	if readTestFile(t, filepath.Join(project, ProjectFilename)) != concurrent {
		t.Fatal("concurrent setting lost")
	}
}
