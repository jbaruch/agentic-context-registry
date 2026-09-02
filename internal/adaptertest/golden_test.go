package adaptertest

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestReferenceAdapterGolden(t *testing.T) {
	RunGolden(t, NewReferenceAdapter("1.0.0"), NewCompiler())
}

func TestGoldenExpectationRequiresPlanOrError(t *testing.T) {
	t.Parallel()

	caseDir := t.TempDir()
	hasExpectation, err := hasGoldenExpectation(caseDir)
	if err != nil || hasExpectation {
		t.Fatalf("empty case expectation = %t, %v", hasExpectation, err)
	}
	if err := os.MkdirAll(filepath.Join(caseDir, "want"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "want", "plan.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hasExpectation, err = hasGoldenExpectation(caseDir)
	if err != nil || !hasExpectation {
		t.Fatalf("plan case expectation = %t, %v", hasExpectation, err)
	}
}

func TestGoldenFileModesIgnoreCheckoutUmask(t *testing.T) {
	wantDir := t.TempDir()
	projectDir := t.TempDir()
	previousUmask := syscall.Umask(0o002)
	defer syscall.Umask(previousUmask)

	wantFiles := filepath.Join(wantDir, "files")
	if err := os.MkdirAll(wantFiles, 0o777); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]os.FileMode{
		"plain.md":  0o666,
		"script.sh": 0o777,
	}
	for name, requestedMode := range fixtures {
		content := []byte(name + "\n")
		wantPath := filepath.Join(wantFiles, name)
		if err := os.WriteFile(wantPath, content, requestedMode); err != nil {
			t.Fatal(err)
		}
		projectPath := filepath.Join(projectDir, name)
		if err := os.WriteFile(projectPath, content, 0o600); err != nil {
			t.Fatal(err)
		}
		projectMode := os.FileMode(0o644)
		if requestedMode&0o111 != 0 {
			projectMode = 0o755
		}
		if err := os.Chmod(projectPath, projectMode); err != nil {
			t.Fatal(err)
		}
	}
	plainInfo, err := os.Stat(filepath.Join(wantFiles, "plain.md"))
	if err != nil {
		t.Fatal(err)
	}
	scriptInfo, err := os.Stat(filepath.Join(wantFiles, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if plainInfo.Mode().Perm() != 0o664 || scriptInfo.Mode().Perm() != 0o775 {
		t.Fatalf("umask fixture modes = %04o, %04o; want 0664, 0775", plainInfo.Mode().Perm(), scriptInfo.Mode().Perm())
	}

	assertGoldenFiles(t, wantDir, projectDir)
}

func TestReferenceAdapterDetect(t *testing.T) {
	t.Parallel()

	reference := NewReferenceAdapter("1.0.0")

	empty := t.TempDir()
	detection, err := reference.Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(os.DirFS(empty))})
	if err != nil || detection.Detected {
		t.Fatalf("Detect(empty project) = %#v, %v, want not detected", detection, err)
	}

	present := t.TempDir()
	if err := os.WriteFile(present+"/AGENTS.md", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detection, err = reference.Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(os.DirFS(present))})
	if err != nil || !detection.Detected || len(detection.Evidence) != 1 || detection.Evidence[0] != "AGENTS.md" {
		t.Fatalf("Detect(AGENTS.md present) = %#v, %v", detection, err)
	}
}

func TestReferenceAdapterRenderIsOrderIndependent(t *testing.T) {
	t.Parallel()

	buildPackage := func(hookOrder []string) adapter.Package {
		hooks := make([]manifest.HookArtifact, len(hookOrder))
		for index, id := range hookOrder {
			hooks[index] = manifest.HookArtifact{ID: id, Path: "hooks/" + id + ".sh", Event: manifest.HookSessionStart}
		}
		return adapter.Package{
			Source:   "github:owner/pkg",
			Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: hooks}},
		}
	}
	render := func(hookOrder []string) []byte {
		t.Helper()
		coordinator, err := adapter.NewCoordinator(NewCompiler(), NewReferenceAdapter("1.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		intents, err := coordinator.Realize(context.Background(), adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{buildPackage(hookOrder)}, realize.Ledger{})
		if err != nil {
			t.Fatal(err)
		}
		if len(intents) != 1 {
			t.Fatalf("intents = %#v, want exactly one hooks.json intent", intents)
		}
		return intents[0].Content
	}

	forward := render([]string{"hook-a", "hook-b", "hook-c"})
	shuffled := render([]string{"hook-c", "hook-a", "hook-b"})
	if string(forward) != string(shuffled) {
		t.Fatalf("rendered config depends on input order:\nforward: %s\nshuffled: %s", forward, shuffled)
	}
}
