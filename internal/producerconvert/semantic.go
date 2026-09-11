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
			cached[index] = proposed
			combined.Edits = append(combined.Edits, proposed.Edits...)
			combined.PolicyChanges = append(combined.PolicyChanges, proposed.PolicyChanges...)
		}
		next, validationErr := validateProposal(ctx, original, combined)
		if validationErr == nil {
			next.Report.AgentRuns = plan.Report.AgentRuns
			next.Report.Notes = append(next.Report.Notes, plan.Report.Notes...)
			return next, nil
		}
		plan.Report.AgentRuns[len(plan.Report.AgentRuns)-1].Failure = validationErr.Error()
		err = refuse("invalid_agent_proposal", "--agent", validationErr.Error())
		previous, feedback = combined, validationErr.Error()
		retryFrom = firstAffectedScope(requests, combined, feedback)
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
	return (within(p.options.PackageRoot, name) || strings.HasPrefix(name, ".github/") || strings.HasPrefix(name, "tests/")) && (bytes.Contains(bytes.ToLower(state.Content), []byte("tessl")) || bytes.Contains(state.Content, []byte("tile.json")))
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
			if edit.Content != "" || len(edit.Replacements) != 0 || !strings.HasPrefix(name, ".github/") {
				return result, fmt.Errorf("%s: only owned Tessl delivery files can be removed", name)
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
		if !seen[policy.Path] || policy.From == "" || policy.To == "" {
			return result, fmt.Errorf("policy changes must explain a changed file with non-empty from/to")
		}
	}
	for name := range seen {
		old := string(p.before[name].Content)
		if strings.Contains(strings.ToLower(old), "skill-review") || regexp.MustCompile(`(?i)(score|threshold)[^\n]*85`).MatchString(old) {
			disclosed := false
			for _, policy := range proposed.PolicyChanges {
				if policy.Path == name {
					disclosed = true
				}
			}
			if !disclosed {
				problems = append(problems, fmt.Errorf("%s: Tessl paid scoring policy change requires an explicit policyChanges record; no ACR equivalent exists", name))
			}
		}
	}
	if len(problems) > 0 {
		return result, errors.Join(problems...)
	}
	directory, err := os.MkdirTemp("", "acr-validation-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(directory)) }()
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
			if err = os.MkdirAll(full, fs.FileMode(state.Mode)); err != nil {
				return result, err
			}
			// MkdirAll applies the process umask. The validation inventory must
			// retain the source mode that the transaction will later compare.
			if err = os.Chmod(full, fs.FileMode(state.Mode)); err != nil {
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
			result.change(name, nil, 0)
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

var foreignInstalledRoots = regexp.MustCompile(`\.tessl/plugins/[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+/`)

var testNames = regexp.MustCompile(`(?m)^\s*(?:def|func)\s+(test_[A-Za-z0-9_]+|Test[A-Za-z0-9_]+)\s*\(`)

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
	if strings.HasPrefix(name, ".github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
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
			oldJobs := member(old.Content[0], "jobs")
			var newJobs *yaml.Node
			if len(new.Content) > 0 {
				newJobs = member(new.Content[0], "jobs")
			}
			if oldJobs != nil {
				for i := 0; i < len(oldJobs.Content); i += 2 {
					job := oldJobs.Content[i+1]
					if removableDeliveryJob(job) {
						continue
					}
					if len(new.Content) == 0 {
						return fmt.Errorf("%s: retain independent review/test workflow", name)
					}
					if err := preserveWorkflowFields(old.Content[0], new.Content[0], false, "jobs", "name"); err != nil {
						return fmt.Errorf("%s: %w", name, err)
					}
					nameOfJob := oldJobs.Content[i].Value
					nextJob := member(newJobs, nameOfJob)
					if nextJob == nil {
						return fmt.Errorf("%s: independent job %s must remain", name, nameOfJob)
					}
					if err := preserveWorkflowJob(job, nextJob, verifiedGHWorkflowPair(original, name)); err != nil {
						return fmt.Errorf("%s job %s: %w", name, nameOfJob, err)
					}
				}
			}
		}
	}
	return nil
}
func workflowSemantic(body []byte) bool {
	// YAML comments can disclose retired services without invoking them. Decode
	// and re-encode values so runnable strings remain visible to the scanner.
	var value any
	if err := yaml.Unmarshal(body, &value); err == nil {
		if encoded, err := yaml.Marshal(value); err == nil {
			body = encoded
		}
	}
	lower := strings.ToLower(string(body))
	return runtimeSemanticOperation(body) != "" || strings.Contains(lower, "setup-tessl") || strings.Contains(lower, "patch-version-publish") || strings.Contains(lower, "/skill-review@")
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
