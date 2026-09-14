package producerconvert

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Order is load-bearing: credential refusal before any read, mode-000 refusal
// before any stage, every scope before combined validation, paid disclosure
// after the deterministic pass. Reordering is a design decision, never a
// refactor, so it has to change this table on purpose.
func TestPreservationConstraintOrder(t *testing.T) {
	for _, tc := range []struct {
		list string
		got  []string
		want []string
	}{
		{"edit", editConstraintNames(), []string{"workflow-removal", "package-file-scheme", "text-bounds", "actions-lock", "public-repository-urls", "foreign-installed-roots", "retained-content", "executable-test-body", "syntax", "python-test-checker"}},
		{"proposal", constraintNames(proposalConstraints), []string{"policy-change-shape", "stageable-modes"}},
		{"candidate", constraintNames(candidateConstraints), []string{"gh-aw-metadata", "paid-disclosure"}},
		{"content", constraintNames(contentConstraints), []string{"test-definitions-remain", "test-assertion-count", "test-bypass-spellings", "workflow-policy"}},
		{"workflow", constraintNames(workflowConstraints), []string{"workflow-document", "workflow-fields", "workflow-removal-jobs", "workflow-jobs"}},
		{"job", constraintNames(workflowJobConstraints), []string{"publisher-step-conditions", "referenced-service-job", "retired-job", "publisher-rewrite", "job-policy"}},
		{"python", pythonConstraintNames(t), []string{"original-test-remains", "owner-checks-remain", "failure-collector-unchanged", "test-registration-remains", "test-decorators-remain", "skips-not-added"}},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s constraints changed:\n got %v\nwant %v", tc.list, tc.got, tc.want)
		}
	}
}

