package migrateapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// The reviewed cutover this fixture reproduces: the installed Tessl rule
// carries a description ACR's activation model has no field for, and the
// replacement package rewrote a skill on purpose. One artifact is lossy, one
// differs by body, and neither is a defect to reconcile.
const (
	reviewedDescription = "keeps the reviewer honest"
	reviewedRuleBody    = "# Always\n"
	reviewedSkillBody   = "# Review, revised for ACR\n"
)

// reviewedConsumer seeds a Tessl consumer carrying the lossy rule and its
// Cursor native, so an accepted artifact's Tessl output is observable.
func reviewedConsumer(t *testing.T) string {
	t.Helper()
	root := seedConsumer(t)
	writeReviewedRule(t, root, reviewedDescription, reviewedRuleBody)
	return root
}

// writeReviewedRule writes the installed rule and the Cursor native together.
// Tessl's native is the rule source behind one frontmatter block; writing one
// without the other is drift, which is a different refusal entirely.
func writeReviewedRule(t *testing.T, root, description, body string) {
	t.Helper()
	source := []byte("---\nalwaysApply: true\ndescription: " + description + "\n---\n" + body)
	writeFile(t, root, ".tessl/plugins/example/alpha/rules/always-rule.md", source, 0o644)
	writeFile(t, root, ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc",
		append([]byte("---\nalwaysApply: true\n---\n\n"), source...), 0o644)
}

// reviewedArchive builds the replacement package. Its rule carries no
// description, and its skill body is the reviewed rewrite.
func reviewedArchive(t *testing.T, skillBody string) []byte {
	t.Helper()
	manifest := "schemaVersion: 1\nname: example/alpha\nversion: 1.0.0\nsource:\n  repository: https://github.com/example/alpha\n" +
		"artifacts:\n  rules:\n    - id: always-rule\n      path: rules/always-rule.md\n      activation:\n        mode: always\n" +
		"  skills:\n    - id: review-change\n      path: skills/review-change\n" +
		"  hooks:\n    - id: session-start\n      event: session-start\n      path: hooks/session-start.sh\n"
	files := []struct {
		name    string
		content string
		mode    int64
	}{
		{"agent-plugin.yaml", manifest, 0o644},
		{"rules/always-rule.md", "---\nalwaysApply: true\n---\n" + reviewedRuleBody, 0o644},
		{"skills/review-change/SKILL.md", skillBody, 0o644},
		{"hooks/session-start.sh", "#!/bin/sh\necho start\n", 0o755},
	}
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, file := range files {
		data := []byte(file.content)
		header := &tar.Header{Name: "example-alpha-commit/" + file.name, Mode: file.mode, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func reviewedApplication(t *testing.T) *Application {
	t.Helper()
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40),
		archive: reviewedArchive(t, reviewedSkillBody),
	}
	return &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
}

func reviewedArgs(project string, extra ...string) []string {
	return reviewedArgsAt(project, "latest", extra...)
}

func reviewedArgsAt(project, requested string, extra ...string) []string {
	args := []string{"migrate", "tessl", "--json", "--project", project, "--map", "example/alpha=github:example/alpha@" + requested}
	return append(args, extra...)
}

type acceptanceEnvelope struct {
	OK     bool                     `json:"ok"`
	Result *migrate.MigrationReport `json:"result"`
	Error  struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Remedy  string `json:"remedy"`
	} `json:"error"`
}

func decodeAcceptanceEnvelope(t *testing.T, payload string) acceptanceEnvelope {
	t.Helper()
	if strings.Count(payload, "\n") != 1 || !strings.HasSuffix(payload, "\n") {
		t.Fatalf("envelope must be one JSON line, got %q", payload)
	}
	var envelope acceptanceEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatalf("envelope does not decode: %v (%q)", err, payload)
	}
	if envelope.Result == nil {
		t.Fatalf("envelope carries no migration report: %q", payload)
	}
	return envelope
}

