package producerconvert

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Executable examples a reader copies out of a skill instruction and runs. Each
// form reads the package version through the retired metadata directory joined
// separately from plugin.json, so a whole-file literal scan never sees the
// manifest path. The examples run for real before and after conversion.
type instructionForm struct {
	name    string
	body    string
	command func(t *testing.T, body string) []string
}

const instructionSplit = "Path('plugins/orbit') / '.tessl-plugin' / 'plugin.json'"

func fencedCommand(open, close string, interpreter ...string) func(*testing.T, string) []string {
	return func(t *testing.T, body string) []string {
		t.Helper()
		_, rest, found := strings.Cut(body, open)
		if !found {
			t.Fatalf("missing %q", open)
		}
		code, _, found := strings.Cut(rest, close)
		if !found {
			t.Fatalf("unterminated %q", close)
		}
		return append(append([]string{}, interpreter...), code)
	}
}

var instructionForms = []instructionForm{
	{
		name:    "fenced backticks python",
		body:    "# Check\nRun this Python example from the repository root to read the package version:\n\n```python\nimport json\nfrom pathlib import Path\np = " + instructionSplit + "\nprint(json.loads(p.read_text())['version'])\n```\n",
		command: fencedCommand("```python\n", "```", "python3", "-B", "-c"),
	},
	{
		name:    "fenced tildes shell",
		body:    "# Check\n\nFrom the repository root:\n\n~~~sh\npython3 -c \"import json;print(json.load(open('plugins/orbit/.tessl-plugin' '/plugin.json'))['version'])\"\n~~~~\n",
		command: fencedCommand("~~~sh\n", "~~~", "sh", "-c"),
	},
	{
		name: "indented python",
		body: "# Check\n\nRun from the repository root:\n\n    import json\n    from pathlib import Path\n    p = " + instructionSplit + "\n    print(json.loads(p.read_text())['version'])\n\nThat prints the version.\n",
		command: func(t *testing.T, body string) []string {
			var code []string
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "    ") {
					code = append(code, line[4:])
				}
			}
			return []string{"python3", "-B", "-c", strings.Join(code, "\n") + "\n"}
		},
	},
	{
		name:    "inline command",
		body:    "# Check\n\nRun `python3 -c \"import json;print(json.load(open('plugins/orbit/.tessl-plugin' + '/plugin.json'))['version'])\"` from the repository root.\n",
		command: fencedCommand("`", "`", "sh", "-c"),
	},
}

func runInstruction(t *testing.T, root string, argv []string) (string, error) {
	t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = root
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	return string(output), err
}

func refusedInstruction(t *testing.T, root, name string, before tree, report Report, err error) {
	t.Helper()
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || refusal.Path != name || report.Wrote {
		t.Fatalf("executable instruction was not refused: %+v %v", report, err)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("refusal changed source")
	}
	absent(t, root, ReceiptPath)
	absent(t, root, transactionPath)
}

func TestExecutableInstructionFormsRefuseBeforeWrites(t *testing.T) {
	for _, form := range instructionForms {
		t.Run(form.name, func(t *testing.T) {
			root, opts := fixture(t)
			name := "plugins/orbit/skills/check/SKILL.md"
			put(t, root, name, form.body, 0o644)
			if output, err := runInstruction(t, root, form.command(t, form.body)); err != nil || output != "2.3.4\n" {
				t.Fatalf("original example: %q %v", output, err)
			}
			before := treeAt(t, root)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				refusedInstruction(t, root, name, before, report, err)
			}
			if output, err := runInstruction(t, root, form.command(t, read(t, root, name))); err != nil || output != "2.3.4\n" {
				t.Fatalf("example after refusal: %q %v", output, err)
			}
		})
	}
}

