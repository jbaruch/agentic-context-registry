package producerconvert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandalonePublishAndIndependentJobs(t *testing.T) {
	out, err := translateWorkflow([]byte(fixturePublisher), "plugins/orbit")
	if err != nil || out != nil {
		t.Fatalf("standalone publisher=%s %v", out, err)
	}
	for _, source := range []string{fixturePublisher + independentTestJob, strings.Replace(fixturePublisher, "  publish:", independentTestJob+"  publish:", 1)} {
		out, err := translateWorkflow([]byte(source), "plugins/orbit")
		if err != nil || !strings.Contains(string(out), independentTestJob) || strings.Contains(string(out), "patch-version-publish") {
			t.Fatalf("independent jobs=%s %v", out, err)
		}
	}
	comment := "\n  # Independent tests retain this explanation.\n"
	commented, err := translateWorkflow([]byte(fixturePublisher+comment+independentTestJob), "plugins/orbit")
	if err != nil || !strings.Contains(string(commented), comment+independentTestJob) {
		t.Fatalf("lost independent job comment: %s %v", commented, err)
	}
	if !strings.Contains(publishWorkflow, "push:\n    tags: ['v*']") || !strings.Contains(publishWorkflow, "path: .") || !strings.Contains(publishWorkflow, "acr-version: v0.1.6") || !strings.Contains(publishWorkflow, "@d3bc96b33b42293aecd1702c04aa94513a3dab1b") {
		t.Fatal("generated publication contract lost explicit pin, version or path")
	}
}

func TestWorkflowUnknownLogicRefuses(t *testing.T) {
	cases := map[string]string{
		"review":           strings.Replace(fixturePublisher, "tesslio/patch-version-publish@v1", "tesslio/skill-review@v1", 1),
		"trigger":          strings.Replace(fixturePublisher, "branches: [main]", "branches: [release]", 1),
		"path":             strings.Replace(fixturePublisher, "path: plugins/orbit", "path: ${{ inputs.path }}", 1),
		"checkout-options": strings.Replace(fixturePublisher, "      - uses: actions/checkout@v4", "      - uses: actions/checkout@v4\n        with:\n          ref: main", 1),
		"permission":       strings.Replace(fixturePublisher, "  contents: write", "  id-token: write", 1),
		"extra-field":      strings.Replace(fixturePublisher, "  publish:\n", "  publish:\n    if: github.actor == 'robot'\n", 1),
		"needs":            fixturePublisher + strings.Replace(independentTestJob, "  test:\n", "  test:\n    needs: publish\n", 1),
		"alias":            strings.Replace(fixturePublisher, "  publish:", "  publish: &publication", 1),
		"duplicate":        strings.Replace(fixturePublisher, "    runs-on: ubuntu-latest", "    runs-on: ubuntu-latest\n    runs-on: other", 1),
		"extra-step":       fixturePublisher + "      - run: ./policy.sh\n",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := translateWorkflow([]byte(source), "plugins/orbit"); err == nil {
				t.Fatal("unknown logic accepted")
			}
		})
	}
}

func TestCorrectionPublisherIdentity(t *testing.T) {
	historical := strings.Replace(independentTestJob, "./tests/run.sh", "curl https://github.com/tessl-labs/research && ./tests/run.sh", 1)
	source := fixturePublisher + historical
	out, err := translateWorkflow([]byte(source), "plugins/orbit")
	expected := strings.Split(fixturePublisher, "  publish:\n")[0] + historical
	if err != nil || string(out) != expected {
		t.Fatalf("historical independent job: %s %v", out, err)
	}
	for _, other := range []string{strings.Replace(fixturePublisher, "  publish:", "  second:", 1), strings.Replace(fixturePublisher, "tesslio/patch-version-publish@v1", "custom/patch-version-publish@v1", 1)} {
		if _, err := translateWorkflow([]byte(other), "plugins/orbit"); err == nil && strings.Contains(other, "custom/") {
			t.Fatal("lookalike publisher accepted")
		}
	}
	both := fixturePublisher + strings.Split(strings.Replace(fixturePublisher, "  publish:", "  second:", 1), "jobs:\n")[1]
	if _, err := translateWorkflow([]byte(both), "plugins/orbit"); err == nil {
		t.Fatal("multiple publishers accepted")
	}
}

func TestCorrectionPublisherRetainedOutputReference(t *testing.T) {
	consumer := strings.Replace(independentTestJob, "./tests/run.sh", "echo '${{ needs.publish.outputs.version }}'", 1)
	if _, err := translateWorkflow([]byte(fixturePublisher+consumer), "plugins/orbit"); err == nil {
		t.Fatal("retired publisher output still referenced")
	}
}

// Fleet publication remains outside the standalone publisher contract, even at
// the formerly recognized revision. These are refusal fixtures, never pins.
const unsupportedFleetCaller = `name: Publish
on:
  push:
    branches: [main]
permissions:
  contents: write
jobs:
  publish:
    uses: jbaruch/coding-policy/.github/workflows/publish-plugin.yml@af116ebf18a7c46a672bf176064908736bc8ac28
    secrets:
      TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}
    with:
      stamp-changelog: false
      publish-mode: as-is
`

