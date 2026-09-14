package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
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

const mixedRunWorkflow = "on: pull_request\njobs:\n  tests:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - name: Install policy and test\n        run: |\n          tessl install owner/policy\n          go test ./...\n"
const reviewInsideTesslJob = "on: pull_request\njobs:\n  review:\n    if: github.event.pull_request.head.repo.full_name == github.repository\n    env:\n      REVIEW_TOKEN: ${{ secrets.REVIEW_TOKEN }}\n    steps:\n      - uses: tesslio/setup-tessl@v2\n      - name: Run tests\n        run: python3 tests/check.py\n      - name: Independent review\n        run: review --required\n"

// weakenedWorkflowRefused drives one workflow replacement through the real
// proposal boundary and requires refusal with unchanged source and no receipt.
func weakenedWorkflowRefused(t *testing.T, name, before, after, reason string) {
	t.Helper()
	root, opts, p := semanticFixture(t)
	put(t, root, name, before, 0o644)
	original := treeAt(t, root)
	p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
	calls := 0
	_, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil })
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("accepted weakened workflow: %v", err)
	}
	if calls != 3 || !matches(original, treeAt(t, root)) {
		t.Fatalf("calls=%d sourceChanged=%t", calls, !matches(original, treeAt(t, root)))
	}
	absent(t, root, ReceiptPath)
	absent(t, root, transactionPath)
}

// serviceOnlyRemovalApplied drives the legitimate setup-Tessl removal through
// the same boundary and requires acceptance and application.
func serviceOnlyRemovalApplied(t *testing.T, name, before string) {
	t.Helper()
	root, opts, p := semanticFixture(t)
	put(t, root, name, before, 0o644)
	fixed := strings.Replace(before, "      - uses: tesslio/setup-tessl@v2\n", "", 1)
	p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: fixed})
	plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { return p, AgentRun{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if read(t, root, name) != fixed {
		t.Fatal("service-only removal was not applied as proposed")
	}
}

func TestSemanticProposalRefusesMixedRunStepBeforeWrites(t *testing.T) {
	const step = "      - name: Install policy and test\n        run: |\n          tessl install owner/policy\n          go test ./...\n"
	t.Run("deleted", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/mixed.yml", mixedRunWorkflow, strings.Replace(mixedRunWorkflow, step, "", 1), "must retain its logic")
	})
	t.Run("rewritten", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/mixed.yml", mixedRunWorkflow, strings.Replace(mixedRunWorkflow, "          tessl install owner/policy\n", "          exit 0\n", 1), "must retain its logic")
	})
	t.Run("retained", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/mixed.yml", mixedRunWorkflow, mixedRunWorkflow, "candidate conversion")
	})
	t.Run("service-only removal", func(t *testing.T) {
		serviceOnlyRemovalApplied(t, ".github/workflows/review.yml", reviewInsideTesslJob)
	})
}

func TestSemanticProposalRefusesJobContinueOnErrorBeforeWrites(t *testing.T) {
	explicit := strings.Replace(reviewInsideTesslJob, "  review:\n", "  review:\n    continue-on-error: true\n", 1)
	t.Run("introduced", func(t *testing.T) {
		weakenedWorkflowRefused(t, ".github/workflows/review.yml", reviewInsideTesslJob, strings.Replace(explicit, "      - uses: tesslio/setup-tessl@v2\n", "", 1), "continue-on-error")
	})
	t.Run("identical explicit", func(t *testing.T) {
		serviceOnlyRemovalApplied(t, ".github/workflows/review.yml", explicit)
	})
}

