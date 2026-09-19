package main

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestRequiredCodexFixturesCannotSkip(t *testing.T) {
	for _, missing := range []string{"GOC", "FFA"} {
		for _, value := range []string{"", "skip"} {
			command := exec.Command(os.Args[0], "-test.run=^TestCodexLiveUpstreamConversion$/^"+missing+"$")
			command.Env = append(os.Environ(), "ACR_CODEX_LIVE=1", "ACR_CODEX_LIVE_REQUIRED=1", "ACR_CODEX_LIVE_"+missing+"="+value)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "missing/skipped fixtures refuse") {
				t.Fatalf("required fixture did not fail: %v\n%s", err, output)
			}
		}
	}
}

func TestOriginalCodexAcceptanceCommandsAndSummaries(t *testing.T) {
	want := []string{"python3", "tests/test_github_sh_envelope.py", "--repo", "tesslio/good-oss-citizen", "--issue-number", "13", "--pr-number", "12", "--file-path", "README.md"}
	if len(codexLiveFixtures[0].tests) != 3 || !reflect.DeepEqual(codexLiveFixtures[0].tests[2], want) {
		t.Fatal("original envelope command changed")
	}
	cases := []struct {
		key     string
		index   int
		summary string
		count   int
	}{
		{"GOC", 0, "PASS all 15 classification cases + CLI/input/error checks", 15},
		{"GOC", 1, "All 16 installer-script tests passed", 16},
		{"GOC", 2, "All 23 commands + 1 negative path emitted valid envelopes against tesslio/good-oss-citizen", 23},
		{"FFA", 0, "pyright 1.1.411\n0 errors, 0 warnings, 0 informations\n19/19 passed\n114/114 passed\n51/51 passed\nAll gates passed.", 184},
	}
	for _, c := range cases {
		count, err := originalSuiteCount(c.key, c.index, "PASS incidental\n"+c.summary+"\n")
		if err != nil || count != c.count {
			t.Fatalf("summary count=%d err=%v", count, err)
		}
		if _, err := originalSuiteCount(c.key, c.index, strings.Repeat("PASS incidental\n", 200)); err == nil {
			t.Fatal("PASS markers substituted for original summary")
		}
		if _, err := originalSuiteCount(c.key, c.index, strings.ReplaceAll(c.summary, "15", "14")); c.key == "GOC" && c.index == 0 && err == nil {
			t.Fatal("weakened classification count accepted")
		}
	}
}
