package producerconvert

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var metadataSpellings = []struct {
	name, filename, body string
}{
	{"pathlib split", "metadata.py", `from pathlib import Path
metadata = Path(__file__).resolve().parents[2] / ".tessl-plugin" / "plugin.json"
`},
	{"python join", "metadata.py", `import os
metadata = os.path.join("..", '.tessl-plugin', 'plugin.json')
`},
	{"python windows", "metadata.py", `from pathlib import PureWindowsPath
metadata = PureWindowsPath(r"..\.tessl-plugin\plugin.json")
`},
	{"javascript join", "metadata.js", "const metadata = path.join(root, '.tessl-plugin', 'plugin.json');\n"},
	{"javascript windows", "metadata.js", `const metadata = "..\\.tessl-plugin\\plugin.json";
`},
	{"powershell join", "metadata.ps1", "$metadata = Join-Path (Join-Path $root '.tessl-plugin') 'plugin.json'\n"},
	{"go join", "metadata.go", "package helper\nimport \"path/filepath\"\nvar metadata = filepath.Join(root, `.tessl-plugin`, `plugin.json`)\n"},
	{"shell variable", "metadata.sh", "#!/bin/sh\nset -eu\ndir=\"$root/.tessl-plugin\"\ncat \"$dir/plugin.json\"\n"},
	{"contiguous control", "metadata.py", `from pathlib import Path
metadata = Path(__file__).resolve().parents[2] / ".tessl-plugin/plugin.json"
`},
}

func TestRetiredMetadataPathSpellingsRefuseBeforeWrites(t *testing.T) {
	for _, tc := range metadataSpellings {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			name := "plugins/orbit/skills/check/" + tc.filename
			put(t, root, name, tc.body, 0o644)
			before := treeAt(t, root)
			report, err := Convert(opts)
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || !strings.Contains(err.Error(), name) || report.Wrote {
				t.Fatalf("metadata dependency was not refused: report=%+v err=%v", report, err)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("refusal changed source")
			}
		})
	}
}

func TestRetiredMetadataProposalSpellingsRefuseBeforeWrites(t *testing.T) {
	for _, tc := range metadataSpellings {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			opts.Agent = "codex"
			name := "plugins/orbit/skills/check/" + tc.filename
			// The known contiguous dependency forces the semantic route even on
			// the old implementation. A proposal must not hide it by splitting it.
			original := strings.Replace(tc.body, ".tessl-plugin", ".tessl-plugin/plugin.json", 1)
			put(t, root, name, original, 0o644)
			before := treeAt(t, root)
			plan, err := prepareDeterministic(opts)
			if err == nil {
				t.Fatal("contiguous source did not require semantic conversion")
			}
			proposed := proposal{Edits: []proposedEdit{{Path: name, BeforeDigest: digest([]byte(original)), Action: "replace", Content: tc.body}}}
			_, err = validateProposal(context.Background(), plan, proposed)
			if err == nil || !strings.Contains(err.Error(), "candidate conversion") || !strings.Contains(err.Error(), name) {
				t.Fatalf("residual dependency was not rejected by candidate conversion: %v", err)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("proposal validation changed source")
			}
		})
	}
}

func TestRetiredMetadataSplitHelperPreservesBehavior(t *testing.T) {
	for _, expression := range []string{`Path(__file__).resolve().parents[2] / ".tessl-plugin" / "plugin.json"`, `Path(__file__).resolve().parents[2] / ".tessl-plugin/plugin.json"`} {
		t.Run(expression, func(t *testing.T) {
			root, opts := fixture(t)
			opts.Agent = "codex"
			name := "plugins/orbit/skills/check/metadata.py"
			original := "#!/usr/bin/env python3\nimport json\nfrom pathlib import Path\nmetadata = " + expression + "\nprint(json.loads(metadata.read_text())[\"version\"])\n"
			put(t, root, name, original, 0o751)
			run := func() {
				t.Helper()
				output, err := exec.Command(filepath.Join(root, name)).CombinedOutput()
				if err != nil || string(output) != "2.3.4\n" {
					t.Fatalf("direct helper: %q %v", output, err)
				}
			}
			run()
			next := strings.Replace(original, expression, `Path(__file__).with_name(".acr-package.json")`, 1)
			calls := 0
			plan, err := prepareWithProvider(context.Background(), opts, func(_ context.Context, agent, request string) (proposal, AgentRun, error) {
				calls++
				if agent != "codex" || !strings.Contains(request, name) {
					t.Fatal("source not submitted to selected provider")
				}
				return proposal{Edits: []proposedEdit{{Path: name, BeforeDigest: digest([]byte(original)), Action: "replace", Content: next}}}, AgentRun{}, nil
			})
			if err != nil || calls != 1 {
				t.Fatalf("semantic route: %v calls=%d", err, calls)
			}
			if _, err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			absent(t, root, "plugins/orbit/.tessl-plugin/plugin.json")
			run()
		})
	}
}

