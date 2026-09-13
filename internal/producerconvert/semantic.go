package producerconvert

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"go.yaml.in/yaml/v3"
)

//go:embed semantic-prompt.txt
var semanticPrompt string

//go:embed check-tests.py
var pythonTestChecks string

type providerCall func(context.Context, string, string) (proposal, AgentRun, error)

func prepareAssisted(ctx context.Context, options Options) (Plan, error) {
	return prepareWithProvider(ctx, options, runProvider)
}

func prepareWithProvider(ctx context.Context, options Options, provider providerCall) (plan Plan, err error) {
	plan, err = prepareDeterministic(options)
	if err == nil {
		return plan, nil
	}
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || plan.Report.Package == "" {
		return plan, err
	}
	for _, block := range plan.Report.Blockers {
		if strings.HasPrefix(block.Path, ".github/") && !supportedDeliveryFile(plan.before, block.Path) {
			return plan, refuse("unsupported_semantic_conversion", block.Path, "unsupported delivery format contains Tessl operations; retain this read-only policy file until its format has a supported conversion")
		}
		if strings.Contains(block.Reason, "competes") || strings.Contains(block.Reason, "ambiguous") {
			return plan, err
		}
	}
	if options.Agent != "claude" && options.Agent != "codex" {
		return plan, refuse("agent_unavailable", "--agent", "select --agent codex or --agent claude explicitly")
	}
	input := semanticInput{Selected: plan.options.PackageRoot, Package: plan.Report.Package, Version: plan.Report.Version, Blockers: plan.Report.Blockers}
	for _, name := range sortedPaths(plan.before) {
		state := plan.before[name]
		if state.Directory || state.Link != "" || state.Digest == "" {
			continue
		}
		if !utf8.Valid(state.Content) {
			continue
		}
		input.Files = append(input.Files, inputFile{name, state.Digest, state.Mode, editable(plan, name), string(state.Content)})
	}
	requests, e := scopeInputs(input)
	if e != nil {
		return plan, e
	}
	if len(requests) == 0 {
		return plan, refuse("unsupported_semantic_conversion", plan.options.PackageRoot, "no owned editable semantic input; correct unsupported references in the source before retrying")
	}

	plan.Report.Notes = append(plan.Report.Notes, "Semantic proposals use the explicitly selected CLI and configured account. ACR validates proposals before writing. ACR-only removes Tessl-only paid scoring; ACR validation is not a replacement score.")
	if len(requests) > 1 {
		plan.Report.Notes = append(plan.Report.Notes, "Large input is split into runtime, instruction and delivery proposals; their combined result is validated and committed in one transaction.")
	}
	original := plan
	previous := proposal{}
	feedback := ""
	cached := make([]proposal, len(requests))
	proposalRuns := make([]int, len(requests))
	retryFrom := 0
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		combined := proposal{}
		for index, input := range requests {
			if index < retryFrom {
				combined.Edits = append(combined.Edits, cached[index].Edits...)
				combined.PolicyChanges = append(combined.PolicyChanges, cached[index].PolicyChanges...)
				continue
			}
			request, e := proposalRequest(input, previous, combined, feedback)
			if e != nil {
				return plan, e
			}
			proposed, run, callErr := provider(ctx, options.Agent, request)
			run.Scope = input.Scope
			plan.Report.AgentRuns = append(plan.Report.AgentRuns, run)
			if callErr != nil {
				return plan, refuse("agent_failed", "--agent", callErr.Error())
			}
			if input.Scope != "" {
				for _, edit := range proposed.Edits {
					if semanticScope(edit.Path) != input.Scope {
						return plan, refuse("invalid_agent_proposal", edit.Path, "proposal edited outside its assigned "+input.Scope+" scope")
					}
				}
			}
			proposalRuns[index] = len(plan.Report.AgentRuns)
			cached[index] = proposed
			combined.Edits = append(combined.Edits, proposed.Edits...)
			combined.PolicyChanges = append(combined.PolicyChanges, proposed.PolicyChanges...)
		}
		next, validationErr := validateProposal(ctx, original, combined)
		attemptNote := fmt.Sprintf("ACR combined validation attempt %d (proposal runs %v)", attempt+1, proposalRuns)
		if validationErr == nil {
			plan.Report.Notes = append(plan.Report.Notes, attemptNote+": passed.")
			next.Report.AgentRuns = plan.Report.AgentRuns
			next.Report.Notes = append(next.Report.Notes, plan.Report.Notes...)
			return next, nil
		}
		attemptNote += ": " + validationErr.Error()
		plan.Report.Notes = append(plan.Report.Notes, attemptNote)
		if errors.As(validationErr, &refusal) && refusal.Code == "unsupported_file_mode" {
			return plan, validationErr
		}
		var attributable bool
		retryFrom, attributable = firstAffectedScope(requests, combined, validationErr)
		if attributable {
			run := &plan.Report.AgentRuns[proposalRuns[retryFrom]-1]
			if run.Failure != "" {
				run.Failure += "\n"
			}
			run.Failure += attemptNote
		}
		err = refuse("invalid_agent_proposal", "--agent", validationErr.Error())
		previous, feedback = combined, validationErr.Error()
	}
	return plan, err
}

