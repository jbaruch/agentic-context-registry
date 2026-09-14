package producerconvert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// The preservation policy for a semantic proposal is a fixed sequence of named
// constraints. Order is load-bearing and pinned by TestPreservationConstraintOrder:
//
//	credential refusal    snapshot.go credentialPath   before any input read
//	edit resolution       resolveEdit                  shape, authority and digest of each edit
//	editConstraints       one resolved edit            findings collected across every edit
//	proposalConstraints   the whole proposal           before any private stage exists
//	staging and oracle    prepareDeterministic         the acceptance test on the staged candidate
//	candidateConstraints  the accepted candidate       after the deterministic pass
//
// contentConstraints, workflowConstraints and workflowJobConstraints are the
// retained-content policy that the edit path and the deterministic workflow
// translation share through preserveChecksWithSource. check-tests.py holds the
// Python test constraints. A new finding adds a named row and a pinned test,
// never a branch inside a driver.

// constraint is one named row of a list applied in order. A nil applies means
// every candidate. Lists run through apply stop at their first failure.
type constraint[T any] struct {
	name    string
	applies func(T) bool
	check   func(T) error
}

func apply[T any](list []constraint[T], candidate T) error {
	for _, item := range list {
		if item.applies != nil && !item.applies(candidate) {
			continue
		}
		if err := item.check(candidate); err != nil {
			return err
		}
	}
	return nil
}

func constraintNames[T any](list []constraint[T]) []string {
	names := make([]string, 0, len(list))
	for _, item := range list {
		names = append(names, item.name)
	}
	return names
}

// editCandidate is one proposed edit after resolveEdit accepted its shape.
type editCandidate struct {
	plan    Plan
	name    string
	exists  bool
	before  fileState
	body    []byte // nil for a removal
	mode    uint32
	removal bool
}

// editConstraint rows run for every resolved edit. A fatal failure ends
// validation; every other failure is collected so independent scope failures
// report together and a later repair can address all of them.
type editConstraint struct {
	name    string
	applies func(editCandidate) bool
	check   func(context.Context, editCandidate) error
	fatal   bool
}

func editConstraintNames() []string {
	names := make([]string, 0, len(editConstraints))
	for _, item := range editConstraints {
		names = append(names, item.name)
	}
	return names
}

func contentEdit(c editCandidate) bool { return !c.removal }

