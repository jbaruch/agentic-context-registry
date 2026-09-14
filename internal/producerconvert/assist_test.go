package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func semanticFixture(t *testing.T) (string, Options, proposal) {
	t.Helper()
	root, opts := fixture(t)
	opts.Agent = "claude"
	name := "plugins/orbit/skills/check/check.sh"
	before := "#!/bin/sh\nset -eu\ntessl install upstream/orbit\nprintf 'orbit-ok\\n'\n"
	put(t, root, name, before, 0o751)
	put(t, root, "LICENSE", "License bytes must survive.\n", 0o644)
	next := "#!/bin/sh\nset -eu\nprintf 'orbit-ok\\n'\n"
	return root, opts, proposal{Edits: []proposedEdit{{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: next}}}
}
func TestSemanticProposalApplyPreservesModeSupportForeignAndRerun(t *testing.T) {
	root, opts, proposed := semanticFixture(t)
	before := treeAt(t, root)
	calls := 0
	provider := func(_ context.Context, selected, request string) (proposal, AgentRun, error) {
		calls++
		if selected != "claude" || strings.Contains(request, root) || strings.Contains(request, "foreign installed content") {
			t.Fatal("unexpected provider authority or context")
		}
		return proposed, AgentRun{Provider: selected, RequestDigest: digest([]byte(request))}, nil
	}
	plan, err := prepareWithProvider(context.Background(), opts, provider)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !matches(before, treeAt(t, root)) {
		t.Fatal("planning changed source or retried")
	}
	if _, err = plan.Apply(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(root, "plugins/orbit/skills/check/check.sh"))
	command.Env = []string{"PATH="}
	if output, err := command.CombinedOutput(); err != nil || string(output) != "orbit-ok\n" {
		t.Fatalf("native helper: %s %v", output, err)
	}
	info, err := os.Stat(command.Path)
	if err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("mode changed: %v %v", info, err)
	}
	if read(t, root, "plugins/orbit/skills/check/LICENSE.acr-"+strings.TrimPrefix(digest([]byte("License bytes must survive.\n")), "sha256:")[:12]) != "License bytes must survive.\n" || read(t, root, "LICENSE") != "License bytes must survive.\n" {
		t.Fatal("license loss")
	}
	if !strings.Contains(read(t, root, "plugins/orbit/skills/check/.acr-package.json"), "destination/nebula") {
		t.Fatal("portable metadata missing")
	}
	if read(t, root, "tessl.json") != `{"dependencies":{"foreign/tools":{"version":"9.8.7"}}}` {
		t.Fatal("foreign state changed")
	}
	current, err := prepareWithProvider(context.Background(), opts, provider)
	if err != nil || !current.Report.Current || calls != 1 {
		t.Fatalf("rerun %v %+v calls=%d", err, current.Report, calls)
	}
}

func TestSemanticRepairAndSourceRace(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		_, opts, p := semanticFixture(t)
		calls := 0
		plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _, request string) (proposal, AgentRun, error) {
			calls++
			if calls == 1 {
				return proposal{}, AgentRun{}, nil
			}
			if !strings.Contains(request, "Validation failure") {
				t.Fatal("missing feedback")
			}
			return p, AgentRun{}, nil
		})
		if err != nil || calls != 2 || len(plan.Report.AgentRuns) != 2 || plan.Report.AgentRuns[0].Failure != "" {
			t.Fatalf("repair: %v %d %+v", err, calls, plan.Report.AgentRuns)
		}
		assertAuditNote(t, plan.Report, 1, "proposal must contain 1..256 edits")
	})
	t.Run("race", func(t *testing.T) {
		root, opts, p := semanticFixture(t)
		calls := 0
		_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
			calls++
			put(t, root, "plugins/orbit/skills/check/data.txt", "concurrent edit\n", 0o640)
			return p, AgentRun{}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "source bytes, modes or paths changed") {
			t.Fatalf("race error: %v calls=%d", err, calls)
		}
		if read(t, root, "plugins/orbit/skills/check/data.txt") != "concurrent edit\n" {
			t.Fatal("overwrote concurrent edit")
		}
		absent(t, root, ReceiptPath)
	})
	t.Run("failure", func(t *testing.T) {
		root, opts, _ := semanticFixture(t)
		before := treeAt(t, root)
		_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
			return proposal{}, AgentRun{}, errors.New("credentials unavailable")
		})
		if err == nil || !strings.Contains(err.Error(), "credentials unavailable") || !matches(before, treeAt(t, root)) {
			t.Fatal("failure was swallowed or wrote input")
		}
	})
}