func editable(p Plan, name string) bool {
	state, exists := p.before[name]
	if !exists || state.Directory || state.Link != "" || distributionNotice(name) || consumerFile(name) || excluded(name) {
		return false
	}
	if path.Base(name) == "tile.json" || strings.Contains(name, "/.tessl-plugin/") || strings.HasPrefix(name, ".tessl-plugin/") || path.Base(name) == ".tesslignore" || path.Base(name) == ".tileignore" || path.Base(name) == manifest.Filename || path.Base(name) == ".acr-package.json" {
		return false
	}
	if !within(p.options.PackageRoot, name) && !strings.HasPrefix(name, ".github/") && !strings.HasPrefix(name, "tests/") {
		return false
	}
	if strings.HasPrefix(name, ".github/") {
		return supportedDeliveryFile(p.before, name) && workflowSemantic(state.Content)
	}
	content := publicRepositoryURLs.ReplaceAll(state.Content, nil)
	reason := runtimeSemanticOperation(content)
	if state.Mode&0o111 == 0 && semanticScope(name) == "instructions" {
		reason = instructionSemanticOperation(content)
	}
	return reason != "" || bytes.Contains(content, []byte(".tessl/plugins/"+p.Report.SourcePackage+"/"))
}

func validateProposal(ctx context.Context, p Plan, proposed proposal) (result Plan, err error) {
	if len(proposed.Edits) == 0 || len(proposed.Edits) > 256 {
		return result, fmt.Errorf("proposal must contain 1..256 edits")
	}
	next := tree{}
	for name, state := range p.before {
		next[name] = state
	}
	seen := map[string]bool{}
	var problems []error
edits:
	for _, edit := range proposed.Edits {
		name := edit.Path
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") || excluded(name) || semanticConsumerPath(name) || seen[name] {
			return result, fmt.Errorf("unexpected or duplicate path %q", name)
		}
		seen[name] = true
		before, exists := p.before[name]
		if exists && (!editable(p, name) || edit.BeforeDigest != before.Digest) {
			return result, fmt.Errorf("%s: protected path or stale beforeDigest", name)
		}
		mode := before.Mode
		if !exists {
			if edit.Action != "create" || edit.BeforeDigest != "" || !p.before[path.Dir(name)].Directory || path.Base(name) == ".acr-package.json" || consumerFile(name) || distributionNotice(name) {
				return result, fmt.Errorf("%s: new file requires an existing skill/test parent and empty beforeDigest", name)
			}
			allowed := strings.HasPrefix(name, "tests/")
			for _, artifact := range p.Report.Artifacts {
				if artifact.Kind == "skill" && within(artifact.Path, name) {
					allowed = true
				}
			}
			// Artifacts are populated only on successful deterministic plans; use the
			// discovered skill directory entries for refused plans as well.
			for parent, state := range p.before {
				if path.Base(parent) == "SKILL.md" && !state.Directory && within(path.Dir(parent), name) {
					allowed = true
				}
			}
			if !allowed {
				return result, fmt.Errorf("%s: new files must belong to an existing skill tree or tests", name)
			}
			mode = 0o644
		}
		var body []byte
		switch edit.Action {
		case "replace", "create":
			if len(edit.Replacements) != 0 {
				return result, fmt.Errorf("%s: content edits cannot contain replacements", name)
			}
			body = []byte(edit.Content)
		case "patch":
			if edit.Content != "" || len(edit.Replacements) == 0 {
				return result, fmt.Errorf("%s: patch requires only replacements", name)
			}
			body = append([]byte(nil), before.Content...)
			for _, r := range edit.Replacements {
				if r.Old == "" || r.Count <= 0 || bytes.Count(body, []byte(r.Old)) != r.Count {
					problems = append(problems, fmt.Errorf("%s: replacement match count differs for %q", name, r.Old))
					continue edits
				}
				body = bytes.ReplaceAll(body, []byte(r.Old), []byte(r.New))
			}
		case "remove":
			if edit.Content != "" || len(edit.Replacements) != 0 || !workflowFile(name) {
				return result, fmt.Errorf("%s: only proven service-only workflows can be removed", name)
			}
			if err := preserveChecksWithSource(name, before.Content, nil, p.before); err != nil {
				problems = append(problems, err)
			}
			delete(next, name)
			continue
		default:
			return result, fmt.Errorf("%s: unsupported edit action %q", name, edit.Action)
		}
		if bytes.Contains(body, []byte("package-file:")) {
			problems = append(problems, fmt.Errorf("%s: package-file: is not an ACR reference scheme; use supported repository-relative skill-file paths", name))
		}
		if len(body) == 0 || len(body) > maxProposalBytes || !utf8.Valid(body) {
			return result, fmt.Errorf("%s: empty, oversized or non-text output", name)
		}
		if name == ".github/aw/actions-lock.json" {
			if err := preserveActionsLock(before.Content, body); err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", name, err))
			}
		}
		if !workflowFile(name) {
			nextURLs, oldURLs := repositoryURLCounts(body), repositoryURLCounts(before.Content)
			checkedURLs := map[string]bool{}
			for _, token := range publicRepositoryURLs.FindAllString(string(before.Content), -1) {
				if checkedURLs[token] {
					continue
				}
				checkedURLs[token] = true
				if nextURLs[token] != oldURLs[token] {
					problems = append(problems, fmt.Errorf("%s: historical public repository URL %s and its multiplicity must survive", name, token))
				}
			}
		}
		for _, foreign := range foreignInstalledRoots.FindAll(before.Content, -1) {
			if string(foreign) == ".tessl/plugins/"+p.Report.SourcePackage+"/" {
				continue
			}
			if bytes.Count(body, foreign) < bytes.Count(before.Content, foreign) {
				problems = append(problems, fmt.Errorf("%s: foreign installed reference %s must survive", name, foreign))
			}
		}
		if err := preserveChecksWithSource(name, before.Content, body, p.before); err != nil {
			problems = append(problems, err)
		}
		if exists && strings.HasPrefix(name, "tests/") && (path.Ext(name) == ".sh" || path.Ext(name) == ".go") {
			adapted := bytes.ReplaceAll(before.Content, []byte(".tessl/plugins/"+p.Report.SourcePackage+"/"), []byte(strings.TrimPrefix(p.options.PackageRoot+"/", "./")))
			if !bytes.Equal(testExecutableBody(adapted, path.Ext(name)), testExecutableBody(body, path.Ext(name))) {
				problems = append(problems, fmt.Errorf("%s: unsupported test edit; retain independent executable checks and registration, adapting only source references or leading comments", name))
			}
		}
		if err := syntaxCheck(ctx, name, body); err != nil {
			problems = append(problems, err)
		}
		if strings.HasPrefix(name, "tests/") && strings.HasSuffix(name, ".py") && exists {
			data, e := json.Marshal(map[string]string{"before": string(before.Content), "after": string(body)})
			if e != nil {
				return result, e
			}
			command := exec.CommandContext(ctx, "python3", "-I", "-S", "-c", pythonTestChecks)
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
			command.Stdin = bytes.NewReader(data)
			if output, e := command.CombinedOutput(); e != nil {
				problems = append(problems, fmt.Errorf("%s: test preservation: %w: %s", name, e, output))
			}
		}
		next[name] = fileState{Content: body, Mode: mode, Digest: digest(body)}
	}
	for _, policy := range proposed.PolicyChanges {
		if !seen[policy.Path] || strings.TrimSpace(policy.From) == "" || strings.TrimSpace(policy.To) == "" {
			return result, fmt.Errorf("policy changes must explain a changed file with non-empty from/to")
		}
	}
	// Every retained regular input is copied into the private stage. Refuse
	// unsupported materialization before creating that stage or repairing a
	// proposal; source ACLs cannot accompany a fresh mode-000 inode.
	for _, name := range sortedPaths(next) {
		state := next[name]
		if !state.Directory && state.Link == "" && state.Mode == 0 {
			return result, unsupportedFileMode(name, "semantic validation staging")
		}
	}
	if len(problems) > 0 {
		return result, errors.Join(problems...)
	}
	directory, err := os.MkdirTemp("", "acr-validation-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, cleanupValidationStage(directory)) }()
	if err = os.Mkdir(filepath.Join(directory, ".git"), 0o700); err != nil {
		return result, err
	}
	stage, err := os.OpenRoot(directory)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stage.Close()) }()

	for _, name := range sortedPaths(next) {
		state := next[name]
		if state.Link != "" {
			return result, fmt.Errorf("%s: symlink inside semantic input is unsupported", name)
		}
		full := filepath.Join(directory, filepath.FromSlash(name))
		if state.Directory {
			if err = os.MkdirAll(full, fs.FileMode(state.Mode)|0o700); err != nil {
				return result, err
			}
			// Populate children before restoring exact source modes; the source
			// directory can be readable/traversable without being writable.
			if err = os.Chmod(full, fs.FileMode(state.Mode)|0o700); err != nil {
				return result, err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return result, err
		}
		if err = writeExclusive(stage, name, state.Content, fs.FileMode(state.Mode)); err != nil {
			return result, fmt.Errorf("staging path collision or write failure: %w", err)
		}
	}
	// Children precede parents so restoring traversal bits cannot prevent a
	// later child chmod. Candidate inventory sees the original exact modes.
	paths := sortedPaths(next)
	for i := len(paths) - 1; i >= 0; i-- {
		name := paths[i]
		if state := next[name]; state.Directory {
			if err = stage.Chmod(name, fs.FileMode(state.Mode)); err != nil {
				return result, err
			}
		}
	}
	options := p.options
	options.PackageRoot = filepath.Join(directory, filepath.FromSlash(p.options.PackageRoot))
	candidate, err := prepareDeterministic(options)
	if err != nil {
		encoded, e := json.Marshal(candidate.Report.Blockers)
		if e != nil {
			return result, e
		}
		return result, fmt.Errorf("candidate conversion: %w; blockers: %s", err, encoded)
	}
	if err := reconcileGHWorkflowMetadata(p.before, candidate.after); err != nil {
		return result, err
	}
	if err := validatePaidDeclarations(p.before, candidate.after, proposed.PolicyChanges, p.options.PackageRoot); err != nil {
		return result, err
	}
	result = p
	result.Report = candidate.Report
	result.Report.RepositoryRoot = p.root
	result.Report.DryRun = p.Report.DryRun
	result.Report.PolicyChanges = proposed.PolicyChanges
	result.after = candidate.after
	result.changes = nil
	for _, name := range sortedPaths(result.before) {
		before := result.before[name]
		after, exists := result.after[name]
		if before.Directory {
			continue
		}
		if !exists {
			result.remove(name)
		} else if before.Digest != after.Digest || before.Mode != after.Mode {
			result.change(name, after.Content, after.Mode)
		}
	}
	for _, name := range sortedPaths(result.after) {
		if _, exists := result.before[name]; !exists {
			state := result.after[name]
			if state.Directory {
				return result, fmt.Errorf("unexpected new directory %s", name)
			}
			result.change(name, state.Content, state.Mode)
		}
	}
	if err = result.sealReceipt(); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return result, err
	}
	err = errors.Join(verifyBefore(root, p.options.PackageRoot, p.before, true), root.Close())
	return result, err
}

