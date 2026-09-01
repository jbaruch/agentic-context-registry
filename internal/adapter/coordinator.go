package adapter

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

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
//
// targetOptions is variadic so every existing caller keeps compiling
// unchanged: omit it for the default (no per-target overrides), or pass
// exactly one map keyed by native target path to request, for example,
// explicit demotion of a specific shared target. It is forwarded verbatim
// to compileOutputs; see TargetOptions.
func (coordinator *Coordinator) Realize(ctx context.Context, project Snapshot, packages []Package, previous realize.Ledger, targetOptions ...map[string]TargetOptions) ([]realize.Intent, error) {
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
		if err := verifyPlanRenderCorrespondence(descriptor, plan, outputs); err != nil {
			return nil, fmt.Errorf("adapter %q: %w", descriptor.ID, err)
		}
		runs = append(runs, adapterRun{adapter: candidate, descriptor: descriptor, plan: plan, outputs: outputs})
	}

	sources := make([]adapterRender, len(runs))
	for index, run := range runs {
		sources[index] = adapterRender{Descriptor: run.descriptor, Outputs: run.outputs}
	}
	intents, err := compileOutputs(project, previous, coordinator.compiler, sources, targetOptions...)
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

// contribution is one (target, kind, mode, owner) tuple an adapter's plan
// promises or its render delivers.
type contribution struct {
	Target string
	Kind   OutputKind
	Mode   fs.FileMode
	Owner  OwnerRef
}

func contributionKey(c contribution) string {
	return strings.Join([]string{
		c.Target, string(c.Kind), fmt.Sprintf("%o", c.Mode),
		c.Owner.Source, c.Owner.ArtifactID, c.Owner.SourcePath, string(c.Owner.Kind), string(c.Owner.Event),
	}, "\x00")
}

func planContributions(plan NativePlan) []contribution {
	contributions := make([]contribution, 0, len(plan.Items))
	for _, item := range plan.Items {
		contributions = append(contributions, contribution{Target: item.Target, Kind: item.Kind, Mode: item.Mode, Owner: item.Owner})
	}
	return contributions
}

func renderContributions(outputs []Output) []contribution {
	var contributions []contribution
	for _, output := range outputs {
		switch output.Kind {
		case OutputGeneratedFile:
			if output.File == nil {
				continue // malformed shape; compileOutputs reports this with a clearer message
			}
			contributions = append(contributions, contribution{Target: output.Target, Kind: output.Kind, Mode: output.Mode, Owner: output.File.Owner})
		case OutputMarkdownInclude:
			for _, insertion := range output.Markdown {
				contributions = append(contributions, contribution{Target: output.Target, Kind: output.Kind, Mode: output.Mode, Owner: insertion.Owner})
			}
		case OutputConfigMerge:
			if output.Config == nil {
				continue
			}
			for _, entry := range output.Config.Entries {
				contributions = append(contributions, contribution{Target: output.Target, Kind: output.Kind, Mode: output.Mode, Owner: entry.Owner})
			}
		default:
			contributions = append(contributions, contribution{Target: output.Target, Kind: output.Kind, Mode: output.Mode})
		}
	}
	return contributions
}

// verifyPlanRenderCorrespondence enforces an exact, per-adapter
// correspondence between what an adapter's Plan promised and what its
// Render actually delivered: every (target, kind, mode, owner) tuple in the
// plan must appear exactly as often in the render, and vice versa. Without
// this, an adapter's Plan with no items could render an arbitrary valid
// output that still gets compiled with no candidate ever reaching its own
// Validate, and another adapter's unrelated output could silently mask a
// planned-but-never-rendered target.
func verifyPlanRenderCorrespondence(descriptor Descriptor, plan NativePlan, outputs []Output) error {
	if plan.Adapter != descriptor {
		return fmt.Errorf("plan stamped for descriptor %+v does not match the registered descriptor %+v", plan.Adapter, descriptor)
	}
	plannedCount := make(map[string]int)
	for _, c := range planContributions(plan) {
		plannedCount[contributionKey(c)]++
	}
	renderedCount := make(map[string]int)
	for _, c := range renderContributions(outputs) {
		renderedCount[contributionKey(c)]++
	}

	var missing, extra []string
	for key, count := range plannedCount {
		if renderedCount[key] < count {
			missing = append(missing, key)
		}
	}
	for key, count := range renderedCount {
		if plannedCount[key] < count {
			extra = append(extra, key)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Errorf("rendered output does not exactly match the plan; missing %d planned contribution(s), %d unplanned extra contribution(s)", len(missing), len(extra))
}
