package migrate

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
)

const (
	tesslManagedMarker     = "<!-- tessl-managed -->"
	reasonUnmanagedPrefix  = "unmanaged-prefix"
	reasonUnmanagedHook    = "unmanaged-hook"
	reasonUnmanagedConfig  = "unmanaged-config"
	reasonUnmanagedSkill   = "unmanaged-skill-file"
	reasonTesslSpanExtra   = "tessl-span-extra"
	reasonTesslIndex       = "tessl-index"
	reasonTesslGitignore   = "tessl-gitignore"
	reasonTesslPackage     = "tessl-package"
	reasonUndeclaredPlugin = "undeclared-plugin-file"
	reasonPluginSymlink    = "plugin-symlink"
	reasonOrphanNative     = "orphan-tessl-native"
	reasonMCPServer        = "mcp-server"
	gitignoreBeginPrefix   = "# === Tessl-generated artifacts (managed by "
	gitignoreEnd           = "# === end Tessl-generated artifacts ==="
)

var mcpPaths = []string{
	".mcp.json",
	".cursor/mcp.json",
	".vscode/mcp.json",
	".github/mcp.json",
	".gemini/settings.json",
}

var tesslAgentTrees = []struct {
	id      string
	covered bool
	files   []string
	dirs    []string
}{
	{id: "claude-code", covered: true, files: []string{".claude/settings.json"}, dirs: []string{".claude/skills"}},
	{id: "codex", covered: true, files: []string{".codex/config.toml"}, dirs: []string{".codex/skills"}},
	{id: "cursor", covered: true, files: []string{".cursor/hooks.json"}, dirs: []string{".cursor/rules", ".cursor/skills"}},
	{id: "gemini", covered: false, files: []string{".gemini/settings.json"}, dirs: []string{".gemini/skills"}},
	{id: "github", covered: false, files: []string{".github/hooks/tessl.json"}, dirs: []string{".github/skills"}},
	{id: "vscode", covered: false, files: []string{".vscode/mcp.json"}, dirs: []string{".vscode/skills"}},
	{id: "agents", covered: false, dirs: []string{".agents/skills"}},
}

var nativeConfigFiles = []string{
	".claude/settings.json",
	".claude/settings.local.json",
	".cursor/hooks.json",
	".gemini/settings.json",
	".github/hooks/tessl.json",
	".codex/config.toml",
}

func classifyProject(snapshot adapter.Snapshot, installs []PackageInstall, extras []string, report *Report) error {
	if err := classifyAgents(snapshot, report); err != nil {
		return err
	}
	if err := classifyInstructionFiles(snapshot, installs, report); err != nil {
		return err
	}
	if err := classifyNativeConfigs(snapshot, report); err != nil {
		return err
	}
	if err := classifyMCP(snapshot, report); err != nil {
		return err
	}
	if err := classifyGitignore(snapshot, report); err != nil {
		return err
	}
	if _, present, err := readOptional(snapshot, rulesIndexPath); err != nil {
		return err
	} else if present {
		report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: rulesIndexPath, Reason: reasonTesslIndex})
	}
	if err := classifyPluginTrees(snapshot, installs, report); err != nil {
		return err
	}
	for _, extra := range extras {
		report.Preserved = appendUnique(report.Preserved, PathRecord{Path: extra, Reason: reasonUnmanagedSkill})
	}
	return classifyOrphanNatives(snapshot, report)
}

func classifyAgents(snapshot adapter.Snapshot, report *Report) error {
	for _, spec := range tesslAgentTrees {
		evidence, err := adapter.ExistingEvidence(snapshot, spec.files, spec.dirs)
		if err != nil {
			return err
		}
		if len(evidence) == 0 {
			continue
		}
		report.Agents = append(report.Agents, AgentCoverage{ID: spec.id, Covered: spec.covered, Evidence: evidence})
	}
	return nil
}

