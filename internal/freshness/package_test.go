package freshness

import (
	"bytes"
	"encoding/json"
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
	output := decodeSessionStartOutput(t, stdout.Bytes())
	if !strings.HasPrefix(output.HookSpecificOutput.AdditionalContext, "Session-start status — ") ||
		!strings.Contains(output.HookSpecificOutput.AdditionalContext, "install acr or set ACR_BIN") {
		t.Fatalf("wrapper additionalContext = %q", output.HookSpecificOutput.AdditionalContext)
	}
	if stderr.Len() != 0 {
		t.Fatalf("wrapper stderr = %q, want empty", stderr.String())
	}
}

func TestSessionStartWrapperFailsOpenAfterFreshnessFailure(t *testing.T) {
	t.Parallel()

	pkg, _ := HookPackage(PolicyInstall)
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
	fakeACR := filepath.Join(root, "fake-acr")
	fakeBody := []byte("#!/usr/bin/env bash\nset -euo pipefail\nprintf '%s\\n' \"$*\" >\"$ACR_TEST_CALL\"\nprintf 'simulated \"freshness\" failure\\n' >&2\nexit 4\n")
	if err := os.WriteFile(fakeACR, fakeBody, 0o755); err != nil {
		t.Fatal(err)
	}
	callPath := filepath.Join(root, "call.txt")
	command := exec.Command("bash", path, "--policy", "install")
	command.Env = append(os.Environ(), "ACR_BIN="+fakeACR, "ACR_TEST_CALL="+callPath)
	command.Stdin = strings.NewReader("")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("wrapper exit = %v, stderr = %q", err, stderr.String())
	}
	call, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	wantCall := "freshness run --project " + canonicalRoot + " --policy install\n"
	if string(call) != wantCall {
		t.Fatalf("wrapper call = %q, want %q", call, wantCall)
	}
	output := decodeSessionStartOutput(t, stdout.Bytes())
	context := output.HookSpecificOutput.AdditionalContext
	if !strings.HasPrefix(context, "Session-start status — ") || !strings.Contains(context, "simulated \"freshness\" failure") || !strings.Contains(context, "failed open") {
		t.Fatalf("wrapper additionalContext = %q", context)
	}
	if stderr.Len() != 0 {
		t.Fatalf("wrapper stderr = %q, want empty", stderr.String())
	}
}

func TestSessionStartWrapperUsesCursorContextField(t *testing.T) {
	t.Parallel()

	pkg, _ := HookPackage(PolicyOutdated)
	body, err := fs.ReadFile(pkg.Root, SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, ".cursor", "hooks", "owner", "session-start.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeACR := filepath.Join(root, "fake-acr")
	if err := os.WriteFile(fakeACR, []byte("#!/usr/bin/env bash\nset -euo pipefail\nprintf 'freshness_outdated: update available\\n' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", path, "--policy", "outdated")
	command.Env = append(os.Environ(), "ACR_BIN="+fakeACR, "CURSOR_VERSION=test")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("wrapper exit = %v, stderr = %q", err, stderr.String())
	}
	output := decodeSessionStartOutput(t, stdout.Bytes())
	if !strings.HasPrefix(output.AdditionalContext, "Session-start status — ") || !strings.Contains(output.AdditionalContext, "update available") {
		t.Fatalf("wrapper additional_context = %q", output.AdditionalContext)
	}
	if output.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("wrapper emitted non-Cursor context = %q", output.HookSpecificOutput.AdditionalContext)
	}
	if stderr.Len() != 0 {
		t.Fatalf("wrapper stderr = %q, want empty", stderr.String())
	}
}

func TestSessionStartWrapperIsSilentWithoutStatus(t *testing.T) {
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
	fakeACR := filepath.Join(root, "fake-acr")
	if err := os.WriteFile(fakeACR, []byte("#!/usr/bin/env bash\nset -euo pipefail\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", path, "--policy", "outdated")
	command.Env = append(os.Environ(), "ACR_BIN="+fakeACR)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("wrapper exit = %v, stderr = %q", err, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("wrapper output without status: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

type sessionStartOutput struct {
	AdditionalContext  string `json:"additional_context"`
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func decodeSessionStartOutput(t *testing.T, raw []byte) sessionStartOutput {
	t.Helper()
	var output sessionStartOutput
	if err := json.Unmarshal(bytes.TrimSpace(raw), &output); err != nil {
		t.Fatalf("decode wrapper output %q: %v", raw, err)
	}
	return output
}

func TestFreshnessPackageNoneContributesNothing(t *testing.T) {
	t.Parallel()

	if _, ok := HookPackage(PolicyNone); ok {
		t.Fatal("HookPackage(none) returned a package")
	}
}