func TestSemanticProposalPreservesAllWorkflowPolicy(t *testing.T) {
	const setup = "      - uses: tesslio/setup-tessl@v2\n"
	for _, level := range []string{"job", "workflow"} {
		for _, field := range []struct{ name, before, after string }{
			{"defaults", "defaults: {run: {shell: 'bash -e {0}'}}", "defaults: {run: {shell: 'bash {0}'}}"},
			{"runs-on", "runs-on: ubuntu-latest", "runs-on: self-hosted"},
			{"env", "env: {PYTHONOPTIMIZE: '0'}", "env: {PYTHONOPTIMIZE: '1'}"},
			{"unknown", "x-policy: {required: true}", "x-policy: {required: false}"},
			{"env expression", "env: '${{ inputs.config }}'", "env: '${{ inputs.other }}'"},
		} {
			for _, change := range []string{"introduced", "changed", "removed", "identical"} {
				t.Run(level+"/"+field.name+"/"+change, func(t *testing.T) {
					workflow := func(setting string) string {
						head, body := "on: pull_request\n", "  review:\n"
						if setting != "" {
							if level == "workflow" {
								head += setting + "\n"
							} else {
								body += "    " + setting + "\n"
							}
						}
						return head + "jobs:\n" + body + "    steps:\n" + setup + "      - run: python3 tests/check.py\n"
					}
					oldSetting, newSetting := field.before, field.after
					switch change {
					case "introduced":
						oldSetting = ""
					case "removed":
						newSetting = ""
					case "identical":
						newSetting = oldSetting
					}
					before := workflow(oldSetting)
					if change == "identical" {
						serviceOnlyRemovalApplied(t, ".github/workflows/policy.yml", before)
						return
					}
					after := strings.Replace(workflow(newSetting), setup, "", 1)
					weakenedWorkflowRefused(t, ".github/workflows/policy.yml", before, after, strings.Split(field.before, ":")[0])
				})
			}
		}
	}
	// Existing entries do not authorize additional values, even those mentioning Tessl.
	for _, added := range []string{"PYTHONOPTIMIZE: '1'", "TESSL_NEW: injected", "UNRELATED: tessl install owner/policy"} {
		t.Run("added env/"+added, func(t *testing.T) {
			after := strings.Replace(reviewInsideTesslJob, setup, "", 1)
			after = strings.Replace(after, "    env:\n", "    env:\n      "+added+"\n", 1)
			weakenedWorkflowRefused(t, ".github/workflows/policy.yml", reviewInsideTesslJob, after, "environment")
		})
	}
}

func TestCorrectionWorkflowOccurrences(t *testing.T) {
	const prefix = "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n"
	const setup = "      - uses: tesslio/setup-tessl@v2\n"
	const check = "      - run: test -e marker\n"
	const mutate = "      - run: touch marker\n"
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"delete-first", "delete-last", "reorder", "keep", "insert"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				steps := check + mutate + check
				next := steps
				switch kind {
				case "delete-first":
					next = mutate + check
				case "delete-last":
					next = check + mutate
				case "reorder":
					next = mutate + check + check
				case "insert":
					next = "      - run: echo preparing\n" + steps
				}
				name := ".github/workflows/occurrences.yml"
				before, after := prefix+setup+steps, prefix+next
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
				checkCorrectionProposal(t, dry, root, opts, p, kind == "keep" || kind == "insert")
			})
		}
	}
	// Execute the observable failure-to-success counterexamples independently of
	// proposal validation. Separate working directories keep marker state local.
	for _, c := range []struct {
		name, script string
		pass         bool
	}{
		{"original", "test -e marker\ntouch marker\ntest -e marker", false},
		{"delete-first", "touch marker\ntest -e marker", true},
		{"reordered", "touch marker\ntest -e marker\ntest -e marker", true},
		{"service-only-removal", "test -e marker\ntouch marker\ntest -e marker", false},
	} {
		t.Run("execution/"+c.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-e", "-c", c.script)
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.pass {
				t.Fatalf("execution: %s %v", out, err)
			}
		})
	}
}