func classifyInstructionFiles(snapshot adapter.Snapshot, installs []PackageInstall, report *Report) error {
	paths := map[string]struct{}{"AGENTS.md": {}, "CLAUDE.md": {}, "GEMINI.md": {}}
	graph, err := preserve.DiscoverIncludeGraphSnapshot(snapshot, rulesIndexPath)
	if err != nil {
		return err
	}
	for _, root := range graph.Roots {
		paths[root] = struct{}{}
	}
	for _, edge := range graph.Edges {
		paths[edge.From] = struct{}{}
		paths[edge.To] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for filename := range paths {
		if tesslOwnedInstruction(filename, installs) {
			continue
		}
		ordered = append(ordered, filename)
	}
	sort.Strings(ordered)
	for _, filename := range ordered {
		content, present, err := readOptional(snapshot, filename)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		classifyMarkdown(filename, content, report)
	}
	return nil
}

func tesslOwnedInstruction(filename string, installs []PackageInstall) bool {
	if filename == rulesIndexPath {
		return true
	}
	for _, install := range installs {
		root := strings.TrimSuffix(install.Root, "/")
		if filename == root || strings.HasPrefix(filename, root+"/") {
			return true
		}
	}
	return false
}

func classifyMarkdown(filename string, content []byte, report *Report) {
	spans := tesslManagedSpans(content)
	if len(spans) == 0 {
		if hasNonManagedContent(content, nil) {
			report.Preserved = appendUnique(report.Preserved, PathRecord{Path: filename, Reason: reasonUnmanagedPrefix})
		}
		return
	}
	if hasNonManagedContent(content, spans) {
		report.Preserved = appendUnique(report.Preserved, PathRecord{Path: filename, Reason: reasonUnmanagedPrefix})
	}
	for _, span := range spans {
		if tesslSpanHasExtra(content[span.start:span.end]) {
			report.Ambiguous = appendUnique(report.Ambiguous, PathRecord{Path: filename, Reason: reasonTesslSpanExtra})
		}
	}
}

type byteSpan struct {
	start int
	end   int
}

func tesslManagedSpans(content []byte) []byteSpan {
	lines := physicalLines(content)
	var spans []byteSpan
	for index, line := range lines {
		text := content[line.start:line.contentEnd]
		if headingLevel(text) == 0 || !bytes.Contains(text, []byte(tesslManagedMarker)) {
			continue
		}
		level := headingLevel(text)
		end := len(content)
		for next := index + 1; next < len(lines); next++ {
			nextLevel := headingLevel(content[lines[next].start:lines[next].contentEnd])
			if nextLevel != 0 && nextLevel <= level {
				end = lines[next].start
				break
			}
		}
		spans = append(spans, byteSpan{start: line.start, end: end})
	}
	return spans
}

func headingLevel(line []byte) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

func hasNonManagedContent(content []byte, spans []byteSpan) bool {
	covered := make([]bool, len(content))
	for _, span := range spans {
		for i := span.start; i < span.end && i < len(content); i++ {
			covered[i] = true
		}
	}
	for i, b := range content {
		if covered[i] {
			continue
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		return true
	}
	return false
}

func tesslSpanHasExtra(span []byte) bool {
	lines := physicalLines(span)
	for index, line := range lines {
		text := bytes.TrimSpace(span[line.start:line.contentEnd])
		if len(text) == 0 {
			continue
		}
		if index == 0 && headingLevel(span[line.start:line.contentEnd]) != 0 {
			continue
		}
		if len(text) > 0 && text[0] == '@' {
			continue
		}
		return true
	}
	return false
}

func classifyNativeConfigs(snapshot adapter.Snapshot, report *Report) error {
	for _, filename := range nativeConfigFiles {
		content, present, err := readOptional(snapshot, filename)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if path.Base(filename) == "settings.local.json" {
			report.Preserved = appendUnique(report.Preserved, PathRecord{Path: filename, Reason: reasonUnmanagedConfig})
			continue
		}
		if strings.HasSuffix(filename, ".toml") {
			if userHookInTOML(content) {
				report.Preserved = appendUnique(report.Preserved, PathRecord{Path: filename, Reason: reasonUnmanagedHook})
			}
			if tomlHasMCP(content) {
				report.Unsupported = appendUnique(report.Unsupported, PathRecord{Path: filename, Reason: reasonMCPServer})
			}
			continue
		}
		if userHookInJSON(content) {
			report.Preserved = appendUnique(report.Preserved, PathRecord{Path: filename, Reason: reasonUnmanagedHook})
		}
	}
	return nil
}

func userHookInJSON(content []byte) bool {
	var document map[string]any
	if err := json.Unmarshal(content, &document); err != nil {
		return false
	}
	return findUserCommand(document["hooks"])
}

func findUserCommand(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if command, ok := typed["command"].(string); ok && !isTesslCommand(command) {
			return true
		}
		for _, child := range typed {
			if findUserCommand(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if findUserCommand(child) {
				return true
			}
		}
	}
	return false
}

func userHookInTOML(content []byte) bool {
	var document map[string]any
	if err := toml.Unmarshal(content, &document); err != nil {
		return false
	}
	return findUserCommand(document["hooks"])
}

func tomlHasMCP(content []byte) bool {
	var document map[string]any
	if err := toml.Unmarshal(content, &document); err != nil {
		return false
	}
	_, ok := document["mcp_servers"]
	return ok
}

func isTesslCommand(command string) bool {
	// Design note §1 / docs/migration.md:33: native ownership is the
	// dispatcher literal at the head of the command. ${TESSL_PLUGIN_DIR}
	// is the plugin.json grammar (note §1 "Manifest-side",
	// docs/migration.md:36), not a native marker.
	trimmed := strings.TrimLeftFunc(command, unicode.IsSpace)
	rest, found := strings.CutPrefix(trimmed, "tessl hook run")
	if !found {
		return false
	}
	if rest == "" {
		return true
	}
	first, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(first)
}

func classifyMCP(snapshot adapter.Snapshot, report *Report) error {
	for _, filename := range mcpPaths {
		_, present, err := readOptional(snapshot, filename)
		if err != nil {
			return err
		}
		if present {
			report.Unsupported = appendUnique(report.Unsupported, PathRecord{Path: filename, Reason: reasonMCPServer})
		}
	}
	return nil
}

func classifyGitignore(snapshot adapter.Snapshot, report *Report) error {
	content, present, err := readOptional(snapshot, ".gitignore")
	if err != nil || !present {
		return err
	}
	if bytes.Contains(content, []byte(gitignoreBeginPrefix)) && bytes.Contains(content, []byte(gitignoreEnd)) {
		report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: ".gitignore", Reason: reasonTesslGitignore})
	}
	return nil
}

