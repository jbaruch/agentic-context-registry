package realizeapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestAntigravitySharedDelivery(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                string
		agents              []string
		shared              bool
		host, content, want string
	}{
		{"codex", []string{"codex", "antigravity"}, true, "AGENTS.md", "user\n", ""},
		{"claude shared host", []string{"claude-code", "antigravity"}, true, "CLAUDE.md", "@AGENTS.md\n", ""},
		{"codex and claude share one host", []string{"claude-code", "codex", "antigravity"}, true, "CLAUDE.md", "@AGENTS.md\n", ""},
		{"wrong host", []string{"claude-code", "antigravity"}, true, "CLAUDE.md", "user\n", "CLAUDE.md"},
		{"override", []string{"codex", "antigravity"}, true, "AGENTS.override.md", "user\n", "Create a regular root AGENTS.md if missing. Add @AGENTS.md to AGENTS.override.md"},
		{"oversized rule host", []string{"codex", "antigravity"}, true, "AGENTS.md", strings.Repeat("é", 11_995) + "\n", "limit 24000 bytes"},
		{"deeper host", []string{"codex", "antigravity"}, true, "AGENTS.md", "@guide.md\n", "redirected to [\"guide.md\"]. Copy any instructions that must still load automatically"},
		{"missing rule producer", []string{"antigravity"}, true, "AGENTS.md", "user\n", "select Codex"},
		{"disabled shared skills", []string{"codex", "antigravity"}, false, "AGENTS.md", "user\n", "Enable sharedSkills"},
		{"cursor rule duplicate", []string{"codex", "cursor", "antigravity"}, true, "AGENTS.md", "user\n", ".cursor/rules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, loader, service := sharedSkillFixture(t, tc.agents, tc.shared, firstSource)
			pkg := loader.packages[firstSource]
			pkg.manifest.Artifacts.Rules = []manifest.RuleArtifact{{ID: "guide", Path: "guide.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}
			loader.packages[firstSource] = pkg
			writeFixture(t, filepath.Join(pkg.root, "guide.md"), []byte("a shared package rule\n"), 0644)
			writeFixture(t, filepath.Join(pkg.root, "skills/skill0/SKILL.md"), []byte("---\nname: first\ndescription: Help with testing\n---\n---\n# First\n"), 0644)
			writeFixture(t, filepath.Join(root, "guide.md"), []byte("user\n"), 0644)
			if tc.content == "@AGENTS.md\n" {
				writeFixture(t, filepath.Join(root, "AGENTS.md"), []byte("user\n"), 0644)
			}
			writeFixture(t, filepath.Join(root, tc.host), []byte(tc.content), 0644)
			initRepository(t, root)
			before := hashProjectTree(t, root)
			if tc.want != "" {
				for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
					_, err := service.Run(ctx, root, nil, mode)
					if err == nil || !strings.Contains(err.Error(), tc.want) || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
						t.Fatalf("refusal or no-write guarantee in %s: %v", mode, err)
					}
				}
				if tc.name != "override" {
					return
				}
				writeFixture(t, filepath.Join(root, "AGENTS.md"), []byte("user instructions\n"), 0644)
				writeFixture(t, filepath.Join(root, tc.host), []byte(tc.content+"@AGENTS.md\n"), 0644)
			}
			if _, err := service.Run(ctx, root, nil, realize.ModeApply); err != nil {
				t.Fatal(err)
			}
			host := string(readProjectFile(t, root, "AGENTS.md"))
			if strings.Count(host, "a shared package rule") != 1 || !strings.Contains(host, "user") {
				t.Fatalf("instruction host: %s", host)
			}
			if tc.name == "override" && string(readProjectFile(t, root, tc.host)) != tc.content+"@AGENTS.md\n" {
				t.Fatal("realize changed the repaired override")
			}
			if got := string(readProjectFile(t, root, ".agents/skills/acr__example__first__skill0/references/notes.md")); got != "notes for skill0\n" {
				t.Fatal(got)
			}
			if result, err := service.Run(ctx, root, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
				t.Fatalf("repeat check: %v", err)
			}
			if _, err := service.Run(ctx, root, []string{"antigravity"}, realize.ModeApply); err == nil || !strings.Contains(err.Error(), "Run acr realize without --agent") || strings.Contains(err.Error(), "Enable sharedSkills") {
				t.Fatalf("subset accepted stale outputs: %v", err)
			}
		})
	}
}

