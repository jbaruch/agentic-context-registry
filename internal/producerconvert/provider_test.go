package producerconvert

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestCorrection12ClaudeNativeKeys(t *testing.T) {
	const init = `{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"native_metadata":{"version":"fixture"}}`
	const output = `{"edits":[],"policyChanges":[]}`
	const result = `{"type":"result","subtype":"success","is_error":false,"structured_output":` + output + `,"usage":{"input_tokens":1}}`
	const tool = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`
	cases := []struct {
		name     string
		events   []string
		accepted bool
	}{
		{"valid-metadata", []string{init, result}, true},
		{"result-before-init", []string{result, init}, true},
		{"tools", []string{strings.Replace(init, `"tools":`, `"tools":["Bash"],"tools":`, 1), result}, false},
		{"mcp", []string{strings.Replace(init, `"mcp_servers":`, `"mcp_servers":[{"name":"foreign"}],"mcp_servers":`, 1), result}, false},
		{"type", []string{init, strings.Replace(tool, `"type":"tool_use"`, `"type":"tool_use","type":"text"`, 1), result}, false},
		{"name", []string{init, strings.Replace(tool, `"name":"Bash"`, `"name":"Bash","name":"StructuredOutput"`, 1), result}, false},
		{"message", []string{init, strings.Replace(tool, `"message":{"content":[{"type":"tool_use","name":"Bash"}]}`, `"message":{"content":[{"type":"tool_use","name":"Bash"}]},"message":{"content":[]}`, 1), result}, false},
		{"content", []string{init, strings.Replace(tool, `"content":[{"type":"tool_use","name":"Bash"}]`, `"content":[{"type":"tool_use","name":"Bash"}],"content":[]`, 1), result}, false},
		{"status", []string{init, strings.Replace(result, `"is_error":false`, `"is_error":true,"is_error":false`, 1)}, false},
		{"subtype", []string{init, strings.Replace(result, `"subtype":"success"`, `"subtype":"error","subtype":"success"`, 1)}, false},
		{"envelope-type", []string{init, strings.Replace(result, `"type":"result"`, `"type":"assistant","type":"result"`, 1)}, false},
		{"api-status", []string{init, strings.Replace(result, `"is_error":false`, `"is_error":false,"api_error_status":403,"api_error_status":0`, 1)}, false},
		{"metadata-duplicate", []string{strings.Replace(init, `"version":"fixture"`, `"version":"other","version":"fixture"`, 1), result}, false},
		{"explicit-tool", []string{strings.Replace(init, "StructuredOutput", "Bash", 1), result}, false},
		{"explicit-mcp", []string{strings.Replace(init, `"mcp_servers":[]`, `"mcp_servers":[{}]`, 1), result}, false},
		{"tool-after-result", []string{init, result, tool}, false},
		{"duplicate-init", []string{init, init, result}, false},
		{"duplicate-result", []string{init, result, result}, false},
		{"error-result", []string{init, strings.Replace(result, `"is_error":false`, `"is_error":true`, 1)}, false},
		{"incomplete", []string{init}, false},
		{"malformed", []string{init, `{"type":`}, false},
	}
	for _, array := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/array=%t", c.name, array), func(t *testing.T) {
				body := strings.Join(c.events, "\n")
				if array {
					body = "[" + strings.Join(c.events, ",") + "]"
				}
				raw, err := claudeProposal([]byte(body))
				if (err == nil) != c.accepted {
					t.Fatalf("accept=%t want=%t: %v", err == nil, c.accepted, err)
				}
				if c.accepted && string(raw) != output {
					t.Fatalf("proposal changed: %s", raw)
				}
			})
		}
	}
}

func TestCorrection12ClaudeProcess(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, array := range []bool{false, true} {
			for _, duplicate := range []bool{false, true} {
				t.Run(fmt.Sprintf("dry=%t/array=%t/duplicate=%t", dry, array, duplicate), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					opts.DryRun = dry
					encoded, err := json.Marshal(p)
					if err != nil {
						t.Fatal(err)
					}
					init := `{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"session_id":"fixture"}`
					if duplicate {
						init = strings.Replace(init, `"tools":`, `"tools":["Bash"],"tools":`, 1)
					}
					result := `{"type":"result","subtype":"success","structured_output":` + string(encoded) + `}`
					body := init + "\n" + result
					if array {
						body = "[" + init + "," + result + "]"
					}
					bin := t.TempDir()
					put(t, bin, "response", body, 0o600)
					put(t, bin, "claude", "#!/bin/sh\nset -eu\ncat \"$ACR_CLAUDE_RESPONSE\"\n", 0o755)
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
					t.Setenv("ACR_CLAUDE_RESPONSE", filepath.Join(bin, "response"))
					cleanup := correctionStageCheck(t)
					defer cleanup()
					before := correction12Inventory(t, root)
					calls := 0
					provider := func(ctx context.Context, agent, request string) (proposal, AgentRun, error) {
						calls++
						return runProvider(ctx, agent, request)
					}
					plan, err := prepareWithProvider(context.Background(), opts, provider)
					correction12Unchanged(t, root, before)
					if duplicate {
						if err == nil || !strings.Contains(err.Error(), "duplicate") {
							t.Fatalf("duplicate receipt accepted: %v", err)
						}
						if calls != 1 || plan.Report.AgentRuns[0].Stdout != body || plan.Report.AgentRuns[0].Failure == "" {
							t.Fatal("lost native failure evidence")
						}
						if _, err := plan.Apply(); err == nil {
							t.Fatal("refused plan applied")
						}
						correction12Unchanged(t, root, before)
					} else {
						if err != nil {
							t.Fatal(err)
						}
						correction12Apply(t, root, opts, plan, before, provider)
						if calls != 1 || read(t, root, p.Edits[0].Path) != p.Edits[0].Content {
							t.Fatal("wrong positive output or rerun")
						}
					}
				})
			}
		}
	}
}