func TestLargeSemanticInputCombinesScopesBeforeOneTransaction(t *testing.T) {
	root, opts, p := semanticFixture(t)
	put(t, root, "plugins/orbit/reference.txt", strings.Repeat("Read-only supporting material.\n", 5000), 0o644)
	before := treeAt(t, root)
	var scopes []string
	plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _, request string) (proposal, AgentRun, error) {
		for _, scope := range []string{"runtime", "instructions", "delivery"} {
			if strings.Contains(request, `"scope":"`+scope+`"`) {
				scopes = append(scopes, scope)
				if scope == "runtime" {
					return p, AgentRun{}, nil
				}
				return proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, AgentRun{}, nil
			}
		}
		t.Fatal("large input was not scoped")
		return proposal{}, AgentRun{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, ",") != "runtime,instructions,delivery" {
		t.Fatalf("scopes=%v", scopes)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("scope calls changed input")
	}
	if len(plan.Report.AgentRuns) != 3 || plan.Report.AgentRuns[1].Scope != "instructions" {
		t.Fatal("scope evidence absent")
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticCanceledApplyDoesNotStartTransaction(t *testing.T) {
	root, opts, p := semanticFixture(t)
	before := treeAt(t, root)
	plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plan.ApplyContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled apply: %v", err)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("canceled apply wrote source")
	}
	absent(t, root, transactionPath)
}

func TestLargeUneditableSemanticSourceRefusesWithoutCallingProvider(t *testing.T) {
	root, opts := fixture(t)
	opts.Agent = "claude"
	if err := os.Remove(filepath.Join(root, ".github/workflows/publish.yml")); err != nil {
		t.Fatal(err)
	}
	put(t, root, "plugins/orbit/skills/inspect/SKILL.md", "# Inspect\nRun `skills/check/missing.sh`.\n", 0o644)
	put(t, root, "plugins/orbit/skills/check/SKILL.md", "# Check\n", 0o644)
	put(t, root, "plugins/orbit/README.md", strings.Repeat("Read-only material.\n", 8000), 0o644)
	before := treeAt(t, root)
	_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
		t.Fatal("called provider without editable inputs")
		return proposal{}, AgentRun{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "no owned editable") {
		t.Fatalf("wrong refusal: %v", err)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("uneditable source changed")
	}
}

func TestScopedRepairRetainsEarlierWorkAndRevalidatesCombined(t *testing.T) {
	for _, failingScope := range []string{"runtime", "delivery"} {
		t.Run(failingScope, func(t *testing.T) {
			root, opts, good := semanticFixture(t)
			put(t, root, "plugins/orbit/reference.txt", strings.Repeat("Read-only context.\n", 10000), 0o644)
			delivery := ".github/workflows/custom.yml"
			oldDelivery := "on: push\njobs:\n  policy:\n    steps:\n      - uses: tesslio/setup-tessl@v2\n      - run: echo preserved\n"
			put(t, root, delivery, oldDelivery, 0o644)
			fixedDelivery := strings.Replace(oldDelivery, "      - uses: tesslio/setup-tessl@v2\n", "", 1)
			before := treeAt(t, root)
			counts := map[string]int{}
			plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _, request string) (proposal, AgentRun, error) {
				scope := ""
				for _, candidate := range []string{"runtime", "instructions", "delivery"} {
					if strings.Contains(request, `"scope":"`+candidate+`"`) {
						scope = candidate
						break
					}
				}
				counts[scope]++
				switch scope {
				case "runtime":
					next := proposal{Edits: append([]proposedEdit(nil), good.Edits...)}
					if failingScope == scope && counts[scope] == 1 {
						next.Edits[0].Content = "#!/bin/sh\nif\n"
					}
					if counts[scope] == 2 {
						next.Edits[0].Content += "# revised-runtime-contract\n"
					}
					return next, AgentRun{}, nil
				case "instructions":
					if counts["runtime"] == 2 && !strings.Contains(request, "revised-runtime-contract") {
						t.Fatal("dependent scope retained stale runtime context")
					}
					return proposal{}, AgentRun{}, nil
				case "delivery":
					if counts["runtime"] == 2 && !strings.Contains(request, "revised-runtime-contract") {
						t.Fatal("delivery retained stale runtime context")
					}
					if failingScope == scope && counts[scope] == 1 {
						return proposal{}, AgentRun{}, nil
					}
					return proposal{Edits: []proposedEdit{{Path: delivery, BeforeDigest: digest([]byte(oldDelivery)), Action: "replace", Content: fixedDelivery}}}, AgentRun{}, nil
				default:
					t.Fatal("missing scope")
					return proposal{}, AgentRun{}, nil
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if failingScope == "runtime" {
				want = 2
			}
			if counts["runtime"] != want || counts["instructions"] != want || counts["delivery"] != 2 {
				t.Fatalf("retry counts: %v", counts)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("scope repair changed input")
			}
			if _, err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			if read(t, root, delivery) != fixedDelivery {
				t.Fatal("combined validation/application lost repaired scope")
			}
		})
	}
}

// checkCorrectionProposal exercises both requested modes with complete input
// inventories and verifies the actual applied bytes for successful proposals.
func checkCorrectionProposal(t *testing.T, dry bool, root string, opts Options, p proposal, accepted bool, reasons ...string) {
	t.Helper()
	checkStage := correctionStageCheck(t)
	defer checkStage()
	opts.DryRun = dry
	original := treeAt(t, root)
	calls := 0
	provider := func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil }
	plan, err := prepareWithProvider(context.Background(), opts, provider)
	if !matches(original, treeAt(t, root)) {
		t.Fatal("planning changed bytes, modes or paths")
	}
	absent(t, root, ReceiptPath)
	absent(t, root, transactionPath)
	if !accepted {
		if err == nil {
			t.Fatal("unsafe proposal accepted")
		}
		for _, reason := range reasons {
			if !strings.Contains(err.Error(), reason) {
				t.Fatalf("missing refusal %q: %v", reason, err)
			}
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("positive required %d calls", calls)
	}
	expected := plan.after
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	for name, state := range expected {
		info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if statErr != nil || uint32(info.Mode().Perm()) != state.Mode {
			t.Fatalf("applied mode differs: %s %v", name, statErr)
		}
		if state.Directory || state.Link != "" {
			continue
		}
		if read(t, root, name) != string(state.Content) {
			t.Fatalf("applied bytes differ: %s", name)
		}
	}
	for _, edit := range p.Edits {
		if edit.Action == "remove" {
			absent(t, root, edit.Path)
		} else if edit.Action == "replace" && !strings.HasSuffix(edit.Path, ".lock.yml") && read(t, root, edit.Path) != edit.Content {
			t.Fatalf("proposal bytes differ: %s", edit.Path)
		}
	}
	applied := treeAt(t, root)
	current, err := prepareWithProvider(context.Background(), opts, provider)
	if err != nil || !current.Report.Current || current.Report.Wrote || calls != 1 || !matches(applied, treeAt(t, root)) {
		t.Fatalf("rerun not inert: %v calls=%d", err, calls)
	}
}

func TestCorrection10CausalAudit(t *testing.T) {
	for _, failing := range []string{"runtime", "instructions", "delivery"} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dry=%v", failing, dry), func(t *testing.T) {
				root, opts, good := semanticFixture(t)
				opts.DryRun = dry
				put(t, root, "plugins/orbit/reference.txt", strings.Repeat("Read-only supporting material.\n", 5000), 0o644)
				paths := map[string]string{"runtime": good.Edits[0].Path, "instructions": "plugins/orbit/skills/inspect/SKILL.md", "delivery": ".github/workflows/publish.yml"}
				before := treeAt(t, root)
				checkStage := correctionStageCheck(t)
				defer checkStage()
				var originals []AgentRun
				plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, selected, request string) (proposal, AgentRun, error) {
					scope := auditRequestScope(t, request)
					run := auditFixtureRun(selected, request, len(originals)+1)
					run.Scope = scope
					originals = append(originals, run)
					p := proposal{}
					if scope == "runtime" {
						p = proposal{Edits: append([]proposedEdit(nil), good.Edits...)}
					}
					if scope == failing {
						p = proposal{Edits: []proposedEdit{{Path: paths[scope], BeforeDigest: "sha256:bad", Action: "replace", Content: read(t, root, paths[scope])}}}
					}
					return p, run, nil
				})
				if err == nil || !strings.Contains(err.Error(), paths[failing]) {
					t.Fatalf("missing stale digest refusal: %v", err)
				}
				wantRuns := map[string]int{"runtime": 9, "instructions": 7, "delivery": 5}[failing]
				if len(plan.Report.AgentRuns) != wantRuns {
					t.Fatalf("runs=%d want=%d", len(plan.Report.AgentRuns), wantRuns)
				}
				failures := 0
				for i, run := range plan.Report.AgentRuns {
					if run.Scope == failing {
						failures++
						if !strings.Contains(run.Failure, fmt.Sprintf("ACR combined validation attempt %d", failures)) || !strings.Contains(run.Failure, paths[failing]) {
							t.Errorf("wrong causal attribution run %d: %+v", i+1, run)
						}
					} else if run.Failure != "" {
						t.Errorf("innocent run %d blamed: %+v", i+1, run)
					}
					run.Failure = ""
					assertAuditRun(t, run, originals[i])
				}
				if failures != 3 {
					t.Fatalf("failures=%d", failures)
				}
				for attempt := 1; attempt <= 3; attempt++ {
					assertAuditNote(t, plan.Report, attempt, paths[failing])
				}
				if !matches(before, treeAt(t, root)) {
					t.Fatal("refusal changed input")
				}
				absent(t, root, ReceiptPath)
				absent(t, root, transactionPath)
			})
		}
	}
}