var editConstraints = []editConstraint{
	{name: "workflow-removal", applies: func(c editCandidate) bool { return c.removal }, check: func(_ context.Context, c editCandidate) error {
		return preserveChecksWithSource(c.name, c.before.Content, nil, c.plan.before)
	}},
	{name: "package-file-scheme", applies: contentEdit, check: func(_ context.Context, c editCandidate) error {
		if bytes.Contains(c.body, []byte("package-file:")) {
			return fmt.Errorf("%s: package-file: is not an ACR reference scheme; use supported repository-relative skill-file paths", c.name)
		}
		return nil
	}},
	{name: "text-bounds", applies: contentEdit, fatal: true, check: func(_ context.Context, c editCandidate) error {
		if len(c.body) == 0 || len(c.body) > maxProposalBytes || !utf8.Valid(c.body) {
			return fmt.Errorf("%s: empty, oversized or non-text output", c.name)
		}
		return nil
	}},
	{name: "actions-lock", applies: func(c editCandidate) bool { return contentEdit(c) && c.name == ".github/aw/actions-lock.json" }, check: func(_ context.Context, c editCandidate) error {
		if err := preserveActionsLock(c.before.Content, c.body); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		return nil
	}},
	{name: "public-repository-urls", applies: func(c editCandidate) bool { return contentEdit(c) && !workflowFile(c.name) }, check: func(_ context.Context, c editCandidate) error {
		nextURLs, oldURLs := repositoryURLCounts(c.body), repositoryURLCounts(c.before.Content)
		checkedURLs := map[string]bool{}
		for _, token := range publicRepositoryURLs.FindAllString(string(c.before.Content), -1) {
			if checkedURLs[token] {
				continue
			}
			checkedURLs[token] = true
			if nextURLs[token] != oldURLs[token] {
				return fmt.Errorf("%s: historical public repository URL %s and its multiplicity must survive", c.name, token)
			}
		}
		return nil
	}},
	{name: "foreign-installed-roots", applies: contentEdit, check: func(_ context.Context, c editCandidate) error {
		for _, foreign := range foreignInstalledRoots.FindAll(c.before.Content, -1) {
			if string(foreign) == ".tessl/plugins/"+c.plan.Report.SourcePackage+"/" {
				continue
			}
			if bytes.Count(c.body, foreign) < bytes.Count(c.before.Content, foreign) {
				return fmt.Errorf("%s: foreign installed reference %s must survive", c.name, foreign)
			}
		}
		return nil
	}},
	{name: "retained-content", applies: contentEdit, check: func(_ context.Context, c editCandidate) error {
		return preserveChecksWithSource(c.name, c.before.Content, c.body, c.plan.before)
	}},
	{name: "executable-test-body", applies: func(c editCandidate) bool {
		return contentEdit(c) && c.exists && strings.HasPrefix(c.name, "tests/") && (path.Ext(c.name) == ".sh" || path.Ext(c.name) == ".go")
	}, check: func(_ context.Context, c editCandidate) error {
		adapted := bytes.ReplaceAll(c.before.Content, []byte(".tessl/plugins/"+c.plan.Report.SourcePackage+"/"), []byte(strings.TrimPrefix(c.plan.options.PackageRoot+"/", "./")))
		if !bytes.Equal(testExecutableBody(adapted, path.Ext(c.name)), testExecutableBody(c.body, path.Ext(c.name))) {
			return fmt.Errorf("%s: unsupported test edit; retain independent executable checks and registration, adapting only source references or leading comments", c.name)
		}
		return nil
	}},
	{name: "syntax", applies: contentEdit, check: func(ctx context.Context, c editCandidate) error {
		return syntaxCheck(ctx, c.name, c.body)
	}},
	{name: "python-test-checker", applies: func(c editCandidate) bool {
		return contentEdit(c) && c.exists && strings.HasPrefix(c.name, "tests/") && strings.HasSuffix(c.name, ".py")
	}, check: pythonTestChecker},
}

// pythonTestChecker runs the embedded parse-only checker exactly once per
// proposed Python test file. It never imports or executes the proposal.
func pythonTestChecker(ctx context.Context, c editCandidate) error {
	data, err := json.Marshal(map[string]string{"before": string(c.before.Content), "after": string(c.body)})
	if err != nil {
		return fmt.Errorf("%s: test preservation: %w", c.name, err)
	}
	command := exec.CommandContext(ctx, "python3", "-I", "-S", "-c", pythonTestChecks)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	command.Stdin = bytes.NewReader(data)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: test preservation: %w: %s", c.name, err, output)
	}
	return nil
}

// proposalCandidate is the whole proposal after every edit resolved, before
// any private stage exists.
type proposalCandidate struct {
	plan     Plan
	proposed proposal
	seen     map[string]bool
	next     tree
}

var proposalConstraints = []constraint[proposalCandidate]{
	{name: "policy-change-shape", check: func(c proposalCandidate) error {
		for _, policy := range c.proposed.PolicyChanges {
			if !c.seen[policy.Path] || strings.TrimSpace(policy.From) == "" || strings.TrimSpace(policy.To) == "" {
				return fmt.Errorf("policy changes must explain a changed file with non-empty from/to")
			}
		}
		return nil
	}},
	// Every retained regular input is copied into the private stage. Refuse
	// unsupported materialization before creating that stage or repairing a
	// proposal; source ACLs cannot accompany a fresh mode-000 inode.
	{name: "stageable-modes", check: func(c proposalCandidate) error {
		for _, name := range sortedPaths(c.next) {
			state := c.next[name]
			if !state.Directory && state.Link == "" && state.Mode == 0 {
				return unsupportedFileMode(name, "semantic validation staging")
			}
		}
		return nil
	}},
}

