package producerconvert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/packageref"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
	"go.yaml.in/yaml/v3"
)

// Plan holds a complete delta and private fingerprint-bound source evidence.
// Apply never trusts caller-edited report fields as filesystem operations.
type Plan struct {
	Report  Report
	root    string
	options Options
	before  tree
	after   tree
	changes []Change
	receipt []byte
}

type receipt struct {
	SchemaVersion  int                          `json:"schemaVersion"`
	Options        Options                      `json:"options"`
	SourcePackage  string                       `json:"sourcePackage"`
	SourceVersion  string                       `json:"sourceVersion"`
	Package        string                       `json:"package"`
	Version        string                       `json:"version"`
	PublishedFiles []string                     `json:"publishedFiles"`
	Artifacts      []tesslplugin.ArtifactRecord `json:"artifacts"`
	Source         tree                         `json:"source"`
	Output         tree                         `json:"output"`
	PolicyChanges  []PolicyChange               `json:"policyChanges,omitempty"`
}

// Convert plans first; unsupported sources and previews do not change the source.
func Convert(options Options) (Report, error) {
	plan, err := Prepare(options)
	if err != nil {
		return plan.Report, err
	}
	if options.DryRun || plan.Report.Current {
		return plan.Report, nil
	}
	return plan.Apply()
}

// Prepare determines identity, the exact delta and distribution inventory.
func Prepare(options Options) (Plan, error) { return PrepareContext(context.Background(), options) }

// PrepareContext cancels an explicitly selected provider with the caller.
func PrepareContext(ctx context.Context, options Options) (Plan, error) {
	if options.Agent != "" {
		return prepareAssisted(ctx, options)
	}
	return prepareDeterministic(options)
}

