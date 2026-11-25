package antigravity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/antigravity"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestAntigravityDetection(t *testing.T) {
	a := antigravity.Adapter{}
	if got := a.Descriptor(); got.ID != "antigravity" || got.Version != "1.0.0" || got.Boundary != 1 {
		t.Fatal(got)
	}
	for _, file := range []string{"GEMINI.md", ".agents/hooks.json", ".agent/hooks.json", "_agents/rules/example.md", "_agent/skills/example/SKILL.md", "AGENTS.md", ".agents/skills/example/SKILL.md"} {
		d, err := a.Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(fstest.MapFS{file: &fstest.MapFile{Data: []byte("test")}})})
		if err != nil || d.Detected != (file != "AGENTS.md" && !strings.HasPrefix(file, ".agents/skills")) {
			t.Fatalf("%s: %+v %v", file, d, err)
		}
	}
	for _, tc := range []struct {
		name, alias, target, evidence string
		directory                     bool
	}{
		{name: "alias", alias: ".agent", target: ".agents"},
		{name: "regular file", alias: ".agent"},
		{name: "dangling", alias: ".agent", target: "missing"},
		{name: "alias spelled in another case", alias: ".Agent", target: ".agents"},
		{name: "plural alias", alias: "_agents", target: ".agents"},
		{name: "alias beside real evidence", alias: ".agent", target: ".agents", evidence: ".agents/GEMINI.md"},
		{name: "skills leaf alias", alias: ".agent/skills", target: "../.agents/skills"},
		{name: "rules leaf alias to a real directory", alias: ".agents/rules", target: "../docs/rules"},
		{name: "rules leaf alias to a root", alias: ".agent/rules", target: "../.agents"},
		{name: "rules leaf regular file", alias: ".agent/rules"},
		{name: "skills leaf regular file", alias: ".agent/skills"},
		{name: "hooks.json alias", alias: ".agent/hooks.json", target: "../AGENTS.md"},
		{name: "hooks.json directory", alias: ".agent/hooks.json", directory: true},
		{name: "dangling workflows leaf", alias: ".agents/workflows", target: "missing"},
		{name: "skills leaf alias spelled in another case", alias: ".agent/Skills", target: "../.agents/skills"},
		{name: "rules leaf alias beside real rules", alias: ".agent/rules", target: "../.agents/rules", evidence: ".agents/rules/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			alias := filepath.Join(root, tc.alias)
			for _, dir := range []string{".agents/skills", "docs/rules", filepath.Dir(tc.alias)} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("user\n"), 0644); err != nil {
				t.Fatal(err)
			}
			var err error
			switch {
			case tc.directory:
				err = os.Mkdir(alias, 0755)
			case tc.target == "":
				err = os.WriteFile(alias, []byte("user\n"), 0644)
			default:
				err = os.Symlink(tc.target, alias)
			}
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if tc.evidence != "" {
				want = []string{tc.evidence}
				if strings.HasSuffix(tc.evidence, "/") {
					err = os.MkdirAll(filepath.Join(root, tc.evidence), 0755)
				} else {
					err = os.WriteFile(filepath.Join(root, tc.evidence), []byte("user\n"), 0644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			project, err := adapter.NewRootSnapshot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := project.Close(); err != nil {
					t.Fatal(err)
				}
			}()
			d, err := a.Detect(context.Background(), adapter.DetectRequest{Project: project})
			if err != nil || d.Detected != (want != nil) || !slices.Equal(d.Evidence, want) {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
	// An unreadable regular hooks.json is real evidence ACR cannot inspect.
	t.Run("unreadable hooks.json", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads mode-000 files")
		}
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".agent"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".agent/hooks.json"), []byte("{}"), 0); err != nil {
			t.Fatal(err)
		}
		project, err := adapter.NewRootSnapshot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := project.Close(); err != nil {
				t.Fatal(err)
			}
		}()
		if d, err := a.Detect(context.Background(), adapter.DetectRequest{Project: project}); err == nil || !strings.Contains(err.Error(), ".agent/hooks.json") {
			t.Fatalf("%+v %v", d, err)
		}
	})
	// A case-sensitive volume can hold a real root beside another spelling.
	distinct := fstest.MapFS{".Agent": &fstest.MapFile{}, ".agent/hooks.json": &fstest.MapFile{Data: []byte("{}")}}
	if d, err := a.Detect(context.Background(), adapter.DetectRequest{Project: adapter.NewFSSnapshot(distinct)}); err != nil || !d.Detected {
		t.Fatalf("real root beside another spelling: %+v %v", d, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(adapter.HookTranslator())); got != "48dd1cb6a1a98ef603763a0a6df778e930dca45da1ec0b539e4a4c37f0c177d3" {
		t.Fatalf("legacy runtime digest changed: %s", got)
	}
}

