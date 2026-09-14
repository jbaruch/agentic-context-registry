package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/packageref"
	"github.com/jbaruch/agentic-context-registry/internal/producerconvert"
)

func TestSemanticProducerNativeCLIProposal(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	root := cleanProducerFixture(t)
	name := "packages/compass/skills/answer/info.py"
	original := "print('tessl install old/compass')\n"
	reverify2Put(t, root, name, original, 0o644)
	sum := sha256.Sum256([]byte(original))
	proposal := map[string]any{"edits": []any{map[string]any{"path": name, "beforeDigest": "sha256:" + hex.EncodeToString(sum[:]), "action": "replace", "content": "print('ACR compass')\n", "replacements": []any{}}}, "policyChanges": []any{}}
	events := []any{map[string]any{"type": "system", "subtype": "init", "tools": []string{"StructuredOutput"}, "mcp_servers": []any{}}, map[string]any{"type": "result", "subtype": "success", "structured_output": proposal}}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	providerDir := t.TempDir()
	reverify2Put(t, providerDir, "claude", "#!/bin/sh\nprintf '%s' '"+strings.ReplaceAll(string(encoded), "'", "'\"'\"'")+"'\n", 0o755)
	t.Setenv("PATH", providerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := []string{"migrate", "tessl-plugin", filepath.Join(root, "packages/compass"), "--acr-only", "--repository", "https://github.com/example/semantic-compass", "--agent", "claude", "--json"}
	before := snapshotProjectTree(t, root)
	preview := project.runBinary(binary, 0, append(append([]string{}, args...), "--dry-run")...)
	if !strings.Contains(preview.stdout, "agentRuns") || !strings.Contains(preview.stdout, "ACR compass") {
		t.Fatal("missing exact provider preview")
	}
	assertTreeUnchanged(t, before, root, "semantic CLI dry-run")
	project.runBinary(binary, 0, args...)
	assertFileBody(t, root, name, "print('ACR compass')\n")
	project.runBinary(binary, 0, args...)
}

// This opt-in harness consumes an externally generated tree verbatim. It makes
// local fixture Git history only; it never rewrites/chmods any authored output.
// Normal CI covers the native CLI with a deterministic provider above.
func TestSemanticLiveGeneratedPublication(t *testing.T) {
	root := os.Getenv("ACR_SEMANTIC_ACCEPTANCE_ROOT")
	if root == "" {
		t.Skip("live-generated tree acceptance is explicitly supplied by the developer")
	}
	binary := journeyBuiltBinary(t)
	journeyGit(t, root, "init", "-q", "-b", "main")
	journeyGit(t, root, "add", "-A")
	value, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	before := semanticAuthoredSnapshot(t, root)
	github := newJourneyGitHub(t)
	producer := newJourneyProject(t, github)
	if journeyGit(t, root, "status", "--porcelain") != "" {
		journeyGit(t, root, "commit", "-qm", "Record exact CLI-generated package")
	}
	tag := "v" + value.Version
	commit := journeyGit(t, root, "rev-parse", "HEAD")
	if journeyGit(t, root, "tag", "--list", tag) == "" {
		journeyGit(t, root, "tag", tag)
	} else if journeyGit(t, root, "rev-parse", tag) != commit {
		t.Fatal("fixture tag differs from generated commit")
	}
	github.PublishSource(value.Name, tag, commit, journeyGitSourceArchive(t, root, tag, commit))
	for _, args := range [][]string{{"publish", root, "--dry-run", "--json"}, {"publish", root, "--json"}} {
		result := producer.runSubprocess(0, args...)
		t.Logf("%v\n%s\n%s", result.args, result.stdout, result.stderr)
	}
	release := github.Repository(value.Name).Releases
	if len(release) != 1 || len(release[0].Assets) != 3 {
		t.Fatalf("publication assets: %+v", release)
	}
	output := os.Getenv("ACR_SEMANTIC_ACCEPTANCE_EVIDENCE")
	if output != "" {
		for _, asset := range release[0].Assets {
			if filepath.Base(asset.Name) != asset.Name {
				t.Fatal("unexpected asset name")
			}
			if err := os.WriteFile(filepath.Join(output, asset.Name), asset.Bytes, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	consumer := newJourneyProject(t, github)
	journeyGit(t, consumer.root, "init", "-q", "-b", "main")
	foreignBody := `{"dependencies":{"foreign/retained":{"version":"8.7.6"}}}` + "\n"
	reverify2Put(t, consumer.root, "tessl.json", foreignBody, 0o644)
	reverify2Put(t, consumer.root, ".tessl/plugins/foreign/retained/SKILL.md", "Foreign user-owned skill.\n", 0o644)
	for _, args := range [][]string{{"init", "--agent", "claude-code", "--agent", "codex", "--agent", "cursor", "--freshness", "none", "--non-interactive"}, {"install", "github:" + value.Name, "--non-interactive"}, {"realize"}, {"check"}} {
		result := consumer.runSubprocess(0, args...)
		t.Logf("%v\n%s\n%s", result.args, result.stdout, result.stderr)
	}
	// Put the production-client composition on PATH for emitted helpers. This
	// shim redirects transport only; it executes exactly the shipped CLI stack.
	shimDir := t.TempDir()
	shim := "#!/bin/sh\nexec '" + strings.ReplaceAll(os.Args[0], "'", "'\"'\"'") + "' -test.run='^TestJourneyComposedSubprocessEntry$' -- \"$@\"\n"
	reverify2Put(t, shimDir, "acr", shim, 0o755)
	runtimeEnv := []string{journeySubprocessEndpoint + "=" + github.Endpoint(), "PATH=" + shimDir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR"), "ACR_STATE_HOME=" + consumer.stateHome, "GH_TOKEN=" + journeyToken, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "PYTHONDONTWRITEBYTECODE=1"}
	for _, agent := range []string{".claude", ".codex", ".cursor"} {
		refs := packageref.SkillReferences{}
		for _, skill := range value.Artifacts.Skills {
			refs.Rebases = append(refs.Rebases, packageref.SkillRebase{SourceRoot: skill.Path, NativeRoot: nativeSkillDirectory(agent, value.Name, skill.ID)})
		}
		for _, skill := range value.Artifacts.Skills {
			native := filepath.Join(consumer.root, nativeSkillDirectory(agent, value.Name, skill.ID))
			entry, err := os.ReadFile(filepath.Join(native, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(entry, []byte("package-file:")) || !bytes.Equal(entry, packageref.RebasePackageReferences(entry, refs)) {
				t.Fatalf("%s %s retains an unresolved owned helper reference: %s", agent, skill.ID, entry)
			}
			metadata, err := os.ReadFile(filepath.Join(native, ".acr-package.json"))
			if err != nil || !strings.Contains(string(metadata), value.Name) {
				t.Fatalf("%s metadata: %s %v", agent, metadata, err)
			}
			// A caller selects a helper and expected output; no source-specific port
			// or alternate generated file enters the publication/realization pipeline.
			helper := os.Getenv("ACR_SEMANTIC_ACCEPTANCE_HELPER")
			if helper == "" || !strings.HasPrefix(helper, skill.ID+"/") {
				continue
			}
			if os.Getenv("ACR_SEMANTIC_ACCEPTANCE_SCAFFOLD") == "1" {
				semanticScaffoldState(t, consumer, value.Name, commit, agent)
			}
			stateBefore, err := dependency.LoadState(consumer.root)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(filepath.Join(native, strings.TrimPrefix(helper, skill.ID+"/")), strings.Fields(os.Getenv("ACR_SEMANTIC_ACCEPTANCE_ARGS"))...)
			command.Dir = consumer.root
			command.Env = runtimeEnv
			if os.Getenv("ACR_SEMANTIC_ACCEPTANCE_SCAFFOLD") == "1" && agent == ".claude" {
				reverify2Put(t, consumer.root, ".github/scripts", "injected parent obstruction\n", 0o644)
				beforeFailure := semanticAuthoredSnapshot(t, consumer.root)
				failed := exec.Command(command.Path, command.Args[1:]...)
				failed.Dir = command.Dir
				failed.Env = command.Env
				output, err := failed.CombinedOutput()
				t.Logf("scaffold obstruction: %s (error=%v)", output, err)
				if err == nil || !reflect.DeepEqual(beforeFailure, semanticAuthoredSnapshot(t, consumer.root)) {
					t.Fatal("failed scaffold did not preserve the complete consumer before-image")
				}
				if err := os.Remove(filepath.Join(consumer.root, ".github/scripts")); err != nil {
					t.Fatal(err)
				}
			}
			output, err := command.CombinedOutput()
			t.Logf("direct %s %s: %s (error=%v)", agent, helper, output, err)
			if err != nil {
				t.Fatal(err)
			}
			stateAfter, err := dependency.LoadState(consumer.root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stateBefore, stateAfter) {
				t.Fatal("native helper changed an existing request, hold, lock or foreign ACR setting")
			}
			assertFileBody(t, consumer.root, "tessl.json", foreignBody)
			assertFileBody(t, consumer.root, ".tessl/plugins/foreign/retained/SKILL.md", "Foreign user-owned skill.\n")
			if os.Getenv("ACR_SEMANTIC_ACCEPTANCE_SCAFFOLD") == "1" {
				settled := semanticAuthoredSnapshot(t, consumer.root)
				again := exec.Command(command.Path, command.Args[1:]...)
				again.Dir = command.Dir
				again.Env = command.Env
				if output, err := again.CombinedOutput(); err != nil {
					t.Fatalf("idempotent scaffold: %v %s", err, output)
				}
				if !reflect.DeepEqual(settled, semanticAuthoredSnapshot(t, consumer.root)) {
					t.Fatal("second native scaffold changed consumer output")
				}
			}
			expected := os.Getenv("ACR_SEMANTIC_ACCEPTANCE_EXPECT")
			if expected != "" && !strings.Contains(string(output), expected) {
				t.Fatalf("direct helper output lacks %q", expected)
			}
		}
	}
	consumer.runSubprocess(0, "check")
	clone := filepath.Join(t.TempDir(), "clone")
	journeyGit(t, filepath.Dir(clone), "clone", "--no-hardlinks", "-q", root, clone)
	receiptBytes, err := os.ReadFile(filepath.Join(clone, producerconvert.ReceiptPath))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Options producerconvert.Options `json:"options"`
	}
	if err := json.Unmarshal(receiptBytes, &receipt); err != nil {
		t.Fatal(err)
	}
	options := receipt.Options
	args := []string{"migrate", "tessl-plugin", filepath.Join(clone, options.PackageRoot), "--acr-only", "--repository", options.Repository, "--agent", options.Agent, "--json"}
	if options.PackageVersion != "" {
		args = append(args, "--package-version", options.PackageVersion)
	}
	if options.AcceptAgentWidening {
		args = append(args, "--accept-agent-widening")
	}
	repeated := producer.runBinary(binary, 0, args...)
	if journeyResult(t, repeated.stdout)["current"] != true {
		t.Fatal("portable clone receipt did not remain inert")
	}
	t.Log("portable clone rerun passed without provider access")
	if !reflect.DeepEqual(before, semanticAuthoredSnapshot(t, root)) {
		t.Fatal("publication changed generated authored files, bytes or modes")
	}
	t.Log(fmt.Sprintf("exact generated package %s@%s commit=%s: publication, install and all three adapters passed", value.Name, tag, commit))
}

func semanticAuthoredSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	value := snapshotProjectTree(t, root)
	for name := range value {
		if strings.HasPrefix(name, "git:") {
			delete(value, name)
		}
	}
	return value
}

func semanticScaffoldState(t *testing.T, consumer *journeyProject, name, commit, agent string) {
	t.Helper()
	if agent == ".codex" {
		consumer.runSubprocess(0, "install", "github:"+name+"@"+commit, "--non-interactive")
	}
	if agent == ".cursor" {
		state, err := dependency.LoadState(consumer.root)
		if err != nil {
			t.Fatal(err)
		}
		state.Project.Dependencies[0].Requested = "latest"
		state.Project.Dependencies[0].Hold = &dependency.Hold{Pin: commit, Rejected: "v99.0.0", Reason: "independent fixture rollback barrier"}
		state.Lock.Dependencies[0].Requested = "latest"
		state.Lock.Dependencies[0].Hold = &dependency.LockHold{RejectedTag: "v99.0.0", RejectedReleaseID: 999, RejectedCommit: strings.Repeat("f", 40)}
		if err := dependency.WriteState(consumer.root, state); err != nil {
			t.Fatal(err)
		}
	}
	consumer.runSubprocess(0, "realize")
	journeyGit(t, consumer.root, "add", "-A")
	if journeyGit(t, consumer.root, "status", "--porcelain") != "" {
		journeyGit(t, consumer.root, "commit", "-qm", "Record consumer fixture before native helper")
	}
	if agent == ".claude" {
		journeyGit(t, consumer.root, "checkout", "-qb", "feat/semantic-native")
	}
}

func TestSemanticProviderInterruptLeavesSourceUntouched(t *testing.T) {
	binary := journeyBuiltBinary(t)
	project := newJourneyProject(t, nil)
	root := cleanProducerFixture(t)
	reverify2Put(t, root, "packages/compass/skills/answer/custom.py", "print('tessl install old/compass')\n", 0o644)
	before := snapshotProjectTree(t, root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	providerDir := t.TempDir()
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(os.Args[0], "'", "'\"'\"'") + "' -test.run='^TestSemanticBlockingProviderEntry$'\n"
	reverify2Put(t, providerDir, "claude", script, 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "migrate", "tessl-plugin", filepath.Join(root, "packages/compass"), "--acr-only", "--repository", "https://github.com/example/cancelled", "--agent", "claude", "--json")
	command.Dir = project.root
	command.Env = append(os.Environ(), "PATH="+providerDir+string(os.PathListSeparator)+os.Getenv("PATH"), "ACR_SEMANTIC_PROVIDER_READY="+listener.Addr().String())
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	acceptError := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			acceptError <- err
			return
		}
		accepted <- connection
	}()
	var connection net.Conn
	select {
	case connection = <-accepted:
	case err := <-acceptError:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("provider handshake did not arrive")
	}
	defer connection.Close()
	signal := make([]byte, 1)
	if _, err := io.ReadFull(connection, signal); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted provider succeeded")
	}
	if !strings.Contains(stderr.String(), "context canceled") || !strings.Contains(stderr.String(), "agent_failed") {
		t.Fatalf("interrupt did not reach provider context: %s", stderr.String())
	}
	assertTreeUnchanged(t, before, root, "interrupted native provider")
}

func TestSemanticBlockingProviderEntry(t *testing.T) {
	address := os.Getenv("ACR_SEMANTIC_PROVIDER_READY")
	if address == "" {
		t.Skip("subprocess handshake fixture only")
	}
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	// The parent retains this connection until cancellation kills this process.
	// No timer, polling or source filesystem handshake determines success.
	var data [1]byte
	if _, err := connection.Read(data[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}