// reviewedCoexistence applies coexistence so finalization has current ACR
// state to work against, and returns the application both phases share.
func reviewedCoexistence(t *testing.T, project string) *Application {
	t.Helper()
	application := reviewedApplication(t)
	if _, stderr, exitCode := runCLI(t, application, reviewedArgs(project)...); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("coexistence exit = %d, stderr = %q", exitCode, stderr)
	}
	return application
}

// reviewedPreview runs the blocked finalization preview and returns its report.
func reviewedPreview(t *testing.T, application *Application, project string) migrate.MigrationReport {
	t.Helper()
	stdout, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--dry-run")...)
	if exitCode != cli.ExitConflict || stdout != "" {
		t.Fatalf("preview exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	envelope := decodeAcceptanceEnvelope(t, stderr)
	if envelope.Error.Code != "finalization_blocked" {
		t.Fatalf("preview error = %+v, want finalization_blocked", envelope.Error)
	}
	return *envelope.Result
}

// projectTree is hashTree without the transaction scaffolding ACR creates and
// reuses on every mutating run. A lock directory is not a project change.
func projectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := hashTree(t, root)
	for path := range tree {
		if path == ".agents/.acr-transactions" || strings.HasPrefix(path, ".agents/.acr-transactions/") {
			delete(tree, path)
		}
	}
	return tree
}

// treeDelta names every path one run added, removed, or rewrote. A failure
// that prints two whole trees is unreadable; this prints the difference.
func treeDelta(before, after map[string]string) []string {
	var delta []string
	for path, digest := range before {
		switch next, present := after[path]; {
		case !present:
			delta = append(delta, "removed "+path)
		case next != digest:
			delta = append(delta, "changed "+path)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			delta = append(delta, "added "+path)
		}
	}
	sort.Strings(delta)
	return delta
}

func changeKeys(changes []migrate.AcceptedChange) []string {
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		keys = append(keys, change.Package+"/"+change.Kind+"/"+change.ID+"/"+change.Reason)
	}
	return keys
}

func blockerCodeSet(report migrate.MigrationReport) map[string]bool {
	codes := make(map[string]bool, len(report.Blockers))
	for _, blocker := range report.Blockers {
		codes[blocker.Code] = true
	}
	return codes
}

// TestReviewedPreviewIssuesABoundTokenAndStillRefuses is the default-refusal
// contract: the preview names the reviewable differences and the evidence
// they are bound to, and finalization still refuses without acceptance.
func TestReviewedPreviewIssuesABoundTokenAndStillRefuses(t *testing.T) {
	project := reviewedConsumer(t)
	application := reviewedCoexistence(t, project)
	before := projectTree(t, project)

	report := reviewedPreview(t, application, project)

	if report.FinalizationReady {
		t.Fatal("the preview reported readiness without acceptance")
	}
	codes := blockerCodeSet(report)
	if !codes["effective-diff"] || !codes["lossy-artifact"] {
		t.Fatalf("blockers = %+v, want the reviewed differences to keep blocking", report.Blockers)
	}
	if len(report.AcceptedChanges) != 0 {
		t.Fatalf("acceptedChanges = %+v, want none without a token", report.AcceptedChanges)
	}
	if report.Acceptance == nil {
		t.Fatal("the preview issued no acceptance evidence")
	}
	if !strings.HasPrefix(report.Acceptance.Token, migrate.AcceptanceTokenPrefix) {
		t.Fatalf("token = %q", report.Acceptance.Token)
	}
	want := []string{"example/alpha/rule/always-rule/lossy", "example/alpha/skill/review-change/body"}
	if got := changeKeys(report.Acceptance.Changes); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reviewable changes = %q, want %q", got, want)
	}
	if len(report.Acceptance.Bindings) != 1 {
		t.Fatalf("bindings = %+v, want one per mapped package", report.Acceptance.Bindings)
	}
	binding := report.Acceptance.Bindings[0]
	if binding.From != "example/alpha" || binding.Source != "github:example/alpha" || binding.Requested != "latest" {
		t.Fatalf("binding names the wrong packages: %+v", binding)
	}
	if binding.TesslDigest == "" || binding.Commit != strings.Repeat("a", 40) || binding.ContentHash == "" {
		t.Fatalf("binding = %+v, want the old package and the resolved replacement", binding)
	}
	if after := projectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("the preview wrote to the project; delta: %v", treeDelta(before, after))
	}
	// Without acceptance the lossy rule's Tessl native is retained, so the
	// accepted run's removal of the same path is the behaviour change.
	retained := false
	for _, record := range report.Retained {
		if record.Path == ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc" {
			retained = true
		}
	}
	if !retained {
		t.Fatalf("retained = %+v, want the lossy rule's native held back", report.Retained)
	}
}

