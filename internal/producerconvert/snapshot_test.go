package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSemanticCredentialPathsRefuseBeforeProvider(t *testing.T) {
	const sentinel = "ACR_FILENAME_REFUSAL_SENTINEL"
	names := []string{".env", ".ENV", ".env.local", ".env.example.local", ".env.sample.bak", "client.pem", "CLIENT.PEM", "server.key", "id_rsa", "id_rsa.pub", "ID_ED25519_backup", ".npmrc", ".NETRC", ".pypirc"}
	for _, scope := range []string{"plugins/orbit", ".github", "tests"} {
		for _, name := range names {
			for _, agent := range []string{"codex", "claude"} {
				for _, dry := range []bool{true, false} {
					label := scope + "/" + name + "/" + agent
					if dry {
						label += "/dry-run"
					} else {
						label += "/apply"
					}
					t.Run(label, func(t *testing.T) {
						root, opts, p := semanticFixture(t)
						opts.Agent = agent
						opts.DryRun = dry
						filename := scope + "/" + name
						put(t, root, filename, sentinel+"\n", 0o600)
						// Ignore rules cannot authorize transmission.
						put(t, root, ".gitignore", "*\n", 0o644)
						before := treeAt(t, root)
						calls := 0
						plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _ string, request string) (proposal, AgentRun, error) {
							calls++
							return p, AgentRun{RequestDigest: digest([]byte(request))}, nil
						})
						if calls != 0 {
							t.Errorf("credential reached provider: calls=%d", calls)
						}
						if err == nil || !strings.Contains(err.Error(), filename) || !strings.Contains(err.Error(), "credential") {
							t.Errorf("expected actionable path-only refusal: %v", err)
						}
						encoded, e := json.Marshal(plan.Report)
						if e != nil {
							t.Fatal(e)
						}
						if strings.Contains(string(encoded), sentinel) || err != nil && strings.Contains(err.Error(), sentinel) {
							t.Error("credential content reached report/error")
						}
						if len(plan.Report.Changes) != 0 || len(plan.Report.AgentRuns) != 0 || len(plan.changes) != 0 || len(plan.receipt) != 0 {
							t.Error("refusal constructed changes or request evidence")
						}
						if !matches(before, treeAt(t, root)) {
							t.Error("refusal changed source")
						}
						absent(t, root, ReceiptPath)
						absent(t, root, transactionPath)
					})
				}
			}
		}
	}
}

func TestSemanticCredentialExamplesAndUnrelatedFilesRemainUsable(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			root, opts, p := semanticFixture(t)
			opts.Agent = agent
			for _, scope := range []string{"plugins/orbit", ".github", "tests"} {
				for _, name := range []string{".env.example", ".ENV.SAMPLE", "ordinary.txt"} {
					put(t, root, scope+"/"+name, "TOKEN=PLACEHOLDER\n", 0o644)
				}
			}
			for _, name := range []string{"other/.env", ".agents/.env", ".codex/.env", ".gemini/.env"} {
				put(t, root, name, "UNRELATED_CREDENTIAL_PLACEHOLDER\n", 0o600)
			}
			original := treeAt(t, root)
			calls := 0
			plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _ string, request string) (proposal, AgentRun, error) {
				calls++
				if !strings.Contains(request, "TOKEN=PLACEHOLDER") || strings.Contains(request, "UNRELATED_CREDENTIAL_PLACEHOLDER") {
					t.Error("incorrect selected/shared input scopes")
				}
				return p, AgentRun{}, nil
			})
			if err != nil || calls != 1 {
				t.Fatalf("ordinary source refused: %v calls=%d", err, calls)
			}
			if !matches(original, treeAt(t, root)) {
				t.Fatal("planning wrote source")
			}
			if _, err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			current, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
				t.Fatal("rerun invoked provider")
				return proposal{}, AgentRun{}, nil
			})
			if err != nil || !current.Report.Current {
				t.Fatalf("rerun: %v", err)
			}
		})
	}
}

func TestSemanticCredentialRefusalPrecedesReadingAndNativeCalls(t *testing.T) {
	for _, scope := range []string{"plugins/orbit", ".github", "tests"} {
		for _, agent := range []string{"codex", "claude"} {
			for _, dry := range []bool{true, false} {
				label := scope + "/" + agent
				if dry {
					label += "/dry-run"
				} else {
					label += "/apply"
				}
				t.Run(label, func(t *testing.T) {
					root, opts, _ := semanticFixture(t)
					opts.Agent = agent
					opts.DryRun = dry
					directory := t.TempDir()
					marker := filepath.Join(directory, "called")
					t.Setenv("PATH", directory)
					put(t, directory, agent, "#!/bin/sh\nprintf called > '"+marker+"'\nexit 7\n", 0o755)
					name := scope + "/.env"
					put(t, root, name, "UNREADABLE_CREDENTIAL_SENTINEL\n", 0o000)
					t.Cleanup(func() {
						if err := os.Chmod(filepath.Join(root, name), 0o600); err != nil {
							t.Error(err)
						}
					})
					report, err := Convert(opts)
					var refusal *Error
					if !errors.As(err, &refusal) || refusal.Code != "credential_input" || !strings.Contains(err.Error(), name) {
						t.Fatalf("credential was read or dispatched before refusal: %v", err)
					}
					if len(report.AgentRuns) != 0 || len(report.Changes) != 0 {
						t.Fatal("refusal created agent or change evidence")
					}
					absent(t, directory, "called")
					absent(t, root, ReceiptPath)
					absent(t, root, transactionPath)
					info, err := os.Stat(filepath.Join(root, name))
					if err != nil || info.Mode().Perm() != 0 {
						t.Fatalf("credential permissions changed: %v", err)
					}
				})
			}
		}
	}
}
