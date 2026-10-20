package producerconvert

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const fleetCaller = `name: Publish
on:
  push:
    branches: [main]
permissions:
  contents: write
  id-token: write
  pull-requests: write
jobs:
  publish:
    uses: jbaruch/coding-policy/.github/workflows/publish-plugin.yml@af116ebf18a7c46a672bf176064908736bc8ac28
    secrets:
      TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}
    with:
      pre-publish-script: .github/scripts/gate.sh
      python-version: '3.12'
      stamp-changelog: true
      skill-review-credit-outage: skip
`

func fleetFixture(t *testing.T, workflow string) (string, Options) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(root+"/.git", 0755); err != nil {
		t.Fatal(err)
	}
	put(t, root, ".tessl-plugin/plugin.json", `{"name":"other/producer","version":"1.2.3","skills":["skills/example"]}`, 0644)
	put(t, root, "skills/example/SKILL.md", "# Example\nIndependent skill.\n", 0644)
	put(t, root, ".github/scripts/gate.sh", "#!/bin/bash\nprintf 'original-gate\\n'\n", 0755)
	put(t, root, ".github/workflows/release.yml", workflow, 0644)
	put(t, root, ".github/workflows/tests.yml", "on: pull_request\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: bash .github/scripts/gate.sh\n", 0644)
	put(t, root, ".github/workflows/review.yml", "on: pull_request\njobs:\n  review:\n    uses: other/review/.github/workflows/review.yml@v1\n", 0644)
	return root, Options{PackageRoot: root, Repository: "https://github.com/other/converted"}
}

func TestFleetPublisherTranslationPreservesOriginalGate(t *testing.T) {
	for _, python := range []bool{true, false} {
		caller := fleetCaller
		script := ".github/scripts/gate.sh"
		if !python {
			script = ".github/scripts/other 'gate $(touch injected).sh"
			caller = strings.ReplaceAll(caller, ".github/scripts/gate.sh", script)
			caller = strings.ReplaceAll(caller, "      python-version: '3.12'\n", "")
		}
		root, opts := fleetFixture(t, caller+independentTestJob)
		if !python {
			put(t, root, script, "#!/bin/bash\nprintf 'original-gate\\n'\n", 0755)
		}
		before := treeAt(t, root)
		plan, err := Prepare(opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Report.PolicyChanges) != 1 {
			t.Fatal("paid service retirement not disclosed")
		}
		if _, err := plan.Apply(); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{script, ".github/workflows/tests.yml", ".github/workflows/review.yml"} {
			after := treeAt(t, root)[name]
			if before[name].Digest != after.Digest || before[name].Mode != after.Mode {
				t.Fatalf("independent bytes/mode changed: %s", name)
			}
		}
		if !strings.Contains(read(t, root, ".github/workflows/release.yml"), independentTestJob) {
			t.Fatal("sibling job changed")
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(read(t, root, publishWorkflowPath)), &doc); err != nil {
			t.Fatal(err)
		}
		jobs := member(doc.Content[0], "jobs")
		publish := member(jobs, "publish")
		gate := member(jobs, "pre-publish")
		needs := member(publish, "needs")
		if needs == nil || len(needs.Content) != 1 || needs.Content[0].Value != "pre-publish" {
			t.Fatal("publication does not require gate")
		}
		if scalar(publish, "uses") != "jbaruch/agentic-context-registry/.github/workflows/publish-package.yml@d3bc96b33b42293aecd1702c04aa94513a3dab1b" || scalar(member(publish, "with"), "path") != "." || scalar(member(publish, "with"), "acr-version") != "v0.1.6" {
			t.Fatal("publisher contract drift")
		}
		steps := member(gate, "steps")
		want := 2
		if python {
			want = 3
		}
		if len(steps.Content) != want {
			t.Fatal("incorrect gate setup")
		}
		if python && (scalar(steps.Content[1], "uses") != gatePython || scalar(member(steps.Content[1], "with"), "python-version") != "3.12") {
			t.Fatal("original Python lost")
		}
		command := exec.Command("bash", "-c", scalar(steps.Content[len(steps.Content)-1], "run"))
		command.Dir = root
		output, e := command.CombinedOutput()
		if e != nil || string(output) != "original-gate\n" {
			t.Fatalf("literal gate invocation failed: %v %s", e, output)
		}
		if _, e := os.Stat(root + "/injected"); !os.IsNotExist(e) {
			t.Fatal("gate path evaluated shell syntax")
		}
	}
}

func TestFleetPublisherUnsupportedContractsRefuse(t *testing.T) {
	mutations := map[string]string{
		"unknown-sha":     strings.ReplaceAll(fleetCaller, "af116ebf18a7c46a672bf176064908736bc8ac28", "main"),
		"invented-target": strings.ReplaceAll(fleetCaller, "publish-plugin.yml", "publish-package.yml"),
		"inherit":         strings.ReplaceAll(fleetCaller, "secrets:\n      TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}", "secrets: inherit"),
		"unknown-secret":  strings.ReplaceAll(fleetCaller, "TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}", "TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}\n      OTHER: ${{ secrets.OTHER }}"),
		"expression":      strings.ReplaceAll(fleetCaller, ".github/scripts/gate.sh", "${{ inputs.gate }}"),
		"missing":         strings.ReplaceAll(fleetCaller, ".github/scripts/gate.sh", "missing.sh"),
		"escape":          strings.ReplaceAll(fleetCaller, ".github/scripts/gate.sh", "../gate.sh"),
		"extra-input":     fleetCaller + "      unexpected: true\n",
		"policy":          strings.ReplaceAll(fleetCaller, "  publish:\n", "  publish:\n    environment: production\n"),
		"concurrency":     "concurrency: release\n" + fleetCaller,
		"permissions":     strings.ReplaceAll(fleetCaller, "contents: write", "contents: read"),
		"needs":           fleetCaller + strings.ReplaceAll(independentTestJob, "  test:\n", "  test:\n    needs: publish\n"),
	}
	for name, caller := range mutations {
		t.Run(name, func(t *testing.T) {
			root, opts := fleetFixture(t, caller)
			before := treeAt(t, root)
			if _, err := Prepare(opts); err == nil {
				t.Fatal("unsupported contract accepted")
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("refusal changed source")
			}
		})
	}
	// Detection is identity-based even when no literal Tessl secret appears.
	if !workflowSemantic([]byte(mutations["inherit"])) {
		t.Fatal("inherited caller escaped detection")
	}
	if err := preserveChecks(".github/workflows/release.yml", []byte(fleetCaller), []byte(mutations["invented-target"])); err == nil {
		t.Fatal("model-invented target accepted")
	}
	if err := preserveChecks(".github/workflows/release.yml", []byte(fleetCaller), []byte(strings.ReplaceAll(fleetCaller, "      pre-publish-script: .github/scripts/gate.sh\n", ""))); err == nil {
		t.Fatal("model dropped original gate")
	}
}

func TestFleetPublisherSemanticReplanDisclosesRetirement(t *testing.T) {
	root, opts := fleetFixture(t, fleetCaller)
	opts.Agent = "codex"
	name := "skills/example/helper.sh"
	old := "#!/bin/sh\ntessl install other/producer\n"
	put(t, root, name, old, 0755)
	p := proposal{Edits: []proposedEdit{{Path: name, BeforeDigest: digest([]byte(old)), Action: "replace", Content: "#!/bin/sh\necho migrated\n"}}}
	plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Report.PolicyChanges) != 1 {
		t.Fatal("deterministic retirement missing after semantic replan")
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
}