func auditRequestScope(t *testing.T, request string) string {
	t.Helper()
	for _, s := range []string{"runtime", "instructions", "delivery"} {
		if strings.Contains(request, `"scope":"`+s+`"`) {
			return s
		}
	}
	return ""
}
func auditFixtureRun(provider, request string, n int) AgentRun {
	return AgentRun{Provider: provider, RuntimeVersion: "fixture-runtime", Isolation: "fixture-isolation", Arguments: []string{"fixture", "--request", fmt.Sprint(n)}, RequestDigest: digest([]byte(fmt.Sprintf("fixture %d\n%s", n, request))), Stdout: fmt.Sprintf("stdout-%d", n), Stderr: fmt.Sprintf("stderr-%d", n), Warnings: []string{fmt.Sprintf("warning-%d", n)}}
}
func assertAuditRun(t *testing.T, got, want AgentRun) {
	t.Helper()
	a, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("native evidence changed: got=%s want=%s", a, b)
	}
}
func assertAuditNote(t *testing.T, report Report, attempt int, reason string) {
	t.Helper()
	prefix := fmt.Sprintf("ACR combined validation attempt %d ", attempt)
	count := 0
	for _, note := range report.Notes {
		if strings.HasPrefix(note, prefix) {
			count++
			if !strings.Contains(note, reason) || !strings.Contains(note, "proposal runs ") {
				t.Fatalf("incomplete attempt history: %s", note)
			}
		}
	}
	if count != 1 {
		t.Fatalf("attempt %d has %d complete records: %v", attempt, count, report.Notes)
	}
}

