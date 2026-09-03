package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// verify8SeedTesslConsumer writes a Tessl consumer holding two unmapped
// packages inside a git repository whose author and committer dates are fixed,
// so the tracking predicate finalization depends on has real git state to read
// and the fixture carries no wall-clock input.
//
// Both packages are unmapped on purpose. A github: mapping cannot be exercised
// through a separately built binary: the fake remote the in-process tests
// inject is not reachable across the process boundary, and reaching the real
// api.github.com from a test is forbidden. The mapped half of that row is
// covered in-process by TestMapWinsOverVendorUnmapped and TestMapSupersedesVendor.
func verify8SeedTesslConsumer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Codex detection evidence, so the built binary resolves an adapter set from
	// the project instead of refusing with "no agent adapters selected".
	if err := os.MkdirAll(filepath.Join(root, ".codex/skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "orphan"} {
		packageRoot := filepath.Join(root, ".tessl/plugins/example", name)
		for _, directory := range []string{
			filepath.Join(packageRoot, ".tessl-plugin"),
			filepath.Join(packageRoot, "rules"),
			filepath.Join(packageRoot, "skills", "review-"+name),
		} {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		files := map[string][]byte{
			filepath.Join(packageRoot, ".tessl-plugin/plugin.json"):       []byte(`{"name":"example/` + name + `","version":"1.2.3","rules":["rules"],"skills":["skills"]}`),
			filepath.Join(packageRoot, "rules/always.md"):                 []byte("Always " + name + ".\n"),
			filepath.Join(packageRoot, "skills/review-"+name+"/SKILL.md"): []byte("# Review " + name + "\n"),
		}
		for filename, content := range files {
			if err := os.WriteFile(filename, content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// The tessl__ native is what makes each skill positively identified as
		// Tessl-owned; without it the skill classifies ambiguous and blocks
		// finalization, which is a fixture defect rather than product behaviour.
		if err := os.Symlink("../../.tessl/plugins/example/"+name+"/skills/review-"+name,
			filepath.Join(root, ".codex/skills/tessl__review-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	document := map[string]any{"name": "consumer", "dependencies": map[string]any{
		"example/alpha":  map[string]string{"version": "1.2.3"},
		"example/orphan": map[string]string{"version": "1.2.3"},
	}}
	tesslJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tessl.json"), tesslJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	verify8GitCommit(t, root)
	return root
}

func verify8GitCommit(t *testing.T, root string) {
	t.Helper()
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "fixture"}} {
		command := exec.Command("git", arguments...)
		command.Dir = root
		command.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=ACR Test", "GIT_AUTHOR_EMAIL=acr@example.invalid",
			"GIT_COMMITTER_NAME=ACR Test", "GIT_COMMITTER_EMAIL=acr@example.invalid",
			"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}
}

func verify8Envelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", stdout, err)
	}
	return envelope
}

// TestVerify8BuiltBinaryVendorsAndFinalizesATesslConsumer drives the whole
// vendoring and finalization arc through a separately built acr binary rather
// than through the in-process application, so the JSON envelope, the exit
// codes, and the on-disk outcome are checked at the process contract the users
// of this feature actually see.
func TestVerify8BuiltBinaryVendorsAndFinalizesATesslConsumer(t *testing.T) {
	binary := reverifyBuildACR(t)
	root := verify8SeedTesslConsumer(t)
	stateHome := t.TempDir()
	sealed := reverify2HashTree(t, root)

	// Without --vendor-unmapped an unmapped package still fails closed, and the
	// refusal writes nothing.
	stdout, stderr, exitCode := hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--non-interactive", "--json")
	if exitCode == 0 {
		t.Fatalf("unmapped packages migrated without --vendor-unmapped: stdout = %q", stdout)
	}
	if stdout != "" || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("refusal is not one stderr envelope: exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code":"unmapped_package"`) {
		t.Fatalf("refusal code = %q, want unmapped_package", stderr)
	}
	if after := reverify2HashTree(t, root); !reflect.DeepEqual(after, sealed) {
		t.Fatal("the unmapped_package refusal changed the project tree")
	}

	// --vendor-unmapped vendors both packages and reports one clean envelope on
	// stdout with nothing on stderr.
	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--vendor-unmapped", "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("vendor exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	envelope := verify8Envelope(t, stdout)
	if envelope["ok"] != true || envelope["command"] != "migrate" {
		t.Fatalf("vendor envelope = %#v", envelope)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok || result["wrote"] != true {
		t.Fatalf("vendor result = %#v", envelope["result"])
	}
	vendored, ok := result["vendored"].([]any)
	if !ok || len(vendored) != 2 {
		t.Fatalf("vendored rows = %#v, want both packages", result["vendored"])
	}
	for _, name := range []string{"alpha", "orphan"} {
		rule := filepath.Join(root, ".agents/vendor/example", name, "rules/always.md")
		content, err := os.ReadFile(rule)
		if err != nil || string(content) != "Always "+name+".\n" {
			t.Fatalf("vendored rule %s = %q, %v", rule, content, err)
		}
	}

	// The vendored project realizes and checks clean offline.
	for _, command := range []string{"realize", "check"} {
		stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
			command, "--project", root, "--agent", "codex", "--json")
		if exitCode != 0 || stderr != "" {
			t.Fatalf("%s exit = %d, stdout = %q, stderr = %q", command, exitCode, stdout, stderr)
		}
	}

	// outdated reports both vendored rows as non-actionable and contacts nothing.
	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"outdated", "--project", root)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("outdated exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "Vendored (not tracked upstream):") {
		t.Fatalf("outdated stdout = %q, want the vendored section", stdout)
	}

	verify8GitCommit(t, root)
	beforeFinalize := reverify2HashTree(t, root)

	// A finalize dry run plans removals and writes nothing.
	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--finalize", "--dry-run", "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("finalize dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	result, ok = verify8Envelope(t, stdout)["result"].(map[string]any)
	if !ok || result["wrote"] != false {
		t.Fatalf("finalize dry-run result = %#v", result)
	}
	if removed, ok := result["removed"].([]any); !ok || len(removed) == 0 {
		t.Fatalf("finalize dry-run planned no removals: %#v", result["removed"])
	}
	if after := reverify2HashTree(t, root); !reflect.DeepEqual(after, beforeFinalize) {
		t.Fatal("the finalize dry run changed the project tree")
	}

	// The applied finalization removes the Tessl installation and keeps the
	// vendored trees the project now depends on.
	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--finalize", "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("finalize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	result, ok = verify8Envelope(t, stdout)["result"].(map[string]any)
	if !ok || result["wrote"] != true || result["mode"] != "finalized" {
		t.Fatalf("finalize result = %#v", result)
	}
	for _, gone := range []string{"tessl.json", ".tessl"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived finalization: %v", gone, err)
		}
	}
	for _, name := range []string{"alpha", "orphan"} {
		if _, err := os.Stat(filepath.Join(root, ".agents/vendor/example", name, "rules/always.md")); err != nil {
			t.Fatalf("finalization removed the vendored tree for %s: %v", name, err)
		}
	}

	// The finalized project still checks clean, and a second finalize is inert.
	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"check", "--project", root, "--agent", "codex", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("check after finalize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	settled := reverify2HashTree(t, root)
	stdout, stderr, exitCode = hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--finalize", "--non-interactive", "--json")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("second finalize exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	result, ok = verify8Envelope(t, stdout)["result"].(map[string]any)
	if !ok || result["wrote"] != false {
		t.Fatalf("second finalize result = %#v", result)
	}
	if after := reverify2HashTree(t, root); !reflect.DeepEqual(after, settled) {
		t.Fatal("the second finalize changed the project tree")
	}
}

// TestVerify8BuiltBinaryRefusesAHandEditedVendorTree proves the content hash
// recorded at vendoring time is enforced on every later realization: a single
// edited byte inside .agents/vendor stops realize and check with a refusal
// naming the recovery, rather than silently realizing drifted content.
func TestVerify8BuiltBinaryRefusesAHandEditedVendorTree(t *testing.T) {
	binary := reverifyBuildACR(t)
	root := verify8SeedTesslConsumer(t)
	stateHome := t.TempDir()

	if _, stderr, exitCode := hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
		"migrate", "tessl", "--project", root, "--vendor-unmapped", "--non-interactive", "--json"); exitCode != 0 || stderr != "" {
		t.Fatalf("vendor exit = %d, stderr = %q", exitCode, stderr)
	}
	edited := filepath.Join(root, ".agents/vendor/example/orphan/rules/always.md")
	if err := os.WriteFile(edited, []byte("Always orphan, tampered.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sealed := reverify2HashTree(t, root)

	for _, command := range []string{"check", "realize"} {
		stdout, stderr, exitCode := hostileRunBinary(t, binary, stateHome, strings.NewReader(""),
			command, "--project", root, "--agent", "codex", "--json")
		if exitCode == 0 {
			t.Fatalf("%s accepted a hand-edited vendor tree: stdout = %q", command, stdout)
		}
		if stdout != "" || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("%s refusal is not one stderr envelope: stdout = %q, stderr = %q", command, stdout, stderr)
		}
		for _, want := range []string{"content hash mismatch for vendor:example/orphan", "restore the vendored tree from version control", "acr migrate tessl --vendor-unmapped"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%s refusal = %q, want %q", command, stderr, want)
			}
		}
		if after := reverify2HashTree(t, root); !reflect.DeepEqual(after, sealed) {
			t.Fatalf("the %s refusal changed the project tree", command)
		}
	}
}