// candidateResult is the staged candidate the deterministic oracle accepted.
type candidateResult struct {
	plan      Plan
	candidate Plan
	proposed  proposal
}

var candidateConstraints = []constraint[candidateResult]{
	{name: "gh-aw-metadata", check: func(c candidateResult) error {
		return reconcileGHWorkflowMetadata(c.plan.before, c.candidate.after)
	}},
	{name: "paid-disclosure", check: func(c candidateResult) error {
		return validatePaidDeclarations(c.plan.before, c.candidate.after, c.proposed.PolicyChanges, c.plan.options.PackageRoot)
	}},
}

// contentCandidate is one retained file's original and proposed bytes. after
// is nil for a removal. original is the snapshot that owns the file, when the
// caller has one.
type contentCandidate struct {
	name     string
	before   []byte
	after    []byte
	original tree
}

func testContent(c contentCandidate) bool { return strings.HasPrefix(c.name, "tests/") }

var contentConstraints = []constraint[contentCandidate]{
	{name: "test-definitions-remain", applies: testContent, check: func(c contentCandidate) error {
		for _, match := range testNames.FindAllSubmatch(c.before, -1) {
			if !bytes.Contains(c.after, match[1]) {
				return fmt.Errorf("%s: original test %s must remain", c.name, match[1])
			}
		}
		return nil
	}},
	{name: "test-assertion-count", applies: testContent, check: func(c contentCandidate) error {
		if bytes.Count(c.after, []byte("assert")) < bytes.Count(c.before, []byte("assert")) {
			return fmt.Errorf("%s: preserve all independent assertions", c.name)
		}
		return nil
	}},
	{name: "test-bypass-spellings", applies: testContent, check: func(c contentCandidate) error {
		for _, weakening := range []string{"@unittest.skip", "pytest.skip(", "expectedFailure"} {
			if bytes.Count(c.after, []byte(weakening)) > bytes.Count(c.before, []byte(weakening)) {
				return fmt.Errorf("%s: added test bypass %s", c.name, weakening)
			}
		}
		return nil
	}},
	{name: "workflow-policy", applies: func(c contentCandidate) bool { return workflowFile(c.name) }, check: workflowPolicy},
}

// workflowDocument is a retained workflow's original top-level mapping and the
// proposed document, nil for a removal.
type workflowDocument struct {
	name     string
	before   *yaml.Node
	after    *yaml.Node
	original tree
}

// top is the proposed top-level mapping once workflow-document accepted it.
func (d workflowDocument) top() *yaml.Node {
	if d.after == nil {
		return nil
	}
	return d.after.Content[0]
}

func workflowPolicy(c contentCandidate) error {
	var old, new yaml.Node
	if err := yaml.Unmarshal(c.before, &old); err != nil {
		return err
	}
	if len(c.after) > 0 {
		if err := yaml.Unmarshal(c.after, &new); err != nil {
			return err
		}
	}
	if len(old.Content) == 0 {
		return nil
	}
	document := workflowDocument{name: c.name, before: old.Content[0], original: c.original}
	if len(c.after) > 0 {
		document.after = &new
	}
	return apply(workflowConstraints, document)
}

func retainedWorkflow(d workflowDocument) bool { return d.after != nil }
func removedWorkflow(d workflowDocument) bool  { return d.after == nil }

