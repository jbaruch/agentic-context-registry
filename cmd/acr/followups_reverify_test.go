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

// TestReverifySessionStartHookPathSurvivesAnUnusableProject drives the real
// binary with the argv the generated session-start wrapper uses, then the real
// wrapper itself. A project root that cannot be opened must leave the process
// intact, keep stdout free of partial output, and reach the agent as exactly one
// hook payload.
func TestReverifySessionStartHookPathSurvivesAnUnusableProject(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "acr")
	build := exec.Command("go", "build", "-o", binary, ".")
	if buildOutput, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acr binary: %v\n%s", err, buildOutput)
	}

	stateHome := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	hookArgs := []string{"freshness", "run", "--project", missing, "--policy", "outdated"}

	t.Run("hook argv without --json", func(t *testing.T) {
		stdout, stderr, exitCode := runACR(t, binary, hookArgs, environmentWith("ACR_STATE_HOME", stateHome))

		if exitCode != 1 {
			t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", exitCode, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want nothing on the result stream", stdout)
		}
		want := "acr freshness: validate project directory in project \"" + missing + "\"; restore directory access or verify --project, then retry: open " + missing + "/.: no such file or directory\n"
		if stderr != want {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
		entries, err := os.ReadDir(stateHome)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("state home entries = %v, want empty", entries)
		}
	})

	t.Run("hook argv with --json", func(t *testing.T) {
		stdout, stderr, exitCode := runACR(t, binary, append(append([]string{}, hookArgs...), "--json"), environmentWith("ACR_STATE_HOME", stateHome))

		if exitCode != 1 {
			t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", exitCode, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty", stdout)
		}
		var envelope struct {
			OK      bool                           `json:"ok"`
			Command string                         `json:"command"`
			Error   struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
			t.Fatalf("decode stderr %q: %v", stderr, err)
		}
		if envelope.OK || envelope.Command != "freshness" || envelope.Error.Code != "project_access_failed" || !strings.Contains(envelope.Error.Message, missing) {
			t.Fatalf("envelope = %#v, want project_access_failed", envelope)
		}
		entries, err := os.ReadDir(stateHome)
		if err != nil || len(entries) != 0 {
			t.Fatalf("freshness state changed: %v, %v", entries, err)
		}

	})

	t.Run("generated wrapper", func(t *testing.T) {
		// The wrapper derives --project from its own location, so its root always
		// exists. This shim rewrites that argument to the missing path and execs
		// the real binary, exercising the wrapper's failure branch against the
		// real diagnostic rather than a hand-written stand-in.
		root := t.TempDir()
		hookDirectory := filepath.Join(root, ".claude", "hooks", "acr-reverify")
		if err := os.MkdirAll(hookDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.FromSlash("../../internal/freshness/session-start.sh"))
		if err != nil {
			t.Fatal(err)
		}
		hook := filepath.Join(hookDirectory, "session-start.sh")
		if err := os.WriteFile(hook, source, 0o755); err != nil {
			t.Fatal(err)
		}
		shim := filepath.Join(root, "shim-acr")
		shimBody := "#!/usr/bin/env bash\nset -euo pipefail\nargs=()\nnext=0\nfor argument in \"$@\"; do\n  if [[ $next -eq 1 ]]; then args+=(\"$ACR_REVERIFY_PROJECT\"); next=0; continue; fi\n  if [[ \"$argument\" == \"--project\" ]]; then next=1; fi\n  args+=(\"$argument\")\ndone\nexec \"$ACR_REVERIFY_BIN\" \"${args[@]}\"\n"
		if err := os.WriteFile(shim, []byte(shimBody), 0o755); err != nil {
			t.Fatal(err)
		}

		environment := environmentWith("ACR_STATE_HOME", stateHome)
		environment = append(environment, "ACR_BIN="+shim, "ACR_REVERIFY_BIN="+binary, "ACR_REVERIFY_PROJECT="+missing)
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer devNull.Close()

		command := exec.Command("bash", hook, "--policy", "outdated")
		command.Env = environment
		command.Stdin = devNull
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("wrapper exited non-zero: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		}
		if stderr.String() != "" {
			t.Fatalf("wrapper stderr = %q, want silence", stderr.String())
		}
		payload := stdout.String()
		if strings.Count(payload, "\n") != 1 || !strings.HasSuffix(payload, "\n") {
			t.Fatalf("wrapper stdout = %q, want exactly one hook payload line", payload)
		}
		var hookPayload struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(payload), &hookPayload); err != nil {
			t.Fatalf("decode wrapper stdout %q: %v", payload, err)
		}
		context := hookPayload.HookSpecificOutput.AdditionalContext
		if hookPayload.HookSpecificOutput.HookEventName != "SessionStart" || !strings.HasPrefix(context, "Session-start status — freshness:") {
			t.Fatalf("hook payload = %#v, want one SessionStart freshness status", hookPayload)
		}
		if !strings.Contains(context, "acr freshness: validate project directory") || !strings.Contains(context, missing) {
			t.Fatalf("hook context = %q, want the project access diagnostic naming %q", context, missing)
		}
		if strings.Contains(context, "ACR_STATE_HOME") || strings.Contains(context, "panic:") {
			t.Fatalf("hook context = %q, want no state-home blame and no crash", context)
		}
	})
}

func runACR(t *testing.T, binary string, args, environment []string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = environment
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	exitCode := 0
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run %v: %v", args, err)
		}
		exitCode = exitError.ExitCode()
	}
	for _, stream := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(stream, "panic:") || strings.Contains(stream, "goroutine ") {
			t.Fatalf("crash output from %v: %q", args, stream)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}