func TestCorrectionRetiredJobReferences(t *testing.T) {
	const retired = "  scoring:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: tesslio/patch-version-publish@v1\n"
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, fields string
			accepted     bool
		}{
			{"scalar", "    needs: scoring\n", false}, {"list", "    needs: [scoring, unrelated]\n", false},
			{"dot", "    if: needs.scoring.result == 'success'\n", false},
			{"bracket", "    if: ${{ needs['scoring'].result == 'success' }}\n", false},
			{"dynamic", "    if: ${{ needs[matrix.producer].result }}\n", false},
			{"whole-context", "    env:\n      RESULTS: ${{ toJSON(needs) }}\n", false},
			{"unrelated", "    needs: unrelated\n    if: needs.unrelated.result == 'success'\n", true},
			{"reusable-output", "    env:\n      VERSION: ${{ jobs.scoring.outputs.version }}\n", false},
			{"none", "", true},
		} {
			t.Run(fmt.Sprintf("%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				survivor := "  check:\n    runs-on: ubuntu-latest\n" + c.fields + "    steps:\n      - run: echo independent\n"
				before := "on: push\njobs:\n" + retired + survivor
				name := ".github/workflows/dependencies.yml"
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: "on: push\njobs:\n" + survivor})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrectionRetiredStepReferences(t *testing.T) {
	const prefix = "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n"
	const setup = "      - uses: tesslio/setup-tessl@v2\n        id: setup\n"
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, fields, consumer, replacement string
			accepted                            bool
		}{
			{"dot", "", "      - run: echo '${{ steps.setup.outputs.version }}'\n", "", false},
			{"bracket", "", "      - run: echo \"${{ steps['setup'].outputs.version }}\"\n", "", false},
			{"dynamic", "", "      - run: echo '${{ steps[matrix.producer].outputs.version }}'\n", "", false},
			{"whole-context", "", "      - run: echo '${{ toJSON(steps) }}'\n", "", false},
			{"outputs", "    outputs:\n      version: ${{ steps.setup.outputs.version }}\n", "      - run: echo check\n", "", false},
			{"id-reuse", "", "      - run: echo '${{ steps.setup.outputs.version }}'\n", "      - id: setup\n        run: echo replacement\n", false},
			{"unrelated", "", "      - run: echo '${{ steps.other.outputs.version }}'\n", "", true},
			{"unreferenced", "", "      - run: echo check\n", "", true},
			{"quoted-context", "", "      - run: echo \"${{ 'steps.setup.outputs.version' }}\"\n", "", true},
			{"other-job", "", "      - run: echo check\n  elsewhere:\n    outputs:\n      version: ${{ steps.setup.outputs.version }}\n    steps:\n      - id: setup\n        run: echo local\n", "", true},
		} {
			t.Run(fmt.Sprintf("%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				before := prefix + c.fields + "    steps:\n" + setup + c.consumer
				after := prefix + c.fields + "    steps:\n" + c.replacement + c.consumer
				name := ".github/workflows/outputs.yml"
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrectionActionIdentity(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, action := range []string{"custom/setup-tessl@v2", "custom/patch-version-publish@v1", "other/policy/.github/actions/skill-review@v1", "jbaruch/coding-policy/skill-review@v1", "./tesslio/setup-tessl@v2", "tesslio/setup-tessl@", "tesslio/setup-tessl@v2", "tesslio/setup-tessl@0123456789012345678901234567890123456789", "jbaruch/coding-policy/.github/actions/skill-review@v1"} {
			for _, keep := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/keep=%t/dry=%t", action, keep, dry), func(t *testing.T) {
					service := action == "tesslio/setup-tessl@v2" || action == "tesslio/setup-tessl@0123456789012345678901234567890123456789" || action == "jbaruch/coding-policy/.github/actions/skill-review@v1"
					root, opts, p := semanticFixture(t)
					prefix := "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n"
					step := "      - uses: " + action + "\n"
					tail := "      - run: echo independent\n"
					before := prefix + step + tail
					after := prefix + tail
					if strings.Contains(action, "skill-review") {
						after = "# " + correction12Notice + "\n" + after
					}
					if keep {
						after = before
					}
					name := ".github/workflows/actions.yml"
					put(t, root, name, before, 0o640)
					p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
					p.PolicyChanges = append(p.PolicyChanges, PolicyChange{Path: name, From: "Paid Tessl skill review", To: "Retired; ACR has no equivalent score"})
					// Unrelated actions remain read-only when the file has no service operation.
					if keep && !service {
						p.Edits = p.Edits[:len(p.Edits)-1]
						p.PolicyChanges = nil
					}
					checkCorrectionProposal(t, dry, root, opts, p, (!keep && service) || (keep && !service))
				})
			}
		}
	}
}