// Every constraint names the tests that pin it, and every named test exists,
// so a new row cannot land without the finding that gave it a reason to exist.
// The map records which tests exercise a row; it does not prove they do.
func TestPreservationConstraintsArePinned(t *testing.T) {
	pinned := map[string][]string{
		"edit/workflow-removal":              {"TestSemanticWorkflowKeepsIndependentReviewInsideTesslJob", "TestCorrectionWholeWorkflowRetirement", "TestCorrection17PaidAndPublisherRetirement"},
		"edit/package-file-scheme":           {"TestProposalConstraintsRefuseBeforeStaging"},
		"edit/text-bounds":                   {"TestProposalConstraintsRefuseBeforeStaging"},
		"edit/actions-lock":                  {"TestCorrection9ActionsLock", "TestGHWorkflowProposalPreservesLockPolicy"},
		"edit/public-repository-urls":        {"TestCorrectionPublicURLTokensAndPatches"},
		"edit/foreign-installed-roots":       {"TestProposalConstraintsRefuseBeforeStaging"},
		"edit/retained-content":              {"TestSemanticProposalRejectsUntrustedEditsBeforeWrites", "TestSemanticProposalPreservesAllWorkflowPolicy"},
		"edit/executable-test-body":          {"TestCorrection14EditableChecks"},
		"edit/syntax":                        {"TestCorrection14DeclaredShell", "TestSemanticWorkflowRejectsPlaceholderAndMultipleDocuments", "TestSemanticValidationReportsIndependentScopeFailuresTogether"},
		"edit/python-test-checker":           {"TestCorrection14PythonObligations", "TestCorrection15DistinctPythonTests", "TestCorrection16NestedPythonChecks", "TestCorrection17ExplicitRaises", "TestSemanticProposalRefusesDecoratedTestBypass"},
		"proposal/policy-change-shape":       {"TestProposalConstraintsRefuseBeforeStaging"},
		"proposal/stageable-modes":           {"TestCorrection9ReadOnlyStaging", "TestCorrection14SemanticZeroStagePortable", "TestCorrection14NativeSemanticZeroStage"},
		"candidate/gh-aw-metadata":           {"TestGHWorkflowMetadataBindsCoherentChangedSource", "TestGHWorkflowMetadataRefusesUnprovenCompilation"},
		"candidate/paid-disclosure":          {"TestCorrection12PaidDeclaration", "TestCorrection12DisclosureAfterDeterministicPass", "TestCorrection14MappedMetadataIsNotPaidPolicy"},
		"content/test-definitions-remain":    {"TestSemanticProtectsForeignConsumerEvidenceAndTestRegistration", "TestSemanticProposalRejectsUntrustedEditsBeforeWrites"},
		"content/test-assertion-count":       {"TestSemanticProposalRejectsUntrustedEditsBeforeWrites"},
		"content/test-bypass-spellings":      {"TestRetainedContentRefusesSpelledTestBypasses"},
		"content/workflow-policy":            {"TestSemanticProposalPreservesAllWorkflowPolicy", "TestCorrectionWorkflowOccurrences"},
		"workflow/workflow-document":         {"TestCorrection11RetainedWorkflowFields"},
		"workflow/workflow-fields":           {"TestCorrection11RetainedWorkflowFields", "TestSemanticProposalPreservesAllWorkflowPolicy"},
		"workflow/workflow-removal-jobs":     {"TestCorrectionWholeWorkflowRetirement"},
		"workflow/workflow-jobs":             {"TestCorrectionWorkflowOccurrences", "TestCorrectionRetiredJobReferences"},
		"job/publisher-step-conditions":      {"TestCorrection18PublisherStepConditions"},
		"job/referenced-service-job":         {"TestCorrectionRetiredJobReferences", "TestCorrection11RetainedWorkflowFields"},
		"job/retired-job":                    {"TestCorrection17PaidAndPublisherRetirement", "TestSemanticWorkflowDisclosureRetainsTestsWithoutService", "TestCorrection11RetainedWorkflowFields"},
		"job/publisher-rewrite":              {"TestCorrection17PublisherPolicy", "TestCorrection17PublisherReusableShape", "TestCorrection18ReusableCallerPermissions"},
		"job/job-policy":                     {"TestSemanticWorkflowRefusesMixedRunStepChanges", "TestSemanticWorkflowPreservesJobContinueOnError", "TestCorrectionRetiredStepReferences", "TestSemanticProposalRefusesMixedRunStepBeforeWrites"},
		"python/original-test-remains":       {"TestPythonTestCheckerExecutionContract", "TestPythonTestCheckerDistinctOwnersAndOccurrences", "TestCorrection15DistinctPythonTests"},
		"python/owner-checks-remain":         {"TestPythonTestCheckerNestedCheckOwners", "TestPythonTestCheckerRaiseStatements", "TestPythonTestCheckerRaiseOwners", "TestCorrection16NestedPythonChecks", "TestCorrection17ExplicitRaises"},
		"python/failure-collector-unchanged": {"TestPythonTestCheckerExecutionContract", "TestPythonTestCheckerDistinctOwnersAndOccurrences"},
		"python/test-registration-remains":   {"TestPythonTestCheckerExecutionContract", "TestCorrection14PythonObligations"},
		"python/test-decorators-remain":      {"TestPythonTestCheckerRefusesTestBypasses", "TestPythonTestCheckerRetainsAdaptedDecorators", "TestSemanticProposalRefusesDecoratedTestBypass"},
		"python/skips-not-added":             {"TestPythonTestCheckerRefusesTestBypasses", "TestSemanticProposalRefusesDecoratedTestBypass"},
	}
	defined := map[string]bool{}
	for _, list := range []struct {
		name  string
		names []string
	}{
		{"edit", editConstraintNames()},
		{"proposal", constraintNames(proposalConstraints)},
		{"candidate", constraintNames(candidateConstraints)},
		{"content", constraintNames(contentConstraints)},
		{"workflow", constraintNames(workflowConstraints)},
		{"job", constraintNames(workflowJobConstraints)},
		{"python", pythonConstraintNames(t)},
	} {
		seen := map[string]bool{}
		for _, name := range list.names {
			if name == "" || seen[name] {
				t.Errorf("%s: unnamed or duplicate constraint %q", list.name, name)
			}
			seen[name] = true
			defined[list.name+"/"+name] = true
		}
	}
	tests := packageTestNames(t)
	for key := range defined {
		names := pinned[key]
		if len(names) == 0 {
			t.Errorf("%s has no pinned test", key)
		}
		for _, name := range names {
			if !tests[name] {
				t.Errorf("%s: pinned test %s does not exist in this package", key, name)
			}
		}
	}
	for key := range pinned {
		if !defined[key] {
			t.Errorf("%s is pinned but is not a constraint", key)
		}
	}
}