// Only supported delivery formats confer edit authority. Unchanged historical
// documents remain ordinary read-only context, even beside an active workflow.
func supportedDeliveryFile(original tree, name string) bool {
	if workflowFile(name) {
		return true
	}
	if strings.HasPrefix(name, ".github/workflows/") && strings.HasSuffix(name, ".md") {
		return verifiedGHWorkflowPair(original, strings.TrimSuffix(name, ".md")+".lock.yml")
	}
	if name == ".github/aw/actions-lock.json" {
		_, _, err := actionsLock(original[name].Content)
		return err == nil
	}
	return false
}

func actionsLock(body []byte) (map[string]any, map[string]any, error) {
	var raw json.RawMessage
	if err := strictJSON(body, &raw); err != nil {
		return nil, nil, err
	}
	// Preserve exact numeric values in unknown retained metadata.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var top map[string]any
	if err := decoder.Decode(&top); err != nil {
		return nil, nil, err
	}
	entries, ok := top["entries"].(map[string]any)
	if top == nil || !ok || entries == nil {
		return nil, nil, fmt.Errorf("actions lock requires an object with an entries object")
	}
	for key, value := range entries {
		entry, ok := value.(map[string]any)
		if !ok || entry == nil {
			return nil, nil, fmt.Errorf("actions lock entry %q requires an object", key)
		}
	}
	return top, entries, nil
}