func TestCorrectionHistoricalContent(t *testing.T) {
	const url = "https://github.com/tessl-labs/original/blob/main/README.md"
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, path, before, after, action string
			accepted                          bool
		}{
			{"codeowners-remove", ".github/CODEOWNERS", "# History " + url + "\n* @independent-team\n", "", "remove", false},
			{"historical-replace", ".github/ISSUE_TEMPLATE/history.md", "History " + url + "\n", "Nothing here\n", "replace", false},
			{"mixed-nonworkflow-remove", ".github/config.txt", "tessl install owner/policy\nretain independent policy\n", "", "remove", false},
			{"repeated-url-delete", "plugins/orbit/skills/check/history.sh", "#!/bin/sh\n# Attribution " + url + "\n# Source " + url + "\ntessl install owner/policy\n", "#!/bin/sh\n# Attribution " + url + "\necho migrated\n", "replace", false},
			{"url-rewrite", "plugins/orbit/skills/check/history.sh", "#!/bin/sh\n# Attribution " + url + "\ntessl install owner/policy\n", "#!/bin/sh\n# Attribution https://github.com/other/project\necho migrated\n", "replace", false},
			{"url-preserved", "plugins/orbit/skills/check/history.sh", "#!/bin/sh\n# Attribution " + url + "\n# Source " + url + "\ntessl install owner/policy\n", "#!/bin/sh\n# Attribution " + url + "\n# Source " + url + "\necho migrated\n", "replace", true},
		} {
			t.Run(fmt.Sprintf("%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				put(t, root, c.path, c.before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: c.path, BeforeDigest: digest([]byte(c.before)), Action: c.action, Content: c.after})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrectionWholeWorkflowRetirement(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"service", "lookalike", "independent", "no-jobs"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				before := "on: push\njobs:\n  scoring:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - uses: tesslio/setup-tessl@v2\n      - uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n"
				switch kind {
				case "lookalike":
					before = strings.Replace(before, "jbaruch/coding-policy/.github/actions/skill-review@v1", "other/policy/skill-review@v1", 1)
				case "independent":
					before += "      - run: echo independent\n"
				case "no-jobs":
					before = "on: push\nenv:\n  TESSL_TOKEN: retired\n"
				}
				name := ".github/workflows/scoring.yml"
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "remove"})
				p.PolicyChanges = append(p.PolicyChanges, PolicyChange{Path: name, From: "Paid Tessl skill review", To: "Retired; ACR has no equivalent score"})
				checkCorrectionProposal(t, dry, root, opts, p, kind == "service")
			})
		}
	}
}

