package producerconvert

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCodexRuntimeRealRelease runs the production runtime contract against
// one real Codex release inside the host's real boundary, without any
// credential. It proves what the deterministic suite cannot: that this
// release parses ACR's argument vector, advertises and honors every required
// control, starts inside the boundary, emits the verified startup prelude and
// reaches the model service, which refuses the missing credential. The lane
// is opt-in through ACR_CODEX_RELEASE_BIN and made mandatory in CI through
// ACR_CODEX_RELEASE_REQUIRED=1, so a missing binary there is a failure.
func TestCodexRuntimeRealRelease(t *testing.T) {
	binary := os.Getenv("ACR_CODEX_RELEASE_BIN")
	expected := os.Getenv("ACR_CODEX_RELEASE_VERSION")
	if binary == "" || expected == "" {
		if os.Getenv("ACR_CODEX_RELEASE_REQUIRED") == "1" {
			t.Fatal("ACR_CODEX_RELEASE_REQUIRED=1 but ACR_CODEX_RELEASE_BIN or ACR_CODEX_RELEASE_VERSION is unset")
		}
		t.Skip("real Codex release probe is supplied through ACR_CODEX_RELEASE_BIN and ACR_CODEX_RELEASE_VERSION")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Fatalf("no verified Codex read boundary on %s", runtime.GOOS)
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	head, err := codexFileHead(resolved)
	if err != nil || !codexNativeMagic(runtime.GOOS, head) {
		t.Fatalf("%s is not a native %s executable: %v", binary, runtime.GOOS, err)
	}
	// An empty configured home: no credential is copied, so the run must end
	// at the model service's refusal, never earlier.
	sourceHome := filepath.Join(t.TempDir(), "codex-home")
	if err := os.Mkdir(sourceHome, 0o700); err != nil {
		t.Fatal(err)
	}
	var environ []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "CODEX_API_KEY" || key == "OPENAI_API_KEY" {
			continue
		}
		environ = append(environ, entry)
	}
	native := codexRuntime{platform: runtime.GOOS, arch: runtime.GOARCH, executable: resolved, home: sourceHome, homeBase: filepath.Join(t.TempDir(), "homes"), environ: environ}
	_, run, err := runCodexWithRuntime(context.Background(), `Return exactly {"edits":[],"policyChanges":[]} and nothing else.`, native)
	if evidence := os.Getenv("ACR_CODEX_RELEASE_EVIDENCE"); evidence != "" {
		record := struct {
			Platform string   `json:"platform"`
			Binary   string   `json:"binary"`
			Run      AgentRun `json:"run"`
			Error    string   `json:"error,omitempty"`
		}{runtime.GOOS + "/" + runtime.GOARCH, resolved, run, ""}
		if err != nil {
			record.Error = err.Error()
		}
		data, marshalErr := json.MarshalIndent(record, "", "  ")
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if mkErr := os.MkdirAll(evidence, 0o755); mkErr != nil {
			t.Fatal(mkErr)
		}
		if writeErr := os.WriteFile(filepath.Join(evidence, "release-"+expected+"-"+runtime.GOOS+"-"+runtime.GOARCH+".json"), data, 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err == nil {
		t.Fatal("an unauthenticated run completed a proposal")
	}
	if run.RuntimeVersion != "codex-cli "+expected {
		t.Fatalf("runtime version = %q, want codex-cli %s (%v)", run.RuntimeVersion, expected, err)
	}
	if run.Isolation == "" || len(run.Arguments) == 0 {
		t.Fatalf("boundary evidence missing: %+v", run)
	}
	if !strings.Contains(err.Error(), "Codex authentication failed (401 Unauthorized)") {
		t.Fatalf("the run did not reach the model service's credential refusal; every earlier stage is a contract failure on this release: %v", err)
	}
	argv := strings.Join(run.Arguments, " ")
	for _, required := range []string{"--disable shell_tool", "--disable code_mode_host", "--disable shell_snapshot", "--enable " + codexSkipHostSkills, "--sandbox read-only", "--ignore-user-config"} {
		if !strings.Contains(argv, required) {
			t.Fatalf("argv lacks %q: %s", required, argv)
		}
	}
	if !strings.Contains(run.Stdout, disabledCodeHost) || !strings.Contains(run.Stdout, disabledSkillDiscovery) || !strings.Contains(run.Stdout, `{"type":"turn.started"}`) {
		t.Fatalf("startup prelude not observed on this release: %s", run.Stdout)
	}
	if strings.Contains(run.Stdout, "command_execution") || strings.Contains(run.Stdout, "mcp_tool_call") {
		t.Fatalf("tool activity observed: %s", run.Stdout)
	}
	if strings.Contains(run.Stderr, "Refusing to create helper binaries") {
		t.Fatalf("isolated home landed inside the runtime's temporary directory: %s", run.Stderr)
	}
	if entries, readErr := os.ReadDir(native.homeBase); readErr != nil || len(entries) != 0 {
		t.Fatalf("isolated home retained: %v %v", entries, readErr)
	}
	verified := false
	for _, release := range CodexVerifiedReleases {
		verified = verified || release.Version == expected
	}
	if !verified {
		t.Logf("release %s passed the runtime contract but is not yet in CodexVerifiedReleases; add it with its date after this evidence is reviewed", expected)
	}
}
