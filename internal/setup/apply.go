package setup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

// Selection is the answered init selection.
type Selection struct {
	Agents    []string
	Freshness string
}

// Result reports what the project declares after Apply.
type Result struct {
	Changed   bool     `json:"changed"`
	Agents    []string `json:"agents"`
	Freshness string   `json:"freshness"`
}

// Configured reports whether the project already has an agents.yaml. Absence,
// not an empty agents list, is what makes a project unconfigured: a project
// that deliberately selected nothing must not be asked again on every install.
// Anything present but not a regular file is left for LoadState to refuse.
func Configured(root string) (bool, error) {
	info, err := os.Lstat(filepath.Join(root, dependency.ProjectFilename))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// Stored returns the selections the project already records. A project with no
// agents.yaml reports an empty selection.
func Stored(root string) (Selection, error) {
	state, err := dependency.LoadState(root)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Agents: append([]string(nil), state.Project.Agents...), Freshness: state.Project.Freshness}, nil
}

// Apply writes selection through the ordinary dependency-state transaction, so
// every other field — dependencies, holds, and unknown top-level keys other
// owners add — is preserved. Re-applying the same selection reports no change
// and writes nothing, and a dry run never writes.
func Apply(root string, selection Selection, dryRun bool) (Result, error) {
	state, err := dependency.LoadState(root)
	if err != nil {
		return Result{}, err
	}
	agents := append([]string(nil), selection.Agents...)
	sort.Strings(agents)
	result := Result{
		Changed:   !reflect.DeepEqual(state.Project.Agents, agents) || state.Project.Freshness != selection.Freshness,
		Agents:    agents,
		Freshness: selection.Freshness,
	}
	if !result.Changed || dryRun {
		return result, nil
	}
	state.Project.Agents = agents
	state.Project.Freshness = selection.Freshness
	if err := dependency.WriteState(root, state); err != nil {
		return Result{}, err
	}
	return result, nil
}