func TestCorrection9QuotedRetirement(t *testing.T) {
	for _, dry := range []bool{true, false} {
		for _, c := range []struct {
			name, expression string
			accepted         bool
		}{
			{"plain", "${{ steps.setup.outcome == 'success' }}", false},
			{"quoted-end", "${{ '}}' != '' && steps.setup.outcome == 'success' }}", false},
			{"bracket", "${{ '}}' != '' && steps['setup'].outcome == 'success' }}", false},
			{"doubled-quote", "${{ 'it''s }}' != '' && steps['setup'].outcome == 'success' }}", false},
			{"escaped-around-end", "${{ '''}}''' != '' && steps.setup.outcome }}", false},
			{"multiple", "${{ 'independent }}' }} ${{ '}}' && steps.setup.outcome }}", false},
			{"multiline", "${{ '}}' != '' &&\nsteps.setup.outcome == 'success' }}", false},
			{"incomplete", "${{ '}}' && steps.setup.outcome", false},
			{"nested-opening", "${{ ${{ steps.setup.outcome }}", false},
			{"literal-only", "${{ 'steps.setup.outcome }}' != '' }}", true},
			{"escaped-literal-only", "${{ 'it''s steps.setup }}' != '' }}", true},
			{"unrelated", "${{ '}}' != '' && steps.other.outcome == 'success' }}", true},
			{"unreferenced", "${{ '}}' }} ${{ 'independent' }}", true},
		} {
			for _, location := range []string{"step-if", "job-output", "other-job"} {
				t.Run(fmt.Sprintf("%s/%s/dry=%t", c.name, location, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					const name = ".github/workflows/quoted.yml"
					const setup = "      - uses: tesslio/setup-tessl@v2\n        id: setup\n"
					expression := strings.ReplaceAll(c.expression, "\n", "\n          ")
					fields, consumer := "", "      - run: echo required\n"
					if location == "step-if" {
						consumer = "      - if: >-\n          " + expression + "\n        run: exit 1\n"
					}
					if location == "job-output" {
						fields = "    outputs:\n      result: >-\n          " + expression + "\n"
					}
					if location == "other-job" {
						consumer += "  elsewhere:\n    runs-on: ubuntu-latest\n    outputs:\n      result: >-\n          " + expression + "\n    steps:\n      - id: setup\n        run: echo other\n"
					}
					before := "on: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n" + fields + "    steps:\n" + setup + consumer
					after := strings.Replace(before, setup, "", 1)
					put(t, root, name, before, 0o640)
					p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
					checkCorrectionProposal(t, dry, root, opts, p, c.accepted || location == "other-job")
				})
			}
		}
		for _, c := range []struct {
			name, expression string
			accepted         bool
		}{
			{"jobs-dot", "${{ '}}' && jobs.scoring.outputs.version }}", false},
			{"jobs-bracket", "${{ 'it''s }}' && jobs['scoring'].outputs.version }}", false},
			{"needs-dot", "${{ '}}' && needs.scoring.result }}", false},
			{"needs-bracket", "${{ '}}' && needs['scoring'].result }}", false},
			{"multiple", "${{ 'independent' }} ${{ '}}' && jobs.scoring.outputs.version }}", false},
			{"literal", "${{ 'jobs.scoring.outputs.version }}' }}", true},
			{"unrelated", "${{ '}}' && jobs.other.outputs.version }}", true},
		} {
			t.Run(fmt.Sprintf("reusable/%s/dry=%t", c.name, dry), func(t *testing.T) {
				root, opts, p := semanticFixture(t)
				const name = ".github/workflows/quoted-job.yml"
				head := "on:\n  workflow_call:\n    outputs:\n      version:\n        value: " + c.expression + "\njobs:\n"
				retired := "  scoring:\n    runs-on: ubuntu-latest\n    outputs:\n      version: released\n    steps:\n      - uses: tesslio/patch-version-publish@v1\n"
				survivor := "  independent:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo required\n"
				before, after := head+retired+survivor, head+survivor
				put(t, root, name, before, 0o640)
				p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
				checkCorrectionProposal(t, dry, root, opts, p, c.accepted)
			})
		}
	}
}