func TestExecutableInstructionBlockEdgesRefuseBeforeWrites(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unclosed fence", "# Check\n\n```python\np = " + instructionSplit + "\n"},
		{"fence inside list item", "- Run it:\n\n  ```sh\n  cat plugins/orbit/'.tessl-plugin'/plugin.json\n  ```\n"},
		{"tab indented code", "# Check\n\n\tcat plugins/orbit/'.tessl-plugin'/plugin.json\n"},
		{"indented code after heading", "# Check\n    cat plugins/orbit/'.tessl-plugin'/plugin.json\n"},
		{"nested indented code in list item", "- Step one\n\n        cat plugins/orbit/'.tessl-plugin'/plugin.json\n"},
		{"double backtick inline command", "Run ``cat plugins/orbit/'.tessl-plugin'/plugin.json`` first.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			name := "plugins/orbit/skills/check/SKILL.md"
			put(t, root, name, tc.body, 0o644)
			before := treeAt(t, root)
			report, err := Convert(opts)
			refusedInstruction(t, root, name, before, report, err)
		})
	}
}

func TestInstructionProseAndNoticesStayOrdinary(t *testing.T) {
	root, opts := fixture(t)
	bodies := map[string]string{
		"plugins/orbit/skills/check/SKILL.md":  "# Check\n\nHistorical Tessl metadata lived in the `.tessl-plugin` directory beside `plugin.json`.\n\n- Step one\n\n    The retired .tessl-plugin directory is history, not a runtime path.\n\n```sh\nprintf 'no metadata dependency\\n'\n```\n",
		"plugins/orbit/skills/check/notes.md":  "# Notes\n    .tessl-plugin-backup/plugin.json is a distinct directory kept by hand.\n",
		"plugins/orbit/skills/check/NOTICE.md": "Attribution for the original author.\n\n```text\n.tessl-plugin/README documented the first release.\n```\n",
		"plugins/orbit/README.md":              "See https://example.test/archive/.tessl-plugin and the `.tessl-plugin` history section.\n",
	}
	for name, body := range bodies {
		put(t, root, name, body, 0o644)
	}
	report, err := Convert(opts)
	if err != nil || !report.Wrote {
		t.Fatalf("ordinary prose, mention or notice blocked: %+v %v", report, err)
	}
	for name, body := range bodies {
		if read(t, root, name) != body {
			t.Fatalf("prose/notice changed: %s", name)
		}
	}
}

func TestCorrection14MarkdownExclusionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refuse     bool
	}{
		{"quote", "> Run `echo .tessl-plugin`.\n", false},
		{"quote fence", "> ```sh\n> echo .tessl-plugin\n> ```\n", false},
		{"html", "<div>\nRun `echo .tessl-plugin`.\n</div>\n", false},
		{"html in list", "- Note:\n\n  <div>\n  Run `echo .tessl-plugin`.\n  </div>\n", false},
		{"quote ends", "> historical\n\nRun `echo .tessl-plugin`.\n", true},
		{"html ends", "<div>\nordinary\n</div>\n\nRun `echo .tessl-plugin`.\n", true},
		{"inline", "Run `echo .tessl-plugin`.\n", true},
		{"fenced", "```sh\necho .tessl-plugin\n```\n", true},
		{"indented", "\n    echo .tessl-plugin\n", true},
		{"quote literal", "> Run `tessl install origin/demo`.\n", true},
		{"html literal", "<div>\ntessl install origin/demo\n</div>\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			name := "plugins/orbit/skills/check/SKILL.md"
			put(t, root, name, tc.body, 0644)
			original := treeAt(t, root)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				p, err := Prepare(opts)
				if tc.refuse {
					if err == nil {
						t.Fatal("executable or whole-text operation accepted")
					}
					if !matches(original, treeAt(t, root)) {
						t.Fatal("refusal mutation")
					}
					assertCorrection14NoResidue(t, root)
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if !matches(original, treeAt(t, root)) {
					t.Fatal("preview mutation")
				}
				if !dry {
					if _, err = p.Apply(); err != nil {
						t.Fatal(err)
					}
					assertCorrection14Applied(t, root, p)
					if read(t, root, name) != tc.body {
						t.Fatal("excluded context bytes changed")
					}
					current, err := Prepare(opts)
					if err != nil || !current.Report.Current {
						t.Fatalf("rerun %v", err)
					}
				}
			}
		})
	}
}