func TestCompanionSubsetPreservesAntigravityRuleHost(t *testing.T) {
	ctx := context.Background()
	root, loader, service := sharedSkillFixture(t, []string{"codex", "antigravity"}, true, firstSource)
	pkg := loader.packages[firstSource]
	pkg.manifest.Artifacts.Rules = []manifest.RuleArtifact{{ID: "guide", Path: "guide.md", Activation: manifest.RuleActivation{Mode: manifest.ActivationAlways}}}
	loader.packages[firstSource] = pkg
	rule := filepath.Join(pkg.root, "guide.md")
	writeFixture(t, rule, []byte("Original package guidance\n"), 0644)
	writeFixture(t, filepath.Join(pkg.root, "skills/skill0/SKILL.md"), []byte("---\nname: first\ndescription: Help with testing\n---\n# First\n"), 0644)
	writeFixture(t, filepath.Join(root, "guide.md"), []byte("Local guide\n"), 0644)
	initRepository(t, root)
	if _, err := service.Run(ctx, root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	const host = "AGENTS.md"
	original := string(readProjectFile(t, root, host))
	writeFixture(t, filepath.Join(root, host), []byte(original+"\n@guide.md\n"), 0644)
	before := hashProjectTree(t, root)
	companion := []string{"codex"}
	for _, mode := range []realize.Mode{realize.ModeDryRun, realize.ModeCheck, realize.ModeApply} {
		_, err := service.Run(ctx, root, companion, mode)
		if err == nil || !strings.Contains(err.Error(), `redirected to ["guide.md"]`) || !strings.Contains(err.Error(), "replace it with an ordinary Markdown link") {
			t.Errorf("expected actionable refusal in %s, got %v", mode, err)
		}
		if !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("%s changed files despite the redirect", mode)
		}
	}

	writeFixture(t, filepath.Join(root, host), []byte(original+"\n[guide](guide.md)\n"), 0644)
	writeFixture(t, rule, []byte("Updated package guidance\n"), 0644)
	before = hashProjectTree(t, root)
	result, err := service.Run(ctx, root, companion, realize.ModeApply)
	if err != nil || !result.Plan.HasChanges() {
		t.Fatalf("expected root-host update, got %v", err)
	}
	if content := string(readProjectFile(t, root, host)); !strings.Contains(content, "Updated package guidance") || !strings.Contains(content, "[guide](guide.md)") {
		t.Fatalf("root host missing the updated rule or preserved link: %q", content)
	}
	after := hashProjectTree(t, root)
	for _, name := range []string{host, dependency.LockFilename, appliedPath} {
		delete(before, name)
		delete(after, name)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("companion-only rule update changed other project files")
	}
	for _, agents := range [][]string{companion, nil} {
		if result, err := service.Run(ctx, root, agents, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
			t.Fatalf("rule delivery is not current for %v: %v", agents, err)
		}
	}
}

func TestAntigravityRedirectedClaudeHostRecovery(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name               string
		agents             []string
		redirect, repaired map[string]string
	}{
		{"root", []string{"claude-code", "antigravity"}, map[string]string{"CLAUDE.md": "user\n@guide.md\n"}, map[string]string{"CLAUDE.md": "user\n@AGENTS.md\n"}},
		{"dot claude", []string{"claude-code", "antigravity"}, map[string]string{".claude/CLAUDE.md": "@../guide.md\n"}, map[string]string{".claude/CLAUDE.md": "@../AGENTS.md\n"}},
		{"both claude hosts", []string{"claude-code", "antigravity"}, map[string]string{"CLAUDE.md": "@guide.md\n", ".claude/CLAUDE.md": "user\n"}, map[string]string{"CLAUDE.md": "@AGENTS.md\n"}},
		{"through AGENTS", []string{"claude-code", "antigravity"}, map[string]string{"CLAUDE.md": "@AGENTS.md\n", "AGENTS.md": "@guide.md\n"}, map[string]string{"AGENTS.md": "[guide](guide.md)\n"}},
		{"codex beside claude", []string{"claude-code", "codex", "antigravity"}, map[string]string{"CLAUDE.md": "user\n", "AGENTS.md": "@guide.md\n"}, map[string]string{"AGENTS.md": "[guide](guide.md)\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, app := uninstallFixture(t, tc.agents, firstSource)
			writeFixture(t, filepath.Join(root, "guide.md"), []byte("must still load\n"), 0640)
			for name, content := range tc.redirect {
				writeFixture(t, filepath.Join(root, name), []byte(content), 0644)
			}
			initRepository(t, root)
			before := hashProjectTree(t, root)
			for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
				_, err := app.service.Run(ctx, root, nil, mode)
				if err == nil || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
					t.Fatalf("redirected host in %s: %v", mode, err)
				}
				for _, want := range []string{"redirected", "guide.md", "Copy any instructions that must still load automatically into root AGENTS.md", "in CLAUDE.md with @AGENTS.md", "in .claude/CLAUDE.md with @../AGENTS.md"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("redirect remedy lacks %q: %v", want, err)
					}
				}
			}
			host := "must still load\n" + tc.repaired["AGENTS.md"]
			writeFixture(t, filepath.Join(root, "AGENTS.md"), []byte(host), 0644)
			for name, content := range tc.repaired {
				if name != "AGENTS.md" {
					writeFixture(t, filepath.Join(root, name), []byte(content), 0644)
				}
			}
			realizeProject(t, app, root)
			got := string(readProjectFile(t, root, "AGENTS.md"))
			if !strings.HasPrefix(got, host) || strings.Count(got, "Guidance from "+firstSource) != 1 || string(readProjectFile(t, root, "guide.md")) != "must still load\n" {
				t.Fatalf("recovered host: %s", got)
			}
			if result, err := app.service.Run(ctx, root, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
				t.Fatalf("recovered host is not current: %v", err)
			}
		})
	}
}

