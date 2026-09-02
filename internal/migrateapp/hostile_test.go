package migrateapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

// TestHostileEndToEndBothManifestShapesOnAReadOnlyTree is the command-level
// no-writes proof over a consumer built from both manifest shapes with native
// trees for three agents. The whole tree is read-only for the run and every
// path is hashed with its mode and symlink target before and after.
func TestHostileEndToEndBothManifestShapesOnAReadOnlyTree(t *testing.T) {
	t.Parallel()

	root := seedDualManifestConsumer(t)
	chmodTree(t, root, 0o555, 0o444)
	t.Cleanup(func() { chmodTree(t, root, 0o755, 0o644) })
	before := hashTreeWithModes(t, root)

	textOut, textErr, textExit := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--project", root)
	if textExit != cli.ExitSuccess || textErr != "" {
		t.Fatalf("text run exit = %d stderr = %q stdout = %q", textExit, textErr, textOut)
	}
	jsonOut, jsonErr, jsonExit := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--json", "--project", root)
	if jsonExit != cli.ExitSuccess || jsonErr != "" {
		t.Fatalf("json run exit = %d stderr = %q stdout = %q", jsonExit, jsonErr, jsonOut)
	}

	after := hashTreeWithModes(t, root)
	if !mapsEqual(before, after) {
		t.Fatalf("read-only inventory mutated the project tree\nbefore=%v\nafter=%v", before, after)
	}

	report := decodeReport(t, jsonOut)
	if !report.DryRun || report.Wrote || report.SchemaVersion != 1 {
		t.Fatalf("report envelope = %+v", report)
	}
	names := map[string]string{}
	for _, pkg := range report.Packages {
		names[pkg.Name] = pkg.Manifest
	}
	if names["example/alpha"] != "plugin.json" {
		t.Fatalf("plugin.json package = %q; packages = %#v", names["example/alpha"], report.Packages)
	}
	if names["example/beta"] != "tile.json" {
		t.Fatalf("legacy tile.json package = %q; packages = %#v", names["example/beta"], report.Packages)
	}
	for _, want := range []string{"Package example/alpha", "Package example/beta", "Preserved", "Unmapped", "Unsupported"} {
		if !strings.Contains(textOut, want) {
			t.Errorf("text output missing %q:\n%s", want, textOut)
		}
	}
}

// TestHostileJSONOutputIsPure checks the --json contract the report exists to
// satisfy: exactly one envelope line on stdout, nothing on stderr, byte-stable
// across runs, and no host path or absolute path anywhere in the payload.
func TestHostileJSONOutputIsPure(t *testing.T) {
	t.Parallel()

	root := seedDualManifestConsumer(t)
	first, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--json", "--project", root)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("exit = %d stderr = %q", exitCode, stderr)
	}
	second, _, _ := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--json", "--project", root)
	if first != second {
		t.Fatalf("json output is not byte-stable\nfirst  = %s\nsecond = %s", first, second)
	}
	if strings.Count(first, "\n") != 1 || !strings.HasSuffix(first, "\n") {
		t.Fatalf("stdout must be one envelope line, got %q", first)
	}
	if strings.Contains(first, root) {
		t.Fatalf("json leaked the host project path %q: %s", root, first)
	}
	report := decodeReport(t, first)
	for _, path := range reportPaths(report) {
		if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
			t.Errorf("report path %q is not a relative POSIX path", path)
		}
	}
}

