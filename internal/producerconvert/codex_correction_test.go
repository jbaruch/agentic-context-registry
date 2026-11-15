package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCodexRequiredDeprecatedControls(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, enabled := range []bool{false, true} {
			t.Run(platform+"/"+map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
				native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
				rows := make([][]any, len(codexDefaultFeatures))
				copy(rows, codexDefaultFeatures)
				for i, row := range rows {
					if row[0] == "shell_tool" {
						rows[i] = []any{"shell_tool", "deprecated", enabled}
					}
				}
				codexSet(t, native, "features", rows)
				_, run, err := runCodexWithRuntime(context.Background(), "request", native)
				if (err != nil) != enabled {
					t.Fatalf("enabled=%t error=%v", enabled, err)
				}
				if enabled && len(run.Arguments) != 0 {
					t.Fatal("source delivered despite enabled required control")
				}
			})
		}
	}
}

func TestCodexMissingSourceHomeWithEnvironmentCredential(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		t.Run(platform, func(t *testing.T) {
			native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
			if err := os.Remove(native.home); err != nil {
				t.Fatal(err)
			}
			native.environ = append(native.environ, "CODEX_API_KEY=synthetic-environment-credential")
			if _, _, err := runCodexWithRuntime(context.Background(), "request", native); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(native.home); !os.IsNotExist(err) {
				t.Fatalf("source home created: %v", err)
			}
		})
	}
}

func TestCodexValidatesRawProposalBeforeRedaction(t *testing.T) {
	for _, text := range []string{"sk-example-not-a-secret", "401 Unauthorized is documented source text"} {
		for _, mismatch := range []bool{false, true} {
			t.Run(text+map[bool]string{true: "/different", false: "/equal"}[mismatch], func(t *testing.T) {
				proposed := proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{{Path: "README.md", From: text, To: "ordinary text"}}}
				native := codexFixture(t, proposed)
				codexSet(t, native, "reasoning_text", "Source text may explain 401 Unauthorized without an authentication failure")
				if mismatch {
					changed := proposed
					changed.PolicyChanges = []PolicyChange{{Path: "README.md", From: "sk-[redacted]", To: "ordinary text"}}
					b, err := json.Marshal(changed)
					if err != nil {
						t.Fatal(err)
					}
					codexSet(t, native, "file_proposal", string(b))
				}
				got, run, err := runCodexWithRuntime(context.Background(), "request", native)
				if (err != nil) != mismatch {
					t.Fatalf("mismatch=%t error=%v", mismatch, err)
				}
				if !mismatch && got.PolicyChanges[0].From != text {
					t.Fatal("parser input changed")
				}
				if strings.Contains(run.Stdout, "sk-example-not-a-secret") {
					t.Fatal("report not redacted")
				}
			})
		}
	}
}

func TestCodexSecretRedactionDoesNotAmplify(t *testing.T) {
	for _, secret := range []string{"e", "d"} {
		t.Run(secret, func(t *testing.T) {
			guard := credentialGuard{""}
			for i := 0; i < 9; i++ {
				guard = append(guard, secret)
			}
			input := strings.Repeat(secret, 1024)
			want := "[redacted]"
			if got := RedactCodexSecrets(input, guard); got != want {
				t.Fatalf("redacted %d input bytes into %d bytes; want %d", len(input), len(got), len(want))
			}
			report := Report{AgentRuns: []AgentRun{{Stdout: input, Stderr: input, Failure: input, Warnings: []string{input}}}}
			var err error = errors.New(input)
			for i := 0; i < 10; i++ {
				report = guard.sanitizeReport(report)
				err = guard.sanitizeError(err)
				run := report.AgentRuns[0]
				for _, got := range []string{run.Stdout, run.Stderr, run.Failure, run.Warnings[0], err.Error()} {
					if got != want {
						t.Fatalf("sanitization %d produced %d bytes; want %d", i+1, len(got), len(want))
					}
				}
			}
		})
	}
}