var workflowConstraints = []constraint[workflowDocument]{
	{name: "workflow-document", applies: retainedWorkflow, check: func(d workflowDocument) error {
		if len(d.after.Content) != 1 || d.after.Content[0].Kind != yaml.MappingNode {
			return fmt.Errorf("%s: workflow requires a mapping", d.name)
		}
		return nil
	}},
	{name: "workflow-fields", applies: retainedWorkflow, check: func(d workflowDocument) error {
		if err := preserveWorkflowFields(d.before, d.top(), false, "jobs", "name"); err != nil {
			return fmt.Errorf("%s: %w", d.name, err)
		}
		return nil
	}},
	{name: "workflow-removal-jobs", applies: removedWorkflow, check: func(d workflowDocument) error {
		jobs := member(d.before, "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) == 0 {
			return fmt.Errorf("%s: removal requires a proven service-only workflow", d.name)
		}
		return nil
	}},
	{name: "workflow-jobs", applies: func(d workflowDocument) bool { return member(d.before, "jobs") != nil }, check: func(d workflowDocument) error {
		jobs := member(d.before, "jobs")
		for i := 0; i < len(jobs.Content); i += 2 {
			job := workflowJob{
				name:     d.name,
				job:      jobs.Content[i].Value,
				before:   jobs.Content[i+1],
				after:    member(member(d.top(), "jobs"), jobs.Content[i].Value),
				kind:     deliveryJobKind(jobs.Content[i+1]),
				workflow: d.top(),
				jobs:     member(d.top(), "jobs"),
				original: d.original,
			}
			if err := apply(workflowJobConstraints, job); err != nil {
				return err
			}
		}
		return nil
	}},
}

// workflowJob is one original job and its proposed counterpart, nil when the
// proposal retires it. workflow and jobs are the proposed document's mapping
// and jobs member, nil for a removed workflow.
type workflowJob struct {
	name     string
	job      string
	before   *yaml.Node
	after    *yaml.Node
	kind     deliveryJobType
	workflow *yaml.Node
	jobs     *yaml.Node
	original tree
}

func (j workflowJob) retained() bool { return j.after != nil }

// reusablePublisher is the one execution-shape exchange a publisher may make.
func (j workflowJob) reusablePublisher() bool {
	return j.kind == publisherDeliveryJob && member(j.after, "uses") != nil
}

var workflowJobConstraints = []constraint[workflowJob]{
	{name: "publisher-step-conditions", check: func(j workflowJob) error {
		if err := preservePublisherStepConditions(j.before, j.after); err != nil {
			return fmt.Errorf("%s job %s: %w", j.name, j.job, err)
		}
		return nil
	}},
	{name: "referenced-service-job", applies: func(j workflowJob) bool { return j.kind != independentDeliveryJob && j.workflow != nil }, check: func(j workflowJob) error {
		if !sameYAML(j.before, j.after) && jobReferences(j.workflow, j.jobs, j.job) {
			return fmt.Errorf("%s: referenced service job %q must remain unchanged", j.name, j.job)
		}
		return nil
	}},
	{name: "retired-job", applies: func(j workflowJob) bool { return !j.retained() }, check: func(j workflowJob) error {
		if removableDeliveryJob(j.before) {
			return nil
		}
		if j.workflow == nil {
			return fmt.Errorf("%s: retain independent review/test workflow and publication job %s policy", j.name, j.job)
		}
		return fmt.Errorf("%s: independent job %s must remain with its policy", j.name, j.job)
	}},
	{name: "publisher-rewrite", applies: func(j workflowJob) bool { return j.retained() && j.reusablePublisher() }, check: func(j workflowJob) error {
		if err := preservePublisherRewrite(j.before, j.after, j.workflow); err != nil {
			return fmt.Errorf("%s job %s: %w", j.name, j.job, err)
		}
		return nil
	}},
	{name: "job-policy", applies: func(j workflowJob) bool { return j.retained() && !j.reusablePublisher() }, check: func(j workflowJob) error {
		if err := preserveWorkflowJob(j.before, j.after, verifiedGHWorkflowPair(j.original, j.name)); err != nil {
			return fmt.Errorf("%s job %s: %w", j.name, j.job, err)
		}
		return nil
	}},
}
