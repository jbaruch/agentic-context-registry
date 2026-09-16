package cli

import "testing"

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