func TestCodexSecretRedactionCoverage(t *testing.T) {
	const mask = "[redacted]"
	key := "sk-" + strings.Repeat("A", 16)
	for _, tc := range []struct {
		name, input, want string
		secrets           []string
	}{
		{"boundary cascade", "dede", mask + "e", []string{"e[", "d"}},
		{"boundary absorbs prefix", "ed", mask, []string{"e[", "d"}},
		{"shifted overlap", "x=abcdef", "x=" + mask, []string{"ab", "bcdef"}},
		{"cross existing mask", mask + "abc", mask, []string{"]abc"}},
		{"self overlap", "aaa", mask, []string{"aa"}},
		{"adjacent masks", mask + mask, mask, nil},
		{"longer value containing mask", "x=pre" + mask + "post!", "x=" + mask + "!", []string{"pre" + mask + "post", "pre"}},
		{"partial mask cascade", "e" + mask, mask, []string{"e[", "d"}},
		{"protected mask interior", "e" + mask, "e" + mask, []string{"d", mask}},
		{"repeated boundary", strings.Repeat("e"+mask, 16), mask, []string{"e[", "d"}},
		{"regex with s", "key " + key, "key " + mask, []string{"s"}},
		{"regex with dash", "key " + key, "key " + mask, []string{"-"}},
		{"regex enabled at boundary", "x" + key, mask, []string{"x"}},
		{"regex leading delimiter", "!" + key + "?", "!" + mask + "?", nil},
		{"unrelated bytes", "a\x00β!", "a\x00β!", []string{""}},
		{"empty input", "", "", []string{"", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				secrets := append([]string(nil), tc.secrets...)
				if reverse {
					for i, j := 0, len(secrets)-1; i < j; i, j = i+1, j-1 {
						secrets[i], secrets[j] = secrets[j], secrets[i]
					}
				}
				secrets = append(secrets, "")
				secrets = append(secrets, tc.secrets...)
				got := RedactCodexSecrets(tc.input, secrets)
				if got != tc.want {
					t.Fatalf("reverse=%t: got %q, want %q", reverse, got, tc.want)
				}
				if len(got) > 10*len(tc.input) {
					t.Fatalf("output %d exceeds 10x input %d", len(got), len(tc.input))
				}
				for i := 0; i < 16; i++ {
					if next := RedactCodexSecrets(got, secrets); next != got {
						t.Fatalf("pass %d changed %q to %q", i+2, got, next)
					}
				}
			}
		})
	}
}

func TestCodexSecretRedactionGeneratedOverlaps(t *testing.T) {
	// Exhaust a fixed alphabet of exposed bytes and reserved markers. These
	// cases exercise both mask edges, multiple regions and changing adjacency.
	units := []string{"a", "e", "d", "[", "]", "[redacted]", "sk-" + strings.Repeat("A", 6)}
	sets := [][]string{
		{"e[", "d"}, {"a[", "d"}, {"aa", "ae", "ed"},
		{"]a", "e[", "d]"}, {"[r", "d"}, {"d]a", "a"},
		{"s", "-"}, {"A", "sk-"}, {"a[redacted]e", "d"},
	}
	for _, a := range units {
		for _, b := range units {
			for _, c := range units {
				for _, d := range units {
					input := a + b + c + d
					for _, secrets := range sets {
						got := RedactCodexSecrets(input, secrets)
						if len(got) > 10*len(input) {
							t.Fatalf("output bound: input=%q secrets=%q output=%q", input, secrets, got)
						}
						if next := RedactCodexSecrets(got, secrets); next != got {
							t.Fatalf("not stable: input=%q secrets=%q first=%q next=%q", input, secrets, got, next)
						}
						reversed := make([]string, len(secrets))
						for i := range secrets {
							reversed[i] = secrets[len(secrets)-1-i]
						}
						if other := RedactCodexSecrets(input, reversed); other != got {
							t.Fatalf("order changed: input=%q secrets=%q first=%q reversed=%q", input, secrets, got, other)
						}
					}
				}
			}
		}
	}
}

func TestCodexSecretRedactionBoundaryChain(t *testing.T) {
	input := strings.Repeat("a", 1<<20) + "d"
	if got := RedactCodexSecrets(input, []string{"a[", "d"}); got != "[redacted]" {
		t.Fatalf("boundary chain produced %d bytes, want one mask", len(got))
	}
}

func BenchmarkCodexSecretRedactionBoundaryChain(b *testing.B) {
	for _, size := range []int{1 << 10, 1 << 15, 1 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			input := strings.Repeat("a", size) + "d"
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := RedactCodexSecrets(input, []string{"a[", "d"}); got != "[redacted]" {
					b.Fatalf("boundary chain produced %d bytes", len(got))
				}
			}
		})
	}
}

