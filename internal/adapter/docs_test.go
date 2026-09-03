package adapter_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
)

func TestReadmeSupportedAgentTableMatchesAdapters(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(content)
	adapters := []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()}
	for _, native := range adapters {
		descriptor := native.Descriptor()
		capabilities := make(map[adapter.ArtifactKind]bool)
		for _, kind := range native.SupportedArtifacts() {
			capabilities[kind] = true
		}
		fields := []string{
			"`" + descriptor.ID + "`",
			"`" + descriptor.Version + "`",
			fmt.Sprintf("`%d`", descriptor.Boundary),
			yesNo(capabilities[adapter.ArtifactRule]),
			yesNo(capabilities[adapter.ArtifactSkill]),
			yesNo(capabilities[adapter.ArtifactScript]),
			yesNo(capabilities[adapter.ArtifactHook] && len(native.SupportedEvents()) != 0),
		}
		row := "| " + strings.Join(fields, " | ") + " |"
		if !strings.Contains(readme, row) {
			t.Errorf("README does not contain adapter-derived row %q", row)
		}
	}
	if !strings.Contains(readme, "| **Capability parity** | All three adapters | v1 | Yes | Yes | Yes | Yes |") {
		t.Error("README does not contain the all-adapter capability parity row")
	}

	ids := make([]string, 0, len(adapters))
	for _, native := range adapters {
		ids = append(ids, native.Descriptor().ID)
	}
	sort.Strings(ids)
	if got, want := strings.Join(ids, ","), "claude-code,codex,cursor"; got != want {
		t.Fatalf("registered adapter IDs = %q, want %q", got, want)
	}
}

func yesNo(value bool) string {
	if value {
		return "Yes"
	}
	return "No"
}