func TestCorrection9ResidualPublisher(t *testing.T) {
	const name = ".github/workflows/publish.yml"
	const setup = "      - uses: tesslio/setup-tessl@v2\n"
	for _, dry := range []bool{true, false} {
		for _, kind := range []string{"single-install", "closed-install", "unchanged-proposal", "supported-proposal", "single-install-removal", "historical", "lookalike", "independent", "standalone", "multiple"} {
			t.Run(fmt.Sprintf("%s/dry=%t", kind, dry), func(t *testing.T) {
				root, opts := fixture(t)
				opts.DryRun = dry
				operations := setup + "      - run: tessl install upstream/orbit\n"
				if kind == "closed-install" || kind == "supported-proposal" {
					operations = setup + serviceInstallStep
				}
				if kind == "historical" {
					operations = "      - run: echo https://github.com/tessl-labs/history\n      # Tessl history\n"
				}
				if kind == "lookalike" {
					operations = "      - uses: other/setup-tessl@v2\n"
				}
				if kind == "independent" {
					operations = ""
				}
				survivor := "  verify:\n    runs-on: ubuntu-latest\n    steps:\n" + operations + "      - run: ./tests/run.sh\n"
				if kind == "standalone" {
					survivor = ""
				}
				before := fixturePublisher + survivor
				if kind == "multiple" {
					before += strings.Replace(fixturePublisher[strings.Index(fixturePublisher, "  publish:"):], "  publish:", "  second:", 1)
				}
				put(t, root, name, before, 0o640)
				original := treeAt(t, root)
				checkStage := correctionStageCheck(t)
				defer checkStage()
				calls := 0
				provider := func(context.Context, string, string) (proposal, AgentRun, error) {
					calls++
					next := before
					if kind == "supported-proposal" || kind == "single-install-removal" {
						next = strings.Replace(before, operations, "", 1)
					}
					return proposal{Edits: []proposedEdit{{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: next}}}, AgentRun{}, nil
				}
				assisted := kind == "unchanged-proposal" || kind == "supported-proposal" || kind == "single-install-removal"
				if assisted {
					opts.Agent = "claude"
				}
				prepare := func() (Plan, error) {
					if assisted {
						return prepareWithProvider(context.Background(), opts, provider)
					}
					return Prepare(opts)
				}
				plan, err := prepare()
				if !matches(original, treeAt(t, root)) {
					t.Fatal("planning mutated input")
				}
				absent(t, root, ReceiptPath)
				absent(t, root, transactionPath)
				checkStage()
				accepted := kind == "supported-proposal" || kind == "historical" || kind == "lookalike" || kind == "independent" || kind == "standalone"
				if !accepted {
					if err == nil || !strings.Contains(err.Error(), name) {
						t.Fatalf("missing workflow refusal: %v", err)
					}
					if !assisted {
						var refusal *Error
						if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" {
							t.Fatalf("wrong blocker: %v", err)
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				expected := ""
				if survivor != "" {
					expected = fixturePublisher[:strings.Index(fixturePublisher, "  publish:")] + survivor
				}
				if kind == "supported-proposal" {
					expected = strings.Replace(expected, operations, "", 1)
				}
				if _, err = plan.Apply(); err != nil {
					t.Fatal(err)
				}
				if expected == "" {
					absent(t, root, name)
				} else if read(t, root, name) != expected {
					t.Fatal("retained job or trigger bytes changed")
				}
				if expected != "" {
					info, err := os.Stat(filepath.Join(root, name))
					if err != nil || info.Mode().Perm() != 0o640 {
						t.Fatalf("mode changed: %v", err)
					}
				}
				if read(t, root, publishWorkflowPath) != publishWorkflow {
					t.Fatal("ACR publisher differs")
				}
				applied := treeAt(t, root)
				firstCalls := calls
				current, err := prepare()
				if err != nil || !current.Report.Current || current.Report.Wrote || calls != firstCalls || !matches(applied, treeAt(t, root)) {
					t.Fatalf("rerun: %v", err)
				}
				if (kind == "supported-proposal" && calls != 1) || (kind != "supported-proposal" && calls != 0) {
					t.Fatalf("unexpected provider calls: %d", calls)
				}
			})
		}
	}
}

// TestCorrection11RetainedWorkflowFields covers the workflow invariant even
// when all original jobs qualify for retirement. The all-service/mixed
// counterexample is adopted from the full10 reviewer probe and judge11 ruling;
// acceptance here requires preservation, not reproduction of the old bypass.
func TestCorrection11RetainedWorkflowFields(t *testing.T) {
	const name = ".github/workflows/scoring.yml"
	const scoring = "      - uses: jbaruch/coding-policy/.github/actions/skill-review@v1\n"
	const retired = "      - run: echo 'Paid Tessl skill review was retired; ACR has no equivalent score.'\n"
	const independent = "  tests:\n    runs-on: ubuntu-latest\n    steps:\n      - run: exit 1\n"
	const environment = "env:\n  KEEP_FIRST: first\n  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n  GH_AW_SECRET_NAMES: FIRST,TESSL_TOKEN,SECOND\n  KEEP_LAST: last\n"
	for _, mixed := range []bool{false, true} {
		for _, dry := range []bool{true, false} {
			for _, kind := range []string{"unchanged", "display-name", "trigger", "permissions", "write-all", "environment", "env-change", "env-remove", "env-order", "credential-add", "credential-cleanup", "all-credentials-removed", "list-remove", "list-order", "comment-only", "nonmapping", "delete", "referenced-replacement", "missing-disclosure"} {
				t.Run(fmt.Sprintf("%s/mixed=%t/dry=%t", kind, mixed, dry), func(t *testing.T) {
					root, opts, p := semanticFixture(t)
					top := "name: Paid scoring\non: pull_request\npermissions:\n  contents: read\n"
					if strings.HasPrefix(kind, "env-") || strings.HasPrefix(kind, "list-") || kind == "credential-cleanup" || kind == "credential-add" {
						top += environment
					}
					if kind == "all-credentials-removed" {
						top += "env:\n  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n"
					}
					if kind == "referenced-replacement" {
						top += "run-name: ${{ jobs.score.outputs.result }}\n"
					}
					before := top + "jobs:\n  score:\n    runs-on: ubuntu-latest\n    steps:\n" + scoring
					if mixed {
						before += independent
					}
					after := strings.Replace(before, scoring, retired, 1)
					accepted, reason := false, ""
					switch kind {
					case "unchanged":
						accepted = true
					case "display-name":
						after = strings.Replace(after, "name: Paid scoring", "name: Scoring retirement notice", 1)
						accepted = true
					case "trigger":
						after = strings.Replace(after, "on: pull_request", "on: workflow_dispatch", 1)
						reason = "on condition/policy"
					case "permissions":
						after = strings.Replace(after, "contents: read", "contents: write", 1)
						reason = "permissions condition/policy"
					case "write-all":
						after = strings.Replace(after, "permissions:\n  contents: read", "permissions: write-all", 1)
						reason = "permissions condition/policy"
					case "environment":
						after = "env:\n  UNRELATED_POLICY: changed\n" + after
						reason = "environment/credential policy"
					case "env-change":
						after = strings.Replace(after, "KEEP_FIRST: first", "KEEP_FIRST: changed", 1)
						reason = "environment/credential policy"
					case "env-remove":
						after = strings.Replace(after, "  KEEP_FIRST: first\n", "", 1)
						reason = "environment/credential policy"
					case "env-order":
						after = strings.Replace(after, "  KEEP_FIRST: first\n", "", 1)
						after = strings.Replace(after, "  KEEP_LAST: last\n", "  KEEP_LAST: last\n  KEEP_FIRST: first\n", 1)
						reason = "environment/credential policy"
					case "credential-add":
						after = strings.Replace(after, "env:\n", "env:\n  TESSL_NEW: placeholder\n", 1)
						reason = "environment/credential policy"
					case "credential-cleanup":
						after = strings.Replace(after, "  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n", "", 1)
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "FIRST,SECOND", 1)
						accepted = true
					case "all-credentials-removed":
						after = strings.Replace(after, "env:\n  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n", "", 1)
						accepted = true
					case "list-remove":
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "FIRST", 1)
						reason = "environment/credential policy"
					case "list-order":
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "SECOND,FIRST", 1)
						reason = "environment/credential policy"
					case "comment-only":
						after, reason = "# Paid score retired\n", "workflow requires a mapping"
					case "nonmapping":
						after, reason = "[]\n", "workflow requires a mapping"
					case "delete":
						accepted, reason = !mixed, "retain independent review/test workflow"
					case "referenced-replacement":
						reason = "referenced service job"
					case "missing-disclosure":
						reason = "policyChanges"
					}
					if strings.HasPrefix(kind, "env-") || strings.HasPrefix(kind, "list-") {
						// Retire service credentials too, so a residual operation cannot
						// hide a changed unrelated field in the counterfactual.
						after = strings.Replace(after, "  TESSL_TOKEN: placeholder\n  SECRET_TESSL_TOKEN: placeholder\n", "", 1)
						after = strings.Replace(after, "FIRST,TESSL_TOKEN,SECOND", "FIRST,SECOND", 1)
					}
					put(t, root, name, before, 0o640)
					edit := proposedEdit{Path: name, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after}
					if kind == "delete" {
						edit.Action, edit.Content = "remove", ""
					}
					p.Edits = append(p.Edits, edit)
					if kind != "missing-disclosure" {
						p.PolicyChanges = []PolicyChange{{Path: name, From: "Tessl paid score", To: "Retire paid score; ACR has no equivalent score"}}
					}
					checkCorrectionProposal(t, dry, root, opts, p, accepted, name, reason)
					if accepted {
						info, err := os.Stat(filepath.Join(root, ReceiptPath))
						if err != nil || info.Mode().Perm() != 0o600 {
							t.Fatalf("accepted receipt mode: %v %v", info, err)
						}
						value, err := manifest.Load(root)
						if err != nil {
							t.Fatal(err)
						}
						files, err := manifest.PackageFiles(root, value)
						if err != nil {
							t.Fatal(err)
						}
						var rec receipt
						if err := json.Unmarshal([]byte(read(t, root, ReceiptPath)), &rec); err != nil {
							t.Fatal(err)
						}
						if strings.Join(files, "\n") != strings.Join(rec.PublishedFiles, "\n") || len(rec.PolicyChanges) != 1 || rec.PolicyChanges[0] != p.PolicyChanges[0] {
							t.Fatalf("receipt inventory or retirement disclosure differs: %+v", rec)
						}
					}
				})
			}
		}
	}
}