func TestRetiredMetadataMentionsPreserveProseAndNotices(t *testing.T) {
	root, opts := fixture(t)
	bodies := map[string]string{
		"plugins/orbit/skills/check/LICENSE":  "Copyright Original author. Historical `.tessl-plugin` metadata attribution.\n",
		"plugins/orbit/README.md":             "Historical `.tessl-plugin` directory; see https://example.test/archive/.tessl-plugin for provenance.\n",
		"plugins/orbit/skills/check/SKILL.md": "# Check\nHistorical Tessl metadata used the `.tessl-plugin` directory.\n",
		"plugins/orbit/skills/check/data.txt": "Tessl history. Distinct directory .tessl-plugin-backup stays useful.\n",
	}
	for name, body := range bodies {
		put(t, root, name, body, 0o644)
	}
	report, err := Convert(opts)
	if err != nil || !report.Wrote {
		t.Fatalf("ordinary prose or legal reference blocked: %+v %v", report, err)
	}
	for name, body := range bodies {
		if read(t, root, name) != body {
			t.Fatalf("prose/notice changed: %s", name)
		}
	}
}

func TestRetiredMetadataRuntimeScopesRefuseBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		executable bool
	}{
		{"plugins/orbit/skills/check/templates/README.md", "Read `root / '.tessl-plugin' / 'plugin.json'`.\n", false},
		{"plugins/orbit/skills/check/notes.md", "#!/bin/sh\nset -eu\ncat '.tessl-plugin'/'plugin.json'\n", true},
		{"tests/test_metadata.py", "metadata = root / '.tessl-plugin' / 'plugin.json'\n", false},
		{".github/workflows/metadata.yml", "on: push\njobs:\n  metadata:\n    runs-on: ubuntu-latest\n    steps:\n      - run: cat '.tessl-plugin'/'plugin.json'\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			opts.Agent = "codex"
			put(t, root, tc.name, tc.body, 0o644)
			if tc.executable {
				put(t, root, tc.name, tc.body, 0o755)
			}
			before := treeAt(t, root)
			plan, err := prepareDeterministic(opts)
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || !strings.Contains(err.Error(), tc.name) || plan.Report.Wrote {
				t.Fatalf("runtime scope bypassed semantic conversion: %v", err)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("runtime scope refusal changed source")
			}
		})
	}
}

func TestRetiredMetadataHookArgumentRefusesBeforeWrites(t *testing.T) {
	root, opts := fixture(t)
	name := "plugins/orbit/.tessl-plugin/plugin.json"
	plugin := read(t, root, name)
	plugin = strings.Replace(plugin, `"skills":`, `"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/start.sh",".tessl-plugin","plugin.json"]}]}]},"skills":`, 1)
	put(t, root, name, plugin, 0o644)
	put(t, root, "plugins/orbit/hooks/start.sh", "#!/bin/sh\nset -eu\nprintf 'hook-ok\\n'\n", 0o755)
	before := treeAt(t, root)
	report, err := Convert(opts)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || !strings.Contains(err.Error(), "hook argument") || report.Wrote {
		t.Fatalf("retired metadata hook argument accepted: %v", err)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("hook argument refusal changed source")
	}
}

func TestCorrection14TesslVariableFamily(t *testing.T) {
	for _, tc := range []struct {
		token  string
		refuse bool
	}{{"TESSL_TOKEN", true}, {"TESSL_TOKEN_2", true}, {"TESSL_TOKEN2", true}, {"TESSL_2", true}, {"TESSL__", true}, {"OTHER_TESSL_TOKEN2", false}, {"TESSL_TOKEN2suffix", false}, {"TESSL_", false}, {"tessl_token2", false}} {
		t.Run(tc.token, func(t *testing.T) {
			root, opts := fixture(t)
			name := "plugins/orbit/skills/check/check.sh"
			put(t, root, name, "#!/bin/sh\ntest -n \"$"+tc.token+"\"\n", 0751)
			before := treeAt(t, root)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				p, err := Prepare(opts)
				if tc.refuse {
					if err == nil {
						t.Fatal("literal Tessl variable accepted")
					}
					if !matches(before, treeAt(t, root)) {
						t.Fatal("refusal mutation")
					}
					assertCorrection14NoResidue(t, root)
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if !dry {
					if _, err = p.Apply(); err != nil {
						t.Fatal(err)
					}
					assertCorrection14Applied(t, root, p)
					current, err := Prepare(opts)
					if err != nil || !current.Report.Current {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