func TestAntigravityRefusesInlineRuleIncludes(t *testing.T) {
	ctx := context.Background()
	root, _, app := uninstallFixture(t, []string{"codex", "antigravity"}, firstSource)
	initRepository(t, root)
	writeFixture(t, filepath.Join(root, "notes.md"), []byte(strings.Repeat("More instructions.\n", 1_650)), 0640)
	const host = "AGENTS.md"
	short := []byte("User instructions\nRead @[Imported instructions](notes.md).\n")
	writeFixture(t, filepath.Join(root, host), short, 0640)
	before := hashProjectTree(t, root)
	for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
		_, err := app.service.Run(ctx, root, nil, mode)
		if err == nil || !strings.Contains(err.Error(), "AGENTS.md:2: ACR cannot verify") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("inline include in %s: %v", mode, err)
		}
	}

	link := []byte(strings.ReplaceAll(string(short), "@[", "["))
	writeFixture(t, filepath.Join(root, host), link, 0640)
	realizeProject(t, app, root)
	got := readProjectFile(t, root, host)
	if !strings.HasPrefix(string(got), string(link)) {
		t.Fatalf("ordinary link missing from %q", got)
	}
	if _, err := app.service.Run(ctx, root, nil, realize.ModeCheck); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, host), []byte(strings.Replace(string(got), "[Imported instructions]", "@[Imported\ninstructions]", 1)), 0640)
	before = hashProjectTree(t, root)
	_, err := app.service.Run(ctx, root, []string{"cursor"}, realize.ModeCheck)
	if err == nil || !strings.Contains(err.Error(), "AGENTS.md:2: ACR cannot verify") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
		t.Fatalf("inline include in carried rule host: %v", err)
	}
}

func TestAntigravityRejectsUnreadableSkill(t *testing.T) {
	root, loader, service := sharedSkillFixture(t, []string{"antigravity"}, true, firstSource)
	for _, content := range []string{"# no metadata\n", "---\nname: first\ndescription: []\n---\n", "# absent"} {
		file := filepath.Join(loader.packages[firstSource].root, "skills/skill0/SKILL.md")
		writeFixture(t, file, []byte(content), 0644)
		if content == "# absent" {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}
		before := hashProjectTree(t, root)
		if _, err := service.Run(context.Background(), root, nil, realize.ModeApply); err == nil || !strings.Contains(err.Error(), firstSource) || !strings.Contains(err.Error(), "skills/skill0/SKILL.md") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("invalid shared skill: %v", err)
		}
	}
}

func TestAntigravityLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, way := range []string{"uninstall", "deselect", "last supported hook"} {
		t.Run(way, func(t *testing.T) {
			root, loader, app := uninstallFixtureWithHooks(t, []string{"antigravity", "codex"}, firstSource)
			state, err := dependency.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			state.Project.Freshness = "none"
			if err := dependency.WriteState(root, state); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(root, ".agent/hooks.json"), []byte("{\"user\":{}}\n"), 0644)
			writeFixture(t, filepath.Join(root, "AGENTS.md"), []byte("user instructions\n"), 0644)
			initRepository(t, root)
			realizeProject(t, app, root)
			executable := ".agent/hooks/acr__example__first__session/session.sh"
			hook := readProjectFile(t, root, executable)
			writeFixture(t, filepath.Join(root, executable), []byte("edited\n"), 0755)
			result, err := app.service.Run(ctx, root, nil, realize.ModeCheck)
			if err == nil && !result.Plan.HasChanges() {
				t.Fatal("check missed hook drift")
			}
			writeFixture(t, filepath.Join(root, executable), hook, 0755)
			switch way {
			case "uninstall":
				_, err = app.service.Uninstall(ctx, root, firstSource, false)
			case "deselect":
				state, err = dependency.LoadState(root)
				if err != nil {
					t.Fatal(err)
				}
				state.Project.Agents = []string{"codex"}
				if err = dependency.WriteState(root, state); err != nil {
					t.Fatal(err)
				}
				_, err = app.service.Run(ctx, root, nil, realize.ModeApply)
			case "last supported hook":
				pkg := loader.packages[firstSource]
				pkg.manifest.Artifacts.Hooks[0].Event = manifest.HookSessionEnd
				loader.packages[firstSource] = pkg
				result, err = app.service.Run(ctx, root, nil, realize.ModeApply)
				if err == nil && len(result.Notices) != 1 {
					t.Fatalf("notices: %+v", result.Notices)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			requireExists(t, root, false, executable, ".agent/hooks/acr-hook.py")
			config := string(readProjectFile(t, root, ".agent/hooks.json"))
			if strings.Contains(config, "acr__") || !strings.Contains(config, "\"user\"") || !strings.Contains(string(readProjectFile(t, root, "AGENTS.md")), "user instructions") {
				t.Fatal("user ownership lost", config)
			}
		})
	}
}

func TestAntigravityPreservesEmptyHookConfig(t *testing.T) {
	ctx := context.Background()
	root, _, app := uninstallFixtureWithHooks(t, []string{"codex", "antigravity"}, firstSource)
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Freshness = "none"
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	const target = ".agent/hooks.json"
	empty := []byte(" { \n}\n")
	writeFixture(t, filepath.Join(root, target), empty, 0640)
	const alias = ".agents/hooks.json"
	null := []byte(" null\n")
	writeFixture(t, filepath.Join(root, alias), null, 0640)
	initRepository(t, root)
	realizeProject(t, app, root)
	if got := string(readProjectFile(t, root, target)); !strings.Contains(got, "acr__example__first__session") {
		t.Fatalf("hook missing from %s", got)
	}
	if result, err := app.service.Run(ctx, root, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
		t.Fatalf("realized config not current: %v", err)
	}
	if _, err := app.service.Uninstall(ctx, root, firstSource, false); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, target); !reflect.DeepEqual(got, empty) {
		t.Fatalf("uninstall changed empty config: %q", got)
	}
	info, err := os.Stat(filepath.Join(root, target))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("config mode after uninstall: %v %v", info, err)
	}
	info, err = os.Stat(filepath.Join(root, alias))
	if err != nil || info.Mode().Perm() != 0640 || !reflect.DeepEqual(readProjectFile(t, root, alias), null) {
		t.Fatalf("null alias bytes or mode changed: %v %v", info, err)
	}
}

