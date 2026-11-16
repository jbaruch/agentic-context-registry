package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCodexCredentialBytes(t *testing.T) {
	if err := ValidateCodexCredential(""); err != nil {
		t.Fatal(err)
	}
	for b := 0; b < 256; b++ {
		value := "synthetic-admission-" + string([]byte{byte(b)})
		admitted := b >= 0x21 && b <= 0x7e && !strings.ContainsRune("\"\\<>&", rune(b))
		err := ValidateCodexCredential(value)
		if (err == nil) != admitted {
			t.Fatalf("byte %02x admission=%t", b, admitted)
		}
		if err != nil && (!strings.Contains(err.Error(), "0x21-0x7e") || !strings.Contains(err.Error(), "codex login") || strings.Contains(err.Error(), value)) {
			t.Fatal("admission error is not controlled and actionable")
		}
	}
	values, err := codexCredentialValues([]byte(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"","access_token":null,"account_id":"ignored\u0001metadata"},"last_refresh":{"ignored":true}}`))
	if err != nil || len(values) != 0 {
		t.Fatal("optional absence or metadata selectivity changed")
	}
}

func TestCodexCredentialShapesRefuseBeforeSpawn(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, auth := range []string{`{broken`, `{"OPENAI_API_KEY":42}`, `{"tokens":{"id_token":true}}`, `{"tokens":{"access_token":[]}}`, `{"tokens":{"refresh_token":{}}}`, `{"tokens":"unsupported"}`} {
			native := codexFixtureFor(t, proposal{}, platform)
			put(t, native.home, "auth.json", auth, 0600)
			before := treeAt(t, native.home)
			_, run, err := runCodexWithRuntime(context.Background(), "request", native)
			if err == nil || len(codexSpawns(t, native)) != 0 || run.Stdout != "" || run.Stderr != "" {
				t.Fatal("invalid auth shape was not refused before spawning")
			}
			if !matches(before, treeAt(t, native.home)) {
				t.Fatal("invalid configured auth changed")
			}
			if entries, err := os.ReadDir(native.homeBase); err != nil || len(entries) != 0 {
				t.Fatal("invalid auth retained a private home")
			}
		}
	}
}

func TestCodexDiagnosticsRedactBeforePrefix(t *testing.T) {
	const secret = "synthetic-diagnostic-credential.0123456789"
	for _, limit := range []int{200, 300, 500} {
		pad := strings.Repeat("p", limit-len(secret)+1)
		input := pad + secret + strings.Repeat("z", limit)
		want := pad + "[redacted]" + strings.Repeat("z", len(secret)-11) + "…"
		if got := bounded(input, limit, []string{secret}); got != want {
			t.Fatalf("%d-byte diagnostic did not redact before shortening", limit)
		}
		if limit == 200 {
			_, err := parseCodexVersion(input, []string{secret})
			if err == nil || err.Error() != fmt.Sprintf("unrecognized Codex version output %q; expected \"codex-cli MAJOR.MINOR.PATCH\" from `codex --version`", want) {
				t.Fatal("version diagnostic retained an incomplete credential")
			}
			_, err = parseCodexFeatures(input, []string{secret})
			if err == nil || err.Error() != fmt.Sprintf("unrecognized Codex feature row %q; expected \"name stage true|false\" from `codex features list`", want) {
				t.Fatal("feature diagnostic retained an incomplete credential")
			}
		}
		if limit == 500 {
			err := codexArgvRejection(input, nil, []string{secret})
			if err.Error() != fmt.Sprintf("unsupported Codex capability: this release rejected ACR's exec arguments: %q", want) {
				t.Fatal("argument diagnostic retained an incomplete credential")
			}
		}
	}
	for _, platform := range codexPlatformsUnderTest {
		for _, limit := range []int{200, 300, 500} {
			t.Run(fmt.Sprintf("%s/%d", platform, limit), func(t *testing.T) {
				native := codexFixtureFor(t, proposal{}, platform)
				put(t, native.home, "auth.json", `{"tokens":{"access_token":"`+secret+`"}}`, 0600)
				prefix := ""
				if limit == 300 {
					prefix = "usage limit "
				}
				pad := prefix + strings.Repeat("p", limit-len(secret)+1-len(prefix))
				input := pad + secret + strings.Repeat("z", limit)
				want := pad + "[redacted]" + strings.Repeat("z", len(secret)-11) + "…"
				var expected string
				switch limit {
				case 200:
					codexSet(t, native, "version", input)
					expected = fmt.Sprintf("unrecognized Codex version output %q; expected \"codex-cli MAJOR.MINOR.PATCH\" from `codex --version`", want)
				case 300:
					codexSet(t, native, "behavior", "usage-limit")
					codexSet(t, native, "usage_message", input)
					expected = fmt.Sprintf("Codex account usage limit reached: %q; wait for the reset or add credits, then rerun the command", want)
				case 500:
					codexSet(t, native, "behavior", "version-exit")
					codexSet(t, native, "stderr_text", input)
					expected = fmt.Sprintf("Codex read boundary positive control failed: exit status 3: %q", want)
				}
				_, run, err := runCodexWithRuntime(context.Background(), "request", native)
				if err == nil || err.Error() != expected || run.Failure != expected {
					t.Fatal("runtime diagnostic did not match the sanitized prefix exactly")
				}
			})
		}
	}
}

func TestCodexCaptureOverflowDiscardsBothStreams(t *testing.T) {
	const secret = "synthetic-capture-credential.0123456789"
	for _, platform := range codexPlatformsUnderTest {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(platform+"/"+stream, func(t *testing.T) {
				root, opts, proposed := semanticFixture(t)
				opts.Agent = "codex"
				native := codexFixtureFor(t, proposed, platform)
				codexSet(t, native, "behavior", "overflow")
				codexSet(t, native, "capture_stream", stream)
				codexSet(t, native, "capture_secret", secret)
				codexSet(t, native, "rotate_auth", true)
				codexSet(t, native, "rotated", secret)
				before, authBefore := treeAt(t, root), treeAt(t, native.home)
				plan, err := prepareWithProvider(context.Background(), opts, func(ctx context.Context, _ string, request string) (proposal, AgentRun, error) {
					return runCodexWithRuntime(ctx, request, native)
				})
				if err == nil || len(plan.Report.AgentRuns) != 1 {
					t.Fatal("overflow did not refuse terminally")
				}
				run := plan.Report.AgentRuns[0]
				if run.Stdout != "" || run.Stderr != "" || run.Failure != "Codex proposal process failed: Codex output exceeded its byte limit; captured output was discarded; inspect agentRuns stdout/stderr" {
					t.Fatal("overflow retained a stream or a variable diagnostic")
				}
				boundary := run.CredentialBoundary
				if boundary == nil || !boundary.AuthInspected || !boundary.RefreshObserved || !boundary.IsolatedHomeRemoved {
					t.Fatal("overflow skipped post-child credential inspection or cleanup")
				}
				data, marshalErr := json.Marshal(plan.Report)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if strings.Contains(string(data)+err.Error(), secret[:len(secret)-1]) {
					t.Fatal("overflow credential prefix reached public evidence")
				}
				if !matches(before, treeAt(t, root)) || !matches(authBefore, treeAt(t, native.home)) {
					t.Fatal("overflow changed source or configured auth")
				}
				if entries, e := os.ReadDir(native.homeBase); e != nil || len(entries) != 0 {
					t.Fatal("overflow retained private home")
				}
			})
		}
	}
}

func TestCodexCaptureExactLimitsAndSplitWrites(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-c", `import sys
for stream, count in ((sys.stdout, 12582912), (sys.stderr, 4194304)):
    for i in range(count // 4096):
        stream.write('x' * 4096)
    stream.flush()
`)
	stdout, stderr, err := captureProviderCommand(context.Background(), command, maxProviderBytes)
	if err != nil || stdout != strings.Repeat("x", maxProviderBytes) || stderr != strings.Repeat("x", maxProposalBytes) {
		t.Fatal("exact-limit capture did not succeed with complete streams")
	}
	const secret = "synthetic-split-credential.0123456789"
	capture := &boundedCapture{limit: maxProposalBytes}
	for _, fragment := range []string{"before ", secret[:15], secret[15:], " after"} {
		if _, err := capture.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if capture.overflow || RedactCodexSecrets(capture.String(), []string{secret}) != "before [redacted] after" {
		t.Fatal("below-limit split writes lost credential coverage")
	}
}

func TestProviderCaptureCompleteCommands(t *testing.T) {
	for _, tc := range []struct {
		name, script, stdout, stderr string
		exit                         int
	}{
		{"empty", "exit 0", "", "", 0},
		{"success", "printf complete; printf diagnostic >&2", "complete", "diagnostic", 0},
		{"nonzero", "printf complete; printf diagnostic >&2; exit 7", "complete", "diagnostic", 7},
		{"signal", "printf partial; printf partial >&2; kill -TERM $$", "", "", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := captureProviderCommand(context.Background(), exec.Command("/bin/sh", "-c", tc.script), maxProviderBytes)
			if stdout != tc.stdout || stderr != tc.stderr {
				t.Fatal("capture released incorrect streams")
			}
			if tc.exit == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exit {
				t.Fatalf("lost process failure: %v", err)
			}
			if strings.Contains(err.Error(), "capture incomplete") != (tc.exit == -1) {
				t.Fatal("capture completion diagnosis incorrect")
			}
		})
	}
	command := exec.Command(filepath.Join(t.TempDir(), "missing-provider"))
	stdout, stderr, err := captureProviderCommand(context.Background(), command, maxProviderBytes)
	if err == nil || !strings.Contains(err.Error(), "capture incomplete") || stdout != "" || stderr != "" {
		t.Fatal("start failure released output or lost diagnosis")
	}
}

func TestCodexCredentialInspectionFailureIsSticky(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		native := codexFixtureFor(t, proposal{}, platform)
		session, err := newCodexSession(native)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := session.close(); err != nil {
				t.Fatal(err)
			}
		}()
		auth := filepath.Join(session.home, ".codex", "auth.json")
		for _, body := range []string{`{"tokens":{"refresh_token":"refused<credential"}}`, `{"tokens":{"refresh_token":"admitted-credential"}}`} {
			if err := os.WriteFile(auth, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := session.run(context.Background(), 0, session.work, nil, 1024, "/bin/sh", "-c", "printf partial-stream; printf partial-stream >&2")
			if err == nil || stdout != "" || stderr != "" || !session.authFailed {
				t.Fatal("a later successful inspection cleared refusal or retained streams")
			}
		}
	}
}
