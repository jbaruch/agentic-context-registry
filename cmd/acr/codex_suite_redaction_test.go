package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise Fatalf in a child test process: both exported evidence and test
// diagnostics must redact the synthetic suite credential while preserving failure.
func TestCodexOriginalSuiteFailureRedactsDiagnostics(t *testing.T) {
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestCodexOriginalSuiteFailureRedactionHelper$", "-test.v")
	child.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "ACR_SUITE_FAILURE_FIXTURE=" + root}
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("failing suite was accepted: %s", output)
	}
	evidence, readErr := os.ReadFile(filepath.Join(root, "baseline-test-1.log"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	fake := "synthetic-" + "suite-credential-0123456789"
	for name, body := range map[string]string{"diagnostics": string(output), "artifact": string(evidence)} {
		if strings.Contains(body, fake) || !strings.Contains(body, "stdout-marker [redacted]") || !strings.Contains(body, "stderr-marker [redacted]") {
			t.Fatalf("%s did not safely retain failure output: %s", name, body)
		}
	}
	if !strings.Contains(string(output), "exit status 17") || !strings.Contains(string(output), "FAIL") {
		t.Fatalf("failure signal lost: %s", output)
	}
}

func TestCodexOriginalSuiteFailureRedactionHelper(t *testing.T) {
	root := os.Getenv("ACR_SUITE_FAILURE_FIXTURE")
	if root == "" {
		t.Skip("subprocess fixture entry point")
	}
	fake := "synthetic-" + "suite-credential-0123456789"
	lane := &codexLiveLane{t: t, evidence: root, secrets: []string{fake}}
	fixture := codexLiveFixture{key: "synthetic", tests: [][]string{{"/bin/sh", "-c", `printf 'stdout-marker %s\n' "$GH_TOKEN"; printf 'stderr-marker %s\n' "$GH_TOKEN" >&2; exit 17`}}}
	lane.originalTests(root, fixture, []string{"GH_TOKEN=" + fake}, "baseline")
	t.Fatal("unreachable: failing suite returned")
}
