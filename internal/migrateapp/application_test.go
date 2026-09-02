package migrateapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func TestMigrateTesslDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	root := seedConsumer(t)
	chmodTree(t, root, 0o555, 0o555)
	t.Cleanup(func() { chmodTree(t, root, 0o755, 0o644) })
	before := hashTree(t, root)

	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--project", root)
	if exitCode != cli.ExitSuccess {
		t.Fatalf("dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("dry-run stderr = %q", stderr)
	}
	if !strings.Contains(stdout, "Tessl inventory") {
		t.Fatalf("dry-run stdout = %q", stdout)
	}
	after := hashTree(t, root)
	if !mapsEqual(before, after) {
		t.Fatalf("dry-run mutated the project tree\nbefore=%v\nafter=%v", before, after)
	}
}

func TestApplyWithoutDryRunDoesNotWrite(t *testing.T) {
	t.Parallel()

	root := seedConsumer(t)
	before := hashTree(t, root)
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
	after := hashTree(t, root)
	if !mapsEqual(before, after) {
		t.Fatalf("apply without --dry-run mutated the project\nbefore=%v\nafter=%v", before, after)
	}
}

func TestMigrateTesslInventoryErrorSurfaces(t *testing.T) {
	t.Parallel()

	root := seedConsumer(t)
	writeFile(t, root, ".claude/settings.json", []byte("{not json"), 0o644)

	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--json", "--project", root)
	if exitCode != cli.ExitOperational {
		t.Fatalf("exit = %d, want %d; stdout = %q stderr = %q", exitCode, cli.ExitOperational, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("failing inventory must not print a partial report, stdout = %q", stdout)
	}
	if !strings.Contains(stderr, `"ok":false`) || !strings.Contains(stderr, `"code":"migrate_failed"`) {
		t.Fatalf("stderr = %q, want migrate_failed on stderr", stderr)
	}
	if !strings.Contains(stderr, ".claude/settings.json") {
		t.Fatalf("stderr = %q, want the failing snapshot path", stderr)
	}
	if count := strings.Count(stderr, "retry the command, then report the failure"); count != 1 {
		t.Fatalf("recovery guidance count = %d, want 1; stderr = %q", count, stderr)
	}
}

func TestMigrateTesslJSONEnvelope(t *testing.T) {
	t.Parallel()

	root := seedConsumer(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl", "--dry-run", "--json", "--project", root)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("json exit = %d stderr = %q stdout = %q", exitCode, stderr, stdout)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("json stdout must be one envelope line, got %q", stdout)
	}
	var envelope struct {
		OK      bool            `json:"ok"`
		Command string          `json:"command"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Command != "migrate" {
		t.Fatalf("envelope = %+v", envelope)
	}
	var result struct {
		SchemaVersion int  `json:"schemaVersion"`
		DryRun        bool `json:"dryRun"`
		Wrote         bool `json:"wrote"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || !result.DryRun || result.Wrote {
		t.Fatalf("result = %+v", result)
	}
}

func TestJSONEnvelopeShape(t *testing.T) {
	t.Parallel()

	root := seedPlugin(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("json exit = %d stderr = %q stdout = %q", exitCode, stderr, stdout)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("json stdout must be one envelope line, got %q", stdout)
	}
	var envelope struct {
		OK      bool            `json:"ok"`
		Command string          `json:"command"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Command != "migrate" {
		t.Fatalf("envelope = %+v", envelope)
	}
	var result tesslpluginReport
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.ReportVersion != 1 || !result.Wrote || result.Manifest != manifest.Filename {
		t.Fatalf("result = %+v", result)
	}
	if result.Ignored == nil || len(result.Ignored) != 0 {
		t.Fatalf("result.ignored = %#v, want []", result.Ignored)
	}
}

func TestMigrateTesslPluginDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	root := seedPlugin(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--dry-run", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("dry-run exit = %d stderr = %q stdout = %q", exitCode, stderr, stdout)
	}
	if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote %s: %v", manifest.Filename, err)
	}
	if !strings.Contains(stdout, `"wrote":false`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestMigrateUnmappedFieldSurvivesJSON(t *testing.T) {
	t.Parallel()

	root := seedPlugin(t)
	plugin := filepath.Join(root, ".tessl-plugin", "plugin.json")
	if err := os.WriteFile(plugin, []byte(`{"name":"example/alpha","version":"1.0.0","private":true,"repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--dry-run", "--json")
	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit = %d stdout = %q stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code":"unmapped_field"`) {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, `"field":"private"`) {
		t.Fatalf("stderr missing field: %q", stderr)
	}
	var envelope struct {
		Result struct {
			DryRun   bool `json:"dryRun"`
			Unmapped []struct {
				Field  string `json:"field"`
				Reason string `json:"reason"`
			} `json:"unmapped"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Unmapped) != 1 || envelope.Result.Unmapped[0].Field != "private" || envelope.Result.Unmapped[0].Reason == "" {
		t.Fatalf("unmapped report = %+v", envelope.Result.Unmapped)
	}
	if !envelope.Result.DryRun {
		t.Fatal("unmapped report lost the invocation's dry-run flag")
	}
}

func TestMigrateUnmappedFieldSurvivesText(t *testing.T) {
	t.Parallel()

	root := seedPlugin(t)
	plugin := filepath.Join(root, ".tessl-plugin", "plugin.json")
	if err := os.WriteFile(plugin, []byte(`{"name":"example/alpha","version":"1.0.0","private":true,"repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root)
	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit = %d stdout = %q stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "unmapped:\n  - private:") {
		t.Fatalf("stderr missing text report: %q", stderr)
	}
}
func TestMigrateUnknownFieldUsesNamedExitCode(t *testing.T) {
	t.Parallel()

	root := seedPlugin(t)
	plugin := filepath.Join(root, ".tessl-plugin", "plugin.json")
	if err := os.WriteFile(plugin, []byte(`{"name":"example/alpha","version":"1.0.0","mystery":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--json")
	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit = %d stdout = %q stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code":"unknown_field"`) {
		t.Fatalf("stderr = %q", stderr)
	}
	var envelope struct {
		Result struct {
			Unmapped []struct {
				Field string `json:"field"`
			} `json:"unmapped"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Unmapped) != 1 || envelope.Result.Unmapped[0].Field != "mystery" {
		t.Fatalf("unmapped report = %+v", envelope.Result.Unmapped)
	}
}

func TestMigrateFallsBackForOtherCommands(t *testing.T) {
	t.Parallel()

	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "list", "--project", t.TempDir(), "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("list fallback exit = %d stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, `"dependencies"`) {
		t.Fatalf("list fallback stdout = %q", stdout)
	}
}

type tesslpluginReport struct {
	ReportVersion int        `json:"reportVersion"`
	Wrote         bool       `json:"wrote"`
	Manifest      string     `json:"manifest"`
	Ignored       []struct{} `json:"ignored"`
}

func runCLI(t *testing.T, application cli.Application, args ...string) (string, string, int) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).Run(context.Background(), args)
	return stdout.String(), stderr.String(), exitCode
}

func seedPlugin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeJSON(t, root, ".tessl-plugin/plugin.json", map[string]any{
		"name":        "example/alpha",
		"version":     "1.0.0",
		"description": "alpha plugin",
		"private":     false,
		"repository":  "https://github.com/example/alpha",
		"rules":       []string{"rules/always.md"},
		"skills":      []string{"skills/review-change"},
	})
	writeFile(t, root, "rules/always.md", []byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeFile(t, root, "skills/review-change/SKILL.md", []byte("# Review\n"), 0o644)
	return root
}

func seedConsumer(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeJSON(t, root, "tessl.json", map[string]any{
		"name": "consumer",
		"mode": "vendored",
		"dependencies": map[string]any{
			"example/alpha": map[string]string{"version": "1.0.0"},
		},
	})
	plugin := map[string]any{
		"name":    "example/alpha",
		"version": "1.0.0",
		"rules":   []string{"rules/always-rule.md"},
		"skills":  []string{"skills/review-change"},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash", "args": []string{"${TESSL_PLUGIN_DIR}/hooks/session-start.sh"},
			}}}},
		},
	}
	writeJSON(t, root, ".tessl/plugins/example/alpha/.tessl-plugin/plugin.json", plugin)
	writeFile(t, root, ".tessl/plugins/example/alpha/rules/always-rule.md", []byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/alpha/skills/review-change/SKILL.md", []byte("# Review\n"), 0o644)
	writeFile(t, root, ".tessl/plugins/example/alpha/hooks/session-start.sh", []byte("#!/bin/sh\necho start\n"), 0o755)
	pluginSkill := filepath.Join(root, ".tessl/plugins/example/alpha/skills/review-change")
	nativeDir := filepath.Join(root, ".claude/skills")
	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(nativeDir, pluginSkill)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(nativeDir, "tessl__review-change")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "AGENTS.md", []byte("# User\n\n## Agent Rules <!-- tessl-managed -->\n\n@.tessl/RULES.md\n"), 0o644)
	return root
}

func writeJSON(t *testing.T, root, relative string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, relative, append(payload, '\n'), 0o644)
}

func writeFile(t *testing.T, root, relative string, content []byte, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			result[rel] = "link→" + filepath.ToSlash(target)
			return nil
		}
		if entry.IsDir() {
			result[rel] = "dir"
			return nil
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		result[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func chmodTree(t *testing.T, root string, dirMode, fileMode os.FileMode) {
	t.Helper()
	var dirs, files []string
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			dirs = append(dirs, filename)
			return nil
		}
		files = append(files, filename)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range files {
		if err := os.Chmod(filename, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	for index := len(dirs) - 1; index >= 0; index-- {
		if err := os.Chmod(dirs[index], dirMode); err != nil {
			t.Fatal(err)
		}
	}
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
