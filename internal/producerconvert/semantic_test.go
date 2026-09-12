package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
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
func TestSemanticProposalRejectsUntrustedEditsBeforeWrites(t *testing.T) {
	for _, name := range []string{"escape", "mode", "protected", "foreign", "stale", "syntax", "unexpected create", "delete test", "assertion loss", "missing test"} {
		t.Run(name, func(t *testing.T) {
			root, opts, p := semanticFixture(t)
			put(t, root, "tests/test_runtime.py", "def test_runtime():\n    assert 'tessl'\n", 0o644)
			before := treeAt(t, root)
			edit := &p.Edits[0]
			switch name {
			case "escape":
				edit.Path = "../escape"
			case "mode":
				edit.Action = "chmod"
			case "protected":
				edit.Path = "LICENSE"
				edit.BeforeDigest = digest([]byte("License bytes must survive.\n"))
			case "foreign":
				edit.Path = "tessl.json"
			case "stale":
				edit.BeforeDigest = "sha256:stale"
			case "syntax":
				edit.Content = "#!/bin/sh\nif\n"
			case "unexpected create":
				edit.Path = "tests/absent/new.py"
				edit.BeforeDigest = ""
				edit.Action = "create"
			case "delete test":
				edit.Path = "tests/test_runtime.py"
				edit.BeforeDigest = digest([]byte("def test_runtime():\n    assert 'tessl'\n"))
				edit.Action = "remove"
				edit.Content = ""
			case "assertion loss":
				edit.Path = "tests/test_runtime.py"
				edit.BeforeDigest = digest([]byte("def test_runtime():\n    assert 'tessl'\n"))
				edit.Content = "def test_runtime():\n    pass\n"
			case "missing test":
				edit.Path = "tests/test_runtime.py"
				edit.BeforeDigest = digest([]byte("def test_runtime():\n    assert 'tessl'\n"))
				edit.Content = "assert True\n"
			}
			calls := 0
			_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil })
			if err == nil || calls != 3 || !matches(before, treeAt(t, root)) {
				t.Fatalf("err=%v calls=%d sourceChanged=%t", err, calls, !matches(before, treeAt(t, root)))
			}
		})
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
func TestProposalStrictJSONAndEnvelope(t *testing.T) {
	for _, raw := range []string{`{"edits":[],"edits":[],"policyChanges":[]}`, `{"edits":[],"extra":1}`, `{"edits":`, `{} {}`, `{"edits":[{"mode":493}]}`} {
		var value proposal
		if err := strictJSON([]byte(raw), &value); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	good := `[{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[]},{"type":"result","subtype":"success","is_error":false,"structured_output":{"edits":[],"policyChanges":[]}}]`
	if _, err := claudeProposal([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Replace(good, "StructuredOutput", "Bash", 1), strings.Replace(good, `"mcp_servers":[]`, `"mcp_servers":[{}]`, 1), strings.Replace(good, `"is_error":false`, `"is_error":true`, 1), strings.Replace(good, `"subtype":"init"`, `"subtype":"other"`, 1), good[:len(good)-2]} {
		if _, err := claudeProposal([]byte(raw)); err == nil {
			t.Fatal("accepted unsafe/incomplete envelope")
		}
	}
}
func TestNativeProviderFailuresAreBoundedAndReadOnly(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	// A native executable fixture exercises real stdin/process/envelope handling.
	encoded, err := json.Marshal([]map[string]any{{"type": "system", "subtype": "init", "tools": []string{"StructuredOutput"}, "mcp_servers": []any{}}, {"type": "result", "subtype": "success", "structured_output": proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, directory, "claude", "#!/bin/sh\nprintf '%s' '"+string(encoded)+"'\n", 0o755)
	_, run, err := runProvider(context.Background(), "claude", "input text")
	if err != nil || run.Stdout == "" || run.RequestDigest != digest([]byte("input text")) {
		t.Fatalf("native fixture %v %+v", err, run)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := runProvider(ctx, "claude", "input"); err == nil {
		t.Fatal("ignored cancellation")
	}
	put(t, directory, "claude", "#!/bin/sh\nprintf 'credential unavailable' >&2\nexit 17\n", 0o755)
	if _, run, err := runProvider(context.Background(), "claude", "input"); err == nil || !strings.Contains(run.Stderr, "credential unavailable") {
		t.Fatal("lost process failure")
	}
	put(t, directory, "claude", "#!/bin/sh\n/usr/bin/head -c 12582913 /dev/zero\n", 0o755)
	if _, run, err := runProvider(context.Background(), "claude", "input"); err == nil || len(run.Stdout) > maxProviderBytes || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized native response: %v bytes=%d", err, len(run.Stdout))
	}
	if _, _, err := runProvider(context.Background(), "claude", strings.Repeat("x", maxRequestBytes+1)); err == nil {
		t.Fatal("accepted oversized input")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	capture := boundedCapture{limit: 8, cancel: cancel}
	if _, err := capture.Write([]byte("0123456789")); err == nil || capture.Len() != 8 || !capture.overflow || ctx.Err() == nil {
		t.Fatal("output was not bounded and canceled")
	}
}

func TestSemanticProtectsForeignConsumerEvidenceAndTestRegistration(t *testing.T) {
	t.Run("consumer settings", func(t *testing.T) {
		root, opts, p := semanticFixture(t)
		put(t, root, ".github/mcp.json", `{"mcpServers":{"foreign":{"command":"tessl","secret":"not-provider-input"}}}`, 0o644)
		put(t, root, ".gemini/commands/foreign.md", "tessl install foreign/tools; private-consumer-prompt\n", 0o644)
		before := treeAt(t, root)
		plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _, request string) (proposal, AgentRun, error) {
			if strings.Contains(request, "not-provider-input") || strings.Contains(request, "private-consumer-prompt") {
				t.Fatal("read foreign consumer input")
			}
			return p, AgentRun{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Apply(); err != nil {
			t.Fatal(err)
		}
		if read(t, root, ".github/mcp.json") != string(before[".github/mcp.json"].Content) || read(t, root, ".gemini/commands/foreign.md") != string(before[".gemini/commands/foreign.md"].Content) {
			t.Fatal("changed foreign consumer state")
		}
	})
	t.Run("comment is not a test", func(t *testing.T) {
		root, opts, p := semanticFixture(t)
		name := "tests/test_policy.py"
		old := "legacy_config = 'tessl.json'\ndef test_policy():\n    assert 1 == 1\ntest_policy()\n"
		put(t, root, name, old, 0o644)
		p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(old)), Action: "replace", Content: "# test_policy assert legacy\nassert True\n"})
		_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
		if err == nil || !strings.Contains(err.Error(), "original test function removed") {
			t.Fatalf("accepted commented-out test: %v", err)
		}
		absent(t, root, ReceiptPath)
	})
	t.Run("transaction rollback", func(t *testing.T) {
		root, opts, p := semanticFixture(t)
		before := treeAt(t, root)
		plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		_, err = plan.apply(transactionHooks{Before: func(phase, name string) error {
			if phase == "commit" {
				return errors.New("injected semantic transaction failure")
			}
			return nil
		}})
		if err == nil || !matches(before, treeAt(t, root)) {
			t.Fatal("semantic edits/support files were not rolled back")
		}
		absent(t, root, ReceiptPath)
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
func TestSemanticWorkflowRejectsPlaceholderAndMultipleDocuments(t *testing.T) {
	for _, body := range []string{"# Retired service\n", "hello\n", "on: push\njobs: {}\n", "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n---\nextra: document\n"} {
		if err := syntaxCheck(context.Background(), ".github/workflows/ci.yml", []byte(body)); err == nil {
			t.Fatalf("accepted invalid workflow %q", body)
		}
	}
}

func TestSemanticCreateCannotOverwriteCaseAliasedProtectedFile(t *testing.T) {
	root, opts, p := semanticFixture(t)
	original := "plugins/orbit/skills/check/UNRELATED.txt"
	target := "plugins/orbit/skills/check/unrelated.txt"
	put(t, root, original, "protected original bytes\n", 0o644)
	_, aliasErr := os.Lstat(filepath.Join(root, target))
	if aliasErr != nil && !errors.Is(aliasErr, os.ErrNotExist) {
		t.Fatal(aliasErr)
	}
	before := treeAt(t, root)
	p.Edits = append(p.Edits, proposedEdit{Path: target, Action: "create", Content: "new support data\n"})
	plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
	if aliasErr == nil {
		if err == nil || !strings.Contains(err.Error(), "staging path collision") {
			t.Fatalf("accepted case-alias overwrite: %v", err)
		}
		if !matches(before, treeAt(t, root)) {
			t.Fatal("aliased proposal changed source")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Apply(); err != nil {
			t.Fatal(err)
		}
		if read(t, root, original) != "protected original bytes\n" || read(t, root, target) != "new support data\n" {
			t.Fatal("distinct case-sensitive paths were conflated")
		}
	}
}

func TestNativeProviderReportsSessionLimit(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	events := `{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[]}
{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"Session limit; resets 17:00"}`
	put(t, directory, "claude", "#!/bin/sh\nprintf '%s' '"+events+"'\nexit 1\n", 0o755)
	_, run, err := runProvider(context.Background(), "claude", "input")
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "resets 17:00") || run.Failure == "" {
		t.Fatalf("lost actionable provider failure: %v %+v", err, run)
	}
	if run.Stdout != events {
		t.Fatal("provider evidence changed")
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

func TestSemanticRepositoryTestsMayPreserveForeignState(t *testing.T) {
	for _, name := range []string{"tests/test_state.py", "plugins/orbit/skills/check/state.py"} {
		t.Run(name, func(t *testing.T) {
			root, options, proposed := semanticFixture(t)
			body := "from pathlib import Path\nforeign = Path('tessl.json')\nassert foreign.name == 'tessl.json'\n"
			put(t, root, name, body, 0o644)
			before := treeAt(t, root)
			plan, err := prepareWithProvider(context.Background(), options, func(context.Context, string, string) (proposal, AgentRun, error) { return proposed, AgentRun{}, nil })
			if strings.HasPrefix(name, "tests/") {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := plan.Apply(); err != nil {
					t.Fatal(err)
				}
				if read(t, root, name) != body {
					t.Fatal("foreign-state assertions changed")
				}
			} else if err == nil || !matches(before, treeAt(t, root)) {
				t.Fatal("runtime state operation was accepted or wrote input")
			}
		})
	}
}

func TestSemanticValidationReportsIndependentScopeFailuresTogether(t *testing.T) {
	root, options, proposed := semanticFixture(t)
	proposed.Edits[0].Content = "#!/bin/sh\nif\n"
	name := ".github/workflows/inspect.yml"
	body := "on: push\njobs:\n  inspect:\n    steps:\n      - run: tessl install maker/policy\n"
	put(t, root, name, body, 0o644)
	proposed.Edits = append(proposed.Edits, proposedEdit{Path: name, Action: "patch", BeforeDigest: digest([]byte(body)), Replacements: []replacement{{Old: "tessl", New: "acr", Count: 2}}})
	before := treeAt(t, root)
	_, err := prepareWithProvider(context.Background(), options, func(context.Context, string, string) (proposal, AgentRun, error) { return proposed, AgentRun{}, nil })
	if err == nil || !strings.Contains(err.Error(), proposed.Edits[0].Path) || !strings.Contains(err.Error(), "replacement match count") || !matches(before, treeAt(t, root)) {
		t.Fatalf("incomplete diagnostics or mutation: %v", err)
	}
}

func TestSemanticValidationPreservesDirectoryModes(t *testing.T) {
	root, options, proposed := semanticFixture(t)
	name := "plugins/orbit/skills/check"
	if err := os.Chmod(filepath.Join(root, name), 0o777); err != nil {
		t.Fatal(err)
	}
	plan, err := prepareWithProvider(context.Background(), options, func(context.Context, string, string) (proposal, AgentRun, error) { return proposed, AgentRun{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if plan.after[name].Mode != 0o777 {
		t.Fatalf("validation changed directory mode: %o", plan.after[name].Mode)
	}
	if report, err := plan.Apply(); err != nil || !report.Wrote {
		t.Fatalf("apply unchanged directory: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, name))
	if err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("directory mode after apply: %v %v", info, err)
	}
}

const mixedRunWorkflow = "on: pull_request\njobs:\n  tests:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - name: Install policy and test\n        run: |\n          tessl install owner/policy\n          go test ./...\n"
const reviewInsideTesslJob = "on: pull_request\njobs:\n  review:\n    if: github.event.pull_request.head.repo.full_name == github.repository\n    env:\n      REVIEW_TOKEN: ${{ secrets.REVIEW_TOKEN }}\n    steps:\n      - uses: tesslio/setup-tessl@v2\n      - name: Run tests\n        run: python3 tests/check.py\n      - name: Independent review\n        run: review --required\n"

// weakenedWorkflowRefused drives one workflow replacement through the real
// proposal boundary and requires refusal with unchanged source and no receipt.
func weakenedWorkflowRefused(t *testing.T, name, before, after, reason string) {
	t.Helper()
	root, opts, p := semanticFixture(t)
	put(t, root, name, before, 0o644)
	original := treeAt(t, root)
	p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
	calls := 0
	_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil })
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("accepted weakened workflow: %v", err)
	}
	if calls != 3 || !matches(original, treeAt(t, root)) {
		t.Fatalf("calls=%d sourceChanged=%t", calls, !matches(original, treeAt(t, root)))
	}
	absent(t, root, ReceiptPath)
	absent(t, root, transactionPath)
}

// serviceOnlyRemovalApplied drives the legitimate setup-Tessl removal through
// the same boundary and requires acceptance and application.
func serviceOnlyRemovalApplied(t *testing.T, name, before string) {
	t.Helper()
	root, opts, p := semanticFixture(t)
	put(t, root, name, before, 0o644)
	fixed := strings.Replace(before, "      - uses: tesslio/setup-tessl@v2\n", "", 1)
	p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: fixed})
	plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if read(t, root, name) != fixed {
		t.Fatal("service-only removal was not applied as proposed")
	}
}

func TestSemanticProposalRefusesMixedRunStepBeforeWrites(t *testing.T) {
	const step = "      - name: Install policy and test\n        run: |\n          tessl install owner/policy\n          go test ./...\n"
	t.Run("deleted", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/mixed.yml", mixedRunWorkflow, strings.Replace(mixedRunWorkflow, step, "", 1), "must retain its logic")
	})
	t.Run("rewritten", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/mixed.yml", mixedRunWorkflow, strings.Replace(mixedRunWorkflow, "          tessl install owner/policy\n", "          exit 0\n", 1), "must retain its logic")
	})
	t.Run("retained", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/mixed.yml", mixedRunWorkflow, mixedRunWorkflow, "candidate conversion")
	})
	t.Run("service-only removal", func(t *testing.T) {
		serviceOnlyRemovalApplied(t, ".github/workflows/review.yml", reviewInsideTesslJob)
	})
}

func TestSemanticProposalRefusesJobContinueOnErrorBeforeWrites(t *testing.T) {
	explicit := strings.Replace(reviewInsideTesslJob, "  review:\n", "  review:\n    continue-on-error: true\n", 1)
	t.Run("introduced", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/review.yml", reviewInsideTesslJob, strings.Replace(explicit, "      - uses: tesslio/setup-tessl@v2\n", "", 1), "continue-on-error")
	})
	t.Run("identical explicit", func(t *testing.T) {
		serviceOnlyRemovalApplied(t, ".github/workflows/review.yml", explicit)
	})
}

// pythonTestCheck mirrors the exact embedded invocation in validateProposal.
func pythonTestCheck(t *testing.T, stdin string) (string, error) {
	t.Helper()
	command := exec.Command("python3", "-I", "-S", "-c", pythonTestChecks)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	command.Stdin = strings.NewReader(stdin)
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestPythonTestCheckerExecutionContract(t *testing.T) {
	const original = "def fail(message):\n    raise AssertionError(message)\n\ndef test_ok():\n    assert 1 == 1\n    if False:\n        fail('never')\n\ntest_ok()\n"
	request := func(after string) string {
		data, err := json.Marshal(map[string]string{"before": original, "after": after})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if output, err := pythonTestCheck(t, request(original)); err != nil || output != "" {
		t.Fatalf("rejected unchanged tests: %v %s", err, output)
	}
	// Parse only: a proposal that would exit or raise if executed still passes.
	inert := "import sys\nsys.exit(99)\nraise RuntimeError('EXECUTED_PROPOSAL')\n" + original
	if output, err := pythonTestCheck(t, request(inert)); err != nil || output != "" {
		t.Fatalf("executed or rejected the proposed program: %v %s", err, output)
	}
	for name, tc := range map[string]struct{ stdin, reason string }{
		"removed test":          {request("def fail(message):\n    raise AssertionError(message)\n"), "original test function removed: test_ok"},
		"assertion loss":        {request("def fail(message):\n    raise AssertionError(message)\n\ndef test_ok():\n    pass\n\ntest_ok()\n"), "original assertion/failure checks removed from test_ok"},
		"changed collector":     {request(strings.Replace(original, "raise AssertionError(message)", "print(message)", 1)), "test failure collector must retain its behavior"},
		"removed invocation":    {request(strings.TrimSuffix(original, "test_ok()\n")), "test invocation/registration removed: test_ok"},
		"invalid proposed code": {request("def test_ok(:\n"), "SyntaxError"},
		"malformed request":     {`{"before": "def test_ok(): pass"`, "JSONDecodeError"},
		"missing field":         {`{"before": "def test_ok(): pass"}`, "KeyError"},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := pythonTestCheck(t, tc.stdin)
			if err == nil || !strings.Contains(output, tc.reason) {
				t.Fatalf("err=%v output=%s", err, output)
			}
		})
	}
}

func TestPythonTestCheckerIsImportSafe(t *testing.T) {
	directory := t.TempDir()
	put(t, directory, "acr_check_tests.py", pythonTestChecks, 0o644)
	const probe = `import ast, importlib.util, io, sys
sys.dont_write_bytecode = True
class Unreadable(io.TextIOBase):
    def readable(self):
        return True
    def read(self, size=-1):
        raise RuntimeError('IMPORT_READ_STDIN')
    def readline(self, size=-1):
        raise RuntimeError('IMPORT_READ_STDIN')
sys.stdin = Unreadable()
captured = io.StringIO()
sys.stdout = sys.stderr = captured
spec = importlib.util.spec_from_file_location('acr_check_tests', sys.argv[1])
module = importlib.util.module_from_spec(spec)
try:
    spec.loader.exec_module(module)
finally:
    sys.stdout, sys.stderr = sys.__stdout__, sys.__stderr__
if captured.getvalue():
    raise SystemExit('import wrote output: ' + captured.getvalue())
tree = ast.parse("def test_a():\n    assert 1\n    self.assertTrue(1)\n    fail('x')\ntest_a()\n")
if set(module.functions(tree)) != {'test_a'} or module.assertions(module.functions(tree)['test_a']) != 3:
    raise SystemExit('helpers unusable after import')
kept = "def test_a():\n    assert 1\ntest_a()\n"
module.check(kept, kept)
try:
    module.check(kept, "def test_a():\n    pass\ntest_a()\n")
except ValueError as error:
    if 'assertion' not in str(error):
        raise
else:
    raise SystemExit('helper accepted assertion loss')
print('IMPORT_OK')
`
	command := exec.Command("python3", "-I", "-S", "-B", "-c", probe, filepath.Join(directory, "acr_check_tests.py"))
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "IMPORT_OK" {
		t.Fatalf("import probe: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(directory, "__pycache__")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import wrote bytecode: %v", err)
	}
}

func TestSemanticProposalPreservesAllWorkflowPolicy(t *testing.T) {
	const setup = "      - uses: tesslio/setup-tessl@v2\n"
	for _, level := range []string{"job", "workflow"} {
		for _, field := range []struct{ name, before, after string }{
			{"defaults", "defaults: {run: {shell: 'bash -e {0}'}}", "defaults: {run: {shell: 'bash {0}'}}"},
			{"runs-on", "runs-on: ubuntu-latest", "runs-on: self-hosted"},
			{"env", "env: {PYTHONOPTIMIZE: '0'}", "env: {PYTHONOPTIMIZE: '1'}"},
			{"unknown", "x-policy: {required: true}", "x-policy: {required: false}"},
			{"env expression", "env: '${{ inputs.config }}'", "env: '${{ inputs.other }}'"},
		} {
			for _, change := range []string{"introduced", "changed", "removed", "identical"} {
				t.Run(level+"/"+field.name+"/"+change, func(t *testing.T) {
					workflow := func(setting string) string {
						head, body := "on: pull_request\n", "  review:\n"
						if setting != "" {
							if level == "workflow" {
								head += setting + "\n"
							} else {
								body += "    " + setting + "\n"
							}
						}
						return head + "jobs:\n" + body + "    steps:\n" + setup + "      - run: python3 tests/check.py\n"
					}
					oldSetting, newSetting := field.before, field.after
					switch change {
					case "introduced":
						oldSetting = ""
					case "removed":
						newSetting = ""
					case "identical":
						newSetting = oldSetting
					}
					before := workflow(oldSetting)
					if change == "identical" {
						serviceOnlyRemovalApplied(t, ".github/workflows/policy.yml", before)
						return
					}
					after := strings.Replace(workflow(newSetting), setup, "", 1)
					weakenedWorkflowRefused(t, ".github/workflows/policy.yml", before, after, strings.Split(field.before, ":")[0])
				})
			}
		}
	}
	// Existing entries do not authorize additional values, even those mentioning Tessl.
	for _, added := range []string{"PYTHONOPTIMIZE: '1'", "TESSL_NEW: injected", "UNRELATED: tessl install owner/policy"} {
		t.Run("added env/"+added, func(t *testing.T) {
			after := strings.Replace(reviewInsideTesslJob, setup, "", 1)
			after = strings.Replace(after, "    env:\n", "    env:\n      "+added+"\n", 1)
			weakenedWorkflowRefused(t, ".github/workflows/policy.yml", reviewInsideTesslJob, after, "environment")
		})
	}
}

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

func TestClaudeReportOmitsRawRequest(t *testing.T) {
	const request = "PRIVATE_SOURCE_REQUEST_SENTINEL"
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	for _, failure := range []bool{false, true} {
		label := "success"
		if failure {
			label = "failure"
		}
		t.Run(label, func(t *testing.T) {
			script := "#!/bin/sh\nprintf '%s' '{\"type\":\"system\",\"subtype\":\"init\",\"tools\":[],\"mcp_servers\":[]}\n{\"type\":\"result\",\"subtype\":\"success\",\"structured_output\":{\"edits\":[],\"policyChanges\":[]}}'\n"
			if failure {
				script = "#!/bin/sh\nprintf 'provider unavailable' >&2\nexit 7\n"
			}
			put(t, directory, "claude", script, 0o755)
			_, run, err := runProvider(context.Background(), "claude", request)
			if (err != nil) != failure {
				t.Fatalf("provider result: %v", err)
			}
			assertDigestOnlyRequest(t, run, request)
		})
	}
}

func assertDigestOnlyRequest(t *testing.T, run AgentRun, request string) {
	t.Helper()
	data, err := json.Marshal(Report{AgentRuns: []AgentRun{run}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"request":`) || strings.Contains(string(data), request) {
		t.Fatal("report exposed raw request")
	}
	if run.RequestDigest != digest([]byte(request)) || !strings.Contains(string(data), run.RequestDigest) {
		t.Fatal("missing or wrong request digest")
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

func TestCorrectionWorkflowOccurrences(t *testing.T) {
	const prefix = "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n"
	const setup = "      - uses: tesslio/setup-tessl@v2\n"
	const check = "      - run: test -e marker\n"
	const mutate = "      - run: touch marker\n"
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"delete-first", "delete-last", "reorder", "keep", "insert"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				steps := check + mutate + check
				next := steps
				switch kind {
				case "delete-first":
					next = mutate + check
				case "delete-last":
					next = check + mutate
				case "reorder":
					next = mutate + check + check
				case "insert":
					next = "      - run: echo preparing\n" + steps
				}
				name := ".github/workflows/occurrences.yml"
				before, after := prefix+setup+steps, prefix+next
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
				checkCorrectionProposal(t, dry, root, opts, p, kind == "keep" || kind == "insert")
			})
		}
	}
	// Execute the observable failure-to-success counterexamples independently of
	// proposal validation. Separate working directories keep marker state local.
	for _, c := range []struct {
		name, script string
		pass         bool
	}{
		{"original", "test -e marker\ntouch marker\ntest -e marker", false},
		{"delete-first", "touch marker\ntest -e marker", true},
		{"reordered", "touch marker\ntest -e marker\ntest -e marker", true},
		{"service-only-removal", "test -e marker\ntouch marker\ntest -e marker", false},
	} {
		t.Run("execution/"+c.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-e", "-c", c.script)
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.pass {
				t.Fatalf("execution: %s %v", out, err)
			}
		})
	}
}

func TestCorrectionRetiredJobReferences(t *testing.T) {
	const retired = "  scoring:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: tesslio/patch-version-publish@v1\n"
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, fields string
			accepted     bool
		}{
			{"scalar", "    needs: scoring\n", false}, {"list", "    needs: [scoring, unrelated]\n", false},
			{"dot", "    if: needs.scoring.result == 'success'\n", false},
			{"bracket", "    if: ${{ needs['scoring'].result == 'success' }}\n", false},
			{"dynamic", "    if: ${{ needs[matrix.producer].result }}\n", false},
			{"whole-context", "    env:\n      RESULTS: ${{ toJSON(needs) }}\n", false},
			{"unrelated", "    needs: unrelated\n    if: needs.unrelated.result == 'success'\n", true},
			{"reusable-output", "    env:\n      VERSION: ${{ jobs.scoring.outputs.version }}\n", false},
			{"none", "", true},
		} {
			t.Run(fmt.Sprintf("%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				survivor := "  check:\n    runs-on: ubuntu-latest\n" + c.fields + "    steps:\n      - run: echo independent\n"
				before := "on: push\njobs:\n" + retired + survivor
				name := ".github/workflows/dependencies.yml"
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: "on: push\njobs:\n" + survivor})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrectionRetiredStepReferences(t *testing.T) {
	const prefix = "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n"
	const setup = "      - uses: tesslio/setup-tessl@v2\n        id: setup\n"
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, fields, consumer, replacement string
			accepted                            bool
		}{
			{"dot", "", "      - run: echo '${{ steps.setup.outputs.version }}'\n", "", false},
			{"bracket", "", "      - run: echo \"${{ steps['setup'].outputs.version }}\"\n", "", false},
			{"dynamic", "", "      - run: echo '${{ steps[matrix.producer].outputs.version }}'\n", "", false},
			{"whole-context", "", "      - run: echo '${{ toJSON(steps) }}'\n", "", false},
			{"outputs", "    outputs:\n      version: ${{ steps.setup.outputs.version }}\n", "      - run: echo check\n", "", false},
			{"id-reuse", "", "      - run: echo '${{ steps.setup.outputs.version }}'\n", "      - id: setup\n        run: echo replacement\n", false},
			{"unrelated", "", "      - run: echo '${{ steps.other.outputs.version }}'\n", "", true},
			{"unreferenced", "", "      - run: echo check\n", "", true},
			{"quoted-context", "", "      - run: echo \"${{ 'steps.setup.outputs.version' }}\"\n", "", true},
			{"other-job", "", "      - run: echo check\n  elsewhere:\n    outputs:\n      version: ${{ steps.setup.outputs.version }}\n    steps:\n      - id: setup\n        run: echo local\n", "", true},
		} {
			t.Run(fmt.Sprintf("%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				before := prefix + c.fields + "    steps:\n" + setup + c.consumer
				after := prefix + c.fields + "    steps:\n" + c.replacement + c.consumer
				name := ".github/workflows/outputs.yml"
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrectionActionIdentity(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, action := range []string{"custom/setup-tessl@v2", "custom/patch-version-publish@v1", "other/policy/.github/actions/skill-review@v1", "jbaruch/coding-policy/skill-review@v1", "./tesslio/setup-tessl@v2", "tesslio/setup-tessl@", "tesslio/setup-tessl@v2", "tesslio/setup-tessl@0123456789012345678901234567890123456789", "jbaruch/coding-policy/.github/actions/skill-review@v1"} {
			for _, keep := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/keep=%t/dry=%t", action, keep, dry), func(t *testing.T) {
					service := action == "tesslio/setup-tessl@v2" || action == "tesslio/setup-tessl@0123456789012345678901234567890123456789" || action == "jbaruch/coding-policy/.github/actions/skill-review@v1"
					root, opts, p := semanticFixture(t)
					prefix := "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n"
					step := "      - uses: " + action + "\n"
					tail := "      - run: echo independent\n"
					before := prefix + step + tail
					after := prefix + tail
					if keep {
						after = before
					}
					name := ".github/workflows/actions.yml"
					put(t, root, name, before, 0o640)
					p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
					p.PolicyChanges = append(p.PolicyChanges, PolicyChange{Path: name, From: "Retire paid service when present", To: "Keep independent check; no replacement score"})
					// Unrelated actions remain read-only when the file has no service operation.
					if keep && !service {
						p.Edits = p.Edits[:len(p.Edits)-1]
						p.PolicyChanges = nil
					}
					checkCorrectionProposal(t, dry, root, opts, p, (!keep && service) || (keep && !service))
				})
			}
		}
	}
}

func TestCorrectionHistoricalContent(t *testing.T) {
	const url = "https://github.com/tessl-labs/original/blob/main/README.md"
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, path, before, after, action string
			accepted                          bool
		}{
			{"codeowners-remove", ".github/CODEOWNERS", "# History " + url + "\n* @independent-team\n", "", "remove", false},
			{"historical-replace", ".github/ISSUE_TEMPLATE/history.md", "History " + url + "\n", "Nothing here\n", "replace", false},
			{"mixed-nonworkflow-remove", ".github/config.txt", "tessl install owner/policy\nretain independent policy\n", "", "remove", false},
			{"repeated-url-delete", "plugins/orbit/skills/check/history.sh", "#!/bin/sh\n# Attribution " + url + "\n# Source " + url + "\ntessl install owner/policy\n", "#!/bin/sh\n# Attribution " + url + "\necho migrated\n", "replace", false},
			{"url-rewrite", "plugins/orbit/skills/check/history.sh", "#!/bin/sh\n# Attribution " + url + "\ntessl install owner/policy\n", "#!/bin/sh\n# Attribution https://github.com/other/project\necho migrated\n", "replace", false},
			{"url-preserved", "plugins/orbit/skills/check/history.sh", "#!/bin/sh\n# Attribution " + url + "\n# Source " + url + "\ntessl install owner/policy\n", "#!/bin/sh\n# Attribution " + url + "\n# Source " + url + "\necho migrated\n", "replace", true},
		} {
			t.Run(fmt.Sprintf("%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				put(t, root, c.path, c.before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: c.path, BeforeDigest: digest([]byte(c.before)), Action: c.action, Content: c.after})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrectionHistoricalDetection(t *testing.T) {
	const history = "https://github.com/tessl-labs/original/blob/main/.tessl-plugin/plugin.json"
	for _, agent := range []string{"", "claude"} {
		for _, executable := range []bool{false, true} {
			t.Run(fmt.Sprintf("agent=%s/operation=%t", agent, executable), func(t *testing.T) {
				root, opts := fixture(t)
				opts.Agent = agent
				files := map[string]string{
					".github/ISSUE_TEMPLATE/history.md":     "# Research\nSee " + history + "\n",
					".github/workflows/history.yml":         "name: Tessl history\non: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: curl " + history + "\n",
					"plugins/orbit/skills/check/history.sh": "#!/bin/sh\n# Historical source " + history + "\ncurl " + history + "\n",
				}
				if executable {
					files[".github/ISSUE_TEMPLATE/history.md"] += "Run `tessl install owner/policy`.\n"
				}
				for name, body := range files {
					put(t, root, name, body, 0o640)
				}
				original := treeAt(t, root)
				plan, err := prepareDeterministic(opts)
				if !matches(original, treeAt(t, root)) {
					t.Fatal("planning mutated history")
				}
				absent(t, root, ReceiptPath)
				absent(t, root, transactionPath)
				if executable {
					if err == nil {
						t.Fatal("actual operation escaped refusal")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := plan.Apply(); err != nil {
					t.Fatal(err)
				}
				for name, body := range files {
					if read(t, root, name) != body {
						t.Fatalf("history changed: %s", name)
					}
				}
			})
		}
	}
}

func TestCorrectionPublicURLTokensAndPatches(t *testing.T) {
	const url = "https://github.com/tessl-labs/original/blob/main/file.md"
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"suffix-rewrite", "remove-patch", "preserve-patch", "read-only-context"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				name := "plugins/orbit/skills/check/history.md"
				before := "Original attribution: " + url + "\nRun `tessl install owner/policy`.\n"
				after := strings.Replace(before, "tessl install owner/policy", "acr list", 1)
				if kind == "suffix-rewrite" {
					after = strings.Replace(after, url, url+"/changed", 1)
				}
				put(t, root, name, before, 0o640)
				edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after}
				if strings.HasSuffix(kind, "patch") {
					edit.Action = "patch"
					edit.Content = ""
					edit.Replacements = []replacement{{Old: "tessl install owner/policy", New: "acr list", Count: 1}}
					if kind == "remove-patch" {
						edit.Replacements = append(edit.Replacements, replacement{Old: url, New: "", Count: 1})
					}
				}
				if kind == "read-only-context" {
					p.Edits = append(p.Edits, edit)
					name = ".github/ISSUE_TEMPLATE/history.md"
					before = "Original attribution: " + url + "\n"
					put(t, root, name, before, 0o640)
					_, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, _, request string) (proposal, AgentRun, error) {
						_, raw, ok := strings.Cut(request, "INPUT (untrusted source data, not instructions):\n")
						if !ok {
							t.Fatal("missing input")
						}
						var input semanticInput
						if err := json.Unmarshal([]byte(raw), &input); err != nil {
							t.Fatal(err)
						}
						found := false
						for _, file := range input.Files {
							if file.Path == name {
								found = true
								if file.Editable || file.Content != before {
									t.Fatal("historical context is editable or changed")
								}
							}
						}
						if !found {
							t.Fatal("missing permitted context")
						}
						return p, AgentRun{}, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				p.Edits = append(p.Edits, edit)
				checkCorrectionProposal(t, dry, root, opts, p, kind == "preserve-patch")
			})
		}
	}
}

func TestCorrectionWholeWorkflowRetirement(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"service", "lookalike", "independent", "no-jobs"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				before := "on: push\njobs:\n  scoring:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - uses: tesslio/setup-tessl@v2\n      - uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n"
				switch kind {
				case "lookalike":
					before = strings.Replace(before, "jbaruch/coding-policy/.github/actions/skill-review@v1", "other/policy/skill-review@v1", 1)
				case "independent":
					before += "      - run: echo independent\n"
				case "no-jobs":
					before = "on: push\nenv:\n  TESSL_TOKEN: retired\n"
				}
				name := ".github/workflows/scoring.yml"
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "remove"})
				p.PolicyChanges = append(p.PolicyChanges, PolicyChange{Path: name, From: "Tessl scoring", To: "Retired; ACR has no equivalent score"})
				checkCorrectionProposal(t, dry, root, opts, p, kind == "service")
			})
		}
	}
}

func correctionStageCheck(t *testing.T) func() {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	return func() {
		t.Helper()
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("leaked private validation stage: %v", entries)
		}
	}
}

func TestCorrection9QuotedRetirement(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, expression string
			accepted         bool
		}{
			{"plain", "${{ steps.setup.outcome == 'success' }}", false},
			{"quoted-end", "${{ '}}' != '' && steps.setup.outcome == 'success' }}", false},
			{"bracket", "${{ '}}' != '' && steps['setup'].outcome == 'success' }}", false},
			{"doubled-quote", "${{ 'it''s }}' != '' && steps['setup'].outcome == 'success' }}", false},
			{"escaped-around-end", "${{ '''}}''' != '' && steps.setup.outcome }}", false},
			{"multiple", "${{ 'independent }}' }} ${{ '}}' && steps.setup.outcome }}", false},
			{"multiline", "${{ '}}' != '' &&\nsteps.setup.outcome == 'success' }}", false},
			{"incomplete", "${{ '}}' && steps.setup.outcome", false},
			{"nested-opening", "${{ ${{ steps.setup.outcome }}", false},
			{"literal-only", "${{ 'steps.setup.outcome }}' != '' }}", true},
			{"escaped-literal-only", "${{ 'it''s steps.setup }}' != '' }}", true},
			{"unrelated", "${{ '}}' != '' && steps.other.outcome == 'success' }}", true},
			{"unreferenced", "${{ '}}' }} ${{ 'independent' }}", true},
		} {
			for _, location := range []string{"step-if", "job-output", "other-job"} {
				t.Run(fmt.Sprintf("%s/%s/dry=%t", c.name, location, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					const name = ".github/workflows/quoted.yml"
					const setup = "      - uses: tesslio/setup-tessl@v2\n        id: setup\n"
					expression := strings.ReplaceAll(c.expression, "\n", "\n          ")
					fields, consumer := "", "      - run: echo required\n"
					if location == "step-if" {
						consumer = "      - if: >-\n          " + expression + "\n        run: exit 1\n"
					}
					if location == "job-output" {
						fields = "    outputs:\n      result: >-\n          " + expression + "\n"
					}
					if location == "other-job" {
						consumer += "  elsewhere:\n    runs-on: ubuntu-latest\n    outputs:\n      result: >-\n          " + expression + "\n    steps:\n      - id: setup\n        run: echo other\n"
					}
					before := "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n" + fields + "    steps:\n" + setup + consumer
					after := strings.Replace(before, setup, "", 1)
					put(t, root, name, before, 0o640)
					p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
					checkCorrectionProposal(t, dry, root, opts, p, c.accepted || location == "other-job")
				})
			}
		}
		for _, c := range []struct {
			name, expression string
			accepted         bool
		}{
			{"jobs-dot", "${{ '}}' && jobs.scoring.outputs.version }}", false},
			{"jobs-bracket", "${{ 'it''s }}' && jobs['scoring'].outputs.version }}", false},
			{"needs-dot", "${{ '}}' && needs.scoring.result }}", false},
			{"needs-bracket", "${{ '}}' && needs['scoring'].result }}", false},
			{"multiple", "${{ 'independent' }} ${{ '}}' && jobs.scoring.outputs.version }}", false},
			{"literal", "${{ 'jobs.scoring.outputs.version }}' }}", true},
			{"unrelated", "${{ '}}' && jobs.other.outputs.version }}", true},
		} {
			t.Run(fmt.Sprintf("reusable/%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				const name = ".github/workflows/quoted-job.yml"
				head := "on:\n  workflow_call:\n    outputs:\n      version:\n        value: " + c.expression + "\njobs:\n"
				retired := "  scoring:\n    runs-on: ubuntu-latest\n    outputs:\n      version: released\n    steps:\n      - uses: tesslio/patch-version-publish@v1\n"
				survivor := "  independent:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo required\n"
				before, after := head+retired+survivor, head+survivor
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrection9ResidualPublisher(t *testing.T) {
	const name = ".github/workflows/publish.yml"
	const setup = "      - uses: tesslio/setup-tessl@v2\n"
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"single-install", "closed-install", "unchanged-proposal", "supported-proposal", "single-install-removal", "historical", "lookalike", "independent", "standalone", "multiple"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts := fixture(t)
				opts.DryRun = dry
				operations := setup + "      - run: tessl install upstream/orbit\n"
				if kind == "closed-install" || kind == "supported-proposal" {
					operations = setup + serviceInstallStep
				}
				if kind == "historical" {
					operations = "      - run: echo https://github.com/tessl-labs/history\n      # Tessl history\n"
				}
				if kind == "lookalike" {
					operations = "      - uses: other/setup-tessl@v2\n"
				}
				if kind == "independent" {
					operations = ""
				}
				survivor := "  verify:\n    runs-on: ubuntu-latest\n    steps:\n" + operations + "      - run: ./tests/run.sh\n"
				if kind == "standalone" {
					survivor = ""
				}
				before := fixturePublisher + survivor
				if kind == "multiple" {
					before += strings.Replace(fixturePublisher[strings.Index(fixturePublisher, "  publish:"):], "  publish:", "  second:", 1)
				}
				put(t, root, name, before, 0o640)
				original := treeAt(t, root)
				checkStage := correctionStageCheck(t)
				defer checkStage()
				calls := 0
				provider := func(context.Context, string, string) (proposal, AgentRun, error) {
					calls++
					next := before
					if kind == "supported-proposal" || kind == "single-install-removal" {
						next = strings.Replace(before, operations, "", 1)
					}
					return proposal{Edits: []proposedEdit{{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: next}}}, AgentRun{}, nil
				}
				assisted := kind == "unchanged-proposal" || kind == "supported-proposal" || kind == "single-install-removal"
				if assisted {
					opts.Agent = "claude"
				}
				prepare := func() (Plan, error) {
					if assisted {
						return prepareWithProvider(context.Background(), opts, provider)
					}
					return Prepare(opts)
				}
				plan, err := prepare()
				if !matches(original, treeAt(t, root)) {
					t.Fatal("planning mutated input")
				}
				absent(t, root, ReceiptPath)
				absent(t, root, transactionPath)
				checkStage()
				accepted := kind == "supported-proposal" || kind == "historical" || kind == "lookalike" || kind == "independent" || kind == "standalone"
				if !accepted {
					if err == nil || !strings.Contains(err.Error(), name) {
						t.Fatalf("missing workflow refusal: %v", err)
					}
					if !assisted {
						var refusal *Error
						if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" {
							t.Fatalf("wrong blocker: %v", err)
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				expected := ""
				if survivor != "" {
					expected = fixturePublisher[:strings.Index(fixturePublisher, "  publish:")] + survivor
				}
				if kind == "supported-proposal" {
					expected = strings.Replace(expected, operations, "", 1)
				}
				if _, err = plan.Apply(); err != nil {
					t.Fatal(err)
				}
				if expected == "" {
					absent(t, root, name)
				} else if read(t, root, name) != expected {
					t.Fatal("retained job or trigger bytes changed")
				}
				if expected != "" {
					info, err := os.Stat(filepath.Join(root, name))
					if err != nil || info.Mode().Perm() != 0o640 {
						t.Fatalf("mode changed: %v", err)
					}
				}
				if read(t, root, publishWorkflowPath) != publishWorkflow {
					t.Fatal("ACR publisher differs")
				}
				applied := treeAt(t, root)
				firstCalls := calls
				current, err := prepare()
				if err != nil || !current.Report.Current || current.Report.Wrote || calls != firstCalls || !matches(applied, treeAt(t, root)) {
					t.Fatalf("rerun: %v", err)
				}
				if (kind == "supported-proposal" && calls != 1) || (kind != "supported-proposal" && calls != 0) {
					t.Fatalf("unexpected provider calls: %d", calls)
				}
			})
		}
	}
}

func TestCorrection9UnsupportedDeliveryFormats(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, name := range []string{".github/CODEOWNERS", ".github/PULL_REQUEST_TEMPLATE.md", ".github/workflows/unpaired.md", ".github/workflows/unsupported.md"} {
			for _, action := range []string{"replace", "patch", "remove", "keep-policy-edit", "unchanged-history", "active-unchanged"} {
				t.Run(fmt.Sprintf("%s/%s/dry=%t", name, action, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					before := "# Setup used tessl install upstream/orbit\n* @required-reviewers\n"
					if strings.Contains(name, "workflows/") {
						before = "---\non: push\ndescription: tessl install upstream/orbit\n---\nKeep independent policy.\n"
					}
					if action == "unchanged-history" {
						before = "# Historical source https://github.com/tessl-labs/original\n* @required-reviewers\n"
					}
					put(t, root, name, before, 0o640)
					if strings.HasSuffix(name, "unsupported.md") {
						put(t, root, ".github/workflows/unsupported.lock.yml", "# gh-aw-metadata: {\"schema_version\":\"v3\",\"compiler_version\":\"v9.0.0\",\"frontmatter_hash\":\"unsupported\"}\non: push\njobs:\n  check:\n    steps:\n      - run: echo independent\n", 0o640)
					}
					if action != "unchanged-history" && action != "active-unchanged" {
						edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: action, Content: "# Uses ACR\n"}
						if action == "keep-policy-edit" {
							edit.Action = "replace"
							edit.Content = "# Uses ACR\n* @required-reviewers\n"
						}
						if action == "patch" {
							edit.Content = ""
							edit.Replacements = []replacement{{Old: before, New: "# Uses ACR\n", Count: 1}}
						}
						if action == "remove" {
							edit.Content = ""
						}
						p.Edits = append(p.Edits, edit)
					}
					checkCorrectionProposal(t, dry, root, opts, p, action == "unchanged-history", name)
					if read(t, root, name) != before {
						t.Fatal("unsupported policy changed")
					}
				})
			}
		}
	}
}

func TestCorrection9ActionsLock(t *testing.T) {
	const name = ".github/aw/actions-lock.json"
	const service = `"tesslio/setup-tessl@v2":{"repo":"tesslio/setup-tessl","version":"v2","sha":"service-pin"},`
	const retained = `"other/check@v1":{"repo":"other/check","version":"v1","sha":"independent-pin","extra":{"policy":[true,"keep"],"counter":9007199254740993}}`
	const original = `{"entries":{` + service + retained + `},"metadata":{"unknown":["keep",1]}}`
	const cleaned = `{"entries":{` + retained + `},"metadata":{"unknown":["keep",1]}}`
	for _, dry := range []bool{true, false} {
		for _, action := range []string{"replace", "patch"} {
			for _, kind := range []string{"valid", "format-only", "unrelated-delete", "retained-pin", "retained-field", "large-number", "add-entry", "add-field", "top-change", "top-add", "top-delete", "duplicate", "nested-duplicate", "duplicate-original", "missing-entries", "null-entries", "array", "scalar-entry", "repo-mismatch", "version-mismatch", "lookalike-owner", "lookalike-path", "empty-ref", "new-service", "publisher", "paid-review"} {
				t.Run(fmt.Sprintf("%s/%s/dry=%t", kind, action, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					before, after := original, cleaned
					switch kind {
					case "format-only":
						after = "{\n  \"metadata\": {\"unknown\": [\"keep\", 1]}, \"entries\": {" + retained + "}\n}\n"
					case "unrelated-delete":
						after = `{"entries":{},"metadata":{"unknown":["keep",1]}}`
					case "retained-pin":
						after = strings.Replace(after, "independent-pin", "changed", 1)
					case "retained-field":
						after = strings.Replace(after, `true,"keep"`, `false,"keep"`, 1)
					case "large-number":
						after = strings.Replace(after, "9007199254740993", "9007199254740992", 1)
					case "add-entry":
						after = strings.Replace(after, `"entries":{`, `"entries":{"new/check@v1":{"repo":"new/check","version":"v1"},`, 1)
					case "add-field":
						after = strings.Replace(after, `"sha":"independent-pin"`, `"added":true,"sha":"independent-pin"`, 1)
					case "top-change":
						after = strings.Replace(after, `["keep",1]`, `["changed",1]`, 1)
					case "top-add":
						after = strings.Replace(after, `"metadata":`, `"new":true,"metadata":`, 1)
					case "top-delete":
						after = `{"entries":{` + retained + `}}`
					case "duplicate":
						after = strings.Replace(after, `"entries":`, `"entries":{},"entries":`, 1)
					case "nested-duplicate":
						after = strings.Replace(after, `"sha":"independent-pin"`, `"sha":"other","sha":"independent-pin"`, 1)
					case "duplicate-original":
						before = strings.Replace(before, `"entries":`, `"entries":{},"entries":`, 1)
					case "missing-entries":
						after = `{"metadata":{"unknown":["keep",1]}}`
					case "null-entries":
						after = `{"entries":null,"metadata":{"unknown":["keep",1]}}`
					case "array":
						after = `[]`
					case "scalar-entry":
						after = strings.Replace(after, `"entries":{`, `"entries":{"bad":1,`, 1)
					case "repo-mismatch":
						before = strings.Replace(before, `"repo":"tesslio/setup-tessl"`, `"repo":"other/setup-tessl"`, 1)
					case "version-mismatch":
						before = strings.Replace(before, `"version":"v2"`, `"version":"v3"`, 1)
					case "lookalike-owner":
						before = strings.ReplaceAll(before, "tesslio/setup-tessl", "other/setup-tessl")
					case "lookalike-path":
						before = strings.ReplaceAll(before, "tesslio/setup-tessl", "tesslio/setup-tessl/child")
					case "empty-ref":
						before = strings.Replace(before, "tesslio/setup-tessl@v2", "tesslio/setup-tessl@", 1)
					case "new-service":
						after = strings.Replace(after, `"entries":{`, `"entries":{`+service, 1)
					case "publisher":
						before = strings.ReplaceAll(before, "tesslio/setup-tessl", "tesslio/patch-version-publish")
					case "paid-review":
						before = strings.ReplaceAll(before, "tesslio/setup-tessl", "jbaruch/coding-policy/.github/actions/skill-review")
					}
					put(t, root, name, before, 0o640)
					edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: action, Content: after}
					if action == "patch" {
						edit.Content = ""
						edit.Replacements = []replacement{{Old: before, New: after, Count: 1}}
					}
					p.Edits = append(p.Edits, edit)
					if kind == "paid-review" {
						p.PolicyChanges = []PolicyChange{{Path: name, From: "Paid scoring service", To: "Retire service; keep independent pins"}}
					}
					accepted := kind == "valid" || kind == "format-only" || kind == "publisher" || kind == "paid-review"
					checkCorrectionProposal(t, dry, root, opts, p, accepted, name)
					if accepted && read(t, root, name) != after {
						t.Fatal("action lock output bytes differ")
					}
				})
			}
		}
	}
}

func TestCorrection9ReadOnlyStaging(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, mode := range []os.FileMode{0o555, 0o755, 0o775, 0o777} {
			for _, invalid := range []bool{false, true} {
				t.Run(fmt.Sprintf("%o/invalid=%t/dry=%t", mode, invalid, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					const dir = ".github/assets"
					put(t, root, dir+"/nested/data.txt", "independent data\n", 0o444)
					put(t, root, dir+"/data.txt", "outer data\n", 0o640)
					for _, name := range []string{dir, dir + "/nested"} {
						full := filepath.Join(root, name)
						if err := os.Chmod(full, mode); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := os.Chmod(full, 0o755); err != nil {
								t.Error(err)
							}
						})
					}
					if mode == 0o555 {
						t.Logf("read-only execution uid=%d", os.Getuid())
						if os.Getuid() != 0 {
							err := os.WriteFile(filepath.Join(root, dir, "unexpected"), []byte("permission control"), 0o600)
							if !errors.Is(err, os.ErrPermission) {
								t.Fatalf("0555 control must deny child creation: %v", err)
							}
						}
					}
					if invalid {
						p.Edits[0].Content = "#!/bin/sh\nset -eu\ntessl install upstream/orbit\nprintf 'still dependent\\n'\n"
					}
					checkCorrectionProposal(t, dry, root, opts, p, !invalid)
					for _, name := range []string{dir, dir + "/nested"} {
						info, err := os.Stat(filepath.Join(root, name))
						if err != nil || info.Mode().Perm() != mode {
							t.Fatalf("original/final mode changed: %s %v", name, err)
						}
					}
					if read(t, root, dir+"/nested/data.txt") != "independent data\n" {
						t.Fatal("read-only data changed")
					}
				})
			}
		}
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

// TestCorrection11RetainedWorkflowFields covers the workflow invariant even
// when all original jobs qualify for retirement. The all-service/mixed
// counterexample is adopted from the full10 reviewer probe and judge11 ruling;
// acceptance here requires preservation, not reproduction of the old bypass.
func TestCorrection11RetainedWorkflowFields(t *testing.T) {
	const name = ".github/workflows/scoring.yml"
	const scoring = "      - uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n"
	const retired = "      - run: echo 'Paid score retired; no equivalent score'\n"
	const independent = "  tests:\n    runs-on: ubuntu-latest\n    steps:\n      - run: exit 1\n"
	const environment = "env:\n  KEEP_FIRST: first\n  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n  GH_AW_SECRET_NAMES: FIRST,TESSL_TOKEN,SECOND\n  KEEP_LAST: last\n"
	for _, mixed := range []bool{false, true} {
		for _, dry := range []bool{true, false} {
			for _, kind := range []string{"unchanged", "display-name", "trigger", "permissions", "write-all", "environment", "env-change", "env-remove", "env-order", "credential-add", "credential-cleanup", "all-credentials-removed", "list-remove", "list-order", "comment-only", "nonmapping", "delete", "referenced-replacement", "missing-disclosure"} {
				t.Run(fmt.Sprintf("%s/mixed=%t/dry=%t", kind, mixed, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					top := "name: Paid scoring\non: pull_request\npermissions:\n  contents: read\n"
					if strings.HasPrefix(kind, "env-") || strings.HasPrefix(kind, "list-") || kind == "credential-cleanup" || kind == "credential-add" {
						top += environment
					}
					if kind == "all-credentials-removed" {
						top += "env:\n  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n"
					}
					if kind == "referenced-replacement" {
						top += "run-name: ${{ jobs.score.outputs.result }}\n"
					}
					before := top + "jobs:\n  score:\n    runs-on: ubuntu-latest\n    steps:\n" + scoring
					if mixed {
						before += independent
					}
					after := strings.Replace(before, scoring, retired, 1)
					accepted, reason := false, ""
					switch kind {
					case "unchanged":
						accepted = true
					case "display-name":
						after = strings.Replace(after, "name: Paid scoring", "name: Scoring retirement notice", 1)
						accepted = true
					case "trigger":
						after = strings.Replace(after, "on: pull_request", "on: workflow_dispatch", 1)
						reason = "on condition/policy"
					case "permissions":
						after = strings.Replace(after, "contents: read", "contents: write", 1)
						reason = "permissions condition/policy"
					case "write-all":
						after = strings.Replace(after, "permissions:\n  contents: read", "permissions: write-all", 1)
						reason = "permissions condition/policy"
					case "environment":
						after = "env:\n  UNRELATED_POLICY: changed\n" + after
						reason = "environment/credential policy"
					case "env-change":
						after = strings.Replace(after, "KEEP_FIRST: first", "KEEP_FIRST: changed", 1)
						reason = "environment/credential policy"
					case "env-remove":
						after = strings.Replace(after, "  KEEP_FIRST: first\n", "", 1)
						reason = "environment/credential policy"
					case "env-order":
						after = strings.Replace(after, "  KEEP_FIRST: first\n", "", 1)
						after = strings.Replace(after, "  KEEP_LAST: last\n", "  KEEP_LAST: last\n  KEEP_FIRST: first\n", 1)
						reason = "environment/credential policy"
					case "credential-add":
						after = strings.Replace(after, "env:\n", "env:\n  TESSL_NEW: placeholder\n", 1)
						reason = "environment/credential policy"
					case "credential-cleanup":
						after = strings.Replace(after, "  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n", "", 1)
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "FIRST,SECOND", 1)
						accepted = true
					case "all-credentials-removed":
						after = strings.Replace(after, "env:\n  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n", "", 1)
						accepted = true
					case "list-remove":
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "FIRST", 1)
						reason = "environment/credential policy"
					case "list-order":
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "SECOND,FIRST", 1)
						reason = "environment/credential policy"
					case "comment-only":
						after, reason = "# Paid score retired\n", "workflow requires a mapping"
					case "nonmapping":
						after, reason = "[]\n", "workflow requires a mapping"
					case "delete":
						accepted, reason = !mixed, "retain independent review/test workflow"
					case "referenced-replacement":
						reason = "referenced service job"
					case "missing-disclosure":
						reason = "policyChanges"
					}
					if strings.HasPrefix(kind, "env-") || strings.HasPrefix(kind, "list-") {
						// Retire service credentials too, so a residual operation cannot
						// hide a changed unrelated field in the counterfactual.
						after = strings.Replace(after, "  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n", "", 1)
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "FIRST,SECOND", 1)
					}
					put(t, root, name, before, 0o640)
					edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after}
					if kind == "delete" {
						edit.Action, edit.Content = "remove", ""
					}
					p.Edits = append(p.Edits, edit)
					if kind != "missing-disclosure" {
						p.PolicyChanges = []PolicyChange{{Path: name, From: "Tessl paid score", To: "Retire paid score; ACR has no equivalent"}}
					}
					checkCorrectionProposal(t, dry, root, opts, p, accepted, name, reason)
					if accepted {
						info, err := os.Stat(filepath.Join(root, ReceiptPath))
						if err != nil || info.Mode().Perm() != 0o600 {
							t.Fatalf("accepted receipt mode: %v %v", info, err)
						}
						value, err := manifest.Load(root)
						if err != nil {
							t.Fatal(err)
						}
						files, err := manifest.PackageFiles(root, value)
						if err != nil {
							t.Fatal(err)
						}
						var rec receipt
						if err := json.Unmarshal([]byte(read(t, root, ReceiptPath)), &rec); err != nil {
							t.Fatal(err)
						}
						if strings.Join(files, "\n") != strings.Join(rec.PublishedFiles, "\n") || len(rec.PolicyChanges) != 1 || rec.PolicyChanges[0] != p.PolicyChanges[0] {
							t.Fatalf("receipt inventory or retirement disclosure differs: %+v", rec)
						}
					}
				})
			}
		}
	}
}
