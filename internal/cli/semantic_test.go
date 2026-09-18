package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/producerconvert"
)

func TestParseExplicitSemanticProviderAndIfMissing(t *testing.T) {
	value, _, err := parseInvocation(CommandMigrate, []string{"tessl-plugin", "src", "--acr-only", "--repository", "https://github.com/owner/package", "--agent", "claude"})
	if err != nil || value.MigrationAgent != "claude" || len(value.Agents) != 0 {
		t.Fatalf("provider parse: %+v %v", value, err)
	}
	value, _, err = parseInvocation(CommandInstall, []string{"github:owner/package@v1.0.0", "--if-missing", "--non-interactive"})
	if err != nil || !value.IfMissing || value.RequestedVersion != "v1.0.0" {
		t.Fatalf("install parse: %+v %v", value, err)
	}
	for _, args := range [][]string{{"--if-missing"}, {"github:owner/package", "--if-missing", "--hold"}, {"github:owner/package", "--if-missing=false"}, {"github:owner/package", "--if-missing", "--if-missing"}} {
		if _, _, err := parseInvocation(CommandInstall, args); err == nil {
			t.Fatalf("accepted install %v", args)
		}
	}
	for _, args := range [][]string{{"tessl", "--agent", "claude"}, {"tessl-plugin", "--agent", "claude"}, {"tessl-plugin", "--acr-only", "--repository", "https://github.com/owner/package", "--agent", "claude", "--agent", "codex"}, {"tessl-plugin", "--acr-only", "--repository", "https://github.com/owner/package", "--agent", "unknown"}} {
		if _, _, err := parseInvocation(CommandMigrate, args); err == nil {
			t.Fatalf("accepted migrate %v", args)
		}
	}
}

func TestParseAuthoredValidation(t *testing.T) {
	for _, args := range [][]string{nil, {"package", "--json"}} {
		got, help, err := parseInvocation(CommandValidate, args)
		want := "."
		if len(args) > 0 {
			want = "package"
		}
		if err != nil || help || got.ValidationPath != want {
			t.Fatalf("validation parse: %+v %v", got, err)
		}
	}
	for _, args := range [][]string{{"one", "two"}, {"--dry-run"}, {"--agent", "codex"}, {"--non-interactive"}} {
		if _, _, err := parseInvocation(CommandValidate, args); err == nil {
			t.Fatalf("accepted validate %v", args)
		}
	}
}

func TestSemanticPromptUsesSupportedPublicCommands(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(docsRepositoryRoot(t), "internal/producerconvert/semantic-prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile("`(acr [^`]+)`").FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		t.Fatal("semantic prompt has no public command examples")
	}
	for _, match := range matches {
		example := strings.NewReplacer("github:OWNER/REPO@vVERSION", "github:example/package@v1.2.3", "PATH", "package").Replace(match[1])
		args := strings.Fields(example)
		command, ok := commandFor(args[1])
		if !ok {
			t.Errorf("prompt advertises unavailable command %q", example)
			continue
		}
		if _, help, err := parseInvocation(command, args[2:]); err != nil || help {
			t.Errorf("prompt advertises invalid invocation %q: %v", example, err)
		}
	}
}

// TestCodexSupportPolicyDocumented pins the CLI reference to the runtime
// contract's own constants: the documented minimum and every verified release
// appear in the policy section, the retired exact-pin sentence is gone, and
// the CI credential is documented beside its settings link.
func TestCodexSupportPolicyDocumented(t *testing.T) {
	root := docsRepositoryRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	reference := string(body)
	if !strings.Contains(reference, "### Codex runtime support policy") {
		t.Fatal("docs/cli.md has no Codex runtime support policy section")
	}
	if !strings.Contains(reference, "minimum `"+producerconvert.CodexMinimumVersion+"`") {
		t.Fatalf("docs/cli.md does not document the minimum Codex version %s", producerconvert.CodexMinimumVersion)
	}
	for _, release := range producerconvert.CodexVerifiedReleases {
		if !strings.Contains(reference, "`"+release.Version+"` ("+release.Released+")") {
			t.Errorf("docs/cli.md does not list verified release %s (%s)", release.Version, release.Released)
		}
	}
	for _, retired := range []string{"exactly `codex-cli", "Codex runtime renewal", "do not silently relax the exact-version check", "Codex currently requires macOS"} {
		if strings.Contains(reference, retired) {
			t.Errorf("docs/cli.md still carries the retired exact-pin text %q", retired)
		}
	}
	for _, required := range []string{"CODEX_API_KEY", "bubblewrap", "sandbox-exec", "`skip_host_skill_discovery`", "`unified_exec`", "401 Unauthorized"} {
		if !strings.Contains(reference, required) {
			t.Errorf("docs/cli.md policy does not mention %s", required)
		}
	}
	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), "CODEX_API_KEY=") || !strings.Contains(string(example), "codex-live.yml") || !strings.Contains(string(example), "settings/secrets/actions") {
		t.Fatalf(".env.example does not document CODEX_API_KEY with its purpose and settings link:\n%s", example)
	}
}