func prepareDeterministic(options Options) (plan Plan, err error) {
	plan.Report = Report{ReportVersion: 1, DryRun: options.DryRun, Manifest: manifest.Filename, Receipt: ReceiptPath, Changes: []Change{}, Blockers: []Blocker{}, Notes: []string{}, PublishedFiles: []string{}}
	if options.Repository == "" {
		return plan, refuse("invalid_options", "--repository", "clean mode requires an explicit target repository")
	}
	options.Repository = strings.TrimSuffix(options.Repository, ".git")
	boundary, selected, err := repositoryBoundary(options.PackageRoot)
	if err != nil {
		return plan, err
	}
	if selected != "." && excluded(selected) {
		return plan, refuse("unsafe_path", selected, "installed consumer and reserved paths cannot be selected as authored producers")
	}
	plan.root = boundary
	plan.Report.RepositoryRoot = boundary
	plan.options = options
	plan.options.PackageRoot = selected
	plan.options.DryRun = false
	root, err := os.OpenRoot(boundary)
	if err != nil {
		return plan, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if _, err := root.Lstat(transactionPath); err == nil {
		return plan, refuse("transaction_conflict", transactionPath, "another migration or interrupted transaction exists; inspect its backups before retrying")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return plan, err
	}
	plan.before, err = snapshot(root, selected, options.Agent != "")
	if err != nil {
		return plan, err
	}
	if data, e := readState(root, ReceiptPath); e == nil {
		return resume(plan, data.Content)
	} else if !errors.Is(e, fs.ErrNotExist) {
		return plan, e
	}
	plan.after = tree{}
	for name, state := range plan.before {
		plan.after[name] = state
	}
	// The enclosing repository is a single distribution boundary. Installed
	// consumers are excluded from this inventory and never counted as producers.
	for _, name := range sortedPaths(plan.before) {
		state := plan.before[name]
		if path.Base(name) == manifest.Filename {
			plan.block(name, "existing ACR manifest competes with root output; use the original source checkout")
		}
		if state.Directory {
			continue
		}
		if state.Link != "" && (within(selected, name) || strings.HasPrefix(name, ".github/")) {
			return plan, refuse("unsafe_path", name, "selected producer and workflow paths must be regular files; symlink traversal is unsupported")
		}
		if (strings.HasSuffix(name, "/.tessl-plugin/plugin.json") || name == ".tessl-plugin/plugin.json" || path.Base(name) == "tile.json") && name != path.Join(selected, ".tessl-plugin/plugin.json") && name != path.Join(selected, "tile.json") {
			plan.block(name, "another authored Tessl package makes root distribution ambiguous; select a repository with one producer package")
		}
	}
	packageRoot := filepath.Join(boundary, filepath.FromSlash(selected))
	sources, err := tesslplugin.Read(packageRoot)
	if err != nil {
		return plan, err
	}
	if sources.Plugin != nil {
		plan.Report.SourcePackage = sources.Plugin.Name
		plan.Report.SourceVersion = sources.Plugin.Version
	}
	if sources.Tile != nil {
		if plan.Report.SourcePackage == "" {
			plan.Report.SourcePackage = sources.Tile.Name
		}
		if plan.Report.SourceVersion == "" {
			plan.Report.SourceVersion = sources.Tile.Version
		}
	}
	retired := map[string]bool{}
	for _, name := range []string{".tessl-plugin/plugin.json", "tile.json", ".tesslignore", ".tileignore"} {
		full := path.Join(selected, name)
		if state, exists := plan.before[full]; exists && !state.Directory {
			retired[full] = true
			plan.change(full, nil, 0)
		}
	}
	// Analyze all authored runtime and delivery files before mapping. This makes
	// custom operations visible even when another limitation also blocks mapping.
	for _, name := range sortedPaths(plan.before) {
		state := plan.before[name]
		if state.Directory || retired[name] || consumerFile(name) {
			continue
		}
		if strings.HasPrefix(name, ".github/") && strings.Contains(strings.ToLower(string(state.Content)), "tessl") && (options.Agent == "" || workflowSemantic(state.Content)) {
			if strings.HasPrefix(name, ".github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
				if !bytes.Contains(state.Content, []byte("tesslio/patch-version-publish@v1")) {
					plan.block(name, "Tessl-dependent workflow is not the recognized standalone publisher; its commands and policy require semantic conversion")
					continue
				}
				next, e := translateWorkflow(state.Content, selected)
				if e != nil {
					plan.block(name, "unsupported Tessl workflow/review policy: "+e.Error())
					continue
				}
				if _, exists := plan.after[publishWorkflowPath]; exists {
					plan.block(publishWorkflowPath, "tag-publish output already exists or multiple publishers select it")
					continue
				}
				mode := state.Mode
				if len(next) == 0 {
					mode = 0
				}
				plan.change(name, next, mode)
				plan.change(publishWorkflowPath, []byte(publishWorkflow), 0o644)
				plan.Report.Notes = append(plan.Report.Notes, "Publication changes from patch releases on main to explicit v* version tags. Independent tests retain their original triggers. Update agent-plugin.yaml before tagging.")
			} else {
				plan.block(name, "Tessl-dependent file outside the recognized standalone publisher requires semantic conversion")
			}
			continue
		}
		if within(selected, name) || options.Agent != "" && strings.HasPrefix(name, "tests/") {
			if reason := semanticOperation(state.Content); reason != "" {
				plan.block(name, reason)
			}
		}
	}
	value, compat, err := tesslplugin.Map(tesslplugin.Options{PackageRoot: packageRoot, AcceptAgentWidening: options.AcceptAgentWidening, DryRun: true}, options.Repository, options.PackageVersion)
	if err != nil {
		if len(plan.Report.Blockers) > 0 {
			return plan, plan.blocked()
		}
		return plan, err
	}
	plan.Report.Package, plan.Report.Version = value.Name, value.Version
	original := value
	published, err := manifest.PlannedPackageFiles(packageRoot, original)
	if err != nil {
		return plan, err
	}
	// Rebase only skill-tree references: these are the paths native adapters
	// materialize across all three agents. References to standalone rules/hooks
	// lack that native path contract and are refused instead of guessed.
	files := map[string]string{}
	roots := []string{".tessl/plugins/" + plan.Report.SourcePackage + "/"}
	for _, artifact := range artifactRecords(original) {
		if artifact.Kind != "skill" {
			roots = append(roots, artifact.Path, path.Join(selected, artifact.Path))
		}
	}
	for _, skill := range original.Artifacts.Skills {
		roots = append(roots, skill.Path+"/")
		if selected != "." {
			roots = append(roots, path.Join(selected, skill.Path)+"/")
		}
	}
	for _, name := range published {
		for _, skill := range original.Artifacts.Skills {
			if strings.HasPrefix(name, skill.Path+"/") {
				target := path.Join(selected, name)
				files[name] = target
				files[target] = target
				files[".tessl/plugins/"+plan.Report.SourcePackage+"/"+name] = target
			}
		}
	}
	for _, name := range sortedPaths(plan.before) {
		state := plan.before[name]
		if state.Directory || retired[name] || consumerFile(name) || !within(selected, name) || strings.HasPrefix(name, ".github/") {
			continue
		}
		next, e := packageref.RewriteFiles(state.Content, files, roots)
		if e != nil {
			plan.block(name, e.Error())
			continue
		}
		if !bytes.Equal(state.Content, next) {
			if distributionNotice(name) {
				plan.block(name, "owned reference in license/notice text cannot be rewritten while preserving its bytes")
				continue
			}
			if !utf8.Valid(state.Content) {
				plan.block(name, "owned references in binary content cannot be safely rewritten")
				continue
			}
			plan.change(name, next, state.Mode)
		}
	}
	for _, hook := range value.Artifacts.Hooks {
		for _, arg := range hook.Args {
			updated, e := packageref.RewriteFiles([]byte(arg), files, roots)
			if e != nil || string(updated) != arg || semanticOperation([]byte(arg)) != "" {
				plan.block(hook.Path, "hook argument contains an owned or Tessl-dependent path; native argument-path translation requires semantic conversion")
			}
		}
	}
	prefixArtifacts(&value, selected)
	value.Source.TesslIdentity = ""
	for _, rule := range value.Artifacts.Rules {
		for _, pattern := range rule.Activation.Paths {
			if strings.Contains(strings.ToLower(pattern), "tessl") || strings.Contains(pattern, "tile.json") {
				plan.block(rule.Path, "Tessl-specific rule activation requires a reviewed policy conversion")
			}
		}
	}
	if options.Agent != "" {
		if err := plan.addSupport(root, value); err != nil {
			return plan, err
		}
	}
	plan.Report.PublishedFiles, err = manifest.PlannedPackageFiles(boundary, value)
	if err != nil {
		return plan, err
	}
	for name := range plan.after {
		if _, exists := plan.before[name]; exists {
			continue
		}
		for _, skill := range value.Artifacts.Skills {
			if strings.HasPrefix(name, skill.Path+"/") {
				plan.Report.PublishedFiles = append(plan.Report.PublishedFiles, name)
				break
			}
		}
	}
	sort.Strings(plan.Report.PublishedFiles)
	publishedSet := map[string]bool{}
	for _, name := range plan.Report.PublishedFiles {
		publishedSet[name] = true
		if retired[name] {
			plan.block(name, "retired producer metadata is inside a published artifact tree")
		}
	}
	for _, name := range sortedPaths(plan.before) {
		if !plan.before[name].Directory && distributionNotice(name) && !publishedSet[name] && options.Agent == "" {
			plan.block(name, "required license/notice file is outside manifest.PackageFiles; support-file packaging is needed before clean conversion")
		}
	}
	if len(plan.Report.Blockers) > 0 {
		return plan, plan.blocked()
	}
	rendered, err := yaml.Marshal(value)
	if err != nil {
		return plan, err
	}
	plan.Report.Notes = append(plan.Report.Notes, ignoreRetirementNotes(compat.Ignored, published)...)
	// Preserve attribution encoded only in the retired manifests as YAML comments
	// in the distributed root manifest. JSON quoting preserves literal newlines.
	var provenance strings.Builder
	sourceRepository := ""
	if sources.Plugin != nil {
		sourceRepository = sources.Plugin.Repository
	}
	if sourceRepository == "" && sources.Tile != nil {
		sourceRepository = sources.Tile.Repository
	}
	for _, field := range []struct{ name, value string }{{"name", plan.Report.SourcePackage}, {"repository", sourceRepository}} {
		if field.value != "" {
			encoded, e := json.Marshal(field.value)
			if e != nil {
				return plan, e
			}
			fmt.Fprintf(&provenance, "# Original %s: %s\n", field.name, encoded)
		}
	}
	for _, loss := range compat.Lossy {
		if loss.Reason == "provenance" {
			encoded, e := json.Marshal(loss.Value)
			if e != nil {
				return plan, e
			}
			fmt.Fprintf(&provenance, "# Original %s: %s\n", loss.Field, encoded)
		}
	}
	rendered = append([]byte(provenance.String()), rendered...)
	plan.change(manifest.Filename, rendered, 0o644)
	plan.Report.Artifacts = artifactRecords(value)
	sort.Slice(plan.changes, func(i, j int) bool { return plan.changes[i].Path < plan.changes[j].Path })
	if err := plan.sealReceipt(); err != nil {
		return plan, err
	}
	// Recheck every input after all parser/inventory reads, before yielding a plan.
	current, err := snapshot(root, selected, options.Agent != "")
	if err != nil {
		return plan, err
	}
	if !matches(plan.before, current) {
		return plan, refuse("source_changed", boundary, "source changed during planning; rerun the dry-run")
	}
	return plan, nil
}

func resume(plan Plan, data []byte) (Plan, error) {
	var rec receipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rec); err != nil {
		return plan, refuse("receipt_conflict", ReceiptPath, "invalid receipt; restore the original receipt or source checkout: "+err.Error())
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return plan, refuse("receipt_conflict", ReceiptPath, "receipt must contain exactly one JSON object")
	}
	if rec.SchemaVersion != 2 || rec.Options != plan.options || rec.Output == nil || rec.Package == "" || rec.SourcePackage == "" {
		return plan, refuse("receipt_conflict", ReceiptPath, "receipt version or conversion options differ; restore the original source for a different migration")
	}
	if !matches(rec.Output, receiptFingerprints(plan.before)) {
		return plan, refuse("receipt_conflict", ReceiptPath, "converted output was edited, added or removed; restore it before rerunning this migration")
	}
	plan.Report.Current = true
	plan.Report.SourcePackage, plan.Report.SourceVersion = rec.SourcePackage, rec.SourceVersion
	plan.Report.Package, plan.Report.Version = rec.Package, rec.Version
	plan.Report.PublishedFiles, plan.Report.Artifacts = rec.PublishedFiles, rec.Artifacts
	plan.Report.PolicyChanges = rec.PolicyChanges
	return plan, nil
}