func TestCorrection10AuditHistory(t *testing.T) {
	for _, scenario := range []string{"runtime-repair", "delivery-repair", "unscoped-repair", "missing-runtime", "runtime-only", "unlocated", "unlocated-repair", "multi-error", "ambiguous", "native-only", "native-after-failure"} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dry=%v", scenario, dry), func(t *testing.T) {
				root, opts, good := semanticFixture(t)
				opts.DryRun = dry
				if scenario != "unscoped-repair" {
					put(t, root, "plugins/orbit/reference.txt", strings.Repeat("Read-only supporting material.\n", 5000), 0o644)
				}
				instruction := "plugins/orbit/skills/inspect/SKILL.md"
				delivery := ".github/workflows/custom.yml"
				oldDelivery := "on: push\njobs:\n  policy:\n    steps:\n      - uses: tesslio/setup-tessl@v2\n      - run: echo preserved\n"
				fixedDelivery := strings.Replace(oldDelivery, "      - uses: tesslio/setup-tessl@v2\n", "", 1)
				if scenario == "delivery-repair" || scenario == "multi-error" {
					put(t, root, delivery, oldDelivery, 0o644)
				}
				if scenario == "missing-runtime" {
					put(t, root, good.Edits[0].Path, good.Edits[0].Content, 0o751)
					body := "# Inspect\nRun `tessl install upstream/orbit`.\n"
					put(t, root, instruction, body, 0o644)
					good = proposal{Edits: []proposedEdit{{Path: instruction, BeforeDigest: digest([]byte(body)), Action: "replace", Content: "# Inspect\nRun `acr install github:destination/nebula`.\n"}}}
				}
				if scenario == "runtime-only" {
					put(t, root, instruction, "# Inspect\n", 0o644)
					if err := os.Remove(filepath.Join(root, ".github/workflows/publish.yml")); err != nil {
						t.Fatal(err)
					}
				}
				before := treeAt(t, root)
				checkStage := correctionStageCheck(t)
				defer checkStage()
				counts := map[string]int{}
				var originals []AgentRun
				var requests []string
				plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, selected, request string) (proposal, AgentRun, error) {
					scope := auditRequestScope(t, request)
					counts[scope]++
					requests = append(requests, request)
					run := auditFixtureRun(selected, request, len(originals)+1)
					run.Scope = scope
					if scenario == "native-only" || scenario == "native-after-failure" && len(originals) == 3 {
						run.Failure = "native execution failed"
						originals = append(originals, run)
						return proposal{}, run, errors.New(run.Failure)
					}
					originals = append(originals, run)
					p := proposal{}
					if scope == "runtime" || scope == "" || scenario == "missing-runtime" && scope == "instructions" {
						p = proposal{Edits: append([]proposedEdit(nil), good.Edits...)}
					}
					if scenario == "unlocated" || scenario == "unlocated-repair" && counts[scope] == 1 {
						return proposal{}, run, nil
					}
					if scenario == "delivery-repair" && scope == "delivery" {
						p = proposal{Edits: []proposedEdit{{Path: delivery, BeforeDigest: digest([]byte(oldDelivery)), Action: "replace", Content: fixedDelivery}}}
						if counts[scope] < 3 {
							p.Edits[0].BeforeDigest = "sha256:bad"
						}
					}
					if scenario == "ambiguous" && scope == "runtime" {
						p = proposal{Edits: []proposedEdit{{Path: good.Edits[0].Path, BeforeDigest: good.Edits[0].BeforeDigest, Action: "patch", Replacements: []replacement{{Old: instruction, New: "replacement", Count: 1}}}}}
					}
					if scenario == "multi-error" {
						if scope == "runtime" {
							p.Edits[0].Content = "#!/bin/sh\nif\n"
						}
						if scope == "delivery" {
							p = proposal{Edits: []proposedEdit{{Path: delivery, BeforeDigest: digest([]byte(oldDelivery)), Action: "patch", Replacements: []replacement{{Old: "tesslio", New: "other", Count: 2}}}}}
						}
					}
					if len(p.Edits) > 0 && scenario != "delivery-repair" && scenario != "multi-error" && scenario != "ambiguous" && scenario != "unlocated-repair" && counts[scope] == 1 {
						p.Edits[0].BeforeDigest = "sha256:bad"
					}
					if scenario == "runtime-repair" {
						if scope == "runtime" && counts[scope] == 2 {
							p.Edits[0].Content += "# revised-runtime-contract\n"
						}
						if scope != "runtime" && counts["runtime"] == 2 && !strings.Contains(request, "revised-runtime-contract") {
							t.Fatal("dependent request lost revised runtime context")
						}
					}
					return p, run, nil
				})
				wantRuns := map[string]int{"runtime-repair": 6, "delivery-repair": 5, "unscoped-repair": 2, "missing-runtime": 4, "runtime-only": 2, "unlocated": 9, "unlocated-repair": 6, "multi-error": 9, "ambiguous": 9, "native-only": 1, "native-after-failure": 4}[scenario]
				if len(originals) != wantRuns || len(plan.Report.AgentRuns) != wantRuns {
					t.Fatalf("request counts %v runs=%d want=%d error=%v", counts, len(plan.Report.AgentRuns), wantRuns, err)
				}
				for i, run := range plan.Report.AgentRuns {
					original := originals[i]
					if original.Failure != "" {
						assertAuditRun(t, run, original)
						continue
					}
					if (scenario == "unlocated" || scenario == "unlocated-repair" || scenario == "multi-error" || scenario == "ambiguous") && run.Failure != "" {
						t.Fatalf("unattributed failure blamed run %d: %s", i+1, run.Failure)
					}
					run.Failure = ""
					assertAuditRun(t, run, original)
				}
				if !matches(before, treeAt(t, root)) {
					t.Fatal("planning changed original bytes/modes/paths")
				}
				absent(t, root, ReceiptPath)
				absent(t, root, transactionPath)
				switch scenario {
				case "native-only":
					if err == nil || !strings.Contains(err.Error(), "native execution failed") {
						t.Fatalf("native failure lost: %v", err)
					}
					for _, note := range plan.Report.Notes {
						if strings.HasPrefix(note, "ACR combined validation attempt ") {
							t.Fatal("invented validation attempt")
						}
					}
					return
				case "native-after-failure":
					if err == nil || !strings.Contains(err.Error(), "native execution failed") {
						t.Fatalf("native failure lost: %v", err)
					}
					assertAuditNote(t, plan.Report, 1, "stale beforeDigest")
					if !strings.Contains(plan.Report.AgentRuns[0].Failure, "stale beforeDigest") {
						t.Fatal("prior validation attribution lost")
					}
					return
				case "unlocated", "multi-error", "ambiguous":
					if err == nil {
						t.Fatal("invalid combined proposal accepted")
					}
					for attempt := 1; attempt <= 3; attempt++ {
						if scenario == "unlocated" {
							assertAuditNote(t, plan.Report, attempt, "proposal must contain 1..256 edits")
						} else if scenario == "ambiguous" {
							assertAuditNote(t, plan.Report, attempt, "replacement match count")
						} else {
							assertAuditNote(t, plan.Report, attempt, "syntax check")
							assertAuditNote(t, plan.Report, attempt, "replacement match count")
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				lastAttempt := 2
				if scenario == "delivery-repair" {
					lastAttempt = 3
				}
				for attempt := 1; attempt < lastAttempt; attempt++ {
					reason := "stale beforeDigest"
					if scenario == "unlocated-repair" {
						reason = "proposal must contain 1..256 edits"
					}
					assertAuditNote(t, plan.Report, attempt, reason)
				}
				assertAuditNote(t, plan.Report, lastAttempt, "passed.")
				if scenario == "delivery-repair" {
					assertAuditNote(t, plan.Report, 1, "proposal runs [1 2 3]")
					assertAuditNote(t, plan.Report, 2, "proposal runs [1 2 4]")
					assertAuditNote(t, plan.Report, 3, "proposal runs [1 2 5]")
					if plan.Report.AgentRuns[0].Failure != "" || plan.Report.AgentRuns[1].Failure != "" || plan.Report.AgentRuns[4].Failure != "" {
						t.Fatal("cached or repaired proposal blamed")
					}
					for _, i := range []int{2, 3} {
						if !strings.Contains(plan.Report.AgentRuns[i].Failure, delivery) {
							t.Fatal("latest used delivery proposal not attributed")
						}
					}
					for _, request := range requests[3:] {
						if !strings.Contains(request, good.Edits[0].Content[:9]) {
							t.Fatal("cached runtime proposal lost from repair context")
						}
					}
				}
				if _, err := plan.Apply(); err != nil {
					t.Fatal(err)
				}
				for name, state := range plan.after {
					info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
					if err != nil || uint32(info.Mode().Perm()) != state.Mode {
						t.Fatalf("mode %s: %v", name, err)
					}
					if !state.Directory && state.Link == "" && read(t, root, name) != string(state.Content) {
						t.Fatalf("output bytes %s", name)
					}
				}
				applied := treeAt(t, root)
				current, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
					t.Fatal("rerun invoked provider")
					return proposal{}, AgentRun{}, nil
				})
				if err != nil || !current.Report.Current || current.Report.Wrote || !matches(applied, treeAt(t, root)) {
					t.Fatalf("rerun not inert: %v", err)
				}
			})
		}
	}
}

