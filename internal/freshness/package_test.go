package freshness

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestFreshnessPackageIdentity(t *testing.T) {
	t.Parallel()

	pkg, ok := HookPackage(PolicyOutdated)
	if !ok {
		t.Fatal("HookPackage(outdated) returned no package")
	}
	if pkg.Source != Source || len(pkg.Manifest.Artifacts.Hooks) != 1 {
		t.Fatalf("package identity = %#v", pkg)
	}
	hook := pkg.Manifest.Artifacts.Hooks[0]
	want := manifest.HookArtifact{
		ID: ArtifactID, Event: manifest.HookSessionStart, Path: SourcePath,
		Args: []string{"--policy", "outdated"},
	}
	if hook.ID != want.ID || hook.Event != want.Event || hook.Path != want.Path || len(hook.Args) != 2 || hook.Args[0] != want.Args[0] || hook.Args[1] != want.Args[1] {
		t.Fatalf("hook identity = %#v, want %#v", hook, want)
	}
	owner := adapter.OwnerRef{Source: pkg.Source, ArtifactID: hook.ID, SourcePath: hook.Path, Kind: adapter.ArtifactHook, Event: hook.Event}
	if owner != (adapter.OwnerRef{Source: Source, ArtifactID: ArtifactID, SourcePath: SourcePath, Kind: adapter.ArtifactHook, Event: manifest.HookSessionStart}) {
		t.Fatalf("owner = %#v", owner)
	}
	body, err := fs.ReadFile(pkg.Root, hook.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "#!/usr/bin/env bash\nset -euo pipefail\n") {
		t.Fatalf("wrapper prefix = %q", body)
	}
}

func TestSessionStartWrapperFailsOpenWhenACRIsMissing(t *testing.T) {
	t.Parallel()

	pkg, _ := HookPackage(PolicyOutdated)
	body, err := fs.ReadFile(pkg.Root, SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, ".claude", "hooks", "owner", "session-start.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", path, "--policy", "outdated")
	command.Env = append(os.Environ(), "ACR_BIN="+filepath.Join(root, "missing-acr"))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("wrapper exit = %v, stderr = %q", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("wrapper stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "install acr or set ACR_BIN") {
		t.Fatalf("wrapper stderr = %q, want recovery guidance", stderr.String())
	}
}

func TestFreshnessPackageNoneContributesNothing(t *testing.T) {
	t.Parallel()

	if _, ok := HookPackage(PolicyNone); ok {
		t.Fatal("HookPackage(none) returned a package")
	}
}
