package migrateapp

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

func hostileSeedPlugin(t *testing.T) string {
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
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "bash",
				"args": []string{"${TESSL_PLUGIN_DIR}/hooks/check-freshness.sh", "--fast"},
			}}}},
		},
	})
	writeFile(t, root, "rules/always.md", []byte("---\nalwaysApply: true\n---\n# Always\n"), 0o644)
	writeFile(t, root, "skills/review-change/SKILL.md", []byte("# Review\n"), 0o644)
	writeFile(t, root, "hooks/check-freshness.sh", []byte("#!/bin/sh\necho hook\n"), 0o755)
	return root
}

func hostileChmodTree(t *testing.T, root string, dirMode, fileMode os.FileMode) {
	t.Helper()
	var dirs, files []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		files = append(files, name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.Chmod(name, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	for index := len(dirs) - 1; index >= 0; index-- {
		if err := os.Chmod(dirs[index], dirMode); err != nil {
			t.Fatal(err)
		}
	}
}

// --json purity on the success path: exactly one envelope on stdout, nothing on
// stderr, exit 0.
func TestHostileJSONSuccessStreamIsPure(t *testing.T) {
	t.Parallel()

	root := hostileSeedPlugin(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--json")
	if exitCode != cli.ExitSuccess {
		t.Fatalf("exit = %d stderr = %q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr must be empty on success, got %q", stderr)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout must be exactly one envelope line, got %q", stdout)
	}
	var envelope struct {
		OK      bool            `json:"ok"`
		Command string          `json:"command"`
		Result  json.RawMessage `json:"result"`
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("stdout is not one JSON value: %v (%q)", err, stdout)
	}
	if decoder.More() {
		t.Fatalf("stdout carries trailing content: %q", stdout)
	}
	if !envelope.OK || envelope.Command != "migrate" {
		t.Fatalf("envelope = %+v", envelope)
	}
	var result struct {
		ReportVersion  int      `json:"reportVersion"`
		Wrote          bool     `json:"wrote"`
		DryRun         bool     `json:"dryRun"`
		Manifest       string   `json:"manifest"`
		SourceManifest string   `json:"sourceManifest"`
		Package        string   `json:"package"`
		Version        string   `json:"version"`
		PublishedFiles []string `json:"publishedFiles"`
		Artifacts      []struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Path  string `json:"path"`
			Event string `json:"event"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.ReportVersion != 1 || !result.Wrote || result.DryRun {
		t.Fatalf("result = %+v", result)
	}
	if result.Manifest != manifest.Filename || result.SourceManifest != "plugin.json" {
		t.Fatalf("manifest fields = %+v", result)
	}
	if result.Package != "example/alpha" || result.Version != "1.0.0" {
		t.Fatalf("identity = %+v", result)
	}
	if len(result.Artifacts) != 3 || len(result.PublishedFiles) == 0 {
		t.Fatalf("artifacts = %+v published = %+v", result.Artifacts, result.PublishedFiles)
	}
	loaded, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := manifest.PackageFiles(root, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.PublishedFiles, "|") != strings.Join(want, "|") {
		t.Fatalf("publishedFiles %v != PackageFiles %v", result.PublishedFiles, want)
	}
}

// --json purity on the refusal path: nothing on stdout, one envelope on stderr,
// exit 1, code and field both present.
func TestHostileJSONErrorStreamIsPure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(t *testing.T, root string)
		wantCode   string
		wantField  string
		wantResult bool
	}{
		{
			name: "privateTrue",
			mutate: func(t *testing.T, root string) {
				writeJSON(t, root, ".tessl-plugin/plugin.json", map[string]any{
					"name": "example/alpha", "version": "1.0.0", "private": true,
					"repository": "https://github.com/example/alpha",
					"rules":      []string{"rules/always.md"},
				})
			},
			wantCode:   "unmapped_field",
			wantField:  "private",
			wantResult: true,
		},
		{
			name: "unknownKey",
			mutate: func(t *testing.T, root string) {
				writeJSON(t, root, ".tessl-plugin/plugin.json", map[string]any{
					"name": "example/alpha", "version": "1.0.0", "publisher": "acme",
					"repository": "https://github.com/example/alpha",
					"rules":      []string{"rules/always.md"},
				})
			},
			wantCode:   "unknown_field",
			wantField:  "publisher",
			wantResult: true,
		},
		{
			name: "repositoryMismatch",
			mutate: func(t *testing.T, root string) {
				writeJSON(t, root, ".tessl-plugin/plugin.json", map[string]any{
					"name": "example/alpha", "version": "1.0.0",
					"repository": "https://github.com/other/alpha",
					"rules":      []string{"rules/always.md"},
				})
			},
			wantCode:  "invalid_source",
			wantField: "source.repository",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := hostileSeedPlugin(t)
			test.mutate(t, root)

			stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--json")
			if exitCode != cli.ExitOperational {
				t.Fatalf("exit = %d want %d (stderr %q)", exitCode, cli.ExitOperational, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must stay empty on refusal, got %q", stdout)
			}
			if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
				t.Fatalf("stderr must be exactly one envelope line, got %q", stderr)
			}
			var envelope struct {
				OK     bool            `json:"ok"`
				Result json.RawMessage `json:"result"`
				Error  struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Field   string `json:"field"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("stderr is not JSON: %v (%q)", err, stderr)
			}
			if envelope.OK {
				t.Fatalf("refusal envelope reports ok:true (%q)", stderr)
			}
			if envelope.Error.Code != test.wantCode {
				t.Fatalf("code = %q want %q", envelope.Error.Code, test.wantCode)
			}
			if envelope.Error.Field != test.wantField {
				t.Fatalf("field = %q want %q", envelope.Error.Field, test.wantField)
			}
			if envelope.Error.Message == "" {
				t.Fatal("refusal carries no message")
			}
			if gotResult := envelope.Result != nil; gotResult != test.wantResult {
				t.Fatalf("result presence = %v want %v: %s", gotResult, test.wantResult, envelope.Result)
			}
			if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(err) {
				t.Fatalf("refused conversion wrote %s: %v", manifest.Filename, err)
			}
		})
	}
}