// Each malformed case was replayed through agy 1.5.14 plugin validate, which
// reported that the loader skips the whole file. A repeated member is refused
// whether or not agy loads it.
func TestAntigravityRefusesUnreadableHookSibling(t *testing.T) {
	ctx := context.Background()
	const malformed = `.agent/hooks.json: Antigravity skips the entire file when hook "old_user_setting" is malformed: `
	const repeated = `.agent/hooks.json: hook "old_user_setting" repeats one member as `
	for _, tc := range []struct{ hook, detail string }{
		{`5`, "hook cannot be a JSON number"},
		{`{"enabled":"false"}`, "cannot be a JSON string"},
		{`{"SessionStart":5}`, "SessionStart cannot be a JSON number"},
		{`{"stop":{}}`, "cannot be a JSON object"},
		{`{"PreInvocation":5}`, "PreInvocation cannot be"},
		{`{"PreToolUse":[5]}`, "cannot be a JSON number"},
		{`{"PreToolUse":[{"matcher":5}]}`, "matcher cannot be"},
		{`{"PostToolUse":[{"hooks":{}}]}`, "hooks cannot be"},
		{`{"SessionStart":[{"command":5}]}`, "command cannot be a JSON number"},
		{`{"Stop":[null]}`, "Stop handler needs a nonempty command"},
		{`{"Stop":[{"matcher":"*","hooks":[{"command":"x"}]}]}`, "needs a nonempty command"},
		{`{"enabled":false,"PostInvocation":[{}]}`, "PostInvocation handler needs"},
		{`{"PreToolUse":[{"hooks":[{"command":""}]}]}`, "PreToolUse handler needs"},
		{`{"PreToolUse":[{"hooks":[{"command":"echo c"}],"Hooks":[{}]}]}`, `"Hooks" and "hooks"`},
		{`{"PostToolUse":[{"hooks":[{"command":"echo c"}],"Hooks":[{"timeout":5}]}]}`, `"Hooks" and "hooks"`},
		{`{"Stop":[{"command":"x"}],"stop":[{}]}`, `"Stop" and "stop"`},
		{`{"Stop":[""],"stop":[]}`, `"Stop" and "stop"`},
		{`{"Stop":[{"command":"x"}],"stop":[{"command":"y"}]}`, `"Stop" and "stop"`},
		{`{"enabled":false,"Enabled":true}`, `"Enabled" and "enabled"`},
		{`{"PostInvocation":[{"command":"x","Command":""}]}`, `"Command" and "command"`},
		{`{"PreToolUse":[{"matcher":"a","hooks":[{"command":"x"}],"hookſ":[{}]}]}`, `"hooks" and "hookſ"`},
		{`{"Stop":[{"command":"x","timeout":1e2}]}`, "timeout cannot be"},
		{`{"Stop":[{"command":"x","timeout":2147483648}]}`, "timeout cannot be"},
		{`{"Stop":[{"type":"http","command":"x"}]}`, `type "http" must be command or prompt`},
		{`{"Stop":[{"prompt":"p"}]}`, "needs a nonempty command"},
		{`{"Stop":[{"command":"x","model":"pro"}]}`, "cannot set prompt or model"},
		{`{"Stop":[{"type":"prompt"}]}`, "needs a nonempty prompt and no command"},
		{`{"Stop":[{"type":"prompt","prompt":"p","model":"FLAſH"}]}`, "must be inherit, flash, pro, or flash_lite"},
		{`{"PostToolUse":[{"hooks":[{"type":"prompt","prompt":"p"}]}]}`, "cannot have type prompt"},
	} {
		t.Run(tc.hook, func(t *testing.T) {
			for _, realized := range []bool{false, true} {
				root, _, app := uninstallFixtureWithHooks(t, []string{"codex", "antigravity"}, firstSource)
				initRepository(t, root)
				config := filepath.Join(root, ".agent/hooks.json")
				content := `{"old_user_setting":` + tc.hook + `}`
				if realized {
					writeFixture(t, config, []byte(`{"old_user_setting":{}}`), 0640)
					realizeProject(t, app, root)
					content = strings.Replace(string(readProjectFile(t, root, ".agent/hooks.json")), `"old_user_setting":{}`, `"old_user_setting":`+tc.hook, 1)
				}
				writeFixture(t, config, []byte(content), 0640)
				before := hashProjectTree(t, root)
				for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
					_, err := app.service.Run(ctx, root, nil, mode)
					prefix := adapter.CodeInvalidNativeEvent + ": " + malformed
					if strings.Contains(tc.detail, `" and "`) {
						prefix = adapter.CodeDuplicateConfigEntry + ": " + repeated + tc.detail + ", and Antigravity ignores letter case. Keep one spelling, then run acr realize --dry-run"
					}
					var native *adapter.NativeValidationError
					if !errors.As(err, &native) || !strings.HasPrefix(native.Error(), prefix) || !strings.Contains(native.Message, tc.detail) || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
						t.Fatalf("invalid native hook sibling (realized=%v) in %s: %v", realized, mode, err)
					}
				}
				if info, err := os.Stat(config); err != nil || info.Mode().Perm() != 0640 {
					t.Fatalf("refusal changed the config mode: %v %v", info, err)
				}
			}
		})
	}
}

