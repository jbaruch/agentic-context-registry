package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reverifyBuildACR builds the shipped binary the way a release does, so the
// stack under test is main.go's composition root rather than a test-local one.
func reverifyBuildACR(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "acr")
	command := exec.Command("go", "build", "-o", binary, "./cmd/acr")
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build acr: %v\n%s", err, output)
	}
	return binary
}

func reverifyRunACR(t *testing.T, binary, stateHome string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	// Drop any ambient ACR_STATE_HOME so the run reads only the temporary state
	// this test owns, rather than depending on exec's dedup order.
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "ACR_STATE_HOME=") {
			continue
		}
		environment = append(environment, entry)
	}
	command.Env = append(environment, "ACR_STATE_HOME="+stateHome)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), exit.ExitCode()
}

func reverifySeedPluginPackage(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(relative, content string) {
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".tessl-plugin/plugin.json",
		`{"name":"example/alpha","version":"1.0.0","description":"alpha plugin",`+
			`"repository":"https://github.com/example/alpha","rules":["rules/always.md"]}`+"\n")
	write("rules/always.md", "---\nalwaysApply: true\n---\n# Always\n")
	return root
}

// The rebase stacked migrate → publish → freshness → realize. Each command must
// still reach its own application through the shipped binary: both migration
// forms are served by migrateapp, and every other command passes through it.
func TestReverifyStackedRunnerDispatchesEachCommand(t *testing.T) {
	t.Parallel()

	binary := reverifyBuildACR(t)
	pkg := reverifySeedPluginPackage(t)
	state := t.TempDir()
	project := t.TempDir()

	t.Run("migrateTesslPluginReachesMigrateApp", func(t *testing.T) {
		stdout, stderr, exitCode := reverifyRunACR(t, binary, state, "migrate", "tessl-plugin", pkg, "--dry-run", "--json")
		if exitCode != 0 {
			t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q, want empty", stderr)
		}
		var envelope struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Result  struct {
				ReportVersion  int    `json:"reportVersion"`
				DryRun         bool   `json:"dryRun"`
				Wrote          bool   `json:"wrote"`
				SourceManifest string `json:"sourceManifest"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
		}
		if !envelope.OK || envelope.Command != "migrate" {
			t.Fatalf("envelope = %+v", envelope)
		}
		if envelope.Result.ReportVersion != 1 || !envelope.Result.DryRun || envelope.Result.Wrote {
			t.Fatalf("result = %+v, want a version-1 dry-run report", envelope.Result)
		}
		if envelope.Result.SourceManifest != "plugin.json" {
			t.Fatalf("sourceManifest = %q", envelope.Result.SourceManifest)
		}
		if _, err := os.Stat(filepath.Join(pkg, "agent-plugin.yaml")); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote agent-plugin.yaml: %v", err)
		}
	})

	t.Run("migrateTesslReachesMigrateApp", func(t *testing.T) {
		stdout, stderr, exitCode := reverifyRunACR(t, binary, state, "migrate", "tessl", "--dry-run", "--project", project, "--json")
		if exitCode != 0 {
			t.Fatalf("exit = %d, want 0; stdout = %q stderr = %q", exitCode, stdout, stderr)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q, want empty", stderr)
		}
		var envelope struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Result  struct {
				SchemaVersion     int  `json:"schemaVersion"`
				DryRun            bool `json:"dryRun"`
				Wrote             bool `json:"wrote"`
				FinalizationReady bool `json:"finalizationReady"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
		}
		if !envelope.OK || envelope.Command != "migrate" || envelope.Result.SchemaVersion != 1 ||
			!envelope.Result.DryRun || envelope.Result.Wrote || envelope.Result.FinalizationReady {
			t.Fatalf("envelope = %+v", envelope)
		}
	})

	t.Run("publishReachesPublishApp", func(t *testing.T) {
		empty := t.TempDir()
		stdout, stderr, exitCode := reverifyRunACR(t, binary, state, "publish", empty, "--dry-run", "--json")
		if exitCode != 1 {
			t.Fatalf("exit = %d, want 1 (stdout %q stderr %q)", exitCode, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty", stdout)
		}
		var envelope struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
			t.Fatalf("stderr is not JSON: %v (%q)", err, stderr)
		}
		if envelope.OK || envelope.Command != "publish" {
			t.Fatalf("envelope = %+v", envelope)
		}
		if envelope.Error.Code != "publish_failed" {
			t.Fatalf("code = %q, want publish_failed", envelope.Error.Code)
		}
		if !strings.Contains(envelope.Error.Message, filepath.Join(empty, "agent-plugin.yaml")) {
			t.Fatalf("message = %q, want the publish loader naming the package path", envelope.Error.Message)
		}
	})

	t.Run("freshnessReachesFreshnessApp", func(t *testing.T) {
		stdout, stderr, exitCode := reverifyRunACR(t, binary, state, "freshness", "run", "--project", project, "--policy", "none", "--json")
		if exitCode != 0 {
			t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q, want empty", stderr)
		}
		var envelope struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Result  struct {
				Policy   string   `json:"policy"`
				Outdated []string `json:"outdated"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
		}
		if !envelope.OK || envelope.Command != "freshness" {
			t.Fatalf("envelope = %+v", envelope)
		}
		if envelope.Result.Policy != "none" {
			t.Fatalf("policy = %q, want none", envelope.Result.Policy)
		}
	})
}