func preserveActionsLock(before, after []byte) error {
	old, oldEntries, err := actionsLock(before)
	if err != nil {
		return err
	}
	next, nextEntries, err := actionsLock(after)
	if err != nil {
		return err
	}
	delete(old, "entries")
	delete(next, "entries")
	if !reflect.DeepEqual(old, next) {
		return fmt.Errorf("actions lock top-level metadata must remain unchanged")
	}
	for key, value := range nextEntries {
		previous, exists := oldEntries[key]
		if !exists || !reflect.DeepEqual(previous, value) {
			return fmt.Errorf("actions lock retained entry %q and all its fields must remain unchanged; no additions", key)
		}
	}
	for key, value := range oldEntries {
		if _, retained := nextEntries[key]; retained {
			continue
		}
		entry := value.(map[string]any) // actionsLock checked every entry's shape.
		repo, repoOK := entry["repo"].(string)
		version, versionOK := entry["version"].(string)
		if !repoOK || !versionOK || !serviceAction(key) || key != repo+"@"+version {
			return fmt.Errorf("actions lock entry %q is not an exactly identified retired service action", key)
		}
	}
	return nil
}

// Only the private stage gains access for cleanup. WalkDir invokes the callback
// before reading a directory's children; original and applied trees are untouched.
func cleanupValidationStage(directory string) error {
	accessErr := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(name, 0o700)
		}
		return nil
	})
	if err := errors.Join(accessErr, os.RemoveAll(directory)); err != nil {
		return fmt.Errorf("clean up private validation stage %s: %w", directory, err)
	}
	return nil
}

