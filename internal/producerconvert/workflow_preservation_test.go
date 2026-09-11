package producerconvert

import (
	"strings"
	"testing"
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
	for _, active := range []string{"run: tessl install owner/policy\n", "uses: owner/policy/.github/actions/skill-review@v1\n", "run: |\n  tessl review skill\n", "env:\n  TESSL_API_KEY: secret\n"} {
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
		return "# Tessl service migration\non: pull_request\njobs:\n  review:\n    steps:\n      - name: Install policy\n        run: |\n          " + strings.ReplaceAll(run, "\n", "\n          ") + "\n      - run: review --required\n"
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
