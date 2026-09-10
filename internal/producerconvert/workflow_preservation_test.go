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