// agy 1.5.14 loads each of these, with at most a warning about an ignored field.
func TestAntigravityPreservesReadableHookSiblings(t *testing.T) {
	for _, hook := range []string{
		`null`,
		`{"enabled":null,"Stop":null,"PreToolUse":[null,{},{"hooks":null}]}`,
		`{"ENABLED":false,"SessionEnd":5,"$schema":5}`,
		`{"Stop":[{"Command":"x","type":"","prompt":"","model":null,"timeout":-1,"other":5}]}`,
		`{"other":1e400}`,
		`{"unknown":{"type":1,"Type":2},"Stop":[{"command":"x","note":[{"model":1,"Model":1e400}],"other":1,"Other":2}]}`,
		`{"PostToolUse":[{"matcher":"(","hooks":[{"command":" "}]}]}`,
		`{"PreToolUse":[{"hooks":[{"type":"prompt","prompt":"p","model":" Flash_Lite"}]}],"PostInvocation":[{"type":"prompt","prompt":"p","command":""}]}`,
	} {
		t.Run(hook, func(t *testing.T) {
			root, _, app := uninstallFixtureWithHooks(t, []string{"codex", "antigravity"}, firstSource)
			state, err := dependency.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			state.Project.Freshness = "none"
			if err := dependency.WriteState(root, state); err != nil {
				t.Fatal(err)
			}
			user := []byte(`{ "user" : ` + hook + ` }` + "\n")
			writeFixture(t, filepath.Join(root, ".agent/hooks.json"), user, 0640)
			initRepository(t, root)
			realizeProject(t, app, root)
			if got := string(readProjectFile(t, root, ".agent/hooks.json")); !strings.Contains(got, hook) || !strings.Contains(got, "acr__example__first__session") {
				t.Fatalf("realize changed a valid user hook: %s", got)
			}
			if result, err := app.service.Run(context.Background(), root, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
				t.Fatalf("valid user hook is not current: %v", err)
			}
			if _, err := app.service.Uninstall(context.Background(), root, firstSource, false); err != nil {
				t.Fatal(err)
			}
			if got := readProjectFile(t, root, ".agent/hooks.json"); !reflect.DeepEqual(got, user) {
				t.Fatalf("uninstall changed a valid user hook: %q", got)
			}
		})
	}
}

func TestAntigravityRefusesAliasHookNames(t *testing.T) {
	ctx := context.Background()
	for _, dir := range []string{".agents", "_agent", "_agents"} {
		t.Run(dir, func(t *testing.T) {
			root, _, app := uninstallFixtureWithHooks(t, []string{"codex", "antigravity"}, firstSource)
			initRepository(t, root)
			target := filepath.Join(root, dir, "hooks.json")
			const userConfig = `{"user-session":{"SessionStart":[{"command":"printf '{}'"}]}}`
			writeFixture(t, target, []byte(userConfig), 0640)
			realizeProject(t, app, root)
			if got := string(readProjectFile(t, root, filepath.ToSlash(filepath.Join(dir, "hooks.json")))); got != userConfig {
				t.Fatalf("alias changed: %s", got)
			}
			writeFixture(t, target, []byte(strings.Replace(userConfig, "user-session", "acr__example__first__session", 1)), 0640)
			before := hashProjectTree(t, root)
			for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
				_, err := app.service.Run(ctx, root, nil, mode)
				if err == nil || !strings.Contains(err.Error(), dir+"/hooks.json") || !strings.Contains(err.Error(), "rename the user hook") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
					t.Fatalf("alias collision %v: %v", mode, err)
				}
			}
		})
	}
}

func TestAntigravityRefusesCarriedCursorHooks(t *testing.T) {
	root, loader, app := uninstallFixtureWithHooks(t, []string{"claude-code", "cursor"}, firstSource)
	pkg := loader.packages[firstSource]
	pkg.manifest.Artifacts.Rules = nil
	loader.packages[firstSource] = pkg
	initRepository(t, root)
	realizeProject(t, app, root)
	before := hashProjectTree(t, root)
	for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
		if _, err := app.service.Run(context.Background(), root, []string{"antigravity"}, mode); err == nil || !strings.Contains(err.Error(), "Claude imports") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("carried Cursor/Claude duplicate in %s: %v", mode, err)
		}
	}
}

func TestAntigravityCoSelectsFreshnessWithCursorAndClaude(t *testing.T) {
	root, _, service := sharedSkillFixture(t, []string{"claude-code", "cursor", "antigravity"}, false)
	initRepository(t, root)
	ctx := context.Background()
	if _, err := service.Run(ctx, root, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".claude/settings.json", ".cursor/hooks.json", ".agent/hooks.json"} {
		if !strings.Contains(string(readProjectFile(t, root, name)), "freshness-session-start") {
			t.Fatalf("freshness hook missing from %s", name)
		}
	}
	if result, err := service.Run(ctx, root, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
		t.Fatalf("freshness check: %v", err)
	}
}

