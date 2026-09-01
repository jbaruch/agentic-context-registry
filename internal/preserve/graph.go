// Package preserve implements preservation-safe compilation of shared agent
// instruction and configuration files.
package preserve

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	CodeUnresolvedInclude = "unresolved_include"
	CodeDuplicateInclude  = "duplicate_include"
	CodeIncludeCycle      = "include_cycle"
	CodeInvalidInclude    = "invalid_include"
)

// IncludeEdge records one recognized instruction include and its source line.
type IncludeEdge struct {
	From string
	To   string
	Line int
}

// Diagnostic is one deterministic include-graph finding. Chain contains the
// complete resolved path from a native root through the affected edge.
type Diagnostic struct {
	Code    string
	Path    string
	Line    int
	Message string
	Chain   []string
	nodes   []string
}

// IncludeGraph is the sorted graph rooted at every regular CLAUDE.md and
// AGENTS.md in a project. Graph findings remain available as warnings until a
// caller selects a root in the affected connected component.
type IncludeGraph struct {
	Roots       []string
	Edges       []IncludeEdge
	Diagnostics []Diagnostic
	adjacent    map[string][]IncludeEdge
}

// GraphError reports graph findings that affect selected roots.
type GraphError struct {
	Diagnostics []Diagnostic
}

func (err *GraphError) Error() string {
	parts := make([]string, 0, len(err.Diagnostics))
	for _, diagnostic := range err.Diagnostics {
		location := diagnostic.Path
		if diagnostic.Line != 0 {
			location = fmt.Sprintf("%s:%d", location, diagnostic.Line)
		}
		parts = append(parts, fmt.Sprintf("%s: %s: %s", diagnostic.Code, location, diagnostic.Message))
	}
	return strings.Join(parts, "; ")
}

type projectFile struct {
	content []byte
	mode    fs.FileMode
}

// DiscoverIncludeGraph walks projectRoot without following symlinks and
// builds its native instruction include graph. Structural findings are
// returned on the graph; filesystem failures return immediately.
func DiscoverIncludeGraph(projectRoot string) (*IncludeGraph, error) {
	files, roots, err := readProjectFiles(projectRoot)
	if err != nil {
		return nil, err
	}
	graph := &IncludeGraph{Roots: roots, adjacent: make(map[string][]IncludeEdge)}
	queue := append([]string(nil), roots...)
	queued := make(map[string]bool, len(queue))
	for _, root := range queue {
		queued[root] = true
	}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		file := files[current]
		if !utf8.Valid(file.content) {
			graph.addDiagnostic(Diagnostic{
				Code: CodeInvalidInclude, Path: current,
				Message: "instruction Markdown is not valid UTF-8",
				nodes:   []string{current},
			})
			continue
		}
		directives, scanDiagnostics := scanIncludeDirectives(current, file.content)
		for _, diagnostic := range scanDiagnostics {
			graph.addDiagnostic(diagnostic)
		}
		seenTargets := make(map[string]IncludeEdge)
		for _, directive := range directives {
			target, normalizeErr := normalizeIncludePath(current, directive.token)
			if normalizeErr != nil {
				graph.addDiagnostic(Diagnostic{
					Code: CodeInvalidInclude, Path: current, Line: directive.line,
					Message: normalizeErr.Error(), nodes: []string{current},
				})
				continue
			}
			edge := IncludeEdge{From: current, To: target, Line: directive.line}
			if first, duplicate := seenTargets[target]; duplicate {
				graph.addDiagnostic(Diagnostic{
					Code: CodeDuplicateInclude, Path: current, Line: directive.line,
					Message: fmt.Sprintf("include %q duplicates line %d", target, first.Line),
					nodes:   []string{current, target},
				})
				continue
			}
			seenTargets[target] = edge
			graph.Edges = append(graph.Edges, edge)
			graph.adjacent[current] = append(graph.adjacent[current], edge)
			targetFile, exists := files[target]
			if !exists || !targetFile.mode.IsRegular() {
				graph.addDiagnostic(Diagnostic{
					Code: CodeUnresolvedInclude, Path: current, Line: directive.line,
					Message: fmt.Sprintf("include %q does not resolve to a regular file", target),
					nodes:   []string{current, target},
				})
				continue
			}
			if !queued[target] {
				queued[target] = true
				queue = append(queue, target)
				sort.Strings(queue)
			}
		}
	}
	sort.Slice(graph.Edges, func(left, right int) bool {
		if graph.Edges[left].From != graph.Edges[right].From {
			return graph.Edges[left].From < graph.Edges[right].From
		}
		if graph.Edges[left].To != graph.Edges[right].To {
			return graph.Edges[left].To < graph.Edges[right].To
		}
		return graph.Edges[left].Line < graph.Edges[right].Line
	})
	for source := range graph.adjacent {
		sort.Slice(graph.adjacent[source], func(left, right int) bool {
			if graph.adjacent[source][left].To != graph.adjacent[source][right].To {
				return graph.adjacent[source][left].To < graph.adjacent[source][right].To
			}
			return graph.adjacent[source][left].Line < graph.adjacent[source][right].Line
		})
	}
	graph.findCycles()
	graph.findMultiplePaths()
	graph.attachChains()
	graph.sortDiagnostics()
	return graph, nil
}

