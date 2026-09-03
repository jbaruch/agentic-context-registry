package setupapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

const heldSource = "github:owner/plugin"

// scriptedApplication answers each call with the next scripted outcome and
// records the invocation it received, so a test asserts what the decorator
// re-invoked rather than what a domain service happened to write.
type scriptedApplication struct {
	calls    []cli.Invocation
	outcomes []scriptedOutcome
}

type scriptedOutcome struct {
	result cli.Result
	err    error
}

func (application *scriptedApplication) Execute(_ context.Context, invocation cli.Invocation) (cli.Result, error) {
	application.calls = append(application.calls, invocation)
	if len(application.calls) > len(application.outcomes) {
		return cli.Result{}, fmt.Errorf("unexpected call %d to the inner application", len(application.calls))
	}
	outcome := application.outcomes[len(application.calls)-1]
	return outcome.result, outcome.err
}

func downgradeRequired() error {
	return &cli.Error{
		ExitCode: cli.ExitUsage,
		Code:     cli.CodeDowngradeChoiceRequired,
		Message:  heldSource + "@v1.0.0 is a rollback from the locked v2.0.0; pass --hold or --pin",
	}
}

func installInvocation(root string) cli.Invocation {
	return cli.Invocation{
		Command:          cli.CommandInstall,
		Source:           heldSource,
		RequestedVersion: "v1.0.0",
		ProjectDirectory: root,
		Output:           cli.OutputText,
	}
}

func runDowngrade(t *testing.T, input string, invocation cli.Invocation, outcomes ...scriptedOutcome) (*scriptedApplication, *bytes.Buffer, cli.Result, error) {
	t.Helper()
	inner := &scriptedApplication{outcomes: outcomes}
	var questions bytes.Buffer
	prompter := NewTerminalPrompter(strings.NewReader(input), &questions, true)
	result, err := NewApplication(inner, prompter).Execute(context.Background(), invocation)
	return inner, &questions, result, err
}

func TestDowngradePromptChoosesHold(t *testing.T) {
	t.Parallel()

	inner, questions, result, err := runDowngrade(t, "hold\n", installInvocation(heldProject(t)),
		scriptedOutcome{err: downgradeRequired()},
		scriptedOutcome{result: cli.Result{Message: "held"}})

	if err != nil || result.Message != "held" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if len(inner.calls) != 2 {
		t.Fatalf("inner application called %d time(s), want 2", len(inner.calls))
	}
	if inner.calls[0].Downgrade != cli.DowngradeUnset || inner.calls[1].Downgrade != cli.DowngradeHold {
		t.Fatalf("downgrade choices = %q, %q", inner.calls[0].Downgrade, inner.calls[1].Downgrade)
	}
	if !strings.Contains(questions.String(), heldSource+"@v1.0.0") {
		t.Fatalf("question = %q, want the rolled-back reference", questions.String())
	}
}

func TestDowngradePromptChoosesPin(t *testing.T) {
	t.Parallel()

	inner, _, result, err := runDowngrade(t, "2\n", installInvocation(heldProject(t)),
		scriptedOutcome{err: downgradeRequired()},
		scriptedOutcome{result: cli.Result{Message: "pinned"}})

	if err != nil || result.Message != "pinned" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if len(inner.calls) != 2 || inner.calls[1].Downgrade != cli.DowngradePin {
		t.Fatalf("inner calls = %#v", inner.calls)
	}
}

func TestDowngradePromptRetriesOnceOnly(t *testing.T) {
	t.Parallel()

	inner, _, _, err := runDowngrade(t, "hold\nhold\nhold\n", installInvocation(heldProject(t)),
		scriptedOutcome{err: downgradeRequired()},
		scriptedOutcome{err: downgradeRequired()})

	var commandErr *cli.Error
	if !errors.As(err, &commandErr) || commandErr.Code != cli.CodeDowngradeChoiceRequired {
		t.Fatalf("Execute() error = %v, want the second refusal to propagate", err)
	}
	if len(inner.calls) != 2 {
		t.Fatalf("inner application called %d time(s), want exactly 2", len(inner.calls))
	}
}

