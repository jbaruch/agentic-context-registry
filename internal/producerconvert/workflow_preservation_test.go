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
