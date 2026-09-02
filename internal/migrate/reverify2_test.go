package migrate

import "testing"

const reverify2Dispatcher = `tessl hook run --plugin-path=".tessl/plugins/example/alpha" --event="SessionStart" --agent=claude-code --schema-version=1`

func TestReverify2DispatcherMustBeAtCommandHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "real dispatcher", command: reverify2Dispatcher, want: true},
		{name: "leading whitespace", command: "\t  " + reverify2Dispatcher, want: true},
		{name: "shell prefix", command: `cd "$X" && ` + reverify2Dispatcher, want: false},
		{name: "runner near miss", command: "tessl-hook-runner", want: false},
		{name: "prefixed binary near miss", command: "mytessl hook run", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isTesslCommand(test.command); got != test.want {
				t.Errorf("isTesslCommand(%q) = %t, want %t", test.command, got, test.want)
			}
		})
	}
}

func TestReverify2NonHeadAndNearMissHooksStayPreserved(t *testing.T) {
	t.Parallel()

	commands := []struct {
		name    string
		command string
	}{
		{name: "shell prefix", command: `cd "$X" && ` + reverify2Dispatcher},
		{name: "runner near miss", command: "tessl-hook-runner"},
		{name: "prefixed binary near miss", command: "mytessl hook run"},
	}
	for _, test := range commands {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeTesslJSON(t, root, map[string]string{"example/alpha": "1.0.0"})
			seedAlpha(t, root, alphaPlugin(false, []string{"skills/review-change"}, ""))
			writeJSON(t, root, ".claude/settings.json", map[string]any{
				"hooks": map[string]any{"SessionStart": []any{
					map[string]any{"hooks": []any{map[string]any{
						"type":    "command",
						"command": reverify2Dispatcher,
					}}},
					map[string]any{"hooks": []any{map[string]any{
						"type":    "command",
						"command": test.command,
					}}},
				}},
			})

			report := inventoryProject(t, root)
			if !hasRecord(report.Preserved, ".claude/settings.json", reasonUnmanagedHook) {
				t.Fatalf("hook command %q was treated as Tessl-owned: %#v", test.command, report.Preserved)
			}
		})
	}
}
