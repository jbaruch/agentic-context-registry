package adapter

import (
	"errors"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestRegisterValidatesAdapters(t *testing.T) {
	t.Parallel()

	valid := stubAdapter{descriptor: testDescriptor("fixture-a", "1.0.0"), artifacts: []ArtifactKind{ArtifactHook, ArtifactRule}, events: []manifest.HookEvent{manifest.HookSessionStart}}

	t.Run("accepts a well-formed adapter and sorts by ID", func(t *testing.T) {
		t.Parallel()
		second := stubAdapter{descriptor: testDescriptor("fixture-b", "1.0.0")}
		registered, err := Register(second, valid)
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		if len(registered) != 2 || registered[0].Descriptor().ID != "fixture-a" || registered[1].Descriptor().ID != "fixture-b" {
			t.Fatalf("Register() = %#v, want sorted by ID", registered)
		}
	})

	t.Run("rejects non-kebab-case ID", func(t *testing.T) {
		t.Parallel()
		bad := stubAdapter{descriptor: testDescriptor("Fixture", "1.0.0")}
		if _, err := Register(bad); err == nil || !strings.Contains(err.Error(), "kebab-case") {
			t.Fatalf("Register() error = %v, want kebab-case rejection", err)
		}
	})

	t.Run("rejects duplicate ID", func(t *testing.T) {
		t.Parallel()
		if _, err := Register(valid, valid); err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("Register() error = %v, want duplicate rejection", err)
		}
	})

	t.Run("rejects invalid semantic version", func(t *testing.T) {
		t.Parallel()
		bad := stubAdapter{descriptor: testDescriptor("fixture-a", "v1")}
		if _, err := Register(bad); err == nil || !strings.Contains(err.Error(), "semantic version") {
			t.Fatalf("Register() error = %v, want semver rejection", err)
		}
	})

	t.Run("rejects boundary mismatch with a typed error", func(t *testing.T) {
		t.Parallel()
		bad := stubAdapter{descriptor: Descriptor{ID: "fixture-a", Version: "1.0.0", Boundary: CurrentBoundaryVersion + 1}}
		_, err := Register(bad)
		var boundaryErr *BoundaryVersionError
		if !errors.As(err, &boundaryErr) || boundaryErr.AdapterID != "fixture-a" {
			t.Fatalf("Register() error = %v, want *BoundaryVersionError", err)
		}
	})

	t.Run("rejects unsorted capabilities", func(t *testing.T) {
		t.Parallel()
		bad := stubAdapter{descriptor: testDescriptor("fixture-a", "1.0.0"), artifacts: []ArtifactKind{ArtifactRule, ArtifactHook}}
		if _, err := Register(bad); err == nil || !strings.Contains(err.Error(), "SupportedArtifacts") {
			t.Fatalf("Register() error = %v, want sorted-capability rejection", err)
		}
	})

	t.Run("rejects duplicate capabilities", func(t *testing.T) {
		t.Parallel()
		bad := stubAdapter{descriptor: testDescriptor("fixture-a", "1.0.0"), events: []manifest.HookEvent{manifest.HookSessionStart, manifest.HookSessionStart}}
		if _, err := Register(bad); err == nil || !strings.Contains(err.Error(), "SupportedEvents") {
			t.Fatalf("Register() error = %v, want duplicate-capability rejection", err)
		}
	})
}

func TestFSSnapshotReadsAndReportsMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "AGENTS.md", "hello\n")
	snapshot := NewFSSnapshot(testDirFS(t, dir))

	observed, err := snapshot.ReadFile("AGENTS.md")
	if err != nil || string(observed.Content) != "hello\n" || observed.Hash != hashContent([]byte("hello\n")) {
		t.Fatalf("ReadFile() = %#v, %v", observed, err)
	}

	if _, err := snapshot.ReadFile("missing.md"); err == nil {
		t.Fatal("ReadFile(missing) error = nil, want fs.ErrNotExist")
	}
}