func classifyPluginTrees(snapshot adapter.Snapshot, installs []PackageInstall, report *Report) error {
	directories, err := directorySnapshot(snapshot)
	if err != nil {
		return err
	}
	owned := make(map[string]struct{})
	for _, install := range installs {
		owned[posixJoin(install.Root, pluginManifestRel)] = struct{}{}
		owned[posixJoin(install.Root, tileManifestName)] = struct{}{}
		packageFile := posixJoin(install.Root, "tessl-package.json")
		if _, present, err := readOptional(snapshot, packageFile); err != nil {
			return err
		} else if present {
			report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: packageFile, Reason: reasonTesslPackage})
			owned[packageFile] = struct{}{}
		}
		for _, rule := range install.Rules {
			owned[posixJoin(install.Root, rule.Path)] = struct{}{}
		}
		for _, skill := range install.Skills {
			owned[posixJoin(install.Root, skill.Path)+"/"] = struct{}{}
		}
		for _, hook := range install.Hooks {
			relpath, ok := hookRelPath(hook.Command, hook.Args)
			if ok {
				owned[posixJoin(install.Root, relpath)] = struct{}{}
			}
		}
		entries, walkErr := adapter.WalkSnapshot(directories, install.Root)
		if walkErr != nil {
			return walkErr
		}
		for _, entry := range entries {
			if entry.Mode&fs.ModeSymlink != 0 {
				report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: entry.Path, Reason: reasonPluginSymlink})
				continue
			}
			if !entry.Mode.IsRegular() {
				continue
			}
			if pluginOwned(entry.Path, owned) {
				continue
			}
			report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: entry.Path, Reason: reasonUndeclaredPlugin})
		}
	}
	return nil
}