func (p *Plan) block(name, reason string) {
	p.Report.Blockers = append(p.Report.Blockers, Blocker{Path: name, Reason: reason})
}
func (p *Plan) blocked() error {
	sort.SliceStable(p.Report.Blockers, func(i, j int) bool { return p.Report.Blockers[i].Path < p.Report.Blockers[j].Path })
	first := p.Report.Blockers[0]
	return refuse("unsupported_semantic_conversion", first.Path, first.Reason+"; see all blockers and retain the source until a supported semantic conversion is available")
}
func (p *Plan) change(name string, data []byte, mode uint32) {
	before, exists := p.before[name]
	c := makeChange(name, before, data, mode, exists)
	for i, existing := range p.changes {
		if existing.Path == name {
			p.changes[i] = c
			goto updated
		}
	}
	p.changes = append(p.changes, c)
updated:
	if mode == 0 {
		delete(p.after, name)
	} else {
		p.after[name] = fileState{Content: data, Mode: mode, Digest: digest(data)}
	}
}
func makeChange(name string, before fileState, data []byte, mode uint32, exists bool) Change {
	operation := "modify"
	if !exists {
		operation = "create"
	}
	if mode == 0 {
		operation = "remove"
	}
	return Change{Path: name, Operation: operation, Before: string(before.Content), After: string(data), BeforeMode: before.Mode, AfterMode: mode, Diff: exactDiff(name, string(before.Content), string(data))}
}
func sortedPaths(t tree) []string {
	names := make([]string, 0, len(t))
	for name := range t {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func within(selected, name string) bool {
	return selected == "." || strings.HasPrefix(name, selected+"/")
}
func consumerFile(name string) bool {
	switch path.Base(name) {
	case "AGENTS.md", "CLAUDE.md", "tessl.json", "tessl-lock.json", "tessl-package.json", "agents.yaml":
		return !strings.Contains(name, "/skills/") && !strings.HasPrefix(name, "skills/")
	}
	return false
}
func distributionNotice(name string) bool {
	base := strings.ToUpper(path.Base(name))
	for _, prefix := range []string{"LICENSE", "LICENCE", "NOTICE", "COPYING", "COPYRIGHT", "AUTHORS"} {
		if base == prefix || strings.HasPrefix(base, prefix+".") || strings.HasPrefix(base, prefix+"-") {
			return true
		}
	}
	return false
}

// Refusals recognize literal operation syntax, never infer arbitrary behavior.
var semanticPatterns = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile("(?m)(?:^|[;&|()`]|\\b(?:exec|command|env|sudo|then|do|if)\\s+)\\s*tessl(?:\\s|$)|[\"']tessl[\"']"), "unknown or custom Tessl command requires semantic conversion"},
	{regexp.MustCompile(`(?i)\btessl\s+(?:--?[^\s]+\s+)*(install|uninstall|update|publish|review|login|plugin|lint|init|tile|build)\b`), "Tessl command/dependency operation has no deterministic semantic translation"},
	{regexp.MustCompile(`(?i)(?:tessl(?:-lock|-package)?\.json|\.tessl-plugin/plugin\.json)`), "Tessl configuration/manifest reference requires semantic conversion; state, pins and rollback cannot be inferred"},
	{regexp.MustCompile(`\bTESSL_[A-Z_]+\b`), "custom Tessl environment or dynamic path operation requires semantic conversion"},
	{regexp.MustCompile(`\.tessl/(?:RULES\.md|tiles/|config|cache|plugins/\$)`), "custom Tessl state or dynamic installed path requires semantic conversion"},
}

