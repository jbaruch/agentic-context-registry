package adapter_test

import (
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func TestTOMLCommandHookValueCompatibility(t *testing.T) {
	const command = "echo \"quoted\"\\path\nnext\targument"
	const want = `{ hooks = [{ type = "command", command = "echo \"quoted\"\\path\nnext\targument" }] }`
	if got := string(adapter.TOMLCommandHookValue(command)); got != want {
		t.Fatalf("hook value = %s, want %s", got, want)
	}
}

func TestTOMLCommandHookValueWithTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout int
		want    string
	}{
		{"short", 3, `{ hooks = [{ type = "command", timeout = 3, command = "echo \"quoted\"" }] }`},
		{"long", 600, `{ hooks = [{ type = "command", timeout = 600, command = "echo \"quoted\"" }] }`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(adapter.TOMLCommandHookValueWithTimeout(`echo "quoted"`, test.timeout)); got != test.want {
				t.Fatalf("hook value = %s, want %s", got, test.want)
			}
		})
	}
}
