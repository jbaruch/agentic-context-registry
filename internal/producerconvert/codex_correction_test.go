package producerconvert

import (
	"context"
	"encoding/json"
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

func TestCodexRefreshRedactionOnEveryOutcome(t *testing.T) {
	for _, behavior := range []string{"rotate-secret", "rotate-fail"} {
		t.Run(behavior, func(t *testing.T) {
			native := codexFixture(t, proposal{Edits: []proposedEdit{}, PolicyChanges: []PolicyChange{}})
			const old = "synthetic-old-refresh-credential"
			const refreshed = "synthetic-new-refresh-credential"
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
			if strings.Contains(string(b), refreshed) || strings.Contains(string(b), old) {
				t.Fatal("credential escaped in report")
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