func TestFleetPublishersRefuseBeforeWrites(t *testing.T) {
	cases := []struct{ name, workflow, script string }{
		{"pinned-absent-if", unsupportedFleetCaller, ""},
		{"unknown-revision", strings.Replace(unsupportedFleetCaller, "af116ebf18a7c46a672bf176064908736bc8ac28", "unknown", 1), ""},
		{"stamping", strings.Replace(unsupportedFleetCaller, "stamp-changelog: false", "stamp-changelog: true", 1), ""},
		{"auto-bump", strings.Replace(unsupportedFleetCaller, "publish-mode: as-is", "publish-mode: auto-bump", 1), ""},
		{"default-auto-bump", strings.Replace(unsupportedFleetCaller, "      publish-mode: as-is\n", "", 1), ""},
		{"branch-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: github.ref == 'refs/heads/main'\n", 1), ""},
		{"expression-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: ${{ github.ref == 'refs/heads/main' }}\n", 1), ""},
		{"true-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: true\n", 1), ""},
		{"false-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: false\n", 1), ""},
		{"empty-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: \n", 1), ""},
		{"null-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: null\n", 1), ""},
		{"flow-sibling", `on: {push: {branches: [main]}}
permissions: {contents: write}
jobs: {test: {runs-on: ubuntu-latest, steps: [{run: ./tests/run.sh}]}, publish: {uses: 'jbaruch/coding-policy/.github/workflows/publish-plugin.yml@af116ebf18a7c46a672bf176064908736bc8ac28', secrets: {TESSL_TOKEN: '${{ secrets.TESSL_TOKEN }}'}, with: {stamp-changelog: false, publish-mode: as-is}}}
`, ""},
		{"oidc-gate", strings.Replace(unsupportedFleetCaller, "  contents: write\n", "  contents: write\n  id-token: write\n", 1) + "      pre-publish-script: .github/scripts/gate.sh\n", "#!/bin/sh\nset -eu\n: \"${ACTIONS_ID_TOKEN_REQUEST_URL:?}\"\n"},
		{"preparation", unsupportedFleetCaller + "      pre-publish-script: .github/scripts/gate.sh\n", "#!/bin/sh\nset -eu\nprintf 'Prepared skill\\n' > skills/example/SKILL.md\n"},
	}
	for _, tc := range cases {
		for _, action := range []string{"deterministic", "retain", "replace", "delete"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				root, opts := preservationFixture(t)
				const name = ".github/workflows/release.yml"
				put(t, root, name, tc.workflow, 0o640)
				if tc.script != "" {
					put(t, root, ".github/scripts/gate.sh", tc.script, 0o751)
				}
				before := correction12Inventory(t, root)
				calls := 0
				var plan Plan
				var err error
				if action == "deterministic" {
					plan, err = Prepare(opts)
				} else {
					opts.Agent = "codex"
					edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(tc.workflow)), Action: "replace", Content: tc.workflow + "# Retained caller.\n"}
					if action == "replace" {
						edit.Content = publishWorkflow
					} else if action == "delete" {
						edit.Action, edit.Content = "remove", ""
					}
					plan, err = prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
						calls++
						return proposal{Edits: []proposedEdit{edit}}, AgentRun{}, nil
					})
					if calls == 0 {
						t.Fatal("assisted refusal did not exercise proposal validation")
					}
				}
				if err == nil || plan.Report.Wrote {
					t.Fatalf("fleet conversion accepted: %v", err)
				}
				if _, err := plan.Apply(); err == nil {
					t.Fatal("refused plan applied")
				}
				correction12Unchanged(t, root, before)
				absent(t, root, ReceiptPath)
				absent(t, root, publishWorkflowPath)
				absent(t, root, transactionPath)
			})
		}
	}
}

func TestStandalonePublisherWithHistoricalFleetComment(t *testing.T) {
	root, opts := fixture(t)
	const comment = "# Historical jbaruch/coding-policy/.github/workflows/publish-plugin.yml@af116ebf18a7c46a672bf176064908736bc8ac28\n"
	put(t, root, ".github/workflows/publish.yml", comment+fixturePublisher+independentTestJob, 0o640)
	plan, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := plan.Apply(); err != nil || !report.Wrote {
		t.Fatalf("standalone publisher refused: %v", err)
	}
	retained := read(t, root, ".github/workflows/publish.yml")
	if retained != comment+strings.Split(fixturePublisher, "  publish:\n")[0]+independentTestJob || read(t, root, publishWorkflowPath) != publishWorkflow {
		t.Fatal("historical comment or independent job bytes changed")
	}
	info, err := os.Stat(filepath.Join(root, ".github/workflows/publish.yml"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("retained workflow mode changed: %v", err)
	}
}