func pluginOwned(filename string, owned map[string]struct{}) bool {
	if _, ok := owned[filename]; ok {
		return true
	}
	for prefix := range owned {
		if strings.HasSuffix(prefix, "/") && strings.HasPrefix(filename, prefix) {
			return true
		}
	}
	return false
}

func markDuplicateSkills(report *Report) {
	claimed := make(map[string][]int)
	for packageIndex, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			if artifact.Kind != kindSkill {
				continue
			}
			claimed[artifact.ID] = append(claimed[artifact.ID], packageIndex)
		}
	}
	for id, packages := range claimed {
		if len(packages) < 2 {
			continue
		}
		for _, packageIndex := range packages {
			artifacts := report.Packages[packageIndex].Artifacts
			for index, artifact := range artifacts {
				if artifact.Kind == kindSkill && artifact.ID == id && artifact.Classification == classMigratable {
					artifacts[index].Classification = classAmbiguous
				}
			}
			report.Packages[packageIndex].Artifacts = artifacts
		}
	}
}

func classifyOrphanNatives(snapshot adapter.Snapshot, report *Report) error {
	directories, err := directorySnapshot(snapshot)
	if err != nil {
		return err
	}
	skillIDs, rulePaths := claimedTesslNatives(report)
	for _, root := range tesslNativeRoots() {
		entries, readErr := readDir(directories, root.dir)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			base := path.Base(entry.Path)
			if !strings.HasPrefix(base, root.prefix) {
				continue
			}
			if _, claimed := rulePaths[entry.Path]; claimed {
				continue
			}
			if _, claimed := skillIDs[strings.TrimPrefix(base, root.prefix)]; claimed {
				continue
			}
			if unmapErr := unmapOrphanNative(directories, entry, report); unmapErr != nil {
				return unmapErr
			}
		}
	}
	return nil
}

func claimedTesslNatives(report *Report) (skillIDs, rulePaths map[string]struct{}) {
	skillIDs = make(map[string]struct{})
	rulePaths = make(map[string]struct{})
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			switch artifact.Kind {
			case kindSkill:
				skillIDs[artifact.ID] = struct{}{}
			case kindRule:
				rulePaths[cursorRuleNative(pkg.TesslIdentity, artifact.ID)] = struct{}{}
			}
		}
	}
	return skillIDs, rulePaths
}

func tesslNativeRoots() []struct {
	dir    string
	prefix string
} {
	seen := make(map[string]struct{}, len(skillNativeDirs)+len(tesslAgentTrees)+1)
	var roots []struct {
		dir    string
		prefix string
	}
	add := func(dir, prefix string) {
		if _, ok := seen[dir]; ok {
			return
		}
		seen[dir] = struct{}{}
		roots = append(roots, struct {
			dir    string
			prefix string
		}{dir: dir, prefix: prefix})
	}
	add(".cursor/rules", "tessl__")
	for _, native := range skillNativeDirs {
		add(native.dir, native.prefix)
	}
	for _, spec := range tesslAgentTrees {
		for _, dir := range spec.dirs {
			add(dir, "tessl__")
		}
	}
	sort.Slice(roots, func(left, right int) bool { return roots[left].dir < roots[right].dir })
	return roots
}

func unmapOrphanNative(snapshot adapter.DirectorySnapshot, entry adapter.ObservedEntry, report *Report) error {
	if entry.Mode&fs.ModeSymlink != 0 || !entry.Mode.IsDir() {
		report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: entry.Path, Reason: reasonOrphanNative})
		return nil
	}
	children, err := adapter.WalkSnapshot(snapshot, entry.Path)
	if err != nil {
		return err
	}
	found := false
	for _, child := range children {
		if child.Mode.IsDir() && child.Mode&fs.ModeSymlink == 0 {
			continue
		}
		report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: child.Path, Reason: reasonOrphanNative})
		found = true
	}
	if !found {
		report.Unmapped = appendUnique(report.Unmapped, PathRecord{Path: entry.Path, Reason: reasonOrphanNative})
	}
	return nil
}