func TestCodexMaskSensitiveRefreshRedactionOnEveryOutcome(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, initial := range []string{"e", "d", "e[", "ab"} {
			refreshed := "d"
			if initial == "d" {
				refreshed = "e"
			}
			if initial == "ab" {
				refreshed = "bcdef"
			}
			for _, behavior := range []string{"rotate-secret", "rotate-fail"} {
				t.Run(platform+"/"+initial+"/"+behavior, func(t *testing.T) {
					native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
					// Register the initial value repeatedly, then collect both values
					// through the real startup and refreshed-auth inspection paths.
					auth, marshalErr := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": initial, "refresh_token": initial}})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					put(t, native.home, "auth.json", string(auth), 0600)
					before := read(t, native.home, "auth.json")
					native.environ = append(native.environ, "CODEX_API_KEY="+initial)
					codexSet(t, native, "behavior", behavior)
					codexSet(t, native, "rotated", refreshed)
					wantStderr := "[redacted]\n"
					boundaryText := ""
					if initial == "e[" {
						boundaryText = "e[redacted]"
					}
					if initial == "ab" {
						boundaryText = "abcdef"
					}
					if boundaryText != "" {
						codexSet(t, native, "reasoning_text", boundaryText)
						codexSet(t, native, "stderr_text", boundaryText)
						wantStderr += "[redacted]\n"
					}
					_, run, err := runCodexWithRuntime(context.Background(), "request", native)
					if (err != nil) != (behavior == "rotate-fail") {
						t.Fatalf("error=%v", err)
					}
					if run.Stderr != wantStderr || len(run.Stdout) > maxProposalBytes {
						t.Fatalf("unexpected redacted output sizes: stdout=%d stderr=%d", len(run.Stdout), len(run.Stderr))
					}
					if boundaryText != "" && behavior == "rotate-secret" && !strings.Contains(run.Stdout, `"text": "[redacted]"`) {
						t.Fatal("reasoning text did not become one complete mask")
					}
					var boundaryErr error
					if boundaryText != "" {
						boundaryErr = run.guard.sanitizeError(errors.New(boundaryText))
						if boundaryErr.Error() != "[redacted]" {
							t.Fatalf("collected guard left boundary error %q", boundaryErr.Error())
						}
					}
					report := Report{AgentRuns: []AgentRun{run}}
					data, marshalErr := json.Marshal(report)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					message := ""
					if err != nil {
						message = err.Error()
					}
					for i := 0; i < 5; i++ {
						report = run.guard.sanitizeReport(report)
						err = run.guard.sanitizeError(err)
						if boundaryErr != nil {
							boundaryErr = run.guard.sanitizeError(boundaryErr)
							if boundaryErr.Error() != "[redacted]" {
								t.Fatalf("boundary error changed on sanitization %d", i+1)
							}
						}
						next, marshalErr := json.Marshal(report)
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						if string(next) != string(data) || (err != nil && err.Error() != message) {
							t.Fatalf("report or error changed on sanitization %d", i+1)
						}
					}
					if read(t, native.home, "auth.json") != before {
						t.Fatal("operator credential changed")
					}
				})
			}
		}
	}
}