// TestNonInteractiveIsNotAcceptance pins the flag that must never stand in for
// a reviewed decision.
func TestNonInteractiveIsNotAcceptance(t *testing.T) {
	project := reviewedConsumer(t)
	application := reviewedCoexistence(t, project)
	before := projectTree(t, project)

	_, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--non-interactive")...)
	if exitCode != cli.ExitConflict {
		t.Fatalf("exit = %d, want a refusal; stderr = %q", exitCode, stderr)
	}
	envelope := decodeAcceptanceEnvelope(t, stderr)
	if !blockerCodeSet(*envelope.Result)["effective-diff"] {
		t.Fatalf("blockers = %+v, want the reviewed differences to keep blocking", envelope.Result.Blockers)
	}
	if after := projectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("--non-interactive finalized without acceptance; delta: %v", treeDelta(before, after))
	}
}

// TestAcceptedReviewedChangesFinalizeAndStayReported is the accepted cutover:
// the exact reviewed differences finalize, only Tessl output is removed, and
// the differences are still reported as differences.
func TestAcceptedReviewedChangesFinalizeAndStayReported(t *testing.T) {
	project := reviewedConsumer(t)
	application := reviewedCoexistence(t, project)
	preview := reviewedPreview(t, application, project)
	before := projectTree(t, project)

	stdout, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--accept-reviewed-changes", preview.Acceptance.Token)...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("accepted finalize exit = %d, stderr = %q", exitCode, stderr)
	}
	envelope := decodeAcceptanceEnvelope(t, stdout)
	report := *envelope.Result
	if !envelope.OK || report.Mode != "finalized" || !report.FinalizationReady || len(report.Blockers) != 0 {
		t.Fatalf("report = mode %q ready %t blockers %+v", report.Mode, report.FinalizationReady, report.Blockers)
	}
	want := []string{"example/alpha/rule/always-rule/lossy", "example/alpha/skill/review-change/body"}
	if got := changeKeys(report.AcceptedChanges); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("acceptedChanges = %q, want %q", got, want)
	}
	if len(report.EffectiveDiffs) != 2 {
		t.Fatalf("effectiveDiffs = %+v, want the accepted differences to stay reported", report.EffectiveDiffs)
	}

	after := projectTree(t, project)
	for path := range before {
		if _, survived := after[path]; survived {
			continue
		}
		if path != "tessl.json" && path != ".tessl" && !strings.HasPrefix(path, ".tessl/") && !strings.Contains(path, "tessl__") {
			t.Errorf("finalization removed a path Tessl does not own: %s", path)
		}
	}
	for _, removed := range []string{
		"tessl.json",
		".cursor/rules/tessl__rule__example__alpha__always-rule.mdc",
		".claude/skills/tessl__review-change",
	} {
		if _, survived := after[removed]; survived {
			t.Errorf("%s survived an accepted finalization", removed)
		}
	}
	removedNative := false
	for _, record := range report.Removed {
		if record.Path == ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc" {
			removedNative = true
			if record.Replacement == "" {
				t.Fatalf("the accepted rule's removal names no ACR replacement: %+v", record)
			}
		}
	}
	if !removedNative {
		t.Fatalf("removed = %+v, want the accepted rule's native retired", report.Removed)
	}
	if agents := readProjectFile(t, project, "AGENTS.md"); !strings.Contains(agents, "# User") {
		t.Fatalf("finalization dropped the operator's own AGENTS.md prefix: %q", agents)
	}
}