func readProjectFiles(projectRoot string) (map[string]projectFile, []string, error) {
	files := make(map[string]projectFile)
	var roots []string
	err := filepath.WalkDir(projectRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == projectRoot {
			return nil
		}
		relative, err := filepath.Rel(projectRoot, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if relative == ".git" || strings.HasPrefix(relative, ".git/") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect project path %q: %w", relative, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read project path %q: %w", relative, err)
		}
		files[relative] = projectFile{content: content, mode: info.Mode()}
		base := path.Base(relative)
		if base == "CLAUDE.md" || base == "AGENTS.md" {
			roots = append(roots, relative)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walk project root %q: %w", projectRoot, err)
	}
	sort.Strings(roots)
	return files, roots, nil
}

type includeDirective struct {
	token string
	line  int
}

func scanIncludeDirectives(filename string, content []byte) ([]includeDirective, []Diagnostic) {
	lines := physicalLines(content)
	var directives []includeDirective
	var diagnostics []Diagnostic
	var fence byte
	var fenceWidth int
	managed := false
	for index, raw := range lines {
		line := trimPhysicalEOL(raw)
		trimmed := bytes.TrimLeft(line, " \t")
		if managed {
			if bytes.HasPrefix(line, []byte("<!-- acr:end ")) {
				managed = false
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("<!-- acr:begin ")) {
			managed = true
			continue
		}
		if marker, width := fenceMarker(trimmed); marker != 0 {
			if fence == 0 {
				fence, fenceWidth = marker, width
			} else if fence == marker && width >= fenceWidth {
				fence, fenceWidth = 0, 0
			}
			continue
		}
		if fence != 0 || len(trimmed) == 0 || trimmed[0] != '@' {
			continue
		}
		fields := bytes.Fields(trimmed)
		if len(fields) == 0 || len(fields[0]) == 1 {
			continue
		}
		directives = append(directives, includeDirective{token: string(fields[0][1:]), line: index + 1})
	}
	if managed {
		diagnostics = append(diagnostics, Diagnostic{
			Code: CodeInvalidInclude, Path: filename,
			Message: "unterminated ACR block while scanning includes", nodes: []string{filename},
		})
	}
	return directives, diagnostics
}

func physicalLines(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	var lines [][]byte
	for start := 0; start < len(content); {
		newline := bytes.IndexByte(content[start:], '\n')
		if newline < 0 {
			lines = append(lines, content[start:])
			break
		}
		end := start + newline + 1
		lines = append(lines, content[start:end])
		start = end
	}
	return lines
}

func trimPhysicalEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line
}

func fenceMarker(line []byte) (byte, int) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	marker := line[0]
	width := 0
	for width < len(line) && line[width] == marker {
		width++
	}
	if width < 3 {
		return 0, 0
	}
	return marker, width
}

