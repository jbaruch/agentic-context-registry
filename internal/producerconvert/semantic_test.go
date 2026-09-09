package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
		if err != nil || calls != 2 || len(plan.Report.AgentRuns) != 2 || plan.Report.AgentRuns[0].Failure == "" {
			t.Fatalf("repair: %v %d %+v", err, calls, plan.Report.AgentRuns)
		}
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
		old := "# tessl fixture\ndef test_policy():\n    assert 1 == 1\ntest_policy()\n"
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
	name := ".github/workflows/inspect.md"
	body := "Run `tessl install maker/policy` before review.\n"
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