const fixtureHook = `#!/usr/bin/env python3
import json, os, sys
with open("received.json", "w") as target:
    json.dump([os.getcwd(), os.environ["CLAUDE_PROJECT_DIR"], sys.argv[1:], json.load(sys.stdin)], target)
sys.stdout.write(os.environ["REPLY"])
sys.stderr.write(os.environ.get("DETAIL", ""))
sys.exit(int(os.environ.get("STATUS", "0")))
`

func TestSixEventsAndRealizedCommands(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sourcePath := "hooks/scope 'one'.py"
	args := []string{"", "~", "#literal", "a b", "'quoted'", "line\nbreak", "$(exit 9); $HOME"}
	pkg := adapter.Package{Source: "github:sample/native", Root: fstest.MapFS{sourcePath: &fstest.MapFile{Data: []byte(fixtureHook), Mode: 0755}}}
	for _, event := range adapter.SortedEvents() {
		pkg.Manifest.Artifacts.Hooks = append(pkg.Manifest.Artifacts.Hooks, manifest.HookArtifact{ID: string(event), Event: event, Path: sourcePath, Args: args})
	}
	coord, err := adapter.NewCoordinator(preserve.NewCompiler(), antigravity.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	prior := realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion}
	plan, notices, err := coord.RealizeWithNotices(ctx, adapter.NewFSSnapshot(os.DirFS(root)), []adapter.Package{pkg}, prior)
	if err != nil || len(plan) != 6 || len(notices) != 2 {
		t.Fatalf("targets=%d notices=%+v err=%v", len(plan), notices, err)
	}
	for i, event := range []string{"session-end", "user-prompt-submit"} {
		if !strings.Contains(notices[i].Message, event) || !strings.Contains(notices[i].Message, pkg.Source) {
			t.Fatal(notices)
		}
	}
	if _, err := realize.NewEngine().Run(root, prior, plan, realize.ModeApply, func(realize.Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".agent/hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	type handler struct {
		Type, Command, Matcher string
		Timeout                int
		Hooks                  []handler
	}
	var doc map[string]map[string][]handler
	if err := json.Unmarshal(data, &doc); err != nil || len(doc) != 4 {
		t.Fatalf("named hooks: %d %v", len(doc), err)
	}
	commands := map[string]string{}
	for event, native := range map[string]string{"session-start": "SessionStart", "pre-tool-use": "PreToolUse", "post-tool-use": "PostToolUse", "stop": "Stop"} {
		field := doc["acr__sample__native__"+event]
		rows := field[native]
		if len(field) != 1 || len(rows) != 1 {
			t.Fatalf("wrong %s shape", event)
		}
		h := rows[0]
		if strings.HasSuffix(event, "tool-use") {
			if h.Matcher != "*" || len(h.Hooks) != 1 || h.Command != "" {
				t.Fatalf("wrong %s matcher", event)
			}
			h = h.Hooks[0]
		}
		if h.Type != "command" || h.Timeout != 30 || h.Command == "" || len(h.Hooks) != 0 {
			t.Fatalf("wrong %s handler", event)
		}
		commands[event] = h.Command
	}
	// The shell itself is exercised below. Replace just the four command
	// strings to compare the complete native schema with a readable golden.
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	for event, command := range commands {
		encoded, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		normalized = bytes.ReplaceAll(normalized, encoded, []byte(`"<`+event+`>"`))
	}
	want, err := os.ReadFile("testdata/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var actual, golden any
	if err := json.Unmarshal(normalized, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &golden); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, golden) {
		t.Fatal("native configuration differs from testdata/hooks.json")
	}

	for _, tc := range []struct {
		name, event, reply, decision, diagnostic string
		code                                     int
	}{
		{"text context", "session-start", "startup news", "", "", 0},
		{"empty", "session-start", "", "", "", 0},
		{"unsupported startup", "session-start", `{"continue":false}`, "", "Rewrite this hook", 0},
		{"absent permission", "pre-tool-use", "{}", "ask", "", 0},
		{"bare approve", "pre-tool-use", `{"decision":"approve"}`, "ask", "", 0},
		{"absent permission with rewrite", "pre-tool-use", `{"hookSpecificOutput":{"updatedInput":{"items":{"new":1}}}}`, "ask", "", 0},
		{"defer", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"defer","updatedInput":{"items":1}}}`, "ask", "", 0},
		{"defer ignores invalid rewrite", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"defer","updatedInput":42}}`, "ask", "", 0},
		{"null legacy decision", "pre-tool-use", `{"decision":null}`, "deny", "invalid hook response", 0},
		{"null permission", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":null}}`, "deny", "invalid hook response", 0},
		{"empty permission", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":""}}`, "deny", "invalid hook response", 0},
		{"duplicate permission", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecision":"allow"}}`, "deny", "invalid hook response", 0},
		{"duplicate decision", "pre-tool-use", `{"decision":"block","decision":"approve"}`, "deny", "invalid hook response", 0},
		{"block precedes permission", "pre-tool-use", `{"decision":"block","hookSpecificOutput":{"permissionDecision":"ask","updatedInput":{"items":1}}}`, "deny", "", 0},
		{"termination discards rewrite", "pre-tool-use", `{"continue":false,"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"items":1}}}`, "deny", "", 0},
		{"allow", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"items":{"new":1}}}}`, "allow", "", 0},
		{"ask", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"confirm"}}`, "force_ask", "", 0},
		{"deny", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"deny","updatedInput":{"items":1}}}`, "deny", "", 0},
		{"exit two", "pre-tool-use", "bogus", "deny", "[hook detail truncated]", 2},
		{"deletion", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{}}}`, "deny", "the tool call was denied. Include every original top-level argument key in updatedInput", 0},
		{"context beside extras", "session-start", `{"systemMessage":"shown","hookSpecificOutput":{"additionalContext":"startup news"}}`, "", "dropped unsupported fields", 0},
		{"before context", "pre-tool-use", `{"hookSpecificOutput":{"additionalContext":"unavailable"}}`, "ask", "dropped unsupported fields", 0},
		{"deny beside extras", "pre-tool-use", `{"systemMessage":"shown","hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"protected path"}}`, "deny", "dropped unsupported fields", 0},
		{"block beside extras", "pre-tool-use", `{"decision":"block","reason":"protected path","unknown":1}`, "deny", "dropped unsupported fields", 0},
		{"stop beside extras", "pre-tool-use", `{"continue":false,"stopReason":"protected path","suppressOutput":false}`, "deny", "dropped unsupported fields", 0},
		{"ask beside extras", "pre-tool-use", `{"suppressOutput":true,"hookSpecificOutput":{"permissionDecision":"ask"}}`, "force_ask", "dropped unsupported fields", 0},
		{"allow beside extras", "pre-tool-use", `{"stopReason":"unused","hookSpecificOutput":{"permissionDecision":"allow","additionalContext":"unavailable"}}`, "allow", "dropped unsupported fields", 0},
		{"permission overrides", "pre-tool-use", `{"permissionOverrides":["*"],"hookSpecificOutput":{"permissionOverrides":["*"]}}`, "ask", "dropped unsupported fields", 0},
		{"ask with rewrite", "pre-tool-use", `{"hookSpecificOutput":{"permissionDecision":"ask","updatedInput":{"items":{"new":1}}}}`, "force_ask", "", 0},
		{"before text", "pre-tool-use", "checked", "ask", "PreToolUse requires JSON; dropped plain text", 0},
		{"banner before JSON", "pre-tool-use", "banner\n{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\"}}", "ask", "PreToolUse requires JSON; dropped plain text", 0},
		{"mistyped suppression", "pre-tool-use", `{"suppressOutput":"true","hookSpecificOutput":{"permissionDecision":"allow"}}`, "deny", "invalid hook response", 0},
		{"mistyped message", "pre-tool-use", `{"systemMessage":5,"hookSpecificOutput":{"permissionDecision":"allow"}}`, "deny", "invalid hook response", 0},
		{"mistyped stop reason", "pre-tool-use", `{"stopReason":5,"hookSpecificOutput":{"permissionDecision":"allow"}}`, "deny", "invalid hook response", 0},
		{"mismatched event", "pre-tool-use", `{"hookSpecificOutput":{"hookEventName":"Stop","permissionDecision":"allow"}}`, "deny", "Rewrite this hook", 0},
		{"invalid output", "pre-tool-use", "{", "deny", "invalid JSON", 0},
		{"unexpected exit", "pre-tool-use", "partial", "deny", "exited 7", 7},
		{"side effect", "post-tool-use", "{}", "", "", 0},
		{"after text", "post-tool-use", "done", "", "PostToolUse requires JSON; dropped plain text", 0},
		{"after extras", "post-tool-use", `{"systemMessage":"shown"}`, "", "dropped unsupported fields", 0},
		{"after block", "post-tool-use", `{"decision":"block","reason":"again"}`, "", "the tool result was kept. Return {}", 0},
		{"after feedback", "post-tool-use", `{"hookSpecificOutput":{"additionalContext":"again"}}`, "", "the tool result was kept. Return {}", 0},
		{"after rewrite", "post-tool-use", `{"hookSpecificOutput":{"updatedMCPToolOutput":"new"}}`, "", "the tool result was kept. Return {}", 0},
		{"continue", "stop", `{"decision":"block","reason":"finish"}`, "continue", "", 0},
		{"stop context", "stop", `{"hookSpecificOutput":{"additionalContext":"more"}}`, "continue", "", 0},
		{"stop exit two", "stop", "", "continue", "", 2},
		{"termination", "stop", `{"continue":false,"decision":"block"}`, "", "", 0},
		{"termination with stderr", "stop", `{"continue":false}`, "", "", 0},
		{"continue beside extras", "stop", `{"decision":"block","reason":"finish","systemMessage":"shown","suppressOutput":true,"stopReason":"unused","unknown":1}`, "continue", "dropped unsupported fields", 0},
		{"termination beside extras", "stop", `{"continue":false,"decision":"block","stopReason":"finished"}`, "", "dropped unsupported fields", 0},
		{"stop text", "stop", "done", "", "Stop requires JSON; dropped plain text", 0},
		{"stop failure", "stop", "partial", "", "exited 7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", commands[tc.event])
			cmd.Dir = filepath.Join(root, ".agent")
			cmd.Env = append(os.Environ(), "REPLY="+tc.reply, fmt.Sprintf("STATUS=%d", tc.code))
			const stopDiagnostic = "hook diagnostic\n"
			if tc.name == "termination with stderr" {
				cmd.Env = append(cmd.Env, "DETAIL="+stopDiagnostic)
			}
			if tc.name == "exit two" {
				cmd.Env = append(cmd.Env, "DETAIL="+strings.Repeat("\x1b", 4000))
			}
			cmd.Stdin = strings.NewReader(`{"conversationId":"sample","transcriptPath":"transcript","stepIdx":3,"toolCall":{"name":"nativeTool","args":{"items":{"old":1}}},"continuationActive":false,"executionNum":3,"error":"preserve"}`)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("exit %v: %s", err, stderr.String())
			}
			if tc.name == "termination with stderr" && (string(out) != "{}\n" || stderr.String() != stopDiagnostic) {
				t.Fatalf("stdout = %q, stderr = %q; want empty native reply and unmodified hook diagnostic", out, stderr.String())
			}
			var reply map[string]any
			if err := json.Unmarshal(out, &reply); err != nil {
				t.Fatal(err)
			}
			if tc.name == "exit two" && (stderr.Len() > 17*1024 || strings.Contains(stderr.String(), "\x1b") || !strings.Contains(stderr.String(), `\u001B`)) {
				t.Fatalf("unbounded or unescaped denial diagnostic (%d bytes)", stderr.Len())
			}
			if tc.diagnostic != "" && (!strings.Contains(stderr.String(), tc.diagnostic) || !strings.Contains(stderr.String(), tc.event)) {
				t.Fatalf("stderr %q", stderr.String())
			}
			if tc.diagnostic != "" && tc.name != "exit two" && !strings.Contains(tc.diagnostic, "dropped") && !strings.Contains(stderr.String(), "acr install before acr realize") {
				t.Fatalf("missing package recovery in %q", stderr.String())
			}
			if tc.decision != "" {
				if reply["decision"] != tc.decision {
					t.Fatalf("unexpected reply %s", out)
				}
				if reply["permissionOverrides"] != nil {
					t.Fatalf("permission overrides reached Antigravity: %s", out)
				}
				if tc.decision == "deny" && (reply["overwrite"] != nil || reply["reason"] == "" || strings.Contains(tc.reply, "protected path") && !strings.Contains(fmt.Sprint(reply["reason"]), "protected path")) {
					t.Fatalf("invalid denial %s", out)
				}
				if (tc.name == "allow" || tc.name == "ask with rewrite" || tc.name == "absent permission with rewrite") && !reflect.DeepEqual(reply["overwrite"], map[string]any{"items": map[string]any{"new": float64(1)}}) {
					t.Fatalf("overwrite %s", out)
				}
				if strings.HasPrefix(tc.name, "defer") && reply["overwrite"] != nil {
					t.Fatalf("defer emitted overwrite: %s", out)
				}
			} else if tc.name == "text context" || tc.name == "context beside extras" {
				if !reflect.DeepEqual(reply, map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]any{"systemMessage": "startup news"}}}}) {
					t.Fatalf("startup %s", out)
				}
			} else if len(reply) != 0 {
				t.Fatalf("want empty reply, got %s", out)
			}
			data, err := os.ReadFile(filepath.Join(root, "received.json"))
			if err != nil {
				t.Fatal(err)
			}
			var received []json.RawMessage
			if err := json.Unmarshal(data, &received); err != nil || len(received) != 4 {
				t.Fatalf("captured input: %v", err)
			}
			var gotArgs []string
			var input map[string]any
			for _, r := range received[:2] {
				var dir string
				if err := json.Unmarshal(r, &dir); err != nil {
					t.Fatal(err)
				}
				realRoot, err := filepath.EvalSymlinks(root)
				if err != nil || dir != realRoot {
					t.Fatalf("project root %q want %q", dir, realRoot)
				}
			}
			if err := json.Unmarshal(received[2], &gotArgs); err != nil || !slices.Equal(gotArgs, args) {
				t.Fatalf("argv %q", gotArgs)
			}
			if err := json.Unmarshal(received[3], &input); err != nil {
				t.Fatal(err)
			}
			realRoot, err := filepath.EvalSymlinks(root)
			if err != nil || input["transcript_path"] != "transcript" || input["cwd"] != realRoot || input["session_id"] != "sample" || input["error"] != "preserve" {
				t.Fatal(input)
			}
			if tc.event == "session-start" && input["source"] != "startup" || tc.event == "stop" && input["stop_hook_active"] != true {
				t.Fatal(input)
			}
			if strings.HasSuffix(tc.event, "tool-use") && (input["tool_name"] != "nativeTool" || input["tool_use_id"] != "sample:3" || input["tool_input"] == nil || input["tool_response"] != nil) {
				t.Fatal(input)
			}
		})
	}
}

