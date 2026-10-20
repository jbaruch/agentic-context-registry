package producerconvert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexStartupContractPerVerifiedRelease holds codexFinal to the startup
// preludes recorded from every verified release on both platforms
// (testdata/codex/PROVENANCE.md). The recorded stream is the oracle: its
// prelude followed by a synthetic completed turn is accepted, the recorded
// unauthenticated tail is refused and classified, and a prelude with the
// disabled-host proof removed or a tool item inserted is refused.
func TestCodexStartupContractPerVerifiedRelease(t *testing.T) {
	body := `{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}` + "\n" + `{"type":"turn.completed"}` + "\n"
	for _, release := range CodexVerifiedReleases {
		for _, platform := range codexPlatforms {
			t.Run(release.Version+"/"+platform, func(t *testing.T) {
				recorded, err := os.ReadFile(filepath.Join("testdata", "codex", release.Version, platform+"-startup.jsonl"))
				if err != nil {
					t.Fatalf("every verified release needs a recorded stream per platform: %v", err)
				}
				stream := string(recorded)
				prelude, tail, found := strings.Cut(stream, `{"type":"turn.started"}`+"\n")
				if !found {
					t.Fatalf("recorded stream has no turn.started: %s", stream)
				}
				prelude += `{"type":"turn.started"}` + "\n"
				if !strings.Contains(prelude, disabledCodeHost) || !strings.Contains(prelude, disabledSkillDiscovery) {
					t.Fatalf("recorded prelude lacks the verified startup proof: %s", prelude)
				}
				if final, warnings, err := codexFinal(prelude+body, ""); err != nil || final != "{}" || len(warnings) != 0 {
					t.Fatalf("recorded prelude refused: %v", err)
				}
				if _, _, err := codexFinal(stream, ""); err == nil {
					t.Fatal("recorded unauthenticated stream accepted")
				}
				if !codexUnauthorized(tail, "") {
					t.Fatalf("recorded tail is not classified as unauthorized: %s", tail)
				}
				for name, mutation := range map[string]string{
					"host proof removed":   strings.Replace(prelude, disabledCodeHost, "Code Mode is available.", 1),
					"tool item inserted":   strings.Replace(prelude, `{"type":"turn.started"}`, `{"type":"item.completed","item":{"type":"command_execution","command":"cat AGENTS.md"}}`+"\n"+`{"type":"turn.started"}`, 1),
					"unknown startup item": strings.Replace(prelude, `{"type":"turn.started"}`, `{"type":"item.completed","item":{"type":"error","message":"loaded plugin acme"}}`+"\n"+`{"type":"turn.started"}`, 1),
				} {
					if mutation == prelude {
						t.Fatalf("mutation %q did not apply", name)
					}
					if _, _, err := codexFinal(mutation+body, ""); err == nil {
						t.Fatalf("mutated prelude (%s) accepted", name)
					}
				}
			})
		}
	}
}

func TestCodexSecretRedaction(t *testing.T) {
	text := "key sk-proj-abcdefghijklmnop then masked sk-a156l***l-key then value SECRET-VALUE-0123456789 end sk-x"
	got := redactCodexSecrets(text, []string{"SECRET-VALUE-0123456789", ""})
	for _, leaked := range []string{"sk-proj-abcdefghijklmnop", "sk-a156l***l-key", "SECRET-VALUE-0123456789"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("%q survived redaction: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "key sk-[redacted] then masked sk-[redacted] then value [redacted] end sk-x") {
		t.Fatalf("redaction changed unrelated text: %s", got)
	}
	if values := codexCredentialValues([]byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"idtoken-0123456789abcdef","access_token":"short","refresh_token":"refresh-0123456789abcdef","account_id":"acct-0123456789abcdef"},"last_refresh":"2026-09-11T15:11:38Z"}`)); len(values) != 3 {
		t.Fatalf("credential values = %v", values)
	}
	if values := codexCredentialValues([]byte(`{"OPENAI_API_KEY":"sk-fixture-key-0123456789"}`)); len(values) != 1 {
		t.Fatalf("api key value = %v", values)
	}
	if values := codexCredentialValues([]byte(`not json`)); values != nil {
		t.Fatalf("malformed credential yielded %v", values)
	}
}

func TestCodexBoundaryEnvironmentForwardsOnlyCredentialsAndProxies(t *testing.T) {
	environ := []string{"PATH=/host/bin", "HOME=/Users/operator", "CODEX_HOME=/Users/operator/.codex", "CODEX_API_KEY=sk-forwarded-0123456789", "HTTPS_PROXY=http://proxy:3128", "OPENAI_API_KEY=sk-not-forwarded", "CODEX_SESSION_ID=abc", "EDITOR=vi", "no_proxy="}
	env := codexBoundaryEnvironment("/opt/acr", "/iso/home", "/work", environ, "SSL_CERT_FILE=/ca.pem")
	joined := strings.Join(env, "\n")
	for _, required := range []string{"PATH=/opt/acr", "HOME=/iso/home", "CODEX_HOME=/iso/home/.codex", "TMPDIR=/work", "LANG=C.UTF-8", "SSL_CERT_FILE=/ca.pem", "CODEX_API_KEY=sk-forwarded-0123456789", "HTTPS_PROXY=http://proxy:3128"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("environment lacks %s: %v", required, env)
		}
	}
	for _, forbidden := range []string{"/host/bin", "/Users/operator", "OPENAI_API_KEY", "CODEX_SESSION_ID", "EDITOR", "no_proxy"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment forwards %s: %v", forbidden, env)
		}
	}
}
