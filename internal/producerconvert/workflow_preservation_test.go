package producerconvert

import (
	"fmt"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestSemanticWorkflowKeepsIndependentReviewInsideTesslJob(t *testing.T) {
	original := `on: pull_request
jobs:
  review:
    if: github.event.pull_request.head.repo.full_name == github.repository
    permissions:
      contents: read
    env:
      REVIEW_TOKEN: ${{ secrets.REVIEW_TOKEN }}
    steps:
      - uses: tesslio/setup-tessl@v2
      - name: Run tests
        run: python3 tests/check.py
      - name: Independent review
        run: review --required
`
	valid := strings.Replace(original, "      - uses: tesslio/setup-tessl@v2\n", "", 1)
	if err := preserveChecks(".github/workflows/review.yml", []byte(original), []byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"", strings.Replace(valid, "on: pull_request", "on: push", 1), strings.Replace(valid, "review --required", "echo pass", 1), strings.Replace(valid, "${{ secrets.REVIEW_TOKEN }}", "''", 1), strings.Replace(valid, "run: python3 tests/check.py", "run: echo skipped", 1)} {
		if err := preserveChecks(".github/workflows/review.yml", []byte(original), []byte(candidate)); err == nil {
			t.Fatalf("accepted review weakening: %s", candidate)
		}
	}
	mixedScore := strings.Replace(original, "tesslio/setup-tessl@v2", "owner/policy/.github/actions/skill-review@1234", 1)
	if err := preserveChecks(".github/workflows/review.yml", []byte(mixedScore), nil); err == nil {
		t.Fatal("removed independent tests beside paid score step")
	}
}

func TestSemanticWorkflowDisclosureRetainsTestsWithoutService(t *testing.T) {
	original := "on: pull_request\njobs:\n  tests:\n    steps:\n      - run: python3 tests/check.py\n"
	disclosure := "# Removed paid Tessl skill-review threshold 85; no equivalent score.\n"
	candidate := disclosure + original
	markdown := "---\nname: Review\ndescription: Paid Tessl skill-review threshold 85 was removed; there is no replacement score.\non: pull_request\n---\nReview the change.\n"
	if workflowSemantic([]byte(markdown)) {
		t.Fatal("required Markdown disclosure is not a runtime service")
	}
	if workflowSemantic([]byte(candidate)) {
		t.Fatal("disclosure comment is not a runtime service")
	}
	if err := preserveChecks(".github/workflows/test.yml", []byte(original), []byte(candidate)); err != nil {
		t.Fatal(err)
	}
	for _, weakened := range []string{strings.Replace(candidate, "on: pull_request", "on: push", 1), strings.Replace(candidate, "python3 tests/check.py", "echo pass", 1)} {
		if err := preserveChecks(".github/workflows/test.yml", []byte(original), []byte(weakened)); err == nil {
			t.Fatal("accepted weakened independent workflow")
		}
	}
	for _, active := range []string{"run: tessl install owner/policy\n", "uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n", "run: |\n  tessl review skill\n", "env:\n  TESSL_API_KEY: secret\n"} {
		if !workflowSemantic([]byte(active)) {
			t.Fatalf("missed active service: %s", active)
		}
	}
}

func TestSemanticWorkflowRefusesMixedRunStepChanges(t *testing.T) {
	const prefix = "on: pull_request\njobs:\n  tests:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n"
	const name = "      - name: Install policy and test\n"
	for label, mixed := range map[string]string{
		"multiline": prefix + name + "        run: |\n          tessl install owner/policy\n          go test ./...\n",
		"compound":  prefix + name + "        run: tessl install owner/policy && go test ./...\n",
	} {
		t.Run(label, func(t *testing.T) {
			if err := preserveChecks(".github/workflows/mixed.yml", []byte(mixed), []byte(prefix)); err == nil {
				t.Fatal("deleted the independent test inside a Tessl-containing step")
			}
			// Retained test text is not proof of execution: unreachable after exit,
			// commented out, inside a skipped branch, or quoted as a substring.
			for _, rewrite := range []string{
				prefix + name + "        run: |\n          exit 0\n          go test ./...\n",
				prefix + name + "        run: |\n          # tessl install owner/policy && go test ./...\n          echo skipped\n",
				prefix + name + "        run: |\n          if false; then go test ./...; fi\n",
				prefix + name + "        run: echo 'skipping go test ./...'\n",
			} {
				if err := preserveChecks(".github/workflows/mixed.yml", []byte(mixed), []byte(rewrite)); err == nil {
					t.Fatalf("accepted rewrite of a mixed run step:\n%s", rewrite)
				}
			}
			// An unchanged mixed step passes preservation; the residual Tessl
			// operation is then the deterministic planner's refusal.
			if err := preserveChecks(".github/workflows/mixed.yml", []byte(mixed), []byte(mixed)); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Positive control: a proven service-only action with inputs and service
	// credentials still disappears while the independent test step remains.
	service := prefix + "      - uses: tesslio/setup-tessl@v2\n        with:\n          version: 1.2.3\n        env:\n          TESSL_TOKEN: ${{ secrets.TESSL_TOKEN }}\n      - run: go test ./...\n"
	if err := preserveChecks(".github/workflows/mixed.yml", []byte(service), []byte(prefix+"      - run: go test ./...\n")); err != nil {
		t.Fatal(err)
	}
	if err := preserveChecks(".github/workflows/mixed.yml", []byte(service), []byte(prefix)); err == nil {
		t.Fatal("removed the independent test beside a service-only action")
	}
}

func TestSemanticWorkflowPreservesJobContinueOnError(t *testing.T) {
	const head = "on: pull_request\njobs:\n  review:\n"
	const body = "    if: github.event.pull_request.head.repo.full_name == github.repository\n    env:\n      REVIEW_TOKEN: ${{ secrets.REVIEW_TOKEN }}\n    steps:\n      - uses: tesslio/setup-tessl@v2\n      - name: Run tests\n        run: python3 tests/check.py\n      - name: Independent review\n        run: review --required\n"
	const expression = "${{ github.event_name == 'push' }}"
	workflow := func(setting string) string {
		if setting == "" {
			return head + body
		}
		return head + "    continue-on-error: " + setting + "\n" + body
	}
	cases := []struct {
		name, before, after string
		accept              bool
	}{
		{"absent on both sides", "", "", true},
		{"identical explicit", "true", "true", true},
		{"identical expression", expression, expression, true},
		{"absent to true", "", "true", false},
		{"false to true", "false", "true", false},
		{"changed expression", expression, "${{ true }}", false},
		{"explicit removed", "true", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := workflow(tc.before)
			candidate := strings.Replace(workflow(tc.after), "      - uses: tesslio/setup-tessl@v2\n", "", 1)
			err := preserveChecks(".github/workflows/review.yml", []byte(original), []byte(candidate))
			if tc.accept && err != nil {
				t.Fatal(err)
			}
			if !tc.accept && (err == nil || !strings.Contains(err.Error(), "continue-on-error")) {
				t.Fatalf("accepted a nonblocking retained test job: %v", err)
			}
		})
	}
}

func TestClosedServiceInstallNearMisses(t *testing.T) {
	const closed = "mkdir -p /tmp/gh-aw/policy\ncd /tmp/gh-aw/policy\ntessl install owner/policy --yes"
	workflow := func(run string) string {
		return "# Tessl service migration\non: pull_request\njobs:\n  review:\n    steps:\n      - uses: tesslio/setup-tessl@v2\n      - name: Install policy\n        run: |\n          " + strings.ReplaceAll(run, "\n", "\n          ") + "\n      - run: review --required\n"
	}
	after := "on: pull_request\njobs:\n  review:\n    steps:\n      - run: review --required\n"
	for _, run := range []string{
		strings.Replace(closed, "tessl install owner/policy --yes", "", 1),
		closed + "\ngo test ./...", closed + " && go test ./...", "exit 0\n" + closed,
		strings.Replace(closed, "owner/policy", "owner/../policy", 1),
		strings.Replace(closed, "owner/policy", "$PACKAGE", 1),
		strings.Replace(closed, "owner/policy", "$(cat package)", 1),
		strings.Replace(closed, "owner/policy", "`cat package`", 1),
		strings.Replace(closed, "owner/policy", "'owner/policy'", 1),
		strings.Replace(closed, "cd /tmp/gh-aw/policy", "cd /tmp/gh-aw/other", 1),
		strings.ReplaceAll(closed, "/tmp/gh-aw/policy", "/tmp/gh-aw/../policy"),
		strings.Replace(closed, "mkdir -p", "mkdir -p ignored", 1),
		closed + " > results", "# " + closed, closed + "\n# go test ./...",
		strings.Replace(closed, " --yes", " --yes --ignore-scripts", 1),
	} {
		t.Run(run, func(t *testing.T) {
			weakenedWorkflowRefused(t, ".github/workflows/service.yml", workflow(run), after, "must retain its logic")
		})
	}
	for _, run := range []string{closed, "mkdir \t-p /tmp/gh-aw/policy \n\ncd\t/tmp/gh-aw/policy\ntessl install owner/policy --yes\n"} {
		if err := preserveChecks(".github/workflows/service.yml", []byte(workflow(run)), []byte(after)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkflowEnvironmentRemovalIsDirectional(t *testing.T) {
	const oldEnv = "env:\n  FIRST: original\n  TESSL_TOKEN: service\n  SECRET_TESSL_TOKEN: service\n  GH_AW_SECRET_NAMES: 'FIRST,TESSL_TOKEN,SECOND'\n  SECOND: original\n"
	const removed = "env:\n  FIRST: original\n  GH_AW_SECRET_NAMES: 'FIRST,SECOND'\n  SECOND: original\n"
	for _, level := range []string{"workflow", "job", "step"} {
		wrap := func(env string) string {
			head := "on: pull_request\n"
			job := "  review:\n"
			step := "      - run: review --required\n"
			indent := func(value, prefix string) string {
				return prefix + strings.ReplaceAll(strings.TrimSuffix(value, "\n"), "\n", "\n"+prefix) + "\n"
			}
			switch level {
			case "workflow":
				head += env
			case "job":
				job += indent(env, "    ")
			case "step":
				step += indent(env, "        ")
			}
			return head + "jobs:\n" + job + "    steps:\n      - uses: tesslio/setup-tessl@v2\n" + step
		}
		before := wrap(oldEnv)
		cases := []struct {
			name, env string
			accept    bool
		}{
			{"unchanged", oldEnv, true}, {"remove only service", removed, true},
			{"added unrelated", removed + "  EXTRA: added\n", false},
			{"changed unrelated", strings.Replace(removed, "FIRST: original", "FIRST: different", 1), false},
			{"removed unrelated", strings.Replace(removed, "  SECOND: original\n", "", 1), false},
			{"changed service", strings.Replace(oldEnv, "TESSL_TOKEN: service", "TESSL_TOKEN: different", 1), false},
			{"added service", removed + "  TESSL_NEW: added\n", false},
			{"reordered entries", strings.Replace(removed, "  FIRST: original\n", "", 1) + "  FIRST: original\n", false},
			{"reordered secret names", strings.Replace(removed, "FIRST,SECOND", "SECOND,FIRST", 1), false},
			{"new secret names", strings.Replace(removed, "FIRST,SECOND", "FIRST,SECOND,TESSL_NEW", 1), false},
			{"removed other secret", strings.Replace(removed, "FIRST,SECOND", "FIRST", 1), false},
		}
		for _, tc := range cases {
			t.Run(level+"/"+tc.name, func(t *testing.T) {
				after := strings.Replace(wrap(tc.env), "      - uses: tesslio/setup-tessl@v2\n", "", 1)
				err := preserveChecks(".github/workflows/policy.yml", []byte(before), []byte(after))
				if (err == nil) != tc.accept {
					t.Fatalf("accept=%t err=%v", tc.accept, err)
				}
			})
		}
	}
	// A value mentioning Tessl does not turn an unrelated key into a credential.
	before := strings.Replace(reviewInsideTesslJob, "${{ secrets.REVIEW_TOKEN }}", "tessl install owner/policy", 1)
	after := strings.Replace(before, "      REVIEW_TOKEN: tessl install owner/policy\n", "", 1)
	if err := preserveChecks(".github/workflows/policy.yml", []byte(before), []byte(after)); err == nil {
		t.Fatal("removed unrelated environment by value")
	}
}

func TestQuotedExpressionsPreserveRetiredProducerDependencies(t *testing.T) {
	const setup = "      - uses: tesslio/setup-tessl@v2\n        id: setup\n"
	for _, c := range []struct {
		name, expression string
		refuse           bool
	}{
		{"implicit-if", "steps.setup.outcome == 'success'", true},
		{"doubled-quotes", "${{ 'can''t close }} here' && steps['setup'].outcome }}", true},
		{"multiple-expressions", "${{ '}}' }} ${{ steps.setup.outcome }}", true},
		{"multiline", "${{ '}}' &&\nsteps.setup.outcome }}", true},
		{"incomplete-quoted", "${{ 'unfinished }} steps.setup", true},
		{"dynamic", "${{ '}}' && steps[matrix.producer].outcome }}", true},
		{"whole-context", "${{ '}}' && toJSON(steps) }}", true},
		{"literal-only", "${{ 'steps.setup }} and it''s literal' }}", false},
		{"unrelated", "${{ '}}' && steps.other.outcome }}", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := "on: push\njobs:\n  check:\n    steps:\n" + setup + "      - if: >-\n          " + strings.ReplaceAll(c.expression, "\n", "\n          ") + "\n        run: exit 1\n"
			after := strings.Replace(before, setup, "", 1)
			err := preserveChecks(".github/workflows/check.yml", []byte(before), []byte(after))
			if (err != nil) != c.refuse {
				t.Fatalf("retirement refusal=%t: %v", c.refuse, err)
			}
			if err := preserveChecks(".github/workflows/check.yml", []byte(before), []byte(before)); err != nil {
				t.Fatalf("unchanged producer refused: %v", err)
			}
		})
	}
}

const correction17PublisherSteps = `    steps:
      - uses: actions/checkout@v4
      - uses: tesslio/patch-version-publish@v1
        with:
          token: ${{ secrets.TESSL_TOKEN }}
          path: plugins/orbit
`
const correction17RetainedSteps = "    steps:\n      - uses: actions/checkout@v4\n      - run: acr validate .\n"

func correction17Workflow(fields, execution string) string {
	return "name: Publish\non:\n  push:\n    branches: [main]\npermissions:\n  contents: write\njobs:\n  publish:\n" + fields + execution
}

// Judge17 requires identical-value controls alongside changes, removals and
// additions. Unknown fields have a structural preservation control here; this
// is not a claim that an arbitrary key is accepted by hosted GitHub Actions.
func TestCorrection17PublisherPolicy(t *testing.T) {
	for _, tc := range []struct{ name, field, changed string }{
		{"if", "    if: ${{ false }}\n", "    if: ${{ true }}\n"},
		{"permissions", "    permissions: {contents: write}\n", "    permissions: {contents: read}\n"},
		{"env", "    env: {RELEASE_POLICY: original}\n", "    env: {RELEASE_POLICY: changed}\n"},
		{"needs", "    needs: test\n", "    needs: [test, another]\n"},
		{"timeout", "    timeout-minutes: 10\n", "    timeout-minutes: 20\n"},
		{"continue-on-error", "    continue-on-error: false\n", "    continue-on-error: true\n"},
		{"defaults", "    defaults: {run: {shell: bash}}\n", "    defaults: {run: {shell: sh}}\n"},
		{"unknown", "    x-policy: {required: original}\n", "    x-policy: {required: changed}\n"},
		{"concurrency", "    concurrency: release\n", "    concurrency: other\n"},
		{"environment", "    environment: production\n", "    environment: staging\n"},
		{"strategy", "    strategy: {matrix: {version: [stable]}}\n", "    strategy: {matrix: {version: [nightly]}}\n"},
		{"runner", "    runs-on: macos-latest\n", "    runs-on: ubuntu-latest\n"},
	} {
		for _, change := range []string{"identical", "changed", "removed", "introduced", "retire-job", "retire-workflow"} {
			for _, dry := range []bool{true, false} {
				t.Run(tc.name+"/"+change+"/dry="+fmt.Sprint(dry), func(t *testing.T) {
					oldField, nextField := tc.field, tc.field
					switch change {
					case "changed":
						nextField = tc.changed
					case "removed":
						nextField = ""
					case "introduced":
						oldField = ""
					}
					if tc.name != "runner" {
						oldField += "    runs-on: ubuntu-latest\n"
						nextField += "    runs-on: ubuntu-latest\n"
					}
					before := correction17Workflow(oldField, correction17PublisherSteps) + independentTestJob
					after := correction17Workflow(nextField, correction17RetainedSteps) + independentTestJob
					if change == "retire-job" {
						after = "name: Publish\non:\n  push:\n    branches: [main]\npermissions:\n  contents: write\njobs:\n" + independentTestJob
					}
					if change == "retire-workflow" {
						before = correction17Workflow(oldField, correction17PublisherSteps)
						after = ""
					}
					checkCorrection17Workflow(t, dry, before, after, change == "identical", false)
				})
			}
		}
	}
}

func TestCorrection17PublisherReusableShape(t *testing.T) {
	const guard = "    if: ${{ false }}\n"
	// The complete existing constant is the publisher contract; tests keep its
	// exact input/ref bytes rather than describing a different reusable call.
	reusable := strings.Split(publishWorkflow, "  publish:\n")[1]
	before := correction17Workflow(guard+"    runs-on: ubuntu-latest\n", correction17PublisherSteps)
	valid := correction17Workflow(guard, reusable)
	for _, tc := range []struct {
		name, before, after string
		accepted            bool
	}{
		{"guarded", before, valid, true},
		{"guard-kept-step-rewrite", before, correction17Workflow(guard+"    runs-on: ubuntu-latest\n", correction17RetainedSteps), true},
		{"guard-sanitized", before, strings.Replace(before, guard, "", 1), false},
		{"guard-removed", before, strings.Replace(valid, guard, "", 1), false},
		{"guard-changed", before, strings.Replace(valid, "${{ false }}", "${{ true }}", 1), false},
		{"guard-introduced", strings.Replace(before, guard, "", 1), valid, false},
		{"whole-workflow", before, "", false},
		{"wrong-workflow", before, strings.Replace(valid, "publish-package.yml@", "another.yml@", 1), false},
		{"wrong-ref", before, strings.Replace(valid, "@d3bc96b33b42293aecd1702c04aa94513a3dab1b", "@main", 1), false},
		{"wrong-root", before, strings.Replace(valid, "path: .", "path: plugins/orbit", 1), false},
		{"wrong-version", before, strings.Replace(valid, "acr-version: v0.1.6", "acr-version: v0.1.5", 1), false},
		{"missing-with", before, strings.Split(valid, "    with:")[0], false},
		{"extra-with", before, valid + "      dry-run: true\n", false},
		{"with-expression", before, strings.Split(valid, "    with:")[0] + "    with: ${{ inputs }}\n", false},
		{"remaining-runner", before, valid + "    runs-on: ubuntu-latest\n", false},
		{"remaining-steps", before, valid + "    steps: []\n", false},
		{"custom-runner", strings.Replace(before, "ubuntu-latest", "macos-latest", 1), valid, false},
		{"runner-list", strings.Replace(before, "runs-on: ubuntu-latest", "runs-on: [ubuntu-latest]", 1), valid, false},
		{"original-uses", before + "    uses: owner/repo/.github/workflows/other.yml@v1\n", valid, false},
		{"original-with", before + "    with: {}\n", valid, false},
		{"removed-service-env", before + "    env: {TESSL_TOKEN: placeholder}\n", valid, true},
		{"unrelated-env", before + "    env: {KEEP: original}\n", valid + "    env: {KEEP: original}\n", false},
		{"dropped-env", before + "    env: {KEEP: original}\n", valid, false},
		{"unchanged-defaults", before + "    defaults: {run: {shell: bash}}\n", valid + "    defaults: {run: {shell: bash}}\n", false},
		{"unchanged-timeout", before + "    timeout-minutes: 10\n", valid + "    timeout-minutes: 10\n", false},
		{"unchanged-outputs", before + "    outputs: {version: released}\n", valid + "    outputs: {version: released}\n", false},
		{"unchanged-unknown", before + "    x-policy: original\n", valid + "    x-policy: original\n", false},
		{"unsupported-if", strings.Replace(before, guard, "    if: [false]\n", 1), strings.Replace(valid, guard, "    if: [false]\n", 1), false},
		{"unsupported-permissions", before + "    permissions: [write]\n", valid + "    permissions: [write]\n", false},
		{"unsupported-needs", before + "    needs: {job: test}\n", valid + "    needs: {job: test}\n", false},
		{"unsupported-name", before + "    name: [publish]\n", valid + "    name: [publish]\n", false},
		{"unsupported-strategy", before + "    strategy: [stable]\n", valid + "    strategy: [stable]\n", false},
		{"unsupported-concurrency", before + "    concurrency: [release]\n", valid + "    concurrency: [release]\n", false},
	} {
		for _, dry := range []bool{true, false} {
			t.Run(tc.name+"/dry="+fmt.Sprint(dry), func(t *testing.T) {
				checkCorrection17Workflow(t, dry, tc.before, tc.after, tc.accepted, false)
			})
		}
	}
	for _, field := range []string{
		"    name: Protected publisher\n", "    if: false\n", "    if: github.ref_type == 'tag'\n",
		"    permissions: {contents: write}\n", "    permissions: write-all\n", "    permissions: {}\n",
		"    needs: test\n", "    needs: [test]\n", "    concurrency: release\n",
		"    concurrency: {group: release, cancel-in-progress: false}\n",
		"    strategy: {matrix: {version: [stable]}, fail-fast: false, max-parallel: 1}\n",
	} {
		for _, dry := range []bool{true, false} {
			t.Run("compatible/"+strings.TrimSpace(field)+"/dry="+fmt.Sprint(dry), func(t *testing.T) {
				original := correction17Workflow(field+"    runs-on: ubuntu-latest\n", correction17PublisherSteps) + independentTestJob
				candidate := correction17Workflow(field, reusable) + independentTestJob
				// Judge18 amends only this insufficient reusable capability
				// expectation; the unchanged policy still passes separate equality controls.
				checkCorrection17Workflow(t, dry, original, candidate, field != "    permissions: {}\n", false)
			})
		}
	}
}

func TestCorrection17PaidAndPublisherRetirement(t *testing.T) {
	const score = "      - uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n"
	const paidPolicy = "    if: ${{ false }}\n    permissions: {contents: read}\n    timeout-minutes: 10\n"
	const notice = "# Paid Tessl skill review was retired; ACR has no equivalent score.\n"
	paid := correction17Workflow(paidPolicy+"    runs-on: ubuntu-latest\n", "    steps:\n      - uses: actions/checkout@v4\n"+score)
	for _, dry := range []bool{true, false} {
		for _, tc := range []struct {
			name, before, after string
			accepted, disclose  bool
		}{
			{"paid-retired", paid, "", true, true},
			{"paid-missing-disclosure", paid, "", false, false},
			{"paid-retained", paid, notice + correction17Workflow(paidPolicy+"    runs-on: ubuntu-latest\n", correction17RetainedSteps), true, true},
			{"paid-guard-lost", paid, notice + correction17Workflow("    runs-on: ubuntu-latest\n", correction17RetainedSteps), false, true},
			{"paid-notice-lost", paid, correction17Workflow(paidPolicy+"    runs-on: ubuntu-latest\n", correction17RetainedSteps), false, true},
			{"mixed-retired", correction17Workflow(paidPolicy+"    runs-on: ubuntu-latest\n", correction17PublisherSteps+score), "", false, true},
			{"mixed-guard-lost", correction17Workflow(paidPolicy+"    runs-on: ubuntu-latest\n", correction17PublisherSteps+score), notice + correction17Workflow("    runs-on: ubuntu-latest\n", correction17RetainedSteps), false, true},
			{"publisher-output-retired", correction17Workflow("    runs-on: ubuntu-latest\n    outputs: {version: released}\n", correction17PublisherSteps), "", true, false},
			{"publisher-service-env-retired", correction17Workflow("    runs-on: ubuntu-latest\n    env: {TESSL_TOKEN: placeholder}\n", correction17PublisherSteps), "", true, false},
			{"publisher-empty-env", correction17Workflow("    runs-on: ubuntu-latest\n    env: {}\n", correction17PublisherSteps), "", false, false},
			{"publisher-expression-env", correction17Workflow("    runs-on: ubuntu-latest\n    env: ${{ inputs }}\n", correction17PublisherSteps), "", false, false},
			{"publisher-unsupported-output", correction17Workflow("    runs-on: ubuntu-latest\n    outputs: [released]\n", correction17PublisherSteps), "", false, false},
			{"publisher-retained-steps-scalar", correction17Workflow("    runs-on: ubuntu-latest\n", correction17PublisherSteps), correction17Workflow("    runs-on: ubuntu-latest\n", "    steps: true\n"), false, false},
			{"publisher-retained-steps-mapping", correction17Workflow("    runs-on: ubuntu-latest\n", correction17PublisherSteps), correction17Workflow("    runs-on: ubuntu-latest\n", "    steps: {run: echo ready}\n"), false, false},
			{"publisher-retained-steps-null", correction17Workflow("    runs-on: ubuntu-latest\n", correction17PublisherSteps), correction17Workflow("    runs-on: ubuntu-latest\n", "    steps: null\n"), false, false},
			{"publisher-retained-steps-empty", correction17Workflow("    runs-on: ubuntu-latest\n", correction17PublisherSteps), correction17Workflow("    runs-on: ubuntu-latest\n", "    steps: []\n"), false, false},
			{"publisher-retained-steps-item", correction17Workflow("    runs-on: ubuntu-latest\n", correction17PublisherSteps), correction17Workflow("    runs-on: ubuntu-latest\n", "    steps: [false]\n"), false, false},
			{"publisher-absent-runner", correction17Workflow("", correction17PublisherSteps), "", true, false},
			{"publisher-setup-only", correction17Workflow("    runs-on: ubuntu-latest\n", "    steps:\n      - uses: tesslio/setup-tessl@v2\n"), "", false, false},
			{"independent-step", paid + "      - run: review --required\n", "", false, true},
		} {
			t.Run(tc.name+"/dry="+fmt.Sprint(dry), func(t *testing.T) {
				checkCorrection17Workflow(t, dry, tc.before, tc.after, tc.accepted, tc.disclose)
			})
		}
	}
}

func checkCorrection17Workflow(t *testing.T, dry bool, before, after string, accepted, disclose bool) {
	t.Helper()
	root, opts, p := semanticFixture(t)
	const name = ".github/workflows/publish.yml"
	put(t, root, name, before, 0o640)
	edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after}
	if after == "" {
		edit.Action = "remove"
	}
	p.Edits = append(p.Edits, edit)
	if disclose {
		p.PolicyChanges = append(p.PolicyChanges, PolicyChange{Path: name, From: "Paid Tessl skill review", To: "Retired; ACR has no equivalent score"})
	}
	checkCorrectionProposal(t, dry, root, opts, p, accepted)
}

// Publisher conditions belong to each execution occurrence, including in jobs
// that contain independent checks. Checkout/validation cannot carry them instead.
func TestCorrection18PublisherStepConditions(t *testing.T) {
	const prefix = "    steps:\n      - uses: actions/checkout@v4\n"
	const oldAction = "      - uses: tesslio/patch-version-publish@v1\n"
	const publish = "      - run: acr publish .\n"
	const validate = "      - run: acr validate .\n"
	const runner = "    runs-on: ubuntu-latest\n"
	reusable := strings.SplitN(publishWorkflow, "  publish:\n", 2)[1]
	for _, guard := range []string{"false", "true", "'false'", "github.ref_type == 'tag'", "${{ false }}", "${{ github.ref_type == 'tag' }}"} {
		condition := "        if: " + guard + "\n"
		original := correction17Workflow(runner, prefix+oldAction+condition)
		retained := correction17Workflow(runner, prefix+publish+condition)
		for _, tc := range []struct {
			name, after string
			accepted    bool
		}{
			{"retained", retained, true},
			{"removed", correction17Workflow(runner, prefix+publish), false},
			{"changed", strings.Replace(retained, condition, "        if: changed\n", 1), false},
			{"old-action-unguarded", strings.Replace(original, condition, "", 1), false},
			{"checkout-decoy", correction17Workflow(runner, prefix+condition+publish), false},
			{"validation-decoy", correction17Workflow(runner, prefix+validate+condition+publish), false},
			{"reusable", correction17Workflow("", reusable), false},
			{"job-guard-transfer", correction17Workflow("    if: "+guard+"\n", reusable), false},
			{"workflow-retired", "", false},
			{"job-retired", strings.SplitN(original, "  publish:\n", 2)[0] + independentTestJob, false},
			{"wrapper", strings.Replace(retained, "acr publish .", "sh -c 'acr publish .'", 1), false},
			{"pipeline", strings.Replace(retained, "acr publish .", "acr publish . | cat", 1), false},
		} {
			for _, dry := range []bool{true, false} {
				t.Run(guard+"/"+tc.name+"/dry="+fmt.Sprint(dry), func(t *testing.T) {
					checkCorrection18Workflow(t, dry, original, tc.after, tc.accepted, false)
				})
			}
		}
		for _, ordinary := range []bool{false, true} {
			for _, paid := range []bool{false, true} {
				suffix, notice := "", ""
				if ordinary {
					suffix += "      - run: review --required\n"
				}
				sourceSuffix := suffix
				if paid {
					sourceSuffix += "      - uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n"
					notice = "# Paid Tessl skill review was retired; ACR has no equivalent score.\n"
				}
				for _, keep := range []bool{true, false} {
					nextCondition := ""
					if keep {
						nextCondition = condition
					}
					for _, dry := range []bool{true, false} {
						t.Run(fmt.Sprintf("%s/ordinary=%t/paid=%t/keep=%t/dry=%t", guard, ordinary, paid, keep, dry), func(t *testing.T) {
							checkCorrection18Workflow(t, dry, original+sourceSuffix, notice+correction17Workflow(runner, prefix+publish+nextCondition+suffix), keep, paid)
						})
					}
				}
			}
		}
	}
	for _, pair := range [][2]string{{"false", "true"}, {"false", "false"}, {"false", "'false'"}, {"${{ false }}", "github.ref_type == 'tag'"}} {
		first, second := "        if: "+pair[0]+"\n", "        if: "+pair[1]+"\n"
		original := correction17Workflow(runner, prefix+oldAction+first+oldAction+second)
		for _, tc := range []struct {
			name, steps string
			accepted    bool
		}{
			{"distinct-kept", publish + first + publish + second, true},
			{"one-candidate", publish + first, false},
			{"second-removed", publish + first + publish, false},
			{"reordered", publish + second + publish + first, pair[0] == pair[1]},
			{"validation-compensation", publish + first + validate + second, false},
		} {
			for _, dry := range []bool{true, false} {
				t.Run(fmt.Sprintf("pair=%q/%s/dry=%t", pair, tc.name, dry), func(t *testing.T) {
					checkCorrection18Workflow(t, dry, original, correction17Workflow(runner, prefix+tc.steps), tc.accepted, false)
				})
			}
		}
	}
	for _, unsupported := range []string{"[false]", "{enabled: false}", "null", "17"} {
		original := correction17Workflow(runner, prefix+oldAction+"        if: "+unsupported+"\n")
		for _, dry := range []bool{true, false} {
			t.Run("unsupported="+unsupported+fmt.Sprint(dry), func(t *testing.T) {
				checkCorrection18Workflow(t, dry, original, strings.Replace(original, oldAction, publish, 1), false, false)
			})
		}
	}
	// Unchanged guarded service actions preserve their field, but still meet the
	// existing deterministic refusal. That boundary must not strip the guard.
	original := correction17Workflow(runner, prefix+oldAction+"        if: false\n")
	if err := preserveChecks(".github/workflows/publish.yml", []byte(original), []byte(original)); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		t.Run("unchanged-old-action/"+fmt.Sprint(dry), func(t *testing.T) { checkCorrection18Workflow(t, dry, original, original, false, false) })
	}
}

func TestCorrection18ReusableCallerPermissions(t *testing.T) {
	reusable := strings.SplitN(publishWorkflow, "  publish:\n", 2)[1]
	workflow := func(top, job, execution string) string {
		return "name: Publish\non:\n  push:\n    tags: ['v*']\n" + top + "jobs:\n  publish:\n" + job + execution
	}
	forms := []struct {
		name, value string
		sufficient  bool
	}{
		{"empty", "{}", false}, {"read-all", "read-all", false}, {"write-all", "write-all", true},
		{"read", "{contents: read}", false}, {"none", "{contents: none}", false}, {"write", "{contents: write}", true},
		{"missing-contents", "{pull-requests: write}", false}, {"unrelated-and-write", "{contents: write, pull-requests: read}", true},
		{"expression", "${{ inputs.permissions }}", false}, {"sequence", "[write]", false}, {"null", "null", false}, {"bool", "true", false},
		{"invalid-map", "{contents: write, issues: invalid}", false},
	}
	for _, form := range forms {
		for _, scope := range []string{"top", "job-over-write", "job-over-pr"} {
			top, job := "permissions: "+form.value+"\n", ""
			if scope != "top" {
				top = "permissions: {contents: write}\n"
				if scope == "job-over-pr" {
					top = "permissions: {pull-requests: write}\n"
				}
				job = "    permissions: " + form.value + "\n"
			}
			original := workflow(top, job+"    runs-on: ubuntu-latest\n", correction17PublisherSteps)
			candidate := workflow(top, job, reusable)
			for _, dry := range []bool{true, false} {
				t.Run(form.name+"/"+scope+"/dry="+fmt.Sprint(dry), func(t *testing.T) { checkCorrection18Workflow(t, dry, original, candidate, form.sufficient, false) })
			}
			// Equality is a separate preservation obligation, even when the new
			// reusable capability contract refuses these unchanged permissions.
			var a, b yaml.Node
			if err := yaml.Unmarshal([]byte(original), &a); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(candidate), &b); err != nil {
				t.Fatal(err)
			}
			if err := preserveWorkflowFields(a.Content[0], b.Content[0], false, "jobs"); err != nil {
				t.Fatal(err)
			}
			if err := preserveWorkflowFields(member(member(a.Content[0], "jobs"), "publish"), member(member(b.Content[0], "jobs"), "publish"), false, "runs-on", "steps", "uses", "with"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name, top, job string
		accepted       bool
	}{
		{"defaults-unknown", "", "", true},
		{"job-write-without-top", "", "    permissions: {contents: write}\n", true},
		{"job-read-without-top", "", "    permissions: read-all\n", false},
		{"sufficient-false-job", "permissions: {contents: write}\n", "    if: false\n", true},
		{"insufficient-false-job", "permissions: {contents: read}\n", "    if: false\n", false},
	} {
		original := workflow(tc.top, tc.job+"    runs-on: ubuntu-latest\n", correction17PublisherSteps)
		candidate := workflow(tc.top, tc.job, reusable)
		if strings.Contains(tc.name, "false-job") {
			original = strings.Replace(original, "tags: ['v*']", "branches: [main]", 1)
			candidate = strings.Replace(candidate, "tags: ['v*']", "branches: [main]", 1)
		}
		for _, dry := range []bool{true, false} {
			t.Run(tc.name+fmt.Sprint(dry), func(t *testing.T) { checkCorrection18Workflow(t, dry, original, candidate, tc.accepted, false) })
		}
	}
	for _, scope := range []string{"top", "job"} {
		top, job := "permissions: {contents: read}\n", ""
		if scope == "job" {
			top = "permissions: {contents: write}\n"
			job = "    permissions: {}\n"
		}
		original := workflow(top, job+"    runs-on: ubuntu-latest\n", correction17PublisherSteps)
		candidate := workflow("permissions: {contents: write}\n", "", reusable)
		for _, dry := range []bool{true, false} {
			t.Run("elevation/"+scope+fmt.Sprint(dry), func(t *testing.T) { checkCorrection18Workflow(t, dry, original, candidate, false, false) })
		}
		// Retained ordinary steps still accept identical explicit low permissions.
		for _, dry := range []bool{true, false} {
			t.Run("ordinary-equality/"+scope+fmt.Sprint(dry), func(t *testing.T) {
				checkCorrection18Workflow(t, dry, original, workflow(top, job+"    runs-on: ubuntu-latest\n", correction17RetainedSteps), true, false)
			})
		}
	}
}