func TestCodexCredentialCollectionRefusal(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, suffix := range []string{"é", "\"", "\\", "\x01", "\n", "\x7f", "<", ">", "&", "\"<>&", " "} {
			for _, field := range []string{"OPENAI_API_KEY", "id_token", "access_token", "refresh_token", "CODEX_API_KEY"} {
				for _, outcome := range []string{"initial", "rotate-secret", "rotate-fail"} {
					if field == "CODEX_API_KEY" && outcome != "initial" {
						continue
					}
					t.Run(platform+"/"+fmt.Sprintf("%x", suffix)+"/"+field+"/"+outcome, func(t *testing.T) {
						root, opts, proposed := semanticFixture(t)
						opts.Agent = "codex"
						native := codexFixtureFor(t, proposed, platform)
						value := "synthetic-refused-credential-" + suffix
						document := map[string]any{"tokens": map[string]string{field: value}}
						if field == "OPENAI_API_KEY" {
							document = map[string]any{field: value}
						}
						auth, err := json.Marshal(document)
						if err != nil {
							t.Fatal(err)
						}
						if outcome == "initial" {
							if field == "CODEX_API_KEY" {
								native.environ = append(native.environ, "CODEX_API_KEY="+value)
								auth = []byte(`{"tokens":{"access_token":"synthetic-admitted-initial"}}`)
							}
						} else {
							codexSet(t, native, "rotated_auth", document)
							codexSet(t, native, "rotated", value)
							codexSet(t, native, "behavior", outcome)
							codexSet(t, native, "reasoning_text", "credential="+value)
							auth = []byte(`{"tokens":{"access_token":"synthetic-admitted-initial"}}`)
						}
						put(t, native.home, "auth.json", string(auth), 0600)
						before, authBefore := treeAt(t, root), treeAt(t, native.home)
						plan, runErr := prepareWithProvider(context.Background(), opts, func(ctx context.Context, _ string, request string) (proposal, AgentRun, error) {
							return runCodexWithRuntime(ctx, request, native)
						})
						if runErr == nil || len(plan.Report.AgentRuns) != 1 {
							t.Fatal("credential collection did not refuse terminally")
						}
						run := plan.Report.AgentRuns[0]
						if run.Stdout != "" || run.Stderr != "" {
							t.Fatal("refused credentials retained captured streams")
						}
						if outcome == "initial" && len(codexSpawns(t, native)) != 0 {
							t.Fatal("initial credential refusal spawned a child")
						}
						if outcome != "initial" {
							boundary := run.CredentialBoundary
							if boundary == nil || boundary.AuthInspected || !boundary.ReportSanitized || !boundary.IsolatedHomeRemoved {
								t.Fatal("refreshed refusal lacks discard/cleanup evidence")
							}
						}
						data, err := json.Marshal(plan.Report)
						if err != nil {
							t.Fatal(err)
						}
						encoded, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						public := string(data) + FormatText(plan.Report) + runErr.Error()
						if strings.Contains(public, value) || strings.Contains(public, string(encoded[1:len(encoded)-1])) {
							t.Fatal("refused credential reached report or error")
						}
						if !matches(before, treeAt(t, root)) || !matches(authBefore, treeAt(t, native.home)) {
							t.Fatal("refusal changed source or configured auth bytes/modes")
						}
						entries, err := os.ReadDir(native.homeBase)
						if err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
							t.Fatal("refusal retained private home")
						}
					})
				}
			}
		}
	}
}

func TestCodexRefreshRedactionOnEveryOutcome(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, refreshed := range []string{"synthetic-new-refresh-credential", "tiny", "x", "tiny-refreshed-credential-value"} {
			for _, behavior := range []string{"rotate-secret", "rotate-fail"} {
				t.Run(platform+"/"+refreshed+"/"+behavior, func(t *testing.T) {
					native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
					old := "synthetic-old-refresh-credential"
					if refreshed == "tiny-refreshed-credential-value" {
						old = "tiny"
					}
					put(t, native.home, "auth.json", `{"tokens":{"refresh_token":"`+old+`"}}`, 0600)
					before := read(t, native.home, "auth.json")
					codexSet(t, native, "behavior", behavior)
					codexSet(t, native, "rotated", refreshed)
					_, run, err := runCodexWithRuntime(context.Background(), "request", native)
					if (err != nil) != (behavior == "rotate-fail") {
						t.Fatalf("error=%v", err)
					}
					b, e := json.Marshal(run)
					if e != nil {
						t.Fatal(e)
					}
					if strings.Contains(string(b), refreshed) || strings.Contains(string(b), old) || run.Stderr != "[redacted]\n" {
						t.Fatal("credential escaped in report")
					}
					if err != nil && strings.Contains(err.Error(), refreshed) {
						t.Fatal("refreshed credential escaped in error")
					}
					boundary := run.CredentialBoundary
					if boundary == nil || !boundary.AuthInspected || !boundary.RefreshObserved || !boundary.ReportSanitized || !boundary.IsolatedHomeRemoved {
						t.Fatal("credential boundary evidence missing")
					}
					if len(run.Warnings) == 0 {
						t.Fatal("missing rotation warning")
					}
					if read(t, native.home, "auth.json") != before {
						t.Fatal("operator credential changed")
					}
					if entries, e := os.ReadDir(native.homeBase); e != nil || len(entries) != 0 {
						t.Fatalf("private home retained: %v", e)
					}
				})
			}
		}
	}
}

func TestCodexInitialShortCredentialRedaction(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, behavior := range []string{"env-echo", "tool"} {
			t.Run(platform+"/"+behavior, func(t *testing.T) {
				native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
				const secret = "tiny"
				put(t, native.home, "auth.json", `{"tokens":{"access_token":"`+secret+`","account_id":"account-metadata"}}`, 0600)
				codexSet(t, native, "behavior", behavior)
				codexSet(t, native, "reasoning_text", "credential "+secret+" account-metadata")
				native.environ = append(native.environ, "CODEX_API_KEY=brief")
				_, run, err := runCodexWithRuntime(context.Background(), "request", native)
				if (err != nil) != (behavior == "tool") {
					t.Fatalf("error=%v", err)
				}
				data, marshalErr := json.Marshal(Report{AgentRuns: []AgentRun{run}})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if strings.Contains(string(data), secret) || strings.Contains(string(data), "brief") || !strings.Contains(run.Stdout, "credential [redacted] account-metadata") {
					t.Fatal("short credential leaked or metadata lost in diagnostic/report")
				}
				if behavior == "env-echo" && !strings.Contains(run.Stderr, "CODEX_API_KEY=[redacted]") {
					t.Fatal("short environment credential leaked in stderr")
				}
			})
		}
	}
}

