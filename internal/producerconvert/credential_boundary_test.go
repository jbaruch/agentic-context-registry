package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const boundarySeed = "synthetic-seed-credential-0123456789"
const boundaryRotated = "synthetic-new-refresh-credential-012345"

func TestCodexCredentialBoundaryRuntime(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		t.Run(platform, func(t *testing.T) {
			for _, scenario := range []string{"seed_proposal", "rotate_proposal", "rotate_patch", "rotate_stream_success", "rotate_stream_failure", "auth_uninspectable", "cancel_cleanup", "raw_integrity", "clean_rotation"} {
				t.Run(scenario, func(t *testing.T) {
					variants := []string{"normal"}
					if scenario == "rotate_proposal" {
						variants = []string{"content", "path", "replacement", "policy"}
					}
					if scenario == "auth_uninspectable" {
						variants = []string{"missing", "corrupt", "directory"}
					}
					if scenario == "rotate_stream_failure" {
						variants = []string{"process", "tool", "truncated"}
					}
					if scenario == "raw_integrity" {
						variants = []string{"equal", "unequal"}
					}
					for _, variant := range variants {
						root, opts, proposed := semanticFixture(t)
						opts.Agent = "codex"
						if scenario == "rotate_patch" {
							name := proposed.Edits[0].Path
							old := read(t, root, name)
							put(t, root, name, "# synthetic-new-TARGET\n"+old, 0751)
							proposed.Edits[0] = proposedEdit{Path: name, BeforeDigest: digest([]byte("# synthetic-new-TARGET\n" + old)), Action: "patch", Replacements: []replacement{{Old: "TARGET", New: "refresh-credential-012345", Count: 1}, {Old: old, New: proposed.Edits[0].Content, Count: 1}}}
						}
						native := codexFixtureFor(t, proposed, platform)
						put(t, native.home, "auth.json", `{"tokens":{"refresh_token":"`+boundarySeed+`"}}`, 0600)
						seed := read(t, native.home, "auth.json")
						if scenario != "seed_proposal" && scenario != "raw_integrity" {
							codexSet(t, native, "rotate_auth", true)
							codexSet(t, native, "rotated", boundaryRotated)
						}
						switch scenario {
						case "seed_proposal":
							proposed.Edits[0].Content += "\n# " + boundarySeed + "\n"
						case "rotate_proposal":
							switch variant {
							case "content":
								proposed.Edits[0].Content += "\n# " + boundaryRotated + "\n"
							case "path":
								proposed.Edits[0].Path = boundaryRotated
							case "replacement":
								proposed.Edits[0].Action = "patch"
								proposed.Edits[0].Content = ""
								proposed.Edits[0].Replacements = []replacement{{Old: "x", New: boundaryRotated, Count: 1}}
							case "policy":
								proposed.PolicyChanges = []PolicyChange{{Path: proposed.Edits[0].Path, From: boundaryRotated, To: "removed"}}
							}
						case "auth_uninspectable":
							codexSet(t, native, "damage_auth", variant)
						case "rotate_stream_failure":
							codexSet(t, native, "behavior", variant)
						case "raw_integrity":
							proposed.Edits[0].Content += "\n# sk-example-not-a-secret\n"
						}
						encoded, e := json.Marshal(proposed)
						if e != nil {
							t.Fatal(e)
						}
						codexSet(t, native, "proposal", string(encoded))
						if scenario == "raw_integrity" && variant == "unequal" {
							codexSet(t, native, "file_proposal", strings.ReplaceAll(string(encoded), "sk-example-not-a-secret", "sk-[redacted]"))
						}
						ctx, cancel := context.WithCancel(context.Background())
						var listener net.Listener
						if scenario == "cancel_cleanup" {
							var e error
							listener, e = net.Listen("tcp", "127.0.0.1:0")
							if e != nil {
								t.Fatal(e)
							}
							codexSet(t, native, "behavior", "block")
							codexSet(t, native, "listener", listener.Addr().String())
							go func() {
								connection, e := listener.Accept()
								if e != nil {
									return
								}
								defer connection.Close()
								var b [5]byte
								_, e = connection.Read(b[:])
								if e == nil {
									cancel()
								}
							}()
						}
						before := treeAt(t, root)
						plan, err := prepareWithProvider(ctx, opts, func(ctx context.Context, _ string, request string) (proposal, AgentRun, error) {
							return runCodexWithRuntime(ctx, request, native)
						})
						cancel()
						if listener != nil {
							if e := listener.Close(); e != nil {
								t.Fatal(e)
							}
						}
						success := scenario == "clean_rotation" || scenario == "rotate_stream_success" || scenario == "raw_integrity" && variant == "equal"
						if (err == nil) != success {
							t.Fatalf("%s/%s success=%t error=%v", scenario, variant, success, err)
						}
						if !matches(before, treeAt(t, root)) {
							t.Fatal("prepare changed source")
						}
						assertCredentialReport(t, plan.Report, err)
						if len(plan.Report.AgentRuns) != 1 {
							t.Fatalf("credential failure was retried: %d runs", len(plan.Report.AgentRuns))
						}
						boundary := plan.Report.AgentRuns[0].CredentialBoundary
						if boundary == nil || !boundary.IsolatedHomeRemoved || !boundary.ReportSanitized {
							t.Fatal("missing finalization evidence")
						}
						if success {
							if !boundary.AuthInspected || !boundary.ProposalChecked {
								t.Fatal("successful run lacks actual checks")
							}
							if plan.Report.CredentialBoundary == nil || !plan.Report.CredentialBoundary.PlanChecked || plan.Report.CredentialBoundary.ApplicationChecked {
								t.Fatal("incorrect prepared boundary")
							}
							report, e := plan.Apply()
							if e != nil {
								t.Fatal(e)
							}
							if report.CredentialBoundary == nil || !report.CredentialBoundary.ApplicationChecked {
								t.Fatal("apply lacks boundary")
							}
							assertCredentialReport(t, report, e)
							current, e := Prepare(opts)
							if e != nil || !current.Report.Current {
								t.Fatalf("rerun: %v", e)
							}
						} else if _, e := os.Stat(filepath.Join(root, ReceiptPath)); !os.IsNotExist(e) {
							t.Fatal("failed proposal produced receipt")
						}
						if read(t, native.home, "auth.json") != seed {
							t.Fatal("seed changed")
						}
						if entries, e := os.ReadDir(native.homeBase); e != nil || len(entries) != 0 {
							t.Fatal("private home retained")
						}
					}
				})
			}
		})
	}
}

