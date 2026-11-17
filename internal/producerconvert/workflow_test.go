package producerconvert

import (
	"context"
	"errors"
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
	const secrets = "    secrets:\n      TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}\n"
	const inputs = "    with:\n      stamp-changelog: false\n      publish-mode: as-is\n"
	const revision = "af116ebf18a7c46a672bf176064908736bc8ac28"
	const uses = "jbaruch/coding-policy/.github/workflows/publish-plugin.yml@"
	const fleetReason = "Reusable fleet publisher migration is unsupported; retain the original source and track support at https://github.com/jbaruch/agentic-context-registry/issues/217. Selecting --agent cannot convert this caller."
	const invalidReason = "Workflow YAML cannot be fully decoded or inspected; correct it to one mapping document with a jobs mapping, job mappings and string uses values, and retain the original source before retrying."
	cases := []struct{ name, workflow, script, reason string }{
		{"uses-alias", "env: {PUB: &pub '" + uses + "v1'}\non: push\njobs: {publish: {uses: *pub}}\n", "", fleetReason},
		{"whole-job-alias", "x-job: &pub {uses: '" + uses + "v1'}\non: push\njobs: {publish: *pub}\n", "", fleetReason},
		{"anchored-job", "on: push\njobs: {publish: &pub {uses: '" + uses + "v1'}}\n", "", fleetReason},
		{"aliased-jobs-map", "x-jobs: &jobs {publish: {uses: '" + uses + "v1'}}\non: push\njobs: *jobs\n", "", fleetReason},
		{"merge-inherited", "x-job: &pub {uses: '" + uses + "v1'}\non: push\njobs: {publish: {<<: *pub}}\n", "", fleetReason},
		{"merge-overridden-to-fleet", "x-job: &pub {uses: other/publisher/.github/workflows/publish.yml@v1}\non: push\njobs: {publish: {<<: *pub, uses: '" + uses + "v1'}}\n", "", fleetReason},
		{"mixed-case-owner-repo", "on: push\njobs: {publish: {uses: JBaruch/Coding-Policy/.github/workflows/publish-plugin.yml@v1}}\n", "", fleetReason},
		{"trailing-comment", strings.Replace(unsupportedFleetCaller, revision, revision+" # publish", 1), "", fleetReason},
		{"explicit-string-tag", strings.Replace(unsupportedFleetCaller, "uses: ", "uses: !!str ", 1), "", fleetReason},
		{"bom", "\ufeff" + unsupportedFleetCaller, "", fleetReason},
		{"malformed-yaml", "on: push\njobs: [\n", "", invalidReason},
		{"unresolved-alias", "on: push\njobs: {publish: {uses: *pub}}\n", "", invalidReason},
		{"cyclic-alias", "on: push\njobs: &jobs {publish: *jobs}\n", "", invalidReason},
		{"duplicate-key", "on: push\njobs: {publish: {uses: other/publisher@v1, uses: '" + uses + "v1'}}\n", "", invalidReason},
		{"trailing-document", unsupportedFleetCaller + "---\njobs: {}\n", "", invalidReason},
		{"trailing-empty-document", unsupportedFleetCaller + "---\n", "", invalidReason},
		{"trailing-invalid-document", unsupportedFleetCaller + "---\njobs: [\n", "", invalidReason},
		{"sequence-root", "- jobs: {}\n", "", invalidReason},
		{"null-root", "null\n", "", invalidReason},
		{"empty-document", "", "", invalidReason},
		{"missing-jobs", "on: push\n", "", invalidReason},
		{"sequence-jobs", "on: push\njobs: []\n", "", invalidReason},
		{"null-jobs", "on: push\njobs: null\n", "", invalidReason},
		{"non-string-job-key", "on: push\njobs: {1: {uses: '" + uses + "v1'}}\n", "", invalidReason},
		{"scalar-job", "on: push\njobs: {publish: invalid}\n", "", invalidReason},
		{"null-job", "on: push\njobs: {publish: null}\n", "", invalidReason},
		{"sequence-uses", "on: push\njobs: {publish: {uses: []}}\n", "", invalidReason},
		{"null-uses", "on: push\njobs: {publish: {uses: null}}\n", "", invalidReason},
		{"boolean-uses", "on: push\njobs: {publish: {uses: false}}\n", "", invalidReason},
		{"pinned-absent-if", unsupportedFleetCaller, "", fleetReason},
		{"inherit", strings.Replace(unsupportedFleetCaller, secrets, "    secrets: inherit\n", 1), "", fleetReason},
		{"unrelated-secrets", strings.ReplaceAll(unsupportedFleetCaller, "TESSL_TOKEN", "PUBLISH_TOKEN"), "", fleetReason},
		{"no-with", strings.Replace(unsupportedFleetCaller, inputs, "", 1), "", fleetReason},
		{"with-without-secrets", strings.Replace(unsupportedFleetCaller, secrets, "", 1), "", fleetReason},
		{"neither", strings.Replace(strings.Replace(unsupportedFleetCaller, secrets, "", 1), inputs, "", 1), "", fleetReason},
		{"tag-inherit", strings.Replace(strings.Replace(unsupportedFleetCaller, revision, "v1.2.3", 1), secrets, "    secrets: inherit\n", 1), "", fleetReason},
		{"unknown-without-bindings", "on: push\njobs:\n  publish:\n    uses: " + uses + "unknown\n", "", fleetReason},
		{"escaped-uses", "on: push\njobs:\n  publish:\n    uses: \"jbaruch\\x2fcoding-policy/.github/workflows/publish-plugin.yml@v1\"\n", "", fleetReason},
		{"flow-inherit", "on: push\njobs: {publish: {uses: '" + uses + "v1', secrets: inherit}}\n", "", fleetReason},
		{"flow-unrelated-secrets", "on: push\njobs: {publish: {uses: '" + uses + "unknown', secrets: {PUBLISH_TOKEN: '${{ secrets.PUBLISH_TOKEN }}'}}}\n", "", fleetReason},
		{"flow-neither", "on: push\njobs: {publish: {uses: '" + uses + revision + "'}}\n", "", fleetReason},
		{"flow-with-without-secrets", "on: push\njobs: {publish: {uses: '" + uses + "v1', with: {publish-mode: as-is}}}\n", "", fleetReason},
		{"flow-branch-without-bindings", "on: push\njobs: {publish: {uses: '" + uses + "release/next'}}\n", "", fleetReason},
		{"standalone-sibling", strings.Replace(unsupportedFleetCaller, secrets, "    secrets: inherit\n", 1) + strings.Split(strings.Replace(fixturePublisher, "  publish:\n", "  standalone:\n", 1), "jobs:\n")[1], "", fleetReason},
		{"unknown-revision", strings.Replace(unsupportedFleetCaller, "af116ebf18a7c46a672bf176064908736bc8ac28", "unknown", 1), "", fleetReason},
		{"stamping", strings.Replace(unsupportedFleetCaller, "stamp-changelog: false", "stamp-changelog: true", 1), "", fleetReason},
		{"auto-bump", strings.Replace(unsupportedFleetCaller, "publish-mode: as-is", "publish-mode: auto-bump", 1), "", fleetReason},
		{"default-auto-bump", strings.Replace(unsupportedFleetCaller, "      publish-mode: as-is\n", "", 1), "", fleetReason},
		{"branch-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: github.ref == 'refs/heads/main'\n", 1), "", fleetReason},
		{"expression-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: ${{ github.ref == 'refs/heads/main' }}\n", 1), "", fleetReason},
		{"true-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: true\n", 1), "", fleetReason},
		{"false-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: false\n", 1), "", fleetReason},
		{"empty-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: \n", 1), "", fleetReason},
		{"null-if", strings.Replace(unsupportedFleetCaller, "  publish:\n", "  publish:\n    if: null\n", 1), "", fleetReason},
		{"flow-sibling", `on: {push: {branches: [main]}}
permissions: {contents: write}
jobs: {test: {runs-on: ubuntu-latest, steps: [{run: ./tests/run.sh}]}, publish: {uses: 'jbaruch/coding-policy/.github/workflows/publish-plugin.yml@af116ebf18a7c46a672bf176064908736bc8ac28', secrets: {TESSL_TOKEN: '${{ secrets.TESSL_TOKEN }}'}, with: {stamp-changelog: false, publish-mode: as-is}}}
`, "", fleetReason},
		{"oidc-gate", strings.Replace(unsupportedFleetCaller, "  contents: write\n", "  contents: write\n  id-token: write\n", 1) + "      pre-publish-script: .github/scripts/gate.sh\n", "#!/bin/sh\nset -eu\n: \"${ACTIONS_ID_TOKEN_REQUEST_URL:?}\"\n", fleetReason},
		{"preparation", unsupportedFleetCaller + "      pre-publish-script: .github/scripts/gate.sh\n", "#!/bin/sh\nset -eu\nprintf 'Prepared skill\\n' > skills/example/SKILL.md\n", fleetReason},
	}
	for _, tc := range cases {
		for _, agent := range []string{"deterministic", "codex", "claude"} {
			actions := []string{"deterministic"}
			if agent != "deterministic" {
				actions = []string{"retain", "replace", "remove-job", "remove-workflow", "edit-other"}
			}
			for _, action := range actions {
				t.Run(tc.name+"/"+agent+"/"+action, func(t *testing.T) {
					root, opts := preservationFixture(t)
					const name = ".github/workflows/release.yml"
					put(t, root, name, tc.workflow, 0o640)
					put(t, root, ".github/workflows/publish.yml", strings.Replace(fixturePublisher, "path: plugins/orbit", "path: .", 1), 0o644)
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
						opts.Agent = agent
						edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(tc.workflow)), Action: "replace", Content: tc.workflow + "# Retained caller.\n"}
						switch action {
						case "replace":
							edit.Content = publishWorkflow
						case "remove-job":
							edit.Content = "on: push\njobs:\n" + independentTestJob
						case "remove-workflow":
							edit.Action, edit.Content = "remove", ""
						case "edit-other":
							edit.Path = "skills/example/SKILL.md"
							edit.BeforeDigest = digest([]byte(read(t, root, edit.Path)))
							edit.Content = "# Example\nUpdated independent skill.\n"
						}
						plan, err = prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
							calls++
							return proposal{Edits: []proposedEdit{edit}}, AgentRun{}, nil
						})
					}
					var refusal *Error
					if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || refusal.Path != name || refusal.Reason != tc.reason || err.Error() != name+": "+tc.reason {
						t.Fatalf("unexpected workflow refusal: %v", err)
					}
					if calls != 0 || plan.Report.Package != "" || len(plan.Report.AgentRuns) != 0 || plan.Report.Wrote || len(plan.Report.Changes) != 0 || len(plan.Report.PublishedFiles) != 0 {
						t.Fatalf("fleet refusal invoked provider or prepared output: calls=%d report=%+v", calls, plan.Report)
					}
					if len(plan.Report.Blockers) != 1 || plan.Report.Blockers[0] != (Blocker{Path: name, Reason: tc.reason}) {
						t.Fatalf("fleet blocker missing source path or issue: %+v", plan.Report.Blockers)
					}
					if _, err := plan.Apply(); err == nil {
						t.Fatal("refused plan applied")
					}
					correction12Unchanged(t, root, before)
					absent(t, root, "agent-plugin.yaml")
					absent(t, root, ReceiptPath)
					absent(t, root, publishWorkflowPath)
					absent(t, root, transactionPath)
				})
			}
		}
	}
}