// These declarations are a finite syntax, not a prose-equivalence classifier.
// Normalize ASCII hyphens, case and whitespace; docs/migration-producer.md lists
// the supported phrases. Keep paid-content detection separate and unchanged.
func declarationText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(text, "-", " "))), " ")
}

const paidGateIdentity = `(paid tessl|tessl paid) (skill review|changed skill review|threshold 85 skill review|score|score gate)`

var paidGateFrom = regexp.MustCompile(`^` + paidGateIdentity + `([ .;:]|$)`)
var paidGateRetirement = regexp.MustCompile(`^(retired|removed|retire (paid )?(tessl skill review|score( gate)?)|remove the scoring only workflow|visibly disclose removal of the paid score gate)([ .;:]|$)`)
var paidGateVisible = regexp.MustCompile(`\b` + paidGateIdentity + `( threshold 85 workflow| workflow| gate)? (was retired|is retired|has been removed|was removed)\b`)

func noEquivalentScore(text string) bool {
	return strings.Contains(text, "acr has no equivalent score") || strings.Contains(text, "without an equivalent score gate")
}

func validatePaidDeclarations(before, after tree, policies []PolicyChange, selected ...string) error {
	var problems []error
	for _, name := range sortedPaths(before) {
		// These protected source manifests are mapped into the root manifest.
		// Retiring their containers does not retire descriptive policy content.
		if len(selected) > 0 && (name == path.Join(selected[0], ".tessl-plugin/plugin.json") || name == path.Join(selected[0], "tile.json")) {
			continue
		}
		old := before[name]
		next, retained := after[name]
		if old.Directory || retained && old.Digest == next.Digest && old.Mode == next.Mode {
			continue
		}
		text := string(old.Content)
		if !strings.Contains(strings.ToLower(text), "skill-review") && !regexp.MustCompile(`(?i)(score|threshold)[^\n]*85`).MatchString(text) {
			continue
		}
		declared := false
		for _, policy := range policies {
			if policy.Path != name {
				continue
			}
			declared = true
			if !paidGateFrom.MatchString(declarationText(policy.From)) || !paidGateRetirement.MatchString(declarationText(policy.To)) {
				problems = append(problems, fmt.Errorf("%s: policyChanges must declare the paid Tessl review/score gate and its retirement using the documented declaration syntax", name))
			}
			// Deleted files and opaque JSON cannot contain a notice. Their concrete
			// record supplies the no-equivalent declaration in the report and receipt.
			if (!retained || name == ".github/aw/actions-lock.json") && !noEquivalentScore(declarationText(policy.To)) {
				problems = append(problems, fmt.Errorf("%s: policyChanges must declare no equivalent ACR score for deleted or opaque output", name))
			}
		}
		if !declared {
			problems = append(problems, fmt.Errorf("%s: Tessl paid scoring policy change requires an explicit policyChanges record; no ACR equivalent exists", name))
		}
		if retained && name != ".github/aw/actions-lock.json" {
			visible := declarationText(string(next.Content))
			if !paidGateVisible.MatchString(visible) || !noEquivalentScore(visible) {
				problems = append(problems, fmt.Errorf("%s: retained paid-gate text requires a visible retirement and no-equivalent-score declaration after all transformations", name))
			}
		}
	}
	return errors.Join(problems...)
}

