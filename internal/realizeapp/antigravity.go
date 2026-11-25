package realizeapp

import (
	"bytes"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

var antigravityInlineInclude = regexp.MustCompile(`@\[[^\]]*\]\([^)]+\)`)

func hasCarriedAntigravity(carried realize.Ledger) bool {
	for _, target := range carried.Targets {
		for _, entry := range target.Entries {
			if entry.Adapter == "antigravity" {
				return true
			}
		}
	}
	return false
}

// When Antigravity is selected, delegated rules must be produced in this run.
// Carried ownership can cause duplicate delivery but cannot prove current delivery.
func verifyAntigravitySurfaces(packages []adapter.Package, intents []realize.Intent, scoped, carried realize.Ledger, snapshot adapter.Snapshot, selectedAntigravity, temporarySelection bool) error {
	// A companion-only run need not produce artifacts for Antigravity, but
	// must keep delivering rules whose root host this run owns.
	var scopedRootEntries []realize.Entry
	if !selectedAntigravity {
		if i := slices.IndexFunc(scoped.Targets, func(target realize.Target) bool { return target.Path == "AGENTS.md" }); i >= 0 {
			scopedRootEntries = scoped.Targets[i].Entries
		}
	}
	hosts := map[string][]string{}
	skills := map[string][]byte{}
	rootRules := map[string]bool{}
	cursorRules := map[string]bool{}
	var plannedRoot *realize.Intent
	var cursorHooks, claudeHooks, nativeAntigravity bool
	observe := func(target string, entries []realize.Entry, planned bool) {
		for _, e := range entries {
			switch {
			case e.ArtifactKind == realize.ArtifactManagedBlock:
				if planned {
					hosts[e.Source] = append(hosts[e.Source], target)
				}
				if target == "AGENTS.md" {
					rootRules[e.Source] = true
				}
			case strings.HasPrefix(target, ".cursor/rules/"):
				cursorRules[e.Source] = true
			case planned && e.Adapter == "antigravity":
				nativeAntigravity = true
			case e.Source == freshness.Source && e.ArtifactID == freshness.ArtifactID && e.SourcePath == freshness.SourcePath:
				// The built-in hook shares a project lock and throttle across adapters.
			case strings.HasPrefix(target, ".cursor/hooks/acr__") && e.ArtifactKind == realize.ArtifactFile:
				cursorHooks = true
			case target == ".claude/settings.json" && e.ArtifactKind == realize.ArtifactStructuredEntry:
				claudeHooks = true
			}
		}
	}
	for _, i := range intents {
		if i.Action == realize.ActionRemove {
			continue
		}
		observe(i.Path, i.Entries, true)
		if i.Path == "AGENTS.md" {
			plannedRoot = &i
		}
		if i.Owner == realize.OwnerCoordinator && path.Base(i.Path) == "SKILL.md" && path.Dir(path.Dir(i.Path)) == realize.SharedSurfaceRoot {
			skills[i.Path] = i.Content
		}
	}
	for _, t := range carried.Targets {
		observe(t.Path, t.Entries, false)
	}
	for _, p := range packages {
		if selectedAntigravity {
			for _, skill := range p.Manifest.Artifacts.Skills {
				name, err := adapter.NativeArtifactName(p.Source, skill.ID)
				if err != nil {
					return err
				}
				sourcePath := path.Join(skill.Path, "SKILL.md")
				content, found := skills[path.Join(realize.SharedSurfaceRoot, name, "SKILL.md")]
				if !found {
					return fmt.Errorf("Antigravity cannot load %s skill %q: include %s with name and description", p.Source, skill.ID, sourcePath)
				}
				header, _, err := adapter.ParseFrontmatter(content, sourcePath)
				if err != nil {
					return fmt.Errorf("Antigravity cannot load skill from %s: %w; fix the YAML frontmatter in %s", p.Source, err, sourcePath)
				}
				for _, field := range []string{"name", "description"} {
					value, ok := header[field].(string)
					if !ok || strings.TrimSpace(value) == "" {
						return fmt.Errorf("Antigravity cannot load skill from %s: set %s in %s to a nonempty YAML string", p.Source, field, sourcePath)
					}
				}
			}
		}
		needsRoot := selectedAntigravity || slices.ContainsFunc(scopedRootEntries, func(entry realize.Entry) bool {
			return entry.ArtifactKind == realize.ArtifactManagedBlock && entry.Source == p.Source
		})
		if needsRoot && len(p.Manifest.Artifacts.Rules) > 0 && !slices.Contains(hosts[p.Source], "AGENTS.md") {
			if slices.Contains(hosts[p.Source], "AGENTS.override.md") {
				return fmt.Errorf("Antigravity reads package rules from root AGENTS.md. Create a regular root AGENTS.md if missing. Add @AGENTS.md to AGENTS.override.md, then run acr realize --dry-run")
			}
			redirected := slices.ContainsFunc(hosts[p.Source], func(host string) bool { return host != "CLAUDE.md" && host != ".claude/CLAUDE.md" })
			if redirected {
				return fmt.Errorf("Antigravity rules from %s are redirected to %q. Copy any instructions that must still load automatically into root AGENTS.md, creating it if missing. For Claude Code, replace the standalone @imports in CLAUDE.md with @AGENTS.md, or in .claude/CLAUDE.md with @../AGENTS.md. If root AGENTS.md has a standalone @include that redirects package rules, replace it with an ordinary Markdown link. Then run acr realize --dry-run", p.Source, hosts[p.Source])
			}
			return fmt.Errorf("Antigravity rules from %s are planned in %q; select Codex with a root AGENTS.md host or configure Claude to share AGENTS.md", p.Source, hosts[p.Source])
		}
		artifacts := p.Manifest.Artifacts
		if temporarySelection && !nativeAntigravity && len(artifacts.Rules)+len(artifacts.Skills)+len(artifacts.Scripts) > 0 {
			return fmt.Errorf("temporary Antigravity selection has no native hook ownership to record its delegated artifacts. Add antigravity to agents.yaml and run acr realize without --agent")
		}
	}
	if len(rootRules) != 0 {
		var content []byte
		if plannedRoot != nil {
			content = plannedRoot.Content
		} else {
			file, err := snapshot.ReadFile("AGENTS.md")
			if err != nil {
				return fmt.Errorf("inspect Antigravity rule host AGENTS.md: %w", err)
			}
			content = file.Content
		}
		const ruleFileLimit = 24_000
		if match := antigravityInlineInclude.FindIndex(content); match != nil {
			line := bytes.Count(content[:match[0]], []byte{'\n'}) + 1
			return fmt.Errorf("AGENTS.md:%d: ACR cannot verify Antigravity's %d-byte rule limit after expanding @[label](path). Replace the inline include with an ordinary Markdown link or move it to a separate native rule file, then run acr realize --dry-run. If the include comes from a package rule, update that package first", line, ruleFileLimit)
		}
		if size := len(content); size > ruleFileLimit {
			return fmt.Errorf("Antigravity would truncate AGENTS.md (%d bytes; limit %d bytes). Preserve existing instructions in a separate document and shorten root AGENTS.md or the package rules, then run acr realize --dry-run", size, ruleFileLimit)
		}
	}
	for _, source := range slices.Sorted(maps.Keys(rootRules)) {
		if cursorRules[source] {
			return fmt.Errorf("Cursor would load %s rules from AGENTS.md and .cursor/rules; remove Cursor from agents.yaml and run the full configured realization", source)
		}
	}
	if cursorHooks && claudeHooks {
		return fmt.Errorf("Cursor would read both .cursor/hooks.json and Claude imports from .claude/settings.json; remove Cursor or Claude Code from agents.yaml and run the full configured realization")
	}
	return nil
}