// TestAcceptedDryRunWritesNothing proves acceptance does not turn a preview
// into an apply.
func TestAcceptedDryRunWritesNothing(t *testing.T) {
	project := reviewedConsumer(t)
	application := reviewedCoexistence(t, project)
	preview := reviewedPreview(t, application, project)
	before := projectTree(t, project)

	stdout, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--dry-run", "--accept-reviewed-changes", preview.Acceptance.Token)...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("accepted dry run exit = %d, stderr = %q", exitCode, stderr)
	}
	report := *decodeAcceptanceEnvelope(t, stdout).Result
	if !report.DryRun || report.Wrote || report.Mode != "finalize" || !report.FinalizationReady {
		t.Fatalf("report = dryRun %t wrote %t mode %q ready %t", report.DryRun, report.Wrote, report.Mode, report.FinalizationReady)
	}
	if len(report.Removed) == 0 {
		t.Fatal("the accepted dry run planned no removal")
	}
	if after := projectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("the accepted dry run wrote to the project; delta: %v", treeDelta(before, after))
	}
}

// TestStaleAcceptanceIsRefused walks every input the token binds: the
// installed package, the requested ref, and the differences themselves.
func TestStaleAcceptanceIsRefused(t *testing.T) {
	for name, test := range map[string]struct {
		mutate    func(*testing.T, string)
		requested string
		token     string
	}{
		"old source rewritten": {mutate: func(t *testing.T, project string) {
			writeReviewedRule(t, project, reviewedDescription, "# Always, rewritten upstream\n")
		}},
		"difference removed": {mutate: func(t *testing.T, project string) {
			writeFile(t, project, ".tessl/plugins/example/alpha/skills/review-change/SKILL.md", []byte(reviewedSkillBody), 0o644)
		}},
		"token from elsewhere": {token: migrate.AcceptanceTokenPrefix + strings.Repeat("0", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			project := reviewedConsumer(t)
			application := reviewedCoexistence(t, project)
			token := reviewedPreview(t, application, project).Acceptance.Token
			if test.token != "" {
				token = test.token
			}
			if test.mutate != nil {
				test.mutate(t, project)
			}
			requested := "latest"
			if test.requested != "" {
				requested = test.requested
			}
			before := projectTree(t, project)

			args := reviewedArgsAt(project, requested, "--finalize", "--accept-reviewed-changes", token)
			_, stderr, exitCode := runCLI(t, application, args...)
			if exitCode != cli.ExitConflict {
				t.Fatalf("exit = %d, want a refusal; stderr = %q", exitCode, stderr)
			}
			envelope := decodeAcceptanceEnvelope(t, stderr)
			if !blockerCodeSet(*envelope.Result)["acceptance-stale"] {
				t.Fatalf("blockers = %+v, want acceptance-stale", envelope.Result.Blockers)
			}
			if len(envelope.Result.AcceptedChanges) != 0 {
				t.Fatalf("acceptedChanges = %+v, want none for stale evidence", envelope.Result.AcceptedChanges)
			}
			if after := projectTree(t, project); !mapsEqual(before, after) {
				t.Fatalf("a stale acceptance changed the project; delta: %v", treeDelta(before, after))
			}
		})
	}
}