var foreignInstalledRoots = regexp.MustCompile(`\.tessl/plugins/[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+/`)

var testNames = regexp.MustCompile(`(?m)^\s*(?:def|func)\s+(test_[A-Za-z0-9_]+|Test[A-Za-z0-9_]+)\s*\(`)

// Shell/Go test behavior has no general equivalence oracle. Accept the owned
// reference adaptation above and ordinary leading commentary only. Preserve
// interpreter/build directives and everything after the first executable line,
// including comments that could be heredoc or raw-string test data.
func testExecutableBody(body []byte, extension string) []byte {
	var result []byte
	comment := []byte("#")
	if extension == ".go" {
		comment = []byte("//")
	}
	for len(body) > 0 {
		line, rest, found := bytes.Cut(body, []byte("\n"))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) != 0 && !bytes.HasPrefix(trimmed, comment) {
			return append(result, body...)
		}
		if bytes.HasPrefix(trimmed, []byte("#!")) || bytes.HasPrefix(trimmed, []byte("//go:")) || bytes.HasPrefix(trimmed, []byte("// +build")) {
			result = append(result, line...)
			if found {
				result = append(result, '\n')
			}
		}
		body = rest
	}
	return result
}

func preserveChecks(name string, before, after []byte) error {
	return preserveChecksWithSource(name, before, after, nil)
}

