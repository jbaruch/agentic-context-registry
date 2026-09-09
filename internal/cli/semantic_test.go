package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