// TestAcceptanceIsBoundToTheResolvedReplacement proves the requested ref is
// part of the evidence: two projects whose packages and differences are
// identical, mapped at different refs, do not share an acceptance.
//
// Changing the ref inside one converged project is refused earlier, by
// project_state_conflict, so the binding is proven across two projects.
func TestAcceptanceIsBoundToTheResolvedReplacement(t *testing.T) {
	tokens := map[string]string{}
	projects := map[string]string{}
	applications := map[string]*Application{}
	for _, requested := range []string{"latest", "v1.0.0"} {
		project := reviewedConsumer(t)
		application := reviewedApplication(t)
		if _, stderr, exitCode := runCLI(t, application, reviewedArgsAt(project, requested)...); exitCode != cli.ExitSuccess || stderr != "" {
			t.Fatalf("coexistence at %s exit = %d, stderr = %q", requested, exitCode, stderr)
		}
		_, stderr, exitCode := runCLI(t, application, reviewedArgsAt(project, requested, "--finalize", "--dry-run")...)
		if exitCode != cli.ExitConflict {
			t.Fatalf("preview at %s exit = %d, stderr = %q", requested, exitCode, stderr)
		}
		report := *decodeAcceptanceEnvelope(t, stderr).Result
		if report.Acceptance == nil {
			t.Fatalf("preview at %s issued no acceptance evidence", requested)
		}
		if report.Acceptance.Bindings[0].Requested != requested {
			t.Fatalf("binding requested = %q, want %q", report.Acceptance.Bindings[0].Requested, requested)
		}
		tokens[requested] = report.Acceptance.Token
		projects[requested] = project
		applications[requested] = application
	}
	if tokens["latest"] == tokens["v1.0.0"] {
		t.Fatalf("both refs issued the same token %q", tokens["latest"])
	}

	project := projects["v1.0.0"]
	before := projectTree(t, project)
	_, stderr, exitCode := runCLI(t, applications["v1.0.0"], reviewedArgsAt(project, "v1.0.0", "--finalize", "--accept-reviewed-changes", tokens["latest"])...)
	if exitCode != cli.ExitConflict {
		t.Fatalf("exit = %d, want a refusal; stderr = %q", exitCode, stderr)
	}
	if !blockerCodeSet(*decodeAcceptanceEnvelope(t, stderr).Result)["acceptance-stale"] {
		t.Fatalf("stderr = %q, want acceptance-stale", stderr)
	}
	if after := projectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("a foreign token changed the project; delta: %v", treeDelta(before, after))
	}
}

// TestAcceptanceOnAnEquivalentProjectIsRefused proves a token cannot be
// supplied to a project that has nothing to accept.
func TestAcceptanceOnAnEquivalentProjectIsRefused(t *testing.T) {
	project := seedConsumer(t)
	github := &integrationGitHub{
		release: dependency.Release{ID: 42, Tag: "v1.0.0"}, commit: strings.Repeat("a", 40),
		archive: migrationPackageArchive(t),
	}
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	if _, stderr, exitCode := runCLI(t, application, reviewedArgs(project)...); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("coexistence exit = %d, stderr = %q", exitCode, stderr)
	}
	before := projectTree(t, project)

	token := migrate.AcceptanceTokenPrefix + strings.Repeat("0", 64)
	_, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--accept-reviewed-changes", token)...)
	if exitCode != cli.ExitConflict {
		t.Fatalf("exit = %d, want a refusal; stderr = %q", exitCode, stderr)
	}
	envelope := decodeAcceptanceEnvelope(t, stderr)
	if !blockerCodeSet(*envelope.Result)["acceptance-stale"] {
		t.Fatalf("blockers = %+v, want acceptance-stale", envelope.Result.Blockers)
	}
	if envelope.Result.Acceptance != nil {
		t.Fatalf("acceptance = %+v, want none for an equivalent project", envelope.Result.Acceptance)
	}
	if after := projectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("a refused acceptance changed the project; delta: %v", treeDelta(before, after))
	}
}

