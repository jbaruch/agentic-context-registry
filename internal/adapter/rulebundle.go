package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

type ruleBundleContributions struct {
	items   []PlanItem
	outputs []Output
}

type instructionRoot struct {
	adapterID string
	path      string
}

func buildRuleBundles(project Snapshot, packages []Package, adapters []Adapter, compiler SharedCompiler) (map[string]ruleBundleContributions, error) {
	selected := make(map[string]bool)
	for _, candidate := range adapters {
		selected[candidate.Descriptor().ID] = true
	}
	if !selected["claude-code"] && !selected["codex"] {
		return nil, nil
	}
	hasRules := false
	for _, pkg := range packages {
		hasRules = hasRules || len(pkg.Manifest.Artifacts.Rules) != 0
	}
	if !hasRules {
		return nil, nil
	}
	var roots []instructionRoot
	if selected["claude-code"] {
		claudeRoots, err := existingInstructionRoots(project, []string{".claude/CLAUDE.md", "CLAUDE.md"}, "CLAUDE.md")
		if err != nil {
			return nil, err
		}
		for _, root := range claudeRoots {
			roots = append(roots, instructionRoot{adapterID: "claude-code", path: root})
		}
	}
	if selected["codex"] {
		codexRoots, err := existingInstructionRoots(project, []string{"AGENTS.md", "AGENTS.override.md"}, "AGENTS.md")
		if err != nil {
			return nil, err
		}
		for _, root := range codexRoots {
			roots = append(roots, instructionRoot{adapterID: "codex", path: root})
		}
	}
	selectedRoots := make([]string, len(roots))
	for index, root := range roots {
		selectedRoots[index] = root.path
	}
	provider, ok := compiler.(IncludeGraphProvider)
	if !ok {
		return nil, fmt.Errorf("shared preservation compiler does not provide an instruction include graph")
	}
	graph, err := provider.DiscoverIncludeGraph(project, selectedRoots)
	if err != nil {
		return nil, err
	}
	if err := graph.ValidateSelected(selectedRoots); err != nil {
		return nil, err
	}

	groups := groupInstructionRoots(roots, graph)
	result := make(map[string]ruleBundleContributions)
	for _, group := range groups {
		groupRoots := make([]string, len(group))
		for index, root := range group {
			groupRoots[index] = root.path
		}
		host, ok := graph.DeepestSharedHost(groupRoots)
		if !ok {
			return nil, fmt.Errorf("selected instruction roots %q have no shared host", groupRoots)
		}
		ownerAdapter := instructionHostOwner(group, host, graph)
		for _, pkg := range sortedPackages(packages) {
			if len(pkg.Manifest.Artifacts.Rules) == 0 {
				continue
			}
			references, err := PackageSkillReferences(pkg, nativeSkillsRoot(ownerAdapter))
			if err != nil {
				return nil, err
			}
			body, err := renderRuleBundle(pkg, references)
			if err != nil {
				return nil, err
			}
			owner := OwnerRef{
				Source: pkg.Source, ArtifactID: packageRulesID(pkg.Source), SourcePath: manifest.Filename, Kind: ArtifactRule,
			}
			contribution := result[ownerAdapter]
			contribution.items = append(contribution.items, PlanItem{Owner: owner, Target: host, Kind: OutputMarkdownInclude, Mode: 0o644})
			contribution.outputs = append(contribution.outputs, Output{
				Target: host, Mode: 0o644, Kind: OutputMarkdownInclude,
				Markdown: []MarkdownInsertion{{Owner: owner, BlockID: CanonicalMarkdownBlockID(owner, ownerAdapter), Body: body}},
			})
			result[ownerAdapter] = contribution
		}
	}
	for adapterID, bundle := range result {
		sort.Slice(bundle.items, func(left, right int) bool {
			return contributionKey(contribution{Target: bundle.items[left].Target, Kind: bundle.items[left].Kind, Mode: bundle.items[left].Mode, Owner: bundle.items[left].Owner}) <
				contributionKey(contribution{Target: bundle.items[right].Target, Kind: bundle.items[right].Kind, Mode: bundle.items[right].Mode, Owner: bundle.items[right].Owner})
		})
		sort.Slice(bundle.outputs, func(left, right int) bool {
			if bundle.outputs[left].Target != bundle.outputs[right].Target {
				return bundle.outputs[left].Target < bundle.outputs[right].Target
			}
			return bundle.outputs[left].Markdown[0].BlockID < bundle.outputs[right].Markdown[0].BlockID
		})
		result[adapterID] = bundle
	}
	return result, nil
}

