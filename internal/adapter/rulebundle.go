package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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
	reachable map[string]int
}

func buildRuleBundles(project Snapshot, packages []Package, adapters []Adapter) (map[string]ruleBundleContributions, error) {
	selected := make(map[string]bool)
	for _, candidate := range adapters {
		selected[candidate.Descriptor().ID] = true
	}
	if !selected["claude-code"] && !selected["codex"] {
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
	for index := range roots {
		reachable, err := instructionReachability(project, roots[index].path, nil)
		if err != nil {
			return nil, err
		}
		roots[index].reachable = reachable
	}

	groups := groupInstructionRoots(roots)
	result := make(map[string]ruleBundleContributions)
	for _, group := range groups {
		host, ownerAdapter := deepestInstructionHost(group)
		for _, pkg := range sortedPackages(packages) {
			if len(pkg.Manifest.Artifacts.Rules) == 0 {
				continue
			}
			body, err := renderRuleBundle(pkg)
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
		observed, err := project.ReadFile(candidate)
		if err == nil {
			if observed.Mode.IsRegular() {
				roots = append(roots, candidate)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect instruction root %q: %w", candidate, err)
		}
	}
	if len(roots) == 0 {
		roots = []string{fallback}
	}
	sort.Strings(roots)
	return roots, nil
}

func instructionReachability(project Snapshot, root string, active map[string]bool) (map[string]int, error) {
	depth := map[string]int{root: 0}
	queue := []string{root}
	if active == nil {
		active = make(map[string]bool)
	}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		observed, err := project.ReadFile(current)
		if errors.Is(err, fs.ErrNotExist) && current == root {
			continue
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("unresolved_include: instruction include %q does not resolve to a regular file", current)
			}
			return nil, err
		}
		includes := scanInstructionIncludes(observed.Content)
		for _, token := range includes {
			if strings.Contains(token, "\\") || strings.HasPrefix(token, "/") || path.Clean(token) != token || token == ".." || strings.HasPrefix(token, "../") {
				return nil, fmt.Errorf("invalid_include: %q uses non-normalized project-relative POSIX syntax", token)
			}
			target := path.Clean(path.Join(path.Dir(current), token))
			if target == ".." || strings.HasPrefix(target, "../") {
				return nil, fmt.Errorf("invalid_include: %q escapes the project", token)
			}
			if active[target] {
				return nil, fmt.Errorf("include_cycle: instruction include cycle reaches %q", target)
			}
			if _, exists := depth[target]; !exists {
				depth[target] = depth[current] + 1
				queue = append(queue, target)
			}
		}
		active[current] = true
	}
	return depth, nil
}

func scanInstructionIncludes(content []byte) []string {
	var includes []string
	lines := bytes.SplitAfter(content, []byte("\n"))
	var fence byte
	managed := false
	for _, raw := range lines {
		line := bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte("\n")), []byte("\r"))
		if bytes.HasPrefix(line, []byte("<!-- acr:begin ")) {
			managed = true
			continue
		}
		if bytes.HasPrefix(line, []byte("<!-- acr:end ")) {
			managed = false
			continue
		}
		if managed {
			continue
		}
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') && trimmed[1] == trimmed[0] && trimmed[2] == trimmed[0] {
			if fence == 0 {
				fence = trimmed[0]
			} else if fence == trimmed[0] {
				fence = 0
			}
			continue
		}
		if fence != 0 || len(trimmed) < 2 || trimmed[0] != '@' {
			continue
		}
		fields := bytes.Fields(trimmed)
		if len(fields) != 0 && len(fields[0]) > 1 {
			includes = append(includes, string(fields[0][1:]))
		}
	}
	return includes
}

func groupInstructionRoots(roots []instructionRoot) [][]instructionRoot {
	var groups [][]instructionRoot
	for _, root := range roots {
		matching := -1
		for index, group := range groups {
			if reachabilityIntersects(root.reachable, group[0].reachable) {
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

func reachabilityIntersects(left, right map[string]int) bool {
	for node := range left {
		if _, exists := right[node]; exists {
			return true
		}
	}
	return false
}

func deepestInstructionHost(group []instructionRoot) (string, string) {
	candidates := make(map[string]int)
	for node, depth := range group[0].reachable {
		candidates[node] = depth
	}
	for _, root := range group[1:] {
		for node, minimum := range candidates {
			depth, exists := root.reachable[node]
			if !exists {
				delete(candidates, node)
				continue
			}
			if depth < minimum {
				candidates[node] = depth
			}
		}
	}
	host := ""
	bestDepth := -1
	for candidate, depth := range candidates {
		if depth > bestDepth || depth == bestDepth && (host == "" || candidate < host) {
			host, bestDepth = candidate, depth
		}
	}
	owner := "claude-code"
	for _, root := range group {
		if root.adapterID == "codex" {
			if _, reaches := root.reachable[host]; reaches {
				owner = "codex"
				break
			}
		}
	}
	return host, owner
}

func sortedPackages(packages []Package) []Package {
	sorted := append([]Package(nil), packages...)
	sort.SliceStable(sorted, func(left, right int) bool { return sorted[left].Source < sorted[right].Source })
	return sorted
}

func renderRuleBundle(pkg Package) ([]byte, error) {
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
		body.Write(content)
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