func TestWorkflowNonFleetEffectiveUsesPreserved(t *testing.T) {
	const fleet = "jbaruch/coding-policy/.github/workflows/publish-plugin.yml@v1"
	for name, workflow := range map[string]string{
		"merge-overridden-before": "x-job: &pub {uses: '" + fleet + "'}\non: push\njobs: {publish: {uses: other/publisher/.github/workflows/publish.yml@v1, <<: *pub}}\n",
		"merge-overridden-after":  "x-job: &pub {uses: '" + fleet + "'}\non: push\njobs: {publish: {<<: *pub, uses: other/publisher/.github/workflows/publish.yml@v1}}\n",
		"path-case":               "on: push\njobs: {publish: {uses: JBaruch/Coding-Policy/.github/workflows/Publish-Plugin.yml@v1}}\n",
	} {
		for _, agent := range []string{"deterministic", "codex", "claude"} {
			t.Run(name+"/"+agent, func(t *testing.T) {
				root, opts := preservationFixture(t)
				const path = ".github/workflows/independent.yml"
				put(t, root, path, workflow, 0o640)
				before := correction12Inventory(t, root)
				calls := 0
				var plan Plan
				var err error
				if agent == "deterministic" {
					plan, err = Prepare(opts)
				} else {
					opts.Agent = agent
					plan, err = prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
						calls++
						return proposal{}, AgentRun{}, nil
					})
				}
				if err != nil || calls != 0 || len(plan.Report.Blockers) != 0 {
					t.Fatalf("non-fleet effective uses refused: calls=%d err=%v report=%+v", calls, err, plan.Report)
				}
				correction12Unchanged(t, root, before)
				if report, err := plan.Apply(); err != nil || !report.Wrote {
					t.Fatalf("non-fleet conversion failed: %v", err)
				}
				info, err := os.Stat(filepath.Join(root, path))
				if err != nil {
					t.Fatal(err)
				}
				if read(t, root, path) != workflow || info.Mode().Perm() != 0o640 || read(t, root, publishWorkflowPath) != publishWorkflow {
					t.Fatal("non-fleet workflow bytes/mode or generated publisher changed")
				}
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
