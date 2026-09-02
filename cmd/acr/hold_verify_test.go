package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifyRebaseCommandsReachTheirOwnApplications(t *testing.T) {
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	project := t.TempDir()

	t.Run("install", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(&stdout, &stderr, []string{"install", "--project", project, "--dry-run", "--json"})
		if exitCode != 0 {
			t.Fatalf("install --dry-run --json exit = %d stderr = %q", exitCode, stderr.String())
		}
		if !jsonCommand(t, stdout.Bytes(), "install") {
			t.Fatalf("install stdout = %q, want the dependency application envelope", stdout.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), "not implemented") {
			t.Fatal("install fell through to UnavailableApplication")
		}
	})

	t.Run("resume", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(&stdout, &stderr, []string{"resume", "github:acme/widget", "--project", project, "--json"})
		if exitCode == 0 {
			t.Fatal("resume on an empty project succeeded")
		}
		if stdout.Len() != 0 {
			t.Fatalf("resume error on stdout: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), `"command":"resume"`) {
			t.Fatalf("resume stderr = %q, want the resume application", stderr.String())
		}
		if strings.Contains(stderr.String(), "not_implemented") {
			t.Fatal("resume fell through to UnavailableApplication")
		}
	})

	t.Run("publish", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(&stdout, &stderr, []string{"publish", project, "--dry-run", "--json"})
		if exitCode == 0 {
			t.Fatal("publish --dry-run --json on an empty directory succeeded")
		}
		if stdout.Len() != 0 {
			t.Fatalf("publish error on stdout: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), `"command":"publish"`) {
			t.Fatalf("publish stderr = %q, want the publish application", stderr.String())
		}
		if strings.Contains(stderr.String(), "not_implemented") {
			t.Fatal("publish fell through to UnavailableApplication")
		}
	})

	t.Run("freshness", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(&stdout, &stderr, []string{"freshness", "run", "--project", project, "--policy", "none", "--json"})
		if exitCode != 0 {
			t.Fatalf("freshness run --json exit = %d stderr = %q", exitCode, stderr.String())
		}
		if !jsonCommand(t, stdout.Bytes(), "freshness") {
			t.Fatalf("freshness stdout = %q, want the freshness application envelope", stdout.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), "not implemented") {
			t.Fatal("freshness fell through to UnavailableApplication")
		}
	})
}

func jsonCommand(t *testing.T, raw []byte, want string) bool {
	t.Helper()
	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return envelope.OK && envelope.Command == want
}