func existingInstructionRoots(project Snapshot, candidates []string, fallback string) ([]string, error) {
	var roots []string
	for _, candidate := range candidates {
		observed, alias, err := readInstructionCandidate(project, candidate)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect instruction root %q: %w", candidate, err)
		}
		if alias != "" {
			// Classification already resolved the link. Only its destination
			// becomes a root; the symlink remains untouched.
			observed.Path = alias
		} else if !observed.Mode.IsRegular() {
			continue
		}
		if snapshot, ok := project.(*RootSnapshot); ok {
			observed.Path, err = snapshot.instructionRootPath(observed.Path)
			if err != nil {
				return nil, fmt.Errorf("normalize instruction root %q: %w", candidate, err)
			}
		}
		roots = append(roots, observed.Path)
	}
	if len(roots) == 0 {
		roots = []string{fallback}
	}
	sort.Strings(roots)
	compact := roots[:0]
	for _, root := range roots {
		if len(compact) == 0 || compact[len(compact)-1] != root {
			compact = append(compact, root)
		}
	}
	return compact, nil
}

// instructionRootPath recovers directory-entry spelling for an already
// link-free root. Native metadata handles filesystem name equivalence without
// merging distinct case-sensitive names or distinct hardlink entries.
func (snapshot *RootSnapshot) instructionRootPath(filename string) (string, error) {
	directory := "."
	for _, component := range strings.Split(filename, "/") {
		requested := path.Join(directory, component)
		info, err := snapshot.root.Lstat(requested)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("instruction root %q changed to a symlink; keep it stable and retry", filename)
		}
		entries, err := snapshot.ReadDir(directory)
		if err != nil {
			return "", err
		}
		// Prefer an exact entry, even when other hardlinks share its inode.
		var exact string
		for _, entry := range entries {
			if path.Base(entry.Path) == component {
				exact = entry.Path
				break
			}
		}
		var matches []string
		for _, entry := range entries {
			if exact != "" && entry.Path != exact {
				continue
			}
			stored, err := snapshot.root.Lstat(entry.Path)
			if err != nil {
				return "", err
			}
			if os.SameFile(info, stored) {
				matches = append(matches, entry.Path)
			}
		}
		if len(matches) != 1 {
			return "", fmt.Errorf("instruction root %q has changed or has ambiguous directory-entry spelling; use an exact on-disk spelling and retry", filename)
		}
		directory = matches[0]
	}
	return directory, nil
}

func groupInstructionRoots(roots []instructionRoot, graph InstructionIncludeGraph) [][]instructionRoot {
	var groups [][]instructionRoot
	for _, root := range roots {
		matching := -1
		for index, group := range groups {
			if _, shared := graph.DeepestSharedHost([]string{root.path, group[0].path}); shared {
				matching = index
				break
			}
		}
		if matching < 0 {
			groups = append(groups, []instructionRoot{root})
		} else {
			groups[matching] = append(groups[matching], root)
		}
	}
	return groups
}

func instructionHostOwner(group []instructionRoot, host string, graph InstructionIncludeGraph) string {
	owner := "claude-code"
	for _, root := range group {
		if root.adapterID == "codex" {
			if graph.Reachable(root.path, host) {
				owner = "codex"
				break
			}
		}
	}
	return owner
}

func sortedPackages(packages []Package) []Package {
	sorted := append([]Package(nil), packages...)
	sort.SliceStable(sorted, func(left, right int) bool { return sorted[left].Source < sorted[right].Source })
	return sorted
}

// nativeSkillsRoot is the installed skills tree of the adapter that owns one
// instruction host. A rule that names a bundled helper resolves against the
// tree the host's owning adapter installs; when Claude and Codex share one
// host, every client reading it reaches the owner's tree by the same
// project-relative path, so one contribution stays reachable to both.
func nativeSkillsRoot(adapterID string) string {
	if adapterID == "codex" {
		return ".codex/skills"
	}
	return ".claude/skills"
}

func renderRuleBundle(pkg Package, references SkillReferences) ([]byte, error) {
	rules := append([]manifest.RuleArtifact(nil), pkg.Manifest.Artifacts.Rules...)
	sort.SliceStable(rules, func(left, right int) bool { return rules[left].ID < rules[right].ID })
	var body bytes.Buffer
	fmt.Fprintf(&body, "## ACR package: %s\n", pkg.Source)
	for _, rule := range rules {
		source, err := ReadPackageFile(pkg, rule.Path)
		if err != nil {
			return nil, fmt.Errorf("read rule %q from %s: %w", rule.ID, pkg.Source, err)
		}
		content, err := StripLeadingFrontmatter(source.Content)
		if err != nil {
			return nil, fmt.Errorf("rule %q from %s: %w", rule.ID, pkg.Source, err)
		}
		fmt.Fprintf(&body, "\n### Rule: %s\n\n", rule.ID)
		if rule.Activation.Mode == manifest.ActivationPaths {
			fmt.Fprintf(&body, "Apply only when a working path matches: %s\n\n", strings.Join(rule.Activation.Paths, ", "))
		}
		body.Write(RebasePackageReferences(content, references))
		if body.Len() != 0 && body.Bytes()[body.Len()-1] != '\n' {
			body.WriteByte('\n')
		}
	}
	return body.Bytes(), nil
}

func packageRulesID(source string) string {
	digest := sha256.Sum256([]byte(source))
	return "package-rules-" + hex.EncodeToString(digest[:])
}