func TestRefuseOccupiedHookField(t *testing.T) {
	name := "acr__sample__native__before"
	pkg := adapter.Package{Source: "github:sample/native", Root: fstest.MapFS{"hook": &fstest.MapFile{Data: []byte("#!/bin/sh\n")}}}
	pkg.Manifest.Artifacts.Hooks = []manifest.HookArtifact{{ID: "before", Event: manifest.HookPreToolUse, Path: "hook"}}
	for _, config := range []string{fmt.Sprintf(`{"%s":{}}`, name), `{"other":{},"other":{}}`, `{"other":{"enabled":false,"enabled":true}}`} {
		project := adapter.NewFSSnapshot(fstest.MapFS{".agent/hooks.json": &fstest.MapFile{Data: []byte(config), Mode: 0644}})
		c, err := adapter.NewCoordinator(preserve.NewCompiler(), antigravity.Adapter{})
		if err != nil {
			t.Fatal(err)
		}
		intents, err := c.Realize(context.Background(), project, []adapter.Package{pkg}, realize.Ledger{SchemaVersion: realize.CurrentLedgerSchemaVersion})
		if err == nil || len(intents) != 0 {
			t.Fatalf("accepted occupied or duplicate field: %s", config)
		}
	}
}

func TestRejectUnrepresentableHookArgument(t *testing.T) {
	pkg := adapter.Package{Source: "github:sample/native"}
	pkg.Manifest.Artifacts.Hooks = []manifest.HookArtifact{
		{ID: "before", Event: manifest.HookPreToolUse, Path: "hook", Args: []string{"fixture", "bad\x00arg"}},
	}
	plan, err := (antigravity.Adapter{}).Plan(context.Background(), adapter.PlanRequest{Packages: []adapter.Package{pkg}})
	if err == nil || !strings.Contains(err.Error(), "args[1]") || len(plan.Items) != 0 {
		t.Fatalf("unrepresentable args planned: %v", err)
	}
}