// Text mode never prints JSON and never writes to stderr on success.
func TestHostileTextModeStreamIsPure(t *testing.T) {
	t.Parallel()

	root := hostileSeedPlugin(t)
	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("exit = %d stderr = %q", exitCode, stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("text mode emitted JSON: %q", stdout)
	}
	if !strings.Contains(stdout, "plugin.json → "+manifest.Filename) {
		t.Fatalf("text output does not name the conversion: %q", stdout)
	}

	stdout, stderr, exitCode = runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("second run exit = %d stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Already current") {
		t.Fatalf("second run text = %q", stdout)
	}
}

// Usage failures are exit 2 and never reach the converter.
func TestHostileUsageFailuresExitTwo(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"migrate", "tessl-plugin", "one", "two"},
		{"migrate", "tessl", "--repository", "https://github.com/example/alpha"},
		{"migrate", "tessl", "--accept-agent-widening"},
		{"migrate", "legacy"},
		{"migrate", "tessl-plugin", "--accept-agent-widening=yes"},
		{"migrate", "tessl-plugin", "--non-interactive"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()
			stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), args...)
			if exitCode != cli.ExitUsage {
				t.Fatalf("exit = %d want %d (stdout %q stderr %q)", exitCode, cli.ExitUsage, stdout, stderr)
			}
			if stderr == "" {
				t.Fatal("usage failure produced no diagnostic")
			}
		})
	}
}

// --dry-run leaves a read-only package untouched and still succeeds.
func TestHostileDryRunOnReadOnlyTree(t *testing.T) {
	t.Parallel()

	root := hostileSeedPlugin(t)
	hostileChmodTree(t, root, 0o555, 0o555)
	t.Cleanup(func() { hostileChmodTree(t, root, 0o755, 0o644) })

	stdout, stderr, exitCode := runCLI(t, NewApplication(nil, "test"), "migrate", "tessl-plugin", root, "--dry-run", "--json")
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("exit = %d stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, `"dryRun":true`) || !strings.Contains(stdout, `"wrote":false`) {
		t.Fatalf("stdout = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, manifest.Filename)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote %s: %v", manifest.Filename, err)
	}
}