func TestDowngradePromptSkippedUnderJSONAndNonInteractive(t *testing.T) {
	t.Parallel()

	tests := map[string]func(cli.Invocation) cli.Invocation{
		"json":            func(invocation cli.Invocation) cli.Invocation { invocation.Output = cli.OutputJSON; return invocation },
		"non-interactive": func(invocation cli.Invocation) cli.Invocation { invocation.NonInteractive = true; return invocation },
	}
	for name, adjust := range tests {
		name, adjust := name, adjust
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			invocation := adjust(installInvocation(heldProject(t)))

			reader := &countingReader{}
			var questions bytes.Buffer
			inner := &scriptedApplication{outcomes: []scriptedOutcome{{err: downgradeRequired()}}}
			prompter := NewTerminalPrompter(reader, &questions, true)

			_, err := NewApplication(inner, prompter).Execute(context.Background(), invocation)

			var commandErr *cli.Error
			if !errors.As(err, &commandErr) || commandErr.Code != cli.CodeDowngradeChoiceRequired {
				t.Fatalf("Execute() error = %v, want the typed refusal", err)
			}
			if !strings.Contains(commandErr.Message, "--hold") || !strings.Contains(commandErr.Message, "--pin") {
				t.Fatalf("refusal = %q, want both flags named", commandErr.Message)
			}
			if len(inner.calls) != 1 || reader.reads != 0 || questions.Len() != 0 {
				t.Fatalf("%s prompted: calls = %d, reads = %d, questions = %q", name, len(inner.calls), reader.reads, questions.String())
			}
		})
	}
}

func TestPromptRefusesInsteadOfBlockingOnEOF(t *testing.T) {
	t.Parallel()

	inner, _, _, err := runDowngrade(t, "", installInvocation(heldProject(t)), scriptedOutcome{err: downgradeRequired()})

	var commandErr *cli.Error
	if !errors.As(err, &commandErr) || commandErr.Code != "downgrade_cancelled" || commandErr.ExitCode != cli.ExitUsage {
		t.Fatalf("Execute() error = %v, want a cancelled rollback", err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("cancelled rollback re-invoked the inner application: %#v", inner.calls)
	}
}

func TestPromptsWriteQuestionsToStderrOnly(t *testing.T) {
	t.Parallel()

	root := heldProject(t)
	before := hashTree(t, root)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	application := NewApplication(dependency.NewApplication(unreachableGitHub{}), NewTerminalPrompter(strings.NewReader("hold\n"), &stderr, true))

	exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).
		Run(context.Background(), []string{"install", heldSource + "@v1.0.0", "--project", root})

	if !strings.Contains(stderr.String(), "rolls a latest dependency backwards") {
		t.Fatalf("question did not reach stderr: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "rolls a latest dependency backwards") {
		t.Fatalf("question reached stdout: %q", stdout.String())
	}
	// The chosen hold reaches the resolver, which this test denies the network,
	// so the run fails after the prompt rather than writing anything.
	if exitCode == cli.ExitSuccess {
		t.Fatalf("install with an unreachable remote exit = %d, want a failure", exitCode)
	}
	if after := hashTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed install wrote files:\n before %#v\n after  %#v", before, after)
	}
}

func TestDowngradePromptCancelWritesNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"explicit cancel":                "cancel\n",
		"empty answer":                   "\n",
		"three unparsable answers":       "maybe\nperhaps\nwhatever\n",
		"end of input":                   "",
		"partial line without newline":   "hold",
		"partial cancel without newline": "cancel",
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := heldProject(t)
			before := hashTree(t, root)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			application := NewApplication(dependency.NewApplication(unreachableGitHub{}), NewTerminalPrompter(strings.NewReader(input), &stderr, true))

			exitCode := cli.New(&stdout, &stderr, application, cli.Build{Version: "test"}).
				Run(context.Background(), []string{"install", heldSource + "@v1.0.0", "--project", root})

			if exitCode != cli.ExitUsage || stdout.Len() != 0 {
				t.Fatalf("cancelled install exit = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "rollback cancelled") || !strings.Contains(stderr.String(), "--pin") {
				t.Fatalf("cancel diagnostic = %q, want the recovery flags", stderr.String())
			}
			if after := hashTree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("cancelled install wrote files:\n before %#v\n after  %#v", before, after)
			}
		})
	}
}

// heldProject writes a project whose only dependency tracks latest at v2.0.0,
// so installing v1.0.0 is a rollback.
func heldProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	state := dependency.State{
		Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.Declaration{
			{Source: heldSource, Requested: "latest"},
		}},
		Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion, Dependencies: []dependency.LockedDependency{{
			Source: heldSource, Requested: "latest", Kind: dependency.ResolutionRelease, ReleaseID: 2, Tag: "v2.0.0",
			Commit: strings.Repeat("a", 40), PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("0", 64),
		}}},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	return root
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		relative, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return relErr
		}
		digest := sha256.Sum256(content)
		digests[filepath.ToSlash(relative)] = hex.EncodeToString(digest[:])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return digests
}

// unreachableGitHub proves a refusal happened before any network call.
type unreachableGitHub struct{}

func (unreachableGitHub) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	return dependency.Release{}, errors.New("remote must not be reached")
}

func (unreachableGitHub) ReleaseByTag(context.Context, dependency.Repository, string) (dependency.Release, error) {
	return dependency.Release{}, errors.New("remote must not be reached")
}

func (unreachableGitHub) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	return "", errors.New("remote must not be reached")
}

func (unreachableGitHub) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	return nil, errors.New("remote must not be reached")
}

func (unreachableGitHub) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	return nil, errors.New("remote must not be reached")
}