func semanticOperation(data []byte) string {
	var reasons []string
	for _, rule := range semanticPatterns {
		if rule.pattern.Match(data) {
			reasons = append(reasons, rule.reason)
		}
	}
	return strings.Join(reasons, "; ")
}

func prefixArtifacts(m *manifest.Manifest, prefix string) {
	for i := range m.Artifacts.Skills {
		m.Artifacts.Skills[i].Path = path.Join(prefix, m.Artifacts.Skills[i].Path)
	}
	for i := range m.Artifacts.Rules {
		m.Artifacts.Rules[i].Path = path.Join(prefix, m.Artifacts.Rules[i].Path)
	}
	for i := range m.Artifacts.Hooks {
		m.Artifacts.Hooks[i].Path = path.Join(prefix, m.Artifacts.Hooks[i].Path)
	}
	for i := range m.Artifacts.Scripts {
		m.Artifacts.Scripts[i].Path = path.Join(prefix, m.Artifacts.Scripts[i].Path)
	}
}
func artifactRecords(m manifest.Manifest) []tesslplugin.ArtifactRecord {
	result := []tesslplugin.ArtifactRecord{}
	for _, v := range m.Artifacts.Rules {
		result = append(result, tesslplugin.ArtifactRecord{ID: v.ID, Kind: "rule", Path: v.Path})
	}
	for _, v := range m.Artifacts.Skills {
		result = append(result, tesslplugin.ArtifactRecord{ID: v.ID, Kind: "skill", Path: v.Path})
	}
	for _, v := range m.Artifacts.Hooks {
		result = append(result, tesslplugin.ArtifactRecord{ID: v.ID, Kind: "hook", Path: v.Path, Event: string(v.Event)})
	}
	return result
}

