// Package setup owns the domain half of the interactive setup flow: detecting
// which agents a project already uses, and writing the answered selections
// into agents.yaml. It holds no prompter and reads no stream, so every
// behavior here is testable without a terminal.
package setup

import (
	"context"
	"fmt"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/claudecode"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/codex"
	"github.com/jbaruch/agentic-context-registry/internal/adapter/cursor"
)

// SupportedAgents lists every adapter a project may select, in stable order.
func SupportedAgents() []string {
	return []string{"claude-code", "codex", "cursor"}
}

// Detect reports which supported agents the project tree already shows
// evidence of, sorted. It writes nothing, and absence of evidence is not an
// error: a project with no agent files detects an empty set.
func Detect(ctx context.Context, project adapter.Snapshot) ([]string, error) {
	adapters := []adapter.Adapter{claudecode.New(), codex.New(), cursor.New()}
	detected := make([]string, 0, len(adapters))
	for _, candidate := range adapters {
		descriptor := candidate.Descriptor()
		detection, err := candidate.Detect(ctx, adapter.DetectRequest{Project: project})
		if err != nil {
			return nil, fmt.Errorf("detect %s: %w; fix the reported project file or pass --agent to select adapters explicitly", descriptor.ID, err)
		}
		if detection.Detected {
			detected = append(detected, descriptor.ID)
		}
	}
	sort.Strings(detected)
	return detected, nil
}