func TestCodexRejectsEmptyProxyHostnameBeforeSpawn(t *testing.T) {
	proxies := []string{
		"http://:8080", "http://:", "socks5://:1080",
		"http://[]", "http://[]:8080",
		"http:// ", "http://%20",
		"http://\u00a0", "http://\u3000",
		"http://%C2%A0", "http://%E3%80%80",
		"http://\u00a0\u3000:8080",
	}
	for _, platform := range codexPlatformsUnderTest {
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			for _, proxy := range proxies {
				t.Run(platform+"/"+name+"/"+proxy, func(t *testing.T) {
					native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
					codexSet(t, native, "behavior", "env-echo")
					native.environ = append(native.environ, name+"="+proxy)
					_, run, err := runCodexWithRuntime(context.Background(), "request", native)
					want := name + " must be a valid proxy URL with a host and no credentials; set an http, https, socks4, socks4a, socks5 or socks5h URL without userinfo, or unset " + name + " before retrying"
					if err == nil || err.Error() != want {
						t.Fatal("missing actionable variable-only proxy refusal")
					}
					if len(codexSpawns(t, native)) != 0 || codexStdinRead(native) {
						t.Fatal("empty proxy hostname reached a child")
					}
					data, marshalErr := json.Marshal(Report{AgentRuns: []AgentRun{run}})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					if strings.Contains(err.Error(), proxy) || strings.Contains(string(data), proxy) {
						t.Fatal("proxy value escaped into diagnostic or report")
					}
				})
			}
		}
	}
}

func TestCodexRejectsProxyCredentialsBeforeSpawn(t *testing.T) {
	var proxies []string
	for _, userinfo := range []string{"example-user:example-pass", "example%2Duser:example%2Dpass", "example-user", ":example-pass", "example-user:bad%escape"} {
		proxies = append(proxies, "http://"+userinfo+"@proxy.example:8080", "http:///"+userinfo+"@proxy.example")
	}
	proxies = append(proxies, "http://", "http:/proxy.example", "//proxy.example", "proxy.example:8080", "file://proxy.example", "ftp://proxy.example", "http://proxy.example:bad", "http://proxy.example/%zz")
	for _, platform := range codexPlatformsUnderTest {
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			for _, proxy := range proxies {
				t.Run(platform+"/"+name+"/"+proxy, func(t *testing.T) {
					native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
					codexSet(t, native, "behavior", "env-echo")
					native.environ = append(native.environ, name+"="+proxy)
					_, run, err := runCodexWithRuntime(context.Background(), "request", native)
					if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "unset "+name) {
						t.Fatal("missing actionable variable-only proxy refusal")
					}
					if len(codexSpawns(t, native)) != 0 || codexStdinRead(native) {
						t.Fatal("proxy credential reached a child")
					}
					data, marshalErr := json.Marshal(Report{AgentRuns: []AgentRun{run}})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					for _, private := range []string{proxy, "example-user", "example-pass", "example%2Duser", "example%2Dpass", "bad%escape", "proxy.example"} {
						if strings.Contains(err.Error(), private) || strings.Contains(string(data), private) {
							t.Fatal("proxy value escaped into diagnostic or report")
						}
					}
				})
			}
		}
	}
}

func TestCodexForwardsCredentialFreeProxies(t *testing.T) {
	for _, platform := range codexPlatformsUnderTest {
		for _, scheme := range []string{"http", "https", "socks4", "socks4a", "socks5", "socks5h"} {
			t.Run(platform+"/"+scheme, func(t *testing.T) {
				native := codexFixtureFor(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}}, platform)
				codexSet(t, native, "behavior", "env-echo")
				for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
					native.environ = append(native.environ, name+"="+scheme+"://proxy.example:8080")
				}
				_, run, err := runCodexWithRuntime(context.Background(), "request", native)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
					if !strings.Contains(run.Stderr, name+"="+scheme+"://proxy.example:8080") {
						t.Fatalf("%s was not forwarded", name)
					}
				}
			})
		}
	}
}
