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

func TestReverifyBuiltBinaryDispatchesEveryStackedApplication(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "acr")
	build := exec.Command("go", "build", "-o", binary, ".")
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build acr binary: %v\n%s", err, buildOutput)
	}

	tests := []struct {
		name     string
		args     []string
		wantExit int
		wantOK   bool
		check    func(*testing.T, map[string]any)
	}{
		{
			name:     "migrate",
			args:     []string{"migrate", "tessl", "--dry-run", "--json"},
			wantExit: 1,
			check: func(t *testing.T, envelope map[string]any) {
				failure := jsonObject(t, envelope, "error")
				if failure["code"] != "tessl_manifest_absent" {
					t.Fatalf("migrate error = %#v", failure)
				}
			},
		},
		{
			name:     "publish",
			args:     []string{"publish", "--dry-run", "--json"},
			wantExit: 1,
			check: func(t *testing.T, envelope map[string]any) {
				failure := jsonObject(t, envelope, "error")
				if failure["code"] != "publish_failed" || !strings.Contains(stringValue(t, failure, "message"), "agent-plugin.yaml") {
					t.Fatalf("publish error = %#v, want publish application manifest diagnostic", failure)
				}
			},
		},
		{
			name:   "freshness",
			args:   []string{"freshness", "run", "--policy", "none", "--json"},
			wantOK: true,
			check: func(t *testing.T, envelope map[string]any) {
				result := jsonObject(t, envelope, "result")
				if result["policy"] != "none" {
					t.Fatalf("freshness result = %#v, want policy none", result)
				}
			},
		},
		{
			name:   "install",
			args:   []string{"install", "--dry-run", "--json"},
			wantOK: true,
			check: func(t *testing.T, envelope map[string]any) {
				result := jsonObject(t, envelope, "result")
				if _, ok := result["changed"].(bool); !ok {
					t.Fatalf("install result = %#v, want dependency change result", result)
				}
				if dependencies, ok := result["dependencies"].([]any); !ok || len(dependencies) != 0 {
					t.Fatalf("install dependencies = %#v, want empty array", result["dependencies"])
				}
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			state := t.TempDir()
			command := exec.Command(binary, test.args...)
			command.Dir = project
			command.Env = environmentWith("ACR_STATE_HOME", state)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr

			err := command.Run()
			exitCode := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("run %v: %v", test.args, err)
				}
				exitCode = exitError.ExitCode()
			}
			if exitCode != test.wantExit {
				t.Fatalf("exit = %d, want %d; stdout = %q stderr = %q", exitCode, test.wantExit, stdout.String(), stderr.String())
			}

			payload := stdout.String()
			unexpected := stderr.String()
			if test.wantExit != 0 {
				payload, unexpected = stderr.String(), stdout.String()
			}
			if unexpected != "" {
				t.Fatalf("unexpected output stream = %q", unexpected)
			}
			if strings.Count(payload, "\n") != 1 || !strings.HasSuffix(payload, "\n") {
				t.Fatalf("JSON output is not exactly one line: %q", payload)
			}
			var envelope map[string]any
			if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
				t.Fatalf("decode JSON output %q: %v", payload, err)
			}
			if envelope["ok"] != test.wantOK || envelope["command"] != test.name {
				t.Fatalf("envelope = %#v, want command %q ok %t", envelope, test.name, test.wantOK)
			}
			test.check(t, envelope)
		})
	}
}

func jsonObject(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want JSON object", key, object[key])
	}
	return value
}

func stringValue(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want string", key, object[key])
	}
	return value
}

func environmentWith(name, value string) []string {
	prefix := name + "="
	environment := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, prefix) {
			environment = append(environment, item)
		}
	}
	return append(environment, prefix+value)
}
