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

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"go.yaml.in/yaml/v3"
)

//go:embed check-tests.py
var pythonTestChecks string

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
	for _, edit := range proposed.Edits {
		candidate, problem, err := resolveEdit(p, edit, seen)
		if err != nil {
			return result, err
		}
		if problem != nil {
			problems = append(problems, problem)
			continue
		}
		for _, constraint := range editConstraints {
			if !constraint.applies(candidate) {
				continue
			}
			if err := constraint.check(ctx, candidate); err != nil {
				if constraint.fatal {
					return result, err
				}
				problems = append(problems, err)
			}
		}
		if candidate.removal {
			delete(next, candidate.name)
			continue
		}
		next[candidate.name] = fileState{Content: candidate.body, Mode: candidate.mode, Digest: digest(candidate.body)}
	}
	if err := apply(proposalConstraints, proposalCandidate{plan: p, proposed: proposed, seen: seen, next: next}); err != nil {
		return result, err
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
	// Re-plan with semantic inventory even if this options copy later has Agent cleared.
	// The oracle is the acceptance test; the constraint lists run alongside it.
	candidate, err := prepareDeterministic(options, true)
	if err != nil {
		encoded, e := json.Marshal(candidate.Report.Blockers)
		if e != nil {
			return result, e
		}
		return result, fmt.Errorf("candidate conversion: %w; blockers: %s", err, encoded)
	}
	if err := apply(candidateConstraints, candidateResult{plan: p, candidate: candidate, proposed: proposed}); err != nil {
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

// resolveEdit checks one edit's shape, authority and digest against the
// snapshot and produces the candidate the edit constraints inspect. A
// malformed edit ends validation. A patch whose replacements do not match
// stays open as a problem so independent scope findings report together.
func resolveEdit(p Plan, edit proposedEdit, seen map[string]bool) (candidate editCandidate, problem, err error) {
	name := edit.Path
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") || excluded(name) || semanticConsumerPath(name) || seen[name] {
		return candidate, nil, fmt.Errorf("unexpected or duplicate path %q", name)
	}
	seen[name] = true
	before, exists := p.before[name]
	if exists && (!editable(p, name) || edit.BeforeDigest != before.Digest) {
		return candidate, nil, fmt.Errorf("%s: protected path or stale beforeDigest", name)
	}
	candidate = editCandidate{plan: p, name: name, exists: exists, before: before, mode: before.Mode}
	if !exists {
		if edit.Action != "create" || edit.BeforeDigest != "" || !p.before[path.Dir(name)].Directory || path.Base(name) == ".acr-package.json" || consumerFile(name) || distributionNotice(name) {
			return candidate, nil, fmt.Errorf("%s: new file requires an existing skill/test parent and empty beforeDigest", name)
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
			return candidate, nil, fmt.Errorf("%s: new files must belong to an existing skill tree or tests", name)
		}
		candidate.mode = 0o644
	}
	switch edit.Action {
	case "replace", "create":
		if len(edit.Replacements) != 0 {
			return candidate, nil, fmt.Errorf("%s: content edits cannot contain replacements", name)
		}
		candidate.body = []byte(edit.Content)
	case "patch":
		if edit.Content != "" || len(edit.Replacements) == 0 {
			return candidate, nil, fmt.Errorf("%s: patch requires only replacements", name)
		}
		body := append([]byte(nil), before.Content...)
		for _, r := range edit.Replacements {
			if r.Old == "" || r.Count <= 0 || bytes.Count(body, []byte(r.Old)) != r.Count {
				return candidate, fmt.Errorf("%s: replacement match count differs for %q", name, r.Old), nil
			}
			body = bytes.ReplaceAll(body, []byte(r.Old), []byte(r.New))
		}
		candidate.body = body
	case "remove":
		if edit.Content != "" || len(edit.Replacements) != 0 || !workflowFile(name) {
			return candidate, nil, fmt.Errorf("%s: only proven service-only workflows can be removed", name)
		}
		candidate.removal = true
	default:
		return candidate, nil, fmt.Errorf("%s: unsupported edit action %q", name, edit.Action)
	}
	return candidate, nil, nil
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

// preserveChecksWithSource applies the retained-content constraints, stopping
// at the first failure. after is nil for a removal.
func preserveChecksWithSource(name string, before, after []byte, original tree) error {
	return apply(contentConstraints, contentCandidate{name: name, before: before, after: after, original: original})
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

// This is a conservative refusal guard, not a shell parser or equivalence proof.
var shellFunctionDefinition = regexp.MustCompile(`(?m)(?:^|[;{} \t])([^ \t\r\n;{}()]+)[ \t]*\([ \t]*\)[ \t]*\{`)
var shellFunctionName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