func TestAntigravitySchemaBoundary(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "acr-hook.py")
	if err := os.WriteFile(runtime, adapter.AntigravityHookTranslator(), 0755); err != nil {
		t.Fatal(err)
	}
	// Exercise the last native schema check directly, including shapes the
	// canonical translator never constructs in a well-formed execution.
	script := `import runpy,sys
schema=runpy.run_path(sys.argv[1])
check=schema["validate_antigravity"]
check("pre-tool-use", {"decision":"ask"})
for event, value in [
    ("pre-tool-use",{}),
    ("pre-tool-use",{"decision":"allow","permissionOverrides":[]}),
    ("pre-tool-use",{"decision":"deny","overwrite":{}}),
    ("pre-tool-use",{"decision":"allow","overwrite":[]}),
    ("pre-tool-use",{"decision":"force_ask","reason":False}),
    ("post-tool-use",{"decision":"continue"}),
    ("session-start",{"injectSteps":[{"systemMessage":{"systemMessage":42}}]}),
    ("stop",{"decision":"allow"})]:
    try: check(event,value)
    except ValueError: continue
    raise AssertionError((event,value))
read=schema["antigravity_input"]
assert read(b'{"executionNum":0,"continuationActive":false}',"stop","/project")["stop_hook_active"] is False
assert read(b'{"executionNum":2}',"stop","/project")["stop_hook_active"] is True
assert read(b'{"executionNum":2,"continuationActive":false}',"stop","/project")["stop_hook_active"] is True
assert read(b'{"executionNum":0,"continuationActive":true}',"stop","/project")["stop_hook_active"] is True
assert read(b'{"transcriptPath":null}',"stop","/project")["transcript_path"] is None
post=read(b'{"cwd":"/native","toolCall":{"args":{},"name":"read","id":"native-id"},"result":{"native":[1]}}',"post-tool-use","/project")
assert post["tool_response"]=={"native":[1]}
assert post["tool_use_id"]=="native-id"
assert post["cwd"]=="/native"
for invalid in [
    b'{"toolCall":{"name":"sample","args":{}}}',
    b'{"cwd":"relative","toolCall":{"name":"sample","args":{},"id":"id"}}',
    b'{"conversationId":"sample","stepIdx":true,"toolCall":{"name":"sample","args":{}}}',
    b'{"toolCall":{"name":"sample","args":{"path":"a","path":"b"},"id":"id"}}',
]:
    try: read(invalid,"pre-tool-use","/project")
    except ValueError: continue
    raise AssertionError(invalid)
`
	if out, err := exec.Command("python3", "-I", "-c", script, runtime).CombinedOutput(); err != nil {
		t.Fatalf("native schema: %s %v", out, err)
	}
}
