package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseLocalInstallPaths(t *testing.T) {
	for _, test := range []struct{ argument, path string }{
		{".", "."}, {"./", "."}, {"..", ".."}, {"../p", "../p"}, {"./p", "p"}, {"/absolute/p", "/absolute/p"}, {"file:.", "."}, {"file:p", "p"}, {"file:./p", "p"}, {"./p@v1", "p@v1"},
	} {
		invocation, _, err := parseInvocation(CommandInstall, []string{test.argument, "--project", "/selected"})
		if err != nil || invocation.LocalPath != test.path || invocation.Source != "" || invocation.Reconcile {
			t.Fatalf("%s: %+v %v", test.argument, invocation, err)
		}
	}
	for _, args := range [][]string{{"file:"}, {"file://host/p"}, {"./p", "--hold"}, {"./p", "--pin"}, {"./p", "--if-missing"}} {
		if _, _, err := parseInvocation(CommandInstall, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, argument := range []string{"plugin", "github:owner/plugin"} {
		invocation, _, err := parseInvocation(CommandInstall, []string{argument})
		if err != nil || invocation.LocalPath != "" || invocation.Source != argument {
			t.Fatalf("source: %+v %v", invocation, err)
		}
	}
}

// This fixture models the source-validation cause, including wrapping by an app.
type rejectedLocalHintSource string

func (source rejectedLocalHintSource) Error() string         { return "invalid source" }
func (source rejectedLocalHintSource) InvalidSource() string { return string(source) }

func TestInstallSourceHintBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		cause      error
		wantSource string
		wantHint   bool
	}{
		{"bare", []string{"install", "another-plugin"}, rejectedLocalHintSource("another-plugin"), "another-plugin", true},
		{"wrapped", []string{"install", "another-plugin"}, fmt.Errorf("resolve: %w", rejectedLocalHintSource("another-plugin")), "another-plugin", true},
		{"github", []string{"install", "github:owner/plugin"}, rejectedLocalHintSource("github:owner/plugin"), "github:owner/plugin", false},
		{"malformed-github", []string{"install", "github:Owner/plugin"}, rejectedLocalHintSource("github:Owner/plugin"), "github:Owner/plugin", false},
		{"slash", []string{"install", "owner/plugin"}, rejectedLocalHintSource("owner/plugin"), "owner/plugin", false},
		{"backslash", []string{"install", `owner\plugin`}, rejectedLocalHintSource(`owner\plugin`), `owner\plugin`, false},
		{"other-source", []string{"install", "another-plugin"}, rejectedLocalHintSource("sibling"), "another-plugin", false},
		{"unrelated-error", []string{"install", "another-plugin"}, errors.New("read project state"), "another-plugin", false},
		{"update", []string{"update", "another-plugin"}, rejectedLocalHintSource("another-plugin"), "another-plugin", false},
		{"resume", []string{"resume", "another-plugin"}, rejectedLocalHintSource("another-plugin"), "another-plugin", false},
		{"reconcile", []string{"install"}, rejectedLocalHintSource("another-plugin"), "", false},
		{"explicit-path", []string{"install", "./another-plugin"}, rejectedLocalHintSource("another-plugin"), "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &Error{ExitCode: ExitOperational, Code: "dependency_operation_failed", Message: test.cause.Error(), Cause: test.cause}
			var stdout, stderr bytes.Buffer
			app := ApplicationFunc(func(_ context.Context, invocation Invocation) (Result, error) {
				if invocation.Source != test.wantSource || (test.wantSource != "" && invocation.LocalPath != "") {
					t.Fatalf("source classification changed: %+v", invocation)
				}
				got := installSourceError(invocation, original)
				if got.Code != original.Code || got.ExitCode != original.ExitCode || got.Cause != original.Cause {
					t.Fatalf("refusal identity changed: %+v", got)
				}
				return Result{}, original
			})
			args := append(append([]string(nil), test.args...), "--json")
			if exit := New(&stdout, &stderr, app, Build{}).Run(context.Background(), args); exit != ExitOperational {
				t.Fatalf("exit %d: %s", exit, stderr.String())
			}
			var envelope struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(envelope.Error.Message, "local directory"); got != test.wantHint {
				t.Fatalf("hint = %t, want %t: %s", got, test.wantHint, stderr.String())
			}
			if test.wantHint && !strings.Contains(envelope.Error.Message, "./another-plugin") {
				t.Fatalf("hint names the wrong directory: %s", stderr.String())
			}
			if !test.wantHint && envelope.Error.Message != commandError(original).Message {
				t.Fatalf("unrelated diagnostic changed: %s", stderr.String())
			}
			if original.Message != test.cause.Error() {
				t.Fatal("rendering mutated the application's error")
			}
		})
	}
}
