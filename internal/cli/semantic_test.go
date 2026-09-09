package cli

import "testing"

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