func preserveChecksWithSource(name string, before, after []byte, original tree) error {
	if strings.HasPrefix(name, "tests/") {
		for _, match := range testNames.FindAllSubmatch(before, -1) {
			if !bytes.Contains(after, match[1]) {
				return fmt.Errorf("%s: original test %s must remain", name, match[1])
			}
		}
		if bytes.Count(after, []byte("assert")) < bytes.Count(before, []byte("assert")) {
			return fmt.Errorf("%s: preserve all independent assertions", name)
		}
		for _, weakening := range []string{"@unittest.skip", "pytest.skip(", "expectedFailure"} {
			if bytes.Count(after, []byte(weakening)) > bytes.Count(before, []byte(weakening)) {
				return fmt.Errorf("%s: added test bypass %s", name, weakening)
			}
		}
	}
	if workflowFile(name) {
		var old, new yaml.Node
		if err := yaml.Unmarshal(before, &old); err != nil {
			return err
		}
		if len(after) > 0 {
			if err := yaml.Unmarshal(after, &new); err != nil {
				return err
			}
		}
		if len(old.Content) > 0 {
			if len(after) > 0 {
				if len(new.Content) != 1 || new.Content[0].Kind != yaml.MappingNode {
					return fmt.Errorf("%s: workflow requires a mapping", name)
				}
				if err := preserveWorkflowFields(old.Content[0], new.Content[0], false, "jobs", "name"); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
			oldJobs := member(old.Content[0], "jobs")
			var newJobs *yaml.Node
			if len(new.Content) > 0 {
				newJobs = member(new.Content[0], "jobs")
			}
			if len(after) == 0 && (oldJobs == nil || oldJobs.Kind != yaml.MappingNode || len(oldJobs.Content) == 0) {
				return fmt.Errorf("%s: removal requires a proven service-only workflow", name)
			}
			if oldJobs != nil {
				for i := 0; i < len(oldJobs.Content); i += 2 {
					job := oldJobs.Content[i+1]
					nameOfJob := oldJobs.Content[i].Value
					nextJob := member(newJobs, nameOfJob)
					if err := preservePublisherStepConditions(job, nextJob); err != nil {
						return fmt.Errorf("%s job %s: %w", name, nameOfJob, err)
					}
					kind := deliveryJobKind(job)
					if kind != independentDeliveryJob && len(new.Content) > 0 && !sameYAML(job, nextJob) && jobReferences(new.Content[0], newJobs, nameOfJob) {
						return fmt.Errorf("%s: referenced service job %q must remain unchanged", name, nameOfJob)
					}
					if nextJob == nil {
						if removableDeliveryJob(job) {
							continue
						}
						if len(new.Content) == 0 {
							return fmt.Errorf("%s: retain independent review/test workflow and publication job %s policy", name, nameOfJob)
						}
						return fmt.Errorf("%s: independent job %s must remain with its policy", name, nameOfJob)
					}
					if kind == publisherDeliveryJob && member(nextJob, "uses") != nil {
						if err := preservePublisherRewrite(job, nextJob, new.Content[0]); err != nil {
							return fmt.Errorf("%s job %s: %w", name, nameOfJob, err)
						}
					} else if err := preserveWorkflowJob(job, nextJob, verifiedGHWorkflowPair(original, name)); err != nil {
						return fmt.Errorf("%s job %s: %w", name, nameOfJob, err)
					}
				}
			}
		}
	}
	return nil
}
func workflowFile(name string) bool {
	return strings.HasPrefix(name, ".github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml"))
}

// Recognize decoded operations and complete action identities, including JSON
// lock keys. Historical URLs/comments alone confer no editing authority.
func workflowSemantic(body []byte) bool {
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err == nil {
		var active func(*yaml.Node) bool
		active = func(node *yaml.Node) bool {
			if node.Kind == yaml.ScalarNode && (serviceAction(node.Value) || runtimeSemanticOperation(publicRepositoryURLs.ReplaceAll([]byte(node.Value), nil)) != "") {
				return true
			}
			for _, child := range node.Content {
				if active(child) {
					return true
				}
			}
			return false
		}
		return active(&document)
	}
	return runtimeSemanticOperation(publicRepositoryURLs.ReplaceAll(body, nil)) != ""
}

var publicRepositoryURLs = regexp.MustCompile("https?://(?:github\\.com|gitlab\\.com|bitbucket\\.org|raw\\.githubusercontent\\.com)/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+[^\\s\"'`<>()\\[\\]{}]*")

func repositoryURLCounts(body []byte) map[string]int {
	counts := map[string]int{}
	for _, token := range publicRepositoryURLs.FindAllString(string(body), -1) {
		counts[token]++
	}
	return counts
}

