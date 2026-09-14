package producerconvert

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

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
				plan, err := prepareDeterministic(opts, opts.Agent != "")
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

func TestCorrection14ResidualDigitVariable(t *testing.T) {
	for _, token := range []string{"TESSL_TOKEN_2", "TESSL_TOKEN2"} {
		t.Run(token, func(t *testing.T) {
			root, opts, p := semanticFixture(t)
			p.Edits[0].Content += "test -n \"$" + token + "\"\n"
			before := treeAt(t, root)
			plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
			if err == nil || !strings.Contains(err.Error(), "Tessl") || plan.Report.Wrote {
				t.Fatalf("residual accepted: %v", err)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("refusal changed source")
			}
			assertCorrection14NoResidue(t, root)
		})
	}
}
