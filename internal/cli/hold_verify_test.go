package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifyResumeRequiresAPositionalAndHoldSHAIsParsedForRefusal(t *testing.T) {
	t.Parallel()

	t.Run("resume without SOURCE is usage", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, rejectingApplication(t), "test").Run(context.Background(), []string{"resume"})
		if exitCode != ExitUsage {
			t.Fatalf("resume exit = %d, want 2", exitCode)
		}
		if stdout.Len() != 0 {
			t.Fatalf("usage diagnostic on stdout: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "acr resume SOURCE") {
			t.Fatalf("stderr = %q, want the required positional named", stderr.String())
		}
	})

	t.Run("resume SOURCE is parsed", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		app := ApplicationFunc(func(_ context.Context, invocation Invocation) (Result, error) {
			if invocation.Command != CommandResume || invocation.Source != "github:acme/widget" || !invocation.DryRun {
				t.Fatalf("invocation = %#v", invocation)
			}
			return Result{Value: map[string]any{"changed": true, "resumed": []string{invocation.Source}}}, nil
		})
		exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), []string{
			"resume", "github:acme/widget", "--dry-run", "--json",
		})
		if exitCode != ExitSuccess || stderr.Len() != 0 {
			t.Fatalf("resume --dry-run --json exit = %d stderr = %q", exitCode, stderr.String())
		}
		var envelope struct {
			OK      bool            `json:"ok"`
			Command string          `json:"command"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("envelope %q: %v", stdout.String(), err)
		}
		if !envelope.OK || envelope.Command != "resume" {
			t.Fatalf("envelope = %#v", envelope)
		}
	})

	t.Run("install --hold with a SHA still requires an explicit version", func(t *testing.T) {
		t.Parallel()
		sha := strings.Repeat("ab", 20)
		var captured Invocation
		app := ApplicationFunc(func(_ context.Context, invocation Invocation) (Result, error) {
			captured = invocation
			return Result{}, usageError("--hold requires a stable release tag, not a commit SHA")
		})
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := New(&stdout, &stderr, app, "test").Run(context.Background(), []string{
			"install", "github:acme/widget@" + sha, "--hold",
		})
		if captured.Command != CommandInstall || captured.RequestedVersion != sha || captured.Downgrade != DowngradeHold {
			t.Fatalf("parser dropped the SHA hold: %#v", captured)
		}
		if exitCode != ExitUsage || stdout.Len() != 0 {
			t.Fatalf("exit = %d stdout = %q stderr = %q", exitCode, stdout.String(), stderr.String())
		}
	})
}