func ignoreRetirementNotes(ignored []tesslplugin.IgnoredItem, published []string) []string {
	var notes []string
	for _, item := range ignored {
		var matches []string
		pattern := strings.TrimPrefix(item.Path, "/")
		for _, name := range published {
			matched, err := path.Match(pattern, name)
			if err != nil {
				notes = append(notes, fmt.Sprintf("Retired .%s entry %q cannot be evaluated as a simple glob: %v. Review the complete publishedFiles inventory.", item.Reason, item.Path, err))
				break
			}
			// These are disclosed literal/directory/simple-glob matches, not a
			// reimplementation of Tessl ignore rules (negation and ** included).
			if matched || name == strings.TrimSuffix(pattern, "/") || strings.HasPrefix(name, strings.TrimSuffix(pattern, "/")+"/") {
				matches = append(matches, name)
			}
		}
		notes = append(notes, fmt.Sprintf("Retired .%s entry %q no longer filters publication. Literal, directory or simple-glob matches in publishedFiles: %v. Review the complete inventory for other ignore syntax.", item.Reason, item.Path, matches))
	}
	return notes
}

func (plan *Plan) sealReceipt() error {
	rec := receipt{SchemaVersion: 2, Options: plan.options, SourcePackage: plan.Report.SourcePackage, SourceVersion: plan.Report.SourceVersion, Package: plan.Report.Package, Version: plan.Report.Version, PublishedFiles: plan.Report.PublishedFiles, Artifacts: plan.Report.Artifacts, Source: receiptFingerprints(plan.before), Output: receiptFingerprints(plan.after), PolicyChanges: plan.Report.PolicyChanges}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	plan.receipt = append(data, '\n')
	sort.Slice(plan.changes, func(i, j int) bool { return plan.changes[i].Path < plan.changes[j].Path })
	plan.Report.Changes = append([]Change(nil), plan.changes...)
	plan.Report.Changes = append(plan.Report.Changes, makeChange(ReceiptPath, fileState{}, plan.receipt, 0o600, false))
	return nil
}
