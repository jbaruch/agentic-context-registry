package realizeapp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// UninstallResult reports one removed dependency and the realization pass that
// removed its owned artifacts.
type UninstallResult struct {
	Source            string                         `json:"source"`
	Changed           bool                           `json:"changed"`
	Removed           *dependency.LockedDependency   `json:"removed,omitempty"`
	Agents            []string                       `json:"agents"`
	Plan              realize.Plan                   `json:"plan"`
	VendorTreeRemoval *realize.VendorTreeRemovalPlan `json:"vendorTreeRemoval,omitempty"`
	Notices           []adapter.Notice               `json:"notices,omitempty"`
}

// RemainingPackagesError reports that re-rendering the packages surviving an
// uninstall needs sources this run could not reach. Only the last dependency
// can be uninstalled fully offline.
type RemainingPackagesError struct {
	Source    string
	Remaining int
	Err       error
}

// Error names the sources the operator has to reach and the command to retry.
func (err *RemainingPackagesError) Error() string {
	return fmt.Sprintf("cannot re-render the %d package(s) that remain after uninstalling %s: %v; re-run 'acr uninstall %s' with access to those sources",
		err.Remaining, err.Source, err.Err, err.Source)
}

// Unwrap exposes the materialization failure.
func (err *RemainingPackagesError) Unwrap() error {
	return err.Err
}

// VendorTreeStillReferencedError refuses a destructive cleanup while another
// declaration resolves to the same ACR-owned vendor directory.
type VendorTreeStillReferencedError struct {
	Source string
	Path   string
}

// Error identifies the declaration and tree that must remain.
func (err *VendorTreeStillReferencedError) Error() string {
	return fmt.Sprintf("vendor tree %s is still referenced after pruning %s; keep the tree and reconcile duplicate declarations before retrying", err.Path, err.Source)
}

// Uninstall drops one declaration and its lock row, then realizes the pruned
// state. Removal is the planner's ordinary work: a generated-only target the
// pruned intent set no longer wants is deleted, and a shared target keeps its
// other owners and loses only this package's entries. No code here deletes a
// file.
//
// Removal keys on the declaration, never on a ledger source match: the
// session-start hook is contributed under this repository's own source, so
// matching ledger entries by source would delete the freshness hook of any
// project that also declares it.
//
// Uninstall accepts no --agent. It realizes across the union of the declared
// agents and every agent recorded in the ownership ledger, so a narrowed
// selection cannot leave another agent's outputs behind.
func (service *Service) Uninstall(ctx context.Context, projectDirectory, source string, dryRun bool) (UninstallResult, error) {
	scheme, err := dependency.SourceScheme(source)
	if err != nil {
		return UninstallResult{}, err
	}
	state, err := dependency.LoadState(projectDirectory)
	if err != nil {
		return UninstallResult{}, err
	}
	pruned, removed, err := dependency.PruneDependency(state, source)
	if err != nil {
		return UninstallResult{}, err
	}
	var vendorRemoval *realize.VendorTreeRemovalPlan
	if scheme == dependency.SchemeVendor {
		identity, err := dependency.ParseVendorSource(source)
		if err != nil {
			return UninstallResult{}, err
		}
		vendorPath := fmt.Sprintf(".agents/vendor/%s/%s", identity.Workspace, identity.Package)
		if vendorTreeReferenced(pruned.Project.Dependencies, identity) {
			return UninstallResult{}, &VendorTreeStillReferencedError{Source: source, Path: vendorPath}
		}
		plan, err := realize.PlanVendorTreeRemoval(projectDirectory, vendorPath)
		if err != nil {
			return UninstallResult{}, err
		}
		vendorRemoval = &plan
	}
	agents, err := coveredAgents(state)
	if err != nil {
		return UninstallResult{}, err
	}
	result := UninstallResult{Source: source, Changed: !reflect.DeepEqual(state, pruned), Removed: removed, Agents: agents, VendorTreeRemoval: vendorRemoval}
	if len(agents) == 0 {
		// Nothing selects an adapter and nothing was ever realized, so the prune
		// is the whole operation and it reaches no network at all.
		if err := service.settle(projectDirectory, pruned, dryRun); err != nil {
			return UninstallResult{}, err
		}
		return result, applyVendorRemoval(projectDirectory, vendorRemoval, dryRun)
	}

	mode := realize.ModeApply
	if dryRun {
		mode = realize.ModeDryRun
	}
	realized, err := service.RunStateFrom(ctx, projectDirectory, state, pruned, agents, mode)
	if err != nil {
		var materialization *MaterializationError
		if errors.As(err, &materialization) {
			return UninstallResult{}, &RemainingPackagesError{Source: source, Remaining: len(pruned.Lock.Dependencies), Err: materialization.Err}
		}
		return UninstallResult{}, err
	}
	result.Plan, result.Notices = realized.Plan, realized.Notices
	result.Changed = result.Changed || realized.Plan.HasChanges()
	if realized.Plan.HasChanges() {
		// The engine's transactional finalizer already wrote the pruned state
		// alongside the next ownership ledger.
		return result, applyVendorRemoval(projectDirectory, vendorRemoval, dryRun)
	}
	if err := service.settle(projectDirectory, pruned, dryRun); err != nil {
		return UninstallResult{}, err
	}
	return result, applyVendorRemoval(projectDirectory, vendorRemoval, dryRun)
}

func applyVendorRemoval(projectDirectory string, plan *realize.VendorTreeRemovalPlan, dryRun bool) error {
	if plan == nil || dryRun {
		return nil
	}
	return realize.ApplyVendorTreeRemoval(projectDirectory, *plan)
}

func vendorTreeReferenced(declarations []dependency.Declaration, identity dependency.VendorIdentity) bool {
	for _, declaration := range declarations {
		if scheme, err := dependency.SourceScheme(declaration.Source); err != nil || scheme != dependency.SchemeVendor {
			continue
		}
		candidate, err := dependency.ParseVendorSource(declaration.Source)
		if err == nil && candidate == identity {
			return true
		}
	}
	return false
}

// settle persists the pruned state when the realization pass planned no change
// and therefore never reached the engine's finalizer. A dry run writes nothing,
// including no state write.
func (service *Service) settle(projectDirectory string, pruned dependency.State, dryRun bool) error {
	if dryRun {
		return nil
	}
	return service.persistState(projectDirectory, pruned)
}

// coveredAgents is the sorted union of the agents agents.yaml selects and every
// agent recorded in the ownership ledger.
func coveredAgents(state dependency.State) ([]string, error) {
	ledger, err := realize.DecodeLedger(state.Lock.Realization)
	if err != nil {
		return nil, err
	}
	covered := make(map[string]struct{}, len(state.Project.Agents))
	for _, agentID := range state.Project.Agents {
		covered[agentID] = struct{}{}
	}
	for _, target := range ledger.Targets {
		for _, entry := range target.Entries {
			covered[entry.Adapter] = struct{}{}
		}
	}
	agents := make([]string, 0, len(covered))
	for agentID := range covered {
		agents = append(agents, agentID)
	}
	sort.Strings(agents)
	return agents, nil
}