func normalizeIncludePath(source, token string) (string, error) {
	if token == "" || strings.ContainsRune(token, '\x00') || strings.Contains(token, "\\") || strings.HasPrefix(token, "/") {
		return "", fmt.Errorf("include path %q must be normalized project-relative POSIX syntax", token)
	}
	if path.Clean(token) != token || token == "." || token == ".." || strings.HasPrefix(token, "../") {
		return "", fmt.Errorf("include path %q must be normalized project-relative POSIX syntax", token)
	}
	resolved := path.Clean(path.Join(path.Dir(source), token))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("include path %q escapes the project", token)
	}
	return resolved, nil
}

func (graph *IncludeGraph) addDiagnostic(diagnostic Diagnostic) {
	diagnostic.Chain = append([]string(nil), diagnostic.Chain...)
	diagnostic.nodes = append([]string(nil), diagnostic.nodes...)
	graph.Diagnostics = append(graph.Diagnostics, diagnostic)
}

func (graph *IncludeGraph) findCycles() {
	state := make(map[string]uint8)
	var stack []string
	var visit func(string)
	visit = func(node string) {
		state[node] = 1
		stack = append(stack, node)
		for _, edge := range graph.adjacent[node] {
			switch state[edge.To] {
			case 0:
				visit(edge.To)
			case 1:
				start := 0
				for start < len(stack) && stack[start] != edge.To {
					start++
				}
				cycle := append([]string(nil), stack[start:]...)
				cycle = append(cycle, edge.To)
				graph.addDiagnostic(Diagnostic{
					Code: CodeIncludeCycle, Path: edge.From, Line: edge.Line,
					Message: "include cycle: " + strings.Join(cycle, " -> "),
					Chain:   cycle, nodes: cycle,
				})
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = 2
	}
	for _, root := range graph.Roots {
		if state[root] == 0 {
			visit(root)
		}
	}
}

func (graph *IncludeGraph) findMultiplePaths() {
	for _, root := range graph.Roots {
		counts := map[string]int{root: 1}
		var walk func(string, map[string]bool)
		walk = func(node string, active map[string]bool) {
			if active[node] {
				return
			}
			nextActive := make(map[string]bool, len(active)+1)
			for key, value := range active {
				nextActive[key] = value
			}
			nextActive[node] = true
			for _, edge := range graph.adjacent[node] {
				counts[edge.To]++
				if counts[edge.To] <= 2 {
					walk(edge.To, nextActive)
				}
			}
		}
		walk(root, nil)
		var duplicates []string
		for node, count := range counts {
			if node != root && count > 1 {
				duplicates = append(duplicates, node)
			}
		}
		sort.Strings(duplicates)
		for _, node := range duplicates {
			chain := graph.shortestPath(root, node)
			graph.addDiagnostic(Diagnostic{
				Code: CodeDuplicateInclude, Path: root,
				Message: fmt.Sprintf("destination %q is reachable by more than one include path", node),
				Chain:   chain, nodes: append([]string{root}, node),
			})
		}
	}
}

func (graph *IncludeGraph) attachChains() {
	for index := range graph.Diagnostics {
		diagnostic := &graph.Diagnostics[index]
		if len(diagnostic.Chain) != 0 {
			continue
		}
		best := []string(nil)
		for _, root := range graph.Roots {
			chain := graph.shortestPath(root, diagnostic.Path)
			if len(chain) == 0 {
				continue
			}
			if len(best) == 0 || len(chain) < len(best) || (len(chain) == len(best) && strings.Join(chain, "\x00") < strings.Join(best, "\x00")) {
				best = chain
			}
		}
		if len(best) == 0 {
			best = []string{diagnostic.Path}
		}
		for _, node := range diagnostic.nodes {
			if len(best) == 0 || best[len(best)-1] != node {
				best = append(best, node)
			}
		}
		diagnostic.Chain = best
	}
}

func (graph *IncludeGraph) sortDiagnostics() {
	sort.Slice(graph.Diagnostics, func(left, right int) bool {
		if graph.Diagnostics[left].Code != graph.Diagnostics[right].Code {
			return graph.Diagnostics[left].Code < graph.Diagnostics[right].Code
		}
		if graph.Diagnostics[left].Path != graph.Diagnostics[right].Path {
			return graph.Diagnostics[left].Path < graph.Diagnostics[right].Path
		}
		if graph.Diagnostics[left].Line != graph.Diagnostics[right].Line {
			return graph.Diagnostics[left].Line < graph.Diagnostics[right].Line
		}
		return graph.Diagnostics[left].Message < graph.Diagnostics[right].Message
	})
}

func (graph *IncludeGraph) shortestPath(root, leaf string) []string {
	if root == leaf {
		return []string{root}
	}
	type route struct{ nodes []string }
	queue := []route{{nodes: []string{root}}}
	seen := map[string]bool{root: true}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		node := current.nodes[len(current.nodes)-1]
		for _, edge := range graph.adjacent[node] {
			if seen[edge.To] {
				continue
			}
			next := append(append([]string(nil), current.nodes...), edge.To)
			if edge.To == leaf {
				return next
			}
			seen[edge.To] = true
			queue = append(queue, route{nodes: next})
		}
	}
	return nil
}

// Reachable reports whether leaf is root or is transitively included by root.
func (graph *IncludeGraph) Reachable(root, leaf string) bool {
	return len(graph.shortestPath(root, leaf)) != 0
}

// ValidateSelected fails when any selected root belongs to a connected
// component carrying a graph diagnostic. Findings in untouched components
// remain available as warnings through Diagnostics.
func (graph *IncludeGraph) ValidateSelected(selectedRoots []string) error {
	selected := make(map[string]bool, len(selectedRoots))
	for _, root := range selectedRoots {
		selected[root] = true
	}
	var affected []Diagnostic
	for _, diagnostic := range graph.Diagnostics {
		for root := range selected {
			if graph.sameComponent(root, diagnostic.nodes) {
				affected = append(affected, diagnostic)
				break
			}
		}
	}
	if len(affected) == 0 {
		return nil
	}
	return &GraphError{Diagnostics: affected}
}

func (graph *IncludeGraph) sameComponent(root string, nodes []string) bool {
	if len(nodes) == 0 {
		return false
	}
	undirected := make(map[string][]string)
	for _, edge := range graph.Edges {
		undirected[edge.From] = append(undirected[edge.From], edge.To)
		undirected[edge.To] = append(undirected[edge.To], edge.From)
	}
	wanted := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		wanted[node] = true
	}
	queue := []string{root}
	seen := map[string]bool{root: true}
	for len(queue) != 0 {
		node := queue[0]
		queue = queue[1:]
		if wanted[node] {
			return true
		}
		for _, next := range undirected[node] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// DeepestSharedHost returns the deepest existing node reachable from every
// selected root. Ties use lexical path order.
func (graph *IncludeGraph) DeepestSharedHost(selectedRoots []string) (string, bool) {
	if len(selectedRoots) == 0 {
		return "", false
	}
	candidates := make(map[string]int)
	for _, root := range selectedRoots {
		queue := []string{root}
		depth := map[string]int{root: 0}
		for len(queue) != 0 {
			node := queue[0]
			queue = queue[1:]
			for _, edge := range graph.adjacent[node] {
				if _, exists := depth[edge.To]; exists {
					continue
				}
				depth[edge.To] = depth[node] + 1
				queue = append(queue, edge.To)
			}
		}
		if root == selectedRoots[0] {
			for node, nodeDepth := range depth {
				candidates[node] = nodeDepth
			}
			continue
		}
		for node := range candidates {
			nodeDepth, exists := depth[node]
			if !exists {
				delete(candidates, node)
				continue
			}
			if nodeDepth < candidates[node] {
				candidates[node] = nodeDepth
			}
		}
	}
	best := ""
	bestDepth := -1
	for candidate, depth := range candidates {
		if depth > bestDepth || (depth == bestDepth && (best == "" || candidate < best)) {
			best, bestDepth = candidate, depth
		}
	}
	return best, best != ""
}
