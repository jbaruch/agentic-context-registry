package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

const (
	usageGuidance       = "review the command usage and retry"
	operationalGuidance = "retry the command, then report the failure at https://github.com/jbaruch/agentic-context-registry/issues if it persists"
)

func TestInvalidDependencySourceIsUsage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sourceGuidance := `invalid source "software-factory"; use github:owner/repository with lowercase canonical names; ` + usageGuidance
	hint := `; to install a local directory, use "./software-factory"`

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "install bare name", args: []string{"install", "software-factory", "--project", root}, want: "acr install: " + sourceGuidance + hint + "\n"},
		{name: "install if-missing", args: []string{"install", "software-factory", "--if-missing", "--project", root}, want: "acr install: " + sourceGuidance + hint + "\n"},
		{name: "install uppercase owner", args: []string{"install", "github:Owner/plugin", "--project", root}, want: "acr install: " + `invalid source "github:Owner/plugin"; use github:owner/repository with lowercase canonical names; ` + usageGuidance + "\n"},
		{name: "install owner/name", args: []string{"install", "owner/plugin", "--project", root}, want: "acr install: " + `invalid source "owner/plugin"; use github:owner/repository with lowercase canonical names; ` + usageGuidance + "\n"},
		{name: "update", args: []string{"update", "software-factory", "--project", root}, want: "acr update: " + sourceGuidance + "\n"},
		{name: "resume", args: []string{"resume", "software-factory", "--project", root}, want: "acr resume: " + sourceGuidance + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := &fakeGitHub{err: errors.New("remote must not be called")}
			assertCommandDiagnostic(t, NewApplication(remote), test.args, cli.ExitUsage, test.want)
			if remote.latestCalls != 0 || remote.releaseCalls != 0 || remote.resolveCalls != 0 || remote.downloadCalls != 0 {
				t.Fatalf("invalid source reached GitHub: %+v", remote)
			}
		})
	}

	t.Run("install json", func(t *testing.T) {
		remote := &fakeGitHub{err: errors.New("remote must not be called")}
		stdout, stderr, exitCode := runApplication(t, NewApplication(remote), "install", "software-factory", "--project", root, "--json")
		if exitCode != cli.ExitUsage || stdout != "" {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
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
			t.Fatal(err)
		}
		want := sourceGuidance + hint
		if envelope.OK || envelope.Command != "install" || envelope.Error.Code != "usage" || envelope.Error.Message != want {
			t.Fatalf("envelope = %#v, want message %q", envelope, want)
		}
		if strings.Contains(stderr, operationalGuidance) {
			t.Fatalf("usage refusal carries operational guidance: %s", stderr)
		}
		if remote.latestCalls != 0 || remote.downloadCalls != 0 {
			t.Fatalf("invalid source reached GitHub: %+v", remote)
		}
	})
}

func TestInstallOperationalFailureKeepsOperationalGuidance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commit := strings.Repeat("b", 40)
	remote := &fakeGitHub{
		latest:   Release{ID: 20, Tag: "v2.0.0"},
		commits:  map[string]string{"v2.0.0": commit},
		archives: map[string][]byte{commit: []byte("not an archive")},
	}
	app := NewApplication(remote)

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			args := []string{"install", "github:owner/plugin", "--project", root}
			if format == "json" {
				args = append(args, "--json")
			}
			stdout, stderr, exitCode := runApplication(t, app, args...)
			if exitCode != cli.ExitOperational || stdout != "" {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			if !strings.Contains(stderr, `"code":"dependency_operation_failed"`) && format == "json" {
				t.Fatalf("stderr = %q, want dependency_operation_failed", stderr)
			}
			if !strings.Contains(stderr, "valid GitHub tarball") || !strings.Contains(stderr, operationalGuidance) {
				t.Fatalf("stderr = %q, want the archive failure and operational guidance", stderr)
			}
			if strings.Contains(stderr, usageGuidance) {
				t.Fatalf("operational failure carries usage guidance: %s", stderr)
			}
			if format == "text" && !strings.HasPrefix(stderr, "acr install: ") {
				t.Fatalf("stderr = %q, want the install diagnostic prefix", stderr)
			}
		})
	}
}

func assertCommandDiagnostic(t *testing.T, app cli.Application, args []string, wantExit int, wantStderr string) {
	t.Helper()
	stdout, stderr, exitCode := runApplication(t, app, args...)
	if exitCode != wantExit || stdout != "" || stderr != wantStderr {
		t.Fatalf("Run(%v)\n exit = %d, want %d\n stdout = %q\n stderr = %q\n want   = %q", args, exitCode, wantExit, stdout, stderr, wantStderr)
	}
	if strings.Contains(stderr, operationalGuidance) {
		t.Fatalf("usage refusal carries operational guidance: %s", stderr)
	}
}

func runApplication(t *testing.T, app cli.Application, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exitCode := cli.New(&stdout, &stderr, app, cli.Build{Version: "test"}).Run(context.Background(), args)
	return stdout.String(), stderr.String(), exitCode
}