func TestAntigravityRefusesCarriedCursorRules(t *testing.T) {
	root, _, app := uninstallFixture(t, []string{"cursor"}, firstSource)
	initRepository(t, root)
	realizeProject(t, app, root)
	before := hashProjectTree(t, root)
	_, err := app.service.Run(context.Background(), root, []string{"codex", "antigravity"}, realize.ModeApply)
	if err == nil || !strings.Contains(err.Error(), ".cursor/rules") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
		t.Fatalf("carried Cursor rule: %v", err)
	}
}

func TestCursorRefusesCarriedAntigravityRuleDelivery(t *testing.T) {
	root, _, app := uninstallFixture(t, []string{"codex", "antigravity"}, firstSource)
	initRepository(t, root)
	realizeProject(t, app, root)
	before := hashProjectTree(t, root)
	for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
		_, err := app.service.Run(context.Background(), root, []string{"cursor"}, mode)
		if err == nil || !strings.Contains(err.Error(), ".cursor/rules") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("carried root rule %v: %v", mode, err)
		}
	}
}

func TestAntigravityTemporaryDelegatedSelection(t *testing.T) {
	ctx := context.Background()
	root, _, app := uninstallFixture(t, []string{"codex"}, firstSource)
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Freshness = "none"
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	initRepository(t, root)
	realizeProject(t, app, root)
	before := hashProjectTree(t, root)
	temporary := []string{"codex", "antigravity"}
	for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
		_, err := app.service.Run(ctx, root, temporary, mode)
		if err == nil || !strings.Contains(err.Error(), "Add antigravity to agents.yaml") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("unrecordable temporary selection in %s: %v", mode, err)
		}
	}
	state, err = dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Agents = temporary
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	realizeProject(t, app, root)
	before = hashProjectTree(t, root)
	for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
		_, err := app.service.Run(ctx, root, []string{"cursor"}, mode)
		if err == nil || !strings.Contains(err.Error(), ".cursor/rules") || !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("duplicate after persisting Antigravity in %s: %v", mode, err)
		}
	}
}

func TestAntigravityUnsupportedSkillRefusal(t *testing.T) {
	root, _, service := sharedSkillFixture(t, []string{"antigravity"}, false, firstSource)
	initRepository(t, root)
	before := hashProjectTree(t, root)
	for _, command := range []string{"realize", "check"} {
		stdout, stderr, exit := runCLI(t, &Application{service: service}, command, "--project", root, "--json")
		var reply struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal([]byte(stderr), &reply); err != nil {
			t.Fatalf("%s: %v (%s)", command, err, stderr)
		}
		if exit != 4 || stdout != "" || reply.Error.Code != "unsupported_adapter_capability" || !strings.Contains(reply.Error.Message, "Enable sharedSkills: true") {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", command, exit, stdout, stderr)
		}
		if !reflect.DeepEqual(before, hashProjectTree(t, root)) {
			t.Fatalf("%s changed project files after refusing the skill", command)
		}
	}
}

func TestAntigravitySkippedHookRecovery(t *testing.T) {
	for _, tc := range []struct {
		name             string
		agents, selected []string
		want             string
	}{
		{"active", []string{"codex", "antigravity"}, nil, "ACR registers this hook for codex in this realization."},
		{"filtered", []string{"codex", "antigravity"}, []string{"antigravity"}, "Run acr realize without --agent to register this hook for the compatible agents already in agents.yaml."},
		{"standalone", []string{"antigravity"}, nil, "Add claude-code, codex, or cursor to agents.yaml and run acr realize to register this hook for that agent."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, loader, app := uninstallFixtureWithHooks(t, tc.agents, firstSource)
			pkg := loader.packages[firstSource]
			hookPath := pkg.manifest.Artifacts.Hooks[0].Path
			pkg.manifest.Artifacts.Rules = nil
			pkg.manifest.Artifacts.Hooks = []manifest.HookArtifact{
				{ID: "end", Path: hookPath, Event: manifest.HookSessionEnd},
				{ID: "prompt", Path: hookPath, Event: manifest.HookUserPromptSubmit},
			}
			loader.packages[firstSource] = pkg
			initRepository(t, root)
			for _, command := range []string{"realize", "check"} {
				args := []string{command, "--project", root, "--json"}
				for _, agent := range tc.selected {
					args = append(args, "--agent", agent)
				}
				stdout, stderr, exit := runCLI(t, app, args...)
				if exit != 0 {
					t.Fatalf("%s: exit %d: %s", command, exit, stderr)
				}
				var reply struct {
					OK     bool
					Result Result
				}
				if err := json.Unmarshal([]byte(stdout), &reply); err != nil || !reply.OK || (command == "check" && reply.Result.Plan.HasChanges()) {
					t.Fatalf("%s: unexpected JSON result: %s (%v)", command, stdout, err)
				}
				var skipped []string
				for _, notice := range reply.Result.Notices {
					if !slices.Contains(cli.NoticeCodes, notice.Code) {
						t.Fatalf("%s emitted unregistered success notice %q", command, notice.Code)
					}
					if notice.Code == "adapter_hook_skipped" {
						skipped = append(skipped, notice.Message)
					}
				}
				want := []string{
					"Antigravity skipped " + firstSource + " hook \"end\" (session-end) because the event has no native equivalent. " + tc.want,
					"Antigravity skipped " + firstSource + " hook \"prompt\" (user-prompt-submit) because the event has no native equivalent. " + tc.want,
				}
				if !slices.Equal(skipped, want) {
					t.Fatalf("%s: skip notices = %q, want %q", command, skipped, want)
				}
			}
			if tc.name == "active" {
				config := string(readProjectFile(t, root, ".codex/config.toml"))
				if !strings.Contains(config, "hooks.SessionEnd") || !strings.Contains(config, "hooks.UserPromptSubmit") {
					t.Fatal("Codex is active but did not register both hooks")
				}
			}
		})
	}
}

