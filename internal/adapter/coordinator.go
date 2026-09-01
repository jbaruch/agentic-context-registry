package adapter

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Coordinator is a library that resolves a fixed set of adapters against
// resolved packages into a realize.Intent set, without ever calling
// realize.Engine itself. Selecting which adapters a project targets, and
// persisting that selection, is outside #10's scope.
type Coordinator struct {
	adapters []Adapter
	compiler SharedCompiler
}

// NewCoordinator validates adapters via Register and returns a Coordinator
// that compiles their output through compiler. compiler may be nil: outputs
// requiring it fail closed rather than silently skip preservation.
func NewCoordinator(compiler SharedCompiler, adapters ...Adapter) (*Coordinator, error) {
	registered, err := Register(adapters...)
	if err != nil {
		return nil, err
	}
	return &Coordinator{adapters: registered, compiler: compiler}, nil
}

type adapterRun struct {
	adapter    Adapter
	descriptor Descriptor
	plan       NativePlan
	outputs    []Output
}

// Realize runs the full adapter boundary: a global capability preflight,
// each adapter's Plan and Render, the trusted compile into realize.Intent,
// and each adapter's Validate over its own compiled candidates. It returns
// intents only when every stage succeeds; the caller must never invoke
// realize.Engine.Run(ModeApply, ...) when Realize returns an error.
func (coordinator *Coordinator) Realize(ctx context.Context, project Snapshot, packages []Package, previous realize.Ledger) ([]realize.Intent, error) {
	if combinations := unsupportedCombinations(coordinator.adapters, packages); len(combinations) != 0 {
		return nil, &UnsupportedError{Combinations: combinations}
	}

	runs := make([]adapterRun, 0, len(coordinator.adapters))
	for _, candidate := range coordinator.adapters {
		descriptor := candidate.Descriptor()
		plan, err := candidate.Plan(ctx, PlanRequest{Project: project, Packages: packages, Previous: previous})
		if err != nil {
			return nil, fmt.Errorf("adapter %q plan: %w", descriptor.ID, err)
		}
		outputs, err := candidate.Render(ctx, RenderRequest{Packages: packages, Plan: plan})
		if err != nil {
			return nil, fmt.Errorf("adapter %q render: %w", descriptor.ID, err)
		}
		runs = append(runs, adapterRun{adapter: candidate, descriptor: descriptor, plan: plan, outputs: outputs})
	}

	sources := make([]adapterRender, len(runs))
	for index, run := range runs {
		sources[index] = adapterRender{Descriptor: run.descriptor, Outputs: run.outputs}
	}
	intents, err := compileOutputs(project, previous, coordinator.compiler, sources)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]realize.Intent, len(intents))
	for _, intent := range intents {
		byPath[intent.Path] = intent
	}

	for _, run := range runs {
		candidates := make([]CandidateFile, 0, len(run.plan.Items))
		for _, item := range run.plan.Items {
			intent, ok := byPath[item.Target]
			if !ok {
				return nil, fmt.Errorf("adapter %q planned target %q was never compiled", run.descriptor.ID, item.Target)
			}
			candidates = append(candidates, CandidateFile{
				Path: intent.Path, Content: intent.Content, Mode: fs.FileMode(intent.Mode), Ownership: intent.Ownership,
			})
		}
		if err := run.adapter.Validate(ctx, ValidateRequest{Plan: run.plan, Files: candidates}); err != nil {
			return nil, fmt.Errorf("adapter %q validate: %w", run.descriptor.ID, err)
		}
	}

	sort.Slice(intents, func(left, right int) bool { return intents[left].Path < intents[right].Path })
	return intents, nil
}