func syntaxCheck(ctx context.Context, name string, body []byte) error {
	switch path.Ext(name) {
	case ".json":
		var value any
		if err := strictJSON(body, &value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	case ".yaml", ".yml":
		var value yaml.Node
		decoder := yaml.NewDecoder(bytes.NewReader(body))
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("%s: invalid YAML: %w", name, err)
		}
		var extra yaml.Node
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: expected exactly one YAML document", name)
		}
		var decoded any
		if err := value.Decode(&decoded); err != nil {
			return fmt.Errorf("%s: invalid YAML: %w", name, err)
		}
		if strings.HasPrefix(name, ".github/workflows/") {
			if len(value.Content) != 1 || value.Content[0].Kind != yaml.MappingNode {
				return fmt.Errorf("%s: workflow requires a mapping", name)
			}
			top := value.Content[0]
			jobs := member(top, "jobs")
			if member(top, "on") == nil || jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) == 0 {
				return fmt.Errorf("%s: workflow requires triggers and a non-empty jobs map; remove a retired service-only workflow instead of leaving a placeholder", name)
			}
		}
	case ".sh", ".py":
		program, args := "bash", []string{"--noprofile", "--norc", "-n"}
		if path.Ext(name) == ".py" {
			program, args = "python3", []string{"-I", "-S", "-c", "import sys; compile(sys.stdin.read(), '<proposal>', 'exec')"}
		} else if bytes.HasPrefix(body, []byte("#!")) {
			first, _, _ := bytes.Cut(body, []byte("\n"))
			declaration := strings.Join(strings.Fields(string(first[2:])), " ")
			switch declaration {
			case "/bin/sh", "/usr/bin/sh", "/usr/bin/env sh":
				program, args = declaration, []string{"-n"}
				if declaration == "/usr/bin/env sh" {
					program = "sh"
				}
				// macOS sh is Bash in POSIX mode: -n still accepts function
				// names it rejects when defining them. Refuse this finite
				// unsupported form without executing candidate definitions.
				for _, definition := range shellFunctionDefinition.FindAllSubmatch(body, -1) {
					if !shellFunctionName.Match(definition[1]) {
						return fmt.Errorf("%s: unsupported sh function name %q; declared sh requires an identifier", name, definition[1])
					}
				}
			case "/bin/bash", "/usr/bin/bash", "/usr/bin/env bash":
				// Only the recognized shell is invoked, never an arbitrary shebang.
				program = declaration
				if declaration == "/usr/bin/env bash" {
					program = "bash"
				}
			default:
				return fmt.Errorf("%s: unsupported declared shell %q; syntax validation supports sh and bash without interpreter arguments", name, declaration)
			}
		}
		command := exec.CommandContext(ctx, program, args...)
		command.Stdin = bytes.NewReader(body)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: syntax check: %w: %s", name, err, output)
		}
	}
	return nil
}

// Existing skill trees are the schema-1 support-file transport shared by all
// adapters. Keep originals intact and carry notices byte-for-byte in one skill.
func (p *Plan) addSupport(_ *os.Root, value manifest.Manifest) error {
	if len(value.Artifacts.Skills) == 0 {
		return refuse("unsupported_support_files", manifest.Filename, "semantic packages need a skill tree for portable metadata and notices")
	}
	metadata, err := json.MarshalIndent(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Source  string `json:"source"`
	}{value.Name, value.Version, "github:" + value.Name}, "", "  ")
	if err != nil {
		return err
	}
	metadata = append(metadata, '\n')
	add := func(name string, data []byte, mode uint32) error {
		if old, exists := p.after[name]; exists {
			if !bytes.Equal(old.Content, data) || old.Mode != mode {
				return refuse("support_collision", name, "portable support file collides with existing content")
			}
			return nil
		}
		p.change(name, data, mode)
		return nil
	}
	for _, skill := range value.Artifacts.Skills {
		if err := add(path.Join(skill.Path, ".acr-package.json"), metadata, 0o644); err != nil {
			return err
		}
	}
	for _, name := range sortedPaths(p.before) {
		state := p.before[name]
		if !distributionNotice(name) || state.Directory {
			continue
		}
		published := false
		for _, skill := range value.Artifacts.Skills {
			if within(skill.Path, name) {
				published = true
			}
		}
		if !published {
			if state.Link != "" || state.Digest == "" {
				return refuse("unsafe_path", name, "notice must be a readable regular file")
			}
			target := path.Join(value.Artifacts.Skills[0].Path, path.Base(name)+".acr-"+strings.TrimPrefix(state.Digest, "sha256:")[:12])
			if err := add(target, state.Content, state.Mode); err != nil {
				return err
			}
			p.Report.Notes = append(p.Report.Notes, fmt.Sprintf("Preserved %s unchanged; publication also carries identical bytes at %s.", name, target))
		}
	}
	return nil
}

// This is a conservative refusal guard, not a shell parser or equivalence proof.
var shellFunctionDefinition = regexp.MustCompile(`(?m)(?:^|[;{} \t])([^ \t\r\n;{}()]+)[ \t]*\([ \t]*\)[ \t]*\{`)
var shellFunctionName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