func TestAntigravityMultipleCompanionRecovery(t *testing.T) {
	ctx := context.Background()
	for _, carried := range []string{"", "claude-code", "cursor"} {
		for _, remove := range []string{"claude-code", "cursor"} {
			t.Run("carried="+carried+"/remove="+remove, func(t *testing.T) {
				root, loader, app := uninstallFixtureWithHooks(t, []string{"antigravity", "claude-code", "cursor"}, firstSource)
				pkg := loader.packages[firstSource]
				pkg.manifest.Artifacts.Rules = nil
				hookPath := pkg.manifest.Artifacts.Hooks[0].Path
				pkg.manifest.Artifacts.Hooks = append(pkg.manifest.Artifacts.Hooks,
					manifest.HookArtifact{ID: "end", Path: hookPath, Event: manifest.HookSessionEnd},
					manifest.HookArtifact{ID: "prompt", Path: hookPath, Event: manifest.HookUserPromptSubmit})
				loader.packages[firstSource] = pkg
				initRepository(t, root)
				if carried != "" {
					if _, err := app.service.Run(ctx, root, []string{carried}, realize.ModeApply); err != nil {
						t.Fatal(err)
					}
				}
				for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck} {
					result, err := app.service.Run(ctx, root, []string{"antigravity"}, mode)
					if err != nil {
						t.Fatal(err)
					}
					var skipped int
					for _, notice := range result.Notices {
						if notice.Code != adapter.CodeHookSkipped {
							continue
						}
						skipped++
						if !strings.Contains(notice.Message, "remove claude-code or cursor from agents.yaml and run acr realize without --agent") {
							t.Fatalf("unusable recovery in %s: %s", mode, notice.Message)
						}
					}
					if skipped != 2 {
						t.Fatalf("%d skip notices in %s", skipped, mode)
					}
				}
				state, err := dependency.LoadState(root)
				if err != nil {
					t.Fatal(err)
				}
				state.Project.Agents = slices.DeleteFunc(state.Project.Agents, func(id string) bool { return id == remove })
				if err := dependency.WriteState(root, state); err != nil {
					t.Fatal(err)
				}
				if _, err := app.service.Run(ctx, root, nil, realize.ModeApply); err != nil {
					t.Fatalf("advertised recovery failed: %v", err)
				}
				if result, err := app.service.Run(ctx, root, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
					t.Fatalf("recovered project is not current: %v", err)
				}
				kept, removed, events := ".cursor/hooks.json", ".claude/settings.json", []string{"sessionEnd", "beforeSubmitPrompt"}
				if remove == "cursor" {
					kept, removed, events = removed, kept, []string{"SessionEnd", "UserPromptSubmit"}
				}
				config := string(readProjectFile(t, root, kept))
				for _, event := range events {
					if !strings.Contains(config, event) {
						t.Fatalf("%s did not register %s", kept, event)
					}
				}
				if data, err := os.ReadFile(filepath.Join(root, removed)); err == nil && strings.Contains(string(data), "acr__") {
					t.Fatalf("%s kept duplicate hooks", removed)
				} else if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			})
		}
	}
}