// TestHostileApplyWithoutDryRunIsInertOnAReadOnlyTree pairs the not_implemented
// contract with the no-writes proof: the refusal must not open a writer, and it
// must not leave a partial report on stdout.
func TestHostileApplyWithoutDryRunIsInertOnAReadOnlyTree(t *testing.T) {
	t.Parallel()

	root := seedDualManifestConsumer(t)
	chmodTree(t, root, 0o555, 0o444)
	t.Cleanup(func() { chmodTree(t, root, 0o755, 0o644) })
	before := hashTreeWithModes(t, root)

	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--project", root, "--json")
	if exitCode != cli.ExitOperational {
		t.Fatalf("apply exit = %d, want %d; stdout = %q stderr = %q", exitCode, cli.ExitOperational, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("apply stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `"code":"not_implemented"`) {
		t.Fatalf("apply stderr = %q", stderr)
	}
	if after := hashTreeWithModes(t, root); !mapsEqual(before, after) {
		t.Fatalf("apply refusal mutated the tree\nbefore=%v\nafter=%v", before, after)
	}
}

type hostileReport struct {
	SchemaVersion int `json:"schemaVersion"`
	DryRun        bool
	Wrote         bool
	Agents        []struct {
		ID       string   `json:"id"`
		Evidence []string `json:"evidence"`
	} `json:"agents"`
	Packages []struct {
		Name      string `json:"name"`
		Manifest  string `json:"manifest"`
		Artifacts []struct {
			ID             string   `json:"id"`
			Kind           string   `json:"kind"`
			Classification string   `json:"classification"`
			Natives        []string `json:"natives"`
		} `json:"artifacts"`
	} `json:"packages"`
	Preserved   []hostilePathRecord `json:"preserved"`
	Unmapped    []hostilePathRecord `json:"unmapped"`
	Ambiguous   []hostilePathRecord `json:"ambiguous"`
	Unsupported []hostilePathRecord `json:"unsupported"`
}

type hostilePathRecord struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func decodeReport(t *testing.T, stdout string) hostileReport {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", stdout, err)
	}
	if !envelope.OK {
		t.Fatalf("envelope not ok: %s", stdout)
	}
	var report hostileReport
	if err := json.Unmarshal(envelope.Result, &report); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return report
}

func reportPaths(report hostileReport) []string {
	var paths []string
	for _, agent := range report.Agents {
		paths = append(paths, agent.Evidence...)
	}
	for _, pkg := range report.Packages {
		for _, artifact := range pkg.Artifacts {
			paths = append(paths, artifact.Natives...)
		}
	}
	for _, records := range [][]hostilePathRecord{report.Preserved, report.Unmapped, report.Ambiguous, report.Unsupported} {
		for _, record := range records {
			paths = append(paths, record.Path)
		}
	}
	return paths
}

func seedDualManifestConsumer(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeJSON(t, root, "tessl.json", map[string]any{
		"name": "consumer",
		"mode": "vendored",
		"dependencies": map[string]any{
			"example/alpha": map[string]string{"version": "1.0.0"},
			"example/beta":  map[string]string{"version": "2.0.0"},
		},
	})

	writeJSON(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"rules":   []string{"rules/always-rule.md", "rules/paths-rule.md"},
		"skills":  []string{"skills/review-change"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
			}}}},
		},
		"nativeHooks": map[string]any{
			"claude-code": map[string]any{
				"Stop": []any{map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/stop.sh"},
				}}}},
			},
		},
	})
	alwaysRule := []byte("---\nalwaysApply: true\n---\n# Always\n")
	writeFile(t, root, ".tessl/plugins/example/alpha/rules/always-rule.md", alwaysRule, 0o644)
	writeFile(t, root, ".tessl/plugins/example/alpha/rules/paths-rule.md",
		[]byte("---\nalwaysApply: false\napplyTo: \"*.go — Go files\"\n---\n# Paths\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/alpha/skills/review-change/SKILL.md", []byte("# Review\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/alpha/hooks/session-start.sh", []byte("#!/bin/sh\necho start\n"), 0o755)
	writeFile(t, root, ".tessl/plugins/example/alpha/hooks/stop.sh", []byte("#!/bin/sh\necho stop\n"), 0o755)
	writeFile(t, root, ".tessl/plugins/example/alpha/tessl-package.json", []byte(`{"name":"example/alpha"}`+"\n"), 0o644)

	writeJSON(t, root, ".tessl/plugins/example/beta/tile.json", map[string]any{
		"name":    "example/beta",
		"version": "2.0.0",
		"rules":   map[string]any{"legacy-rule": map[string]string{"rules": "rules/legacy-rule.md"}},
		"skills":  map[string]any{"legacy-skill": map[string]string{"path": "skills/legacy-skill/SKILL.md"}},
	})
	writeFile(t, root, ".tessl/plugins/example/beta/rules/legacy-rule.md",
		[]byte("---\nalwaysApply: true\n---\n# Legacy\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/beta/skills/legacy-skill/SKILL.md", []byte("# Legacy\n"), 0o644)

	writeFile(t, root, ".tessl/RULES.md",
		[]byte("# Agent Rules\n\n## always\n\n@plugins/example/alpha/rules/always-rule.md\n"), 0o644)
	writeFile(t, root, "AGENTS.md",
		[]byte("# User title\n\nUser prose lives here.\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n"), 0o644)
	writeFile(t, root, "CLAUDE.md", []byte("# Claude notes\n\n@AGENTS.md\n"), 0o644)
	writeFile(t, root, ".gitignore",
		[]byte("*.log\n# === Tessl-generated artifacts (managed by example/alpha) ===\n.tessl/\n# === end Tessl-generated artifacts ===\n"), 0o644)

	writeJSON(t, root, ".claude/settings.json", map[string]any{
		"hooks": map[string]any{"SessionStart": []any{
			map[string]any{"hooks": []any{map[string]any{
				"type":    "command",
				"command": `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="SessionStart" --agent=claude-code --schema-version=1`,
			}}},
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "user-hook.sh"}}},
		}},
	})
	writeFile(t, root, ".claude/settings.local.json", []byte(`{"permissions":{}}`+"\n"), 0o644)
	writeJSON(t, root, ".cursor/hooks.json", map[string]any{
		"version": 1,
		"hooks": map[string]any{"sessionStart": []any{
			map[string]any{"command": `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="sessionStart" --agent=cursor --schema-version=1`},
		}},
	})
	writeFile(t, root, ".codex/config.toml", []byte("[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"tessl hook run --plugin-path=\\\".tessl/plugins/example/alpha\\\" --event=\\\"SessionStart\\\" --agent=codex --schema-version=1\"\n"), 0o644)
	writeJSON(t, root, ".cursor/mcp.json", map[string]any{"mcpServers": map[string]any{"tessl": map[string]any{}}})
	writeFile(t, root, ".cursor/rules/tessl__rule__example__alpha__always-rule.mdc",
		append([]byte("---\nalwaysApply: true\n---\n\n"), alwaysRule...), 0o644)

	linkSkill(t, root, ".claude/skills", "example/alpha", "review-change")
	linkSkill(t, root, ".codex/skills", "example/alpha", "review-change")
	linkSkill(t, root, ".cursor/skills", "example/alpha", "review-change")
	return root
}

func linkSkill(t *testing.T, root, nativeDir, identity, id string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(".tessl/plugins/"+identity+"/skills/"+id))
	native := filepath.Join(root, filepath.FromSlash(nativeDir), "tessl__"+id)
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(filepath.Dir(native), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, native); err != nil {
		t.Fatal(err)
	}
}

func hashTreeWithModes(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			result[relative] = "link→" + filepath.ToSlash(target)
			return nil
		}
		if entry.IsDir() {
			result[relative] = "dir " + info.Mode().Perm().String()
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		result[relative] = info.Mode().Perm().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