// packageTestNames reads this package's test sources, which is where a pinned
// test has to live for `go test` to run it beside the constraint it pins.
func packageTestNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	declaration := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	result := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range declaration.FindAllSubmatch(source, -1) {
			result[string(match[1])] = true
		}
	}
	return result
}

// Rows no earlier round pinned on their own. Each refuses through
// validateProposal before any private stage exists and leaves the source alone.
func TestProposalConstraintsRefuseBeforeStaging(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(*proposal, tree)
	}{
		{"package-file-scheme", "package-file: is not an ACR reference scheme", func(p *proposal, _ tree) {
			p.Edits[0].Content += "# see package-file:skills/check/data.txt\n"
		}},
		{"text-bounds/empty", "empty, oversized or non-text output", func(p *proposal, _ tree) { p.Edits[0].Content = "" }},
		{"text-bounds/oversized", "empty, oversized or non-text output", func(p *proposal, _ tree) {
			p.Edits[0].Content += strings.Repeat("#", maxProposalBytes)
		}},
		{"text-bounds/non-text", "empty, oversized or non-text output", func(p *proposal, _ tree) { p.Edits[0].Content += "# \xff\n" }},
		{"foreign-installed-roots", "foreign installed reference .tessl/plugins/foreign/tools/ must survive", func(p *proposal, before tree) {
			const name = "plugins/orbit/skills/inspect/SKILL.md"
			p.Edits = append(p.Edits, proposedEdit{Path: name, BeforeDigest: before[name].Digest, Action: "replace", Content: "# Inspect\nRun `plugins/orbit/skills/check/check.sh`.\nRead [data](skills/check/data.txt).\n"})
		}},
		{"policy-change-shape/unedited", "policy changes must explain a changed file with non-empty from/to", func(p *proposal, _ tree) {
			p.PolicyChanges = []PolicyChange{{Path: "LICENSE", From: "Paid Tessl skill review", To: "Retired; ACR has no equivalent score."}}
		}},
		{"policy-change-shape/blank", "policy changes must explain a changed file with non-empty from/to", func(p *proposal, _ tree) {
			p.PolicyChanges = []PolicyChange{{Path: p.Edits[0].Path, From: " ", To: "Retired; ACR has no equivalent score."}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, opts, proposed := semanticFixture(t)
			p, err := prepareDeterministic(opts, true)
			if err == nil {
				t.Fatal("missing semantic trigger")
			}
			tc.mutate(&proposed, p.before)
			before := treeAt(t, root)
			check := correctionStageCheck(t)
			defer check()
			result, err := validateProposal(context.Background(), p, proposed)
			if err == nil || !strings.Contains(err.Error(), tc.reason) || result.receipt != nil {
				t.Fatalf("expected %s refusal: %v", tc.name, err)
			}
			if !matches(before, treeAt(t, root)) {
				t.Fatal("refusal mutated source")
			}
		})
	}
}

// The spelling rows stay beside the resolved-name checker; overlapping findings
// keep their own reasons rather than collapsing into one.
func TestRetainedContentRefusesSpelledTestBypasses(t *testing.T) {
	const before = "def test_ok():\n    assert 1\n"
	for _, spelling := range []string{"@unittest.skip", "pytest.skip(", "expectedFailure"} {
		after := "# " + spelling + "\n" + before
		if err := preserveChecks("tests/test_ok.py", []byte(before), []byte(after)); err == nil || !strings.Contains(err.Error(), "added test bypass "+spelling) {
			t.Fatalf("%s accepted: %v", spelling, err)
		}
		if err := preserveChecks("tests/test_ok.py", []byte(after), []byte(after)); err != nil {
			t.Fatalf("unchanged spelling refused: %v", err)
		}
		if err := preserveChecks("skills/check/notes.py", []byte(before), []byte(after)); err != nil {
			t.Fatalf("non-test path refused: %v", err)
		}
	}
}