// Full physical inventories and preview-derived deltas extend the full11
// verifier oracles. Unlike producer snapshots they include consumer/Git state.
func correction12Inventory(t *testing.T, root string) tree {
	t.Helper()
	result := tree{}
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if full == root {
			return nil
		}
		name, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		state := fileState{Mode: uint32(info.Mode().Perm()), Directory: entry.IsDir()}
		if info.Mode()&fs.ModeSymlink != 0 {
			state.Link, err = os.Readlink(full)
		} else if info.Mode().IsRegular() {
			state.Content, err = os.ReadFile(full)
			state.Digest = digest(state.Content)
		}
		if err != nil {
			return err
		}
		result[filepath.ToSlash(name)] = state
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func correction12Unchanged(t *testing.T, root string, before tree) {
	t.Helper()
	if !reflect.DeepEqual(before, correction12Inventory(t, root)) {
		t.Fatal("complete bytes/modes/paths/links changed")
	}
}
func correction12Apply(t *testing.T, root string, opts Options, plan Plan, before tree, provider providerCall) {
	t.Helper()
	expected := tree{}
	for name, state := range before {
		expected[name] = state
	}
	for _, change := range plan.Report.Changes {
		if change.Operation == "remove" {
			delete(expected, change.Path)
		} else {
			expected[change.Path] = fileState{Content: []byte(change.After), Digest: digest([]byte(change.After)), Mode: change.AfterMode}
		}
	}
	applied, err := plan.Apply()
	if err != nil || !applied.Wrote {
		t.Fatalf("Apply: %v %+v", err, applied)
	}
	correction12Unchanged(t, root, expected)
	info, err := os.Stat(filepath.Join(root, ReceiptPath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt must be0600: %v", err)
	}
	current, err := prepareWithProvider(context.Background(), opts, provider)
	if err != nil || !current.Report.Current || current.Report.Wrote {
		t.Fatalf("rerun: %v %+v", err, current.Report)
	}
	correction12Unchanged(t, root, expected)
}