// TestAcceptanceNeverClearsAnUnrelatedGate is the containment contract: a
// valid acceptance answers the reviewed differences and nothing else.
func TestAcceptanceNeverClearsAnUnrelatedGate(t *testing.T) {
	for name, test := range map[string]struct {
		seed func(*testing.T, string)
		code string
	}{
		"uncovered agent": {code: "uncovered-agent", seed: func(t *testing.T, project string) {
			writeFile(t, project, ".gemini/settings.json", []byte(`{"contextFileName":"GEMINI.md"}`+"\n"), 0o644)
		}},
		"orphan shared link": {code: "shared-skill-orphan", seed: func(t *testing.T, project string) {
			linkSharedSkill(t, project, "tessl__ghost", "../../.tessl/plugins/example/alpha/skills/ghost")
		}},
		"ambiguous host span": {code: "ambiguous-path", seed: func(t *testing.T, project string) {
			writeFile(t, project, "AGENTS.md",
				[]byte("# User\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n\nhand-written inside the span\n"), 0o644)
		}},
		"untracked manifest": {code: "untracked-finalization-state", seed: func(t *testing.T, project string) {
			command := exec.Command("git", fixtureGitArguments([]string{"init", "-q"})...)
			command.Dir = project
			command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			project := reviewedConsumer(t)
			// The gate is seeded before coexistence: a project ACR has not
			// converged on refuses for pending state, which would prove
			// nothing about the gate under test.
			test.seed(t, project)
			application := reviewedCoexistence(t, project)
			before := projectTree(t, project)

			report := reviewedPreview(t, application, project)
			if report.Acceptance == nil {
				t.Fatal("the preview issued no acceptance evidence")
			}

			_, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--accept-reviewed-changes", report.Acceptance.Token)...)
			if exitCode != cli.ExitConflict {
				t.Fatalf("exit = %d, want a refusal; stderr = %q", exitCode, stderr)
			}
			envelope := decodeAcceptanceEnvelope(t, stderr)
			codes := blockerCodeSet(*envelope.Result)
			if !codes[test.code] {
				t.Fatalf("blockers = %+v, want %s to survive acceptance", envelope.Result.Blockers, test.code)
			}
			if codes["effective-diff"] || codes["lossy-artifact"] {
				t.Fatalf("blockers = %+v, want the accepted differences cleared", envelope.Result.Blockers)
			}
			if after := projectTree(t, project); !mapsEqual(before, after) {
				t.Fatalf("a blocked run changed the project; delta: %v", treeDelta(before, after))
			}
		})
	}
}

// TestAcceptedFinalizationRollsBackOnFailure proves acceptance does not weaken
// the transaction: a failure restores every file and reports no removal.
func TestAcceptedFinalizationRollsBackOnFailure(t *testing.T) {
	project := reviewedConsumer(t)
	application := reviewedCoexistence(t, project)
	token := reviewedPreview(t, application, project).Acceptance.Token
	before := projectTree(t, project)

	injected := errors.New("injected finalization failure")
	original := applyFinalizationFileTransaction
	applyFinalizationFileTransaction = func(projectDirectory string, edits []realize.FileTransactionEdit, finalize func() error) error {
		return realize.ApplyFileTransactionWithHooks(projectDirectory, edits, finalize, realize.FileTransactionHooks{
			AfterEdit: func(index int, _ realize.FileTransactionEdit) error {
				if index == 0 {
					return injected
				}
				return nil
			},
		})
	}
	defer func() { applyFinalizationFileTransaction = original }()

	_, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--accept-reviewed-changes", token)...)
	if exitCode != cli.ExitConflict && exitCode != cli.ExitOperational {
		t.Fatalf("exit = %d, want a failure; stderr = %q", exitCode, stderr)
	}
	envelope := decodeAcceptanceEnvelope(t, stderr)
	if len(envelope.Result.Removed) != 0 || len(envelope.Result.Reanchored) != 0 {
		t.Fatalf("a rolled-back run reported results: removed=%+v reanchored=%+v", envelope.Result.Removed, envelope.Result.Reanchored)
	}
	if after := projectTree(t, project); !mapsEqual(before, after) {
		t.Fatalf("the rollback did not restore the project; delta: %v", treeDelta(before, after))
	}
}