// The correction18 API controls retain complete physical evidence and execute
// the version adaptation; a failing provider proves current reruns are inert.
func checkCorrection18Workflow(t *testing.T, dry bool, before, after string, accepted, disclose bool) {
	t.Helper()
	checkStage := correctionStageCheck(t)
	defer checkStage()
	root, opts, p := semanticFixture(t)
	const helper = "plugins/orbit/skills/check/version.py"
	old := "import json\nfrom pathlib import Path\nmetadata = Path(__file__).resolve().parents[2] / '.tessl-plugin/plugin.json'\nprint(json.loads(metadata.read_text())['version'])\n"
	next := strings.Replace(old, "Path(__file__).resolve().parents[2] / '.tessl-plugin/plugin.json'", "Path(__file__).with_name('.acr-package.json')", 1)
	put(t, root, helper, old, 0o751)
	p.Edits = append(p.Edits, proposedEdit{Path: helper, BeforeDigest: digest([]byte(old)), Action: "replace", Content: next})
	opts.PackageVersion = "2.3.4"
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
	put(t, root, ".claude/unchanged", "consumer bytes\n", 0o640)
	if err := os.Symlink("unchanged", filepath.Join(root, ".claude/link")); err != nil {
		t.Fatal(err)
	}
	original := correction12Inventory(t, root)
	inodes := map[string]os.FileInfo{}
	for name := range original {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		inodes[name] = info
	}
	unchanged := func() {
		t.Helper()
		correction12Unchanged(t, root, original)
		for name, info := range inodes {
			now, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
			if err != nil || !os.SameFile(info, now) {
				t.Fatalf("inode changed: %s %v", name, err)
			}
		}
		absent(t, root, ReceiptPath)
		absent(t, root, transactionPath)
	}
	opts.DryRun = dry
	calls := 0
	provider := func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil }
	plan, err := prepareWithProvider(context.Background(), opts, provider)
	unchanged()
	if !accepted {
		if err == nil {
			t.Fatal("unsafe publisher proposal accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("proposal calls=%d", calls)
	}
	if dry {
		unchanged()
		return
	}
	sentinel := func(context.Context, string, string) (proposal, AgentRun, error) {
		t.Error("current rerun invoked provider")
		return proposal{}, AgentRun{}, errors.New("provider must not run")
	}
	correction12Apply(t, root, opts, plan, original, sentinel)
	if after != "" && read(t, root, name) != after {
		t.Fatal("retained workflow differs from proposal")
	}
	command := exec.Command("python3", "-I", "-S", filepath.Join(root, helper))
	if output, err := command.CombinedOutput(); err != nil || string(output) != "2.3.4\n" {
		t.Fatalf("version adaptation: %s %v", output, err)
	}
	applied := correction12Inventory(t, root)
	appliedInodes := map[string]os.FileInfo{}
	for name := range applied {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		appliedInodes[name] = info
	}
	current, err := prepareWithProvider(context.Background(), opts, sentinel)
	if err != nil || !current.Report.Current || current.Report.Wrote || len(current.Report.AgentRuns) != 0 {
		t.Fatalf("current rerun: %v %+v", err, current.Report)
	}
	correction12Unchanged(t, root, applied)
	for name, info := range appliedInodes {
		now, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !os.SameFile(info, now) {
			t.Fatalf("rerun inode changed: %s %v", name, err)
		}
	}
	absent(t, root, transactionPath)
}