func assertCredentialReport(t *testing.T, report Report, err error) {
	t.Helper()
	data, e := json.Marshal(report)
	if e != nil {
		t.Fatal(e)
	}
	output := string(data) + FormatText(report)
	if err != nil {
		output += err.Error()
	}
	for _, secret := range []string{boundarySeed, boundaryRotated} {
		if strings.Contains(output, secret) {
			t.Fatal("credential reached public evidence")
		}
	}
}

func TestCodexCredentialGuardSurvivesScopesAndRepair(t *testing.T) {
	for _, scenario := range []string{"later-scope", "repair", "apply-recheck"} {
		t.Run(scenario, func(t *testing.T) {
			root, opts, good := semanticFixture(t)
			opts.Agent = "codex"
			if scenario == "later-scope" {
				put(t, root, "plugins/orbit/reference.txt", strings.Repeat("Supporting material.\n", 8000), 0644)
				good.Edits[0].Content += "# " + boundaryRotated + "\n"
			}
			before := treeAt(t, root)
			calls := 0
			plan, err := prepareWithProvider(context.Background(), opts, func(ctx context.Context, _ string, request string) (proposal, AgentRun, error) {
				calls++
				p := good
				if scenario == "later-scope" && calls > 1 {
					p = proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}
				}
				if scenario == "repair" && calls == 1 {
					p.Edits = append([]proposedEdit{}, p.Edits...)
					p.Edits[0].BeforeDigest = "sha256:stale"
				}
				native := codexFixture(t, p)
				put(t, native.home, "auth.json", `{"tokens":{"refresh_token":"`+boundarySeed+`"}}`, 0600)
				if calls > 1 || scenario == "apply-recheck" {
					codexSet(t, native, "rotate_auth", true)
					codexSet(t, native, "rotated", boundaryRotated)
				}
				return runCodexWithRuntime(ctx, request, native)
			})
			if scenario == "later-scope" {
				if err == nil {
					t.Fatal("later scope credential escaped earlier proposal")
				}
				assertCredentialReport(t, plan.Report, err)
				if !matches(before, treeAt(t, root)) {
					t.Fatal("scope failure changed source")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "repair" {
				if calls != 2 || plan.Report.AgentRuns[0].FailureKind != "semantic_validation" || plan.Report.AgentRuns[1].FailureKind != "" {
					t.Fatalf("repair classification: calls=%d runs=%+v", calls, plan.Report.AgentRuns)
				}
				// Caller edits to presentation do not affect the operational materialized plan.
				opts.Agent = ""
				if _, e := plan.Apply(); e != nil {
					t.Fatal(e)
				}
				current, e := Prepare(opts)
				if e != nil || !current.Report.Current {
					t.Fatalf("provider-free rerun failed: %v", e)
				}
			} else {
				// Simulate corruption of an internal materialized buffer after prepare.
				name := good.Edits[0].Path
				state := plan.after[name]
				state.Content = append(state.Content, []byte("# "+boundaryRotated+"\n")...)
				plan.after[name] = state
				report, e := plan.Apply()
				if e == nil {
					t.Fatal("application failed to recheck materialized output")
				}
				assertCredentialReport(t, report, e)
				if !matches(before, treeAt(t, root)) {
					t.Fatal("apply guard mutated source")
				}
			}
		})
	}
}

func TestSemanticFailureKindsRequireTrustedOrigins(t *testing.T) {
	semantic := semanticErrorf("file: ordinary validation")
	if !onlySemanticValidation(semantic) {
		t.Fatal("typed validation lost classification")
	}
	for _, failure := range []error{fmt.Errorf("file: ordinary validation"), errors.Join(semantic, os.ErrPermission), context.Canceled} {
		if onlySemanticValidation(failure) {
			t.Fatal("operational or untyped failure classified from text")
		}
	}
}

func TestOperationalValidationFailureDoesNotRetry(t *testing.T) {
	root, opts, proposed := semanticFixture(t)
	name := proposed.Edits[0].Path
	original := strings.Replace(read(t, root, name), "#!/bin/sh", "#!/usr/bin/env sh", 1)
	put(t, root, name, original, 0751)
	proposed.Edits[0].BeforeDigest = digest([]byte(original))
	proposed.Edits[0].Content = strings.Replace(proposed.Edits[0].Content, "#!/bin/sh", "#!/usr/bin/env sh", 1)
	before := treeAt(t, root)
	calls := 0
	plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, selected, request string) (proposal, AgentRun, error) {
		calls++
		t.Setenv("PATH", t.TempDir())
		return proposed, AgentRun{Provider: selected, RequestDigest: digest([]byte(request))}, nil
	})
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("operational failure retried or misclassified: calls=%d err=%v", calls, err)
	}
	for _, run := range plan.Report.AgentRuns {
		if run.FailureKind != "" {
			t.Fatal("operational failure marked semantic")
		}
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("operational failure changed source")
	}
}