// TestAcceptanceTextOutputNamesTheDifferences keeps the human-readable
// contract in step: accepted changes are printed as accepted differences and
// still appear among the effective differences.
func TestAcceptanceTextOutputNamesTheDifferences(t *testing.T) {
	project := reviewedConsumer(t)
	application := reviewedCoexistence(t, project)
	token := reviewedPreview(t, application, project).Acceptance.Token

	stdout, stderr, exitCode := runCLI(t, application, "migrate", "tessl", "--project", project,
		"--map", "example/alpha=github:example/alpha@latest", "--finalize", "--dry-run", "--accept-reviewed-changes", token)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("text run exit = %d, stderr = %q", exitCode, stderr)
	}
	for _, clause := range []string{
		"Accepted reviewed changes",
		"example/alpha rule always-rule  lossy",
		"detail: description",
		"example/alpha skill review-change  body",
		"Effective differences",
	} {
		if !strings.Contains(stdout, clause) {
			t.Errorf("text output does not state %q:\n%s", clause, stdout)
		}
	}
	if strings.Contains(stdout, "Effective differences\n  (none)") {
		t.Fatalf("accepted differences were reported as equivalence:\n%s", stdout)
	}
}

// TestAcceptanceFlagUsage pins the argument contract: acceptance is only ever
// an input to finalization, and never a repeatable or plugin-side flag.
func TestAcceptanceFlagUsage(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	token := migrate.AcceptanceTokenPrefix + strings.Repeat("0", 64)
	for name, test := range map[string]struct {
		args    []string
		message string
	}{
		"without finalize": {
			args:    []string{"migrate", "tessl", "--project", project, "--accept-reviewed-changes", token},
			message: "--accept-reviewed-changes requires --finalize",
		},
		"on the plugin converter": {
			args:    []string{"migrate", "tessl-plugin", "--project", project, "--accept-reviewed-changes", token},
			message: "only supported by acr migrate tessl",
		},
		"repeated with different values": {
			args: []string{"migrate", "tessl", "--project", project, "--finalize",
				"--accept-reviewed-changes", token, "--accept-reviewed-changes", migrate.AcceptanceTokenPrefix + strings.Repeat("1", 64)},
			message: "--accept-reviewed-changes may be specified only once",
		},
		"without a value": {
			args:    []string{"migrate", "tessl", "--project", project, "--finalize", "--accept-reviewed-changes"},
			message: "--accept-reviewed-changes requires a value",
		},
		"on an unrelated command": {
			args:    []string{"realize", "--project", project, "--accept-reviewed-changes", token},
			message: "--accept-reviewed-changes is not supported by realize",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), test.args...)
			if exitCode != cli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr = %q", exitCode, cli.ExitUsage, stderr)
			}
			if !strings.Contains(stderr, test.message) {
				t.Fatalf("stderr = %q, want %q", stderr, test.message)
			}
		})
	}
}

// TestMissingReplacementIsNeverAcceptable proves the one difference class the
// mechanism refuses to bind: an artifact the replacement does not carry.
func TestMissingReplacementIsNeverAcceptable(t *testing.T) {
	project := reviewedConsumer(t)
	writeFile(t, project, ".tessl/plugins/example/alpha/skills/retired/SKILL.md", []byte("# Retired\n"), 0o644)
	plugin := readProjectFile(t, project, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json")
	writeFile(t, project, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json",
		[]byte(strings.Replace(plugin, `"skills/review-change"`, `"skills/review-change","skills/retired"`, 1)), 0o644)

	application := reviewedCoexistence(t, project)
	report := reviewedPreview(t, application, project)

	for _, change := range report.Acceptance.Changes {
		if change.ID == "retired" {
			t.Fatalf("a missing replacement was offered as acceptable: %+v", change)
		}
	}
	_, stderr, exitCode := runCLI(t, application, reviewedArgs(project, "--finalize", "--accept-reviewed-changes", report.Acceptance.Token)...)
	if exitCode != cli.ExitConflict {
		t.Fatalf("exit = %d, want a refusal; stderr = %q", exitCode, stderr)
	}
	envelope := decodeAcceptanceEnvelope(t, stderr)
	missing := false
	for _, blocker := range envelope.Result.Blockers {
		if blocker.Code == "effective-diff" && strings.HasSuffix(blocker.ID, "/retired") && blocker.Detail == migrate.DiffMissingInACR {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("blockers = %+v, want the missing replacement to keep blocking", envelope.Result.Blockers)
	}
}
