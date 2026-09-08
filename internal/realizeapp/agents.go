package realizeapp

import (
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// MixedAdapterTargetError reports one ledger target owned by both selected and
// omitted agents. Today's adapters own disjoint native trees, so realization
// cannot produce such a target; a hand-edited ledger still can, and scoping it
// either way would drop ownership entries, so the invocation fails closed.
type MixedAdapterTargetError struct {
	Path string
}

// Error names the invocation that plans the whole ledger instead.
func (err *MixedAdapterTargetError) Error() string {
	return fmt.Sprintf("realization target %q is owned by both the selected and the omitted agents; re-run without --agent", err.Path)
}

// splitLedger partitions previous into the targets the selected agents own and
// the targets belonging wholly to agents this invocation omits. The planner
// compares against scoped alone, so an --agent subset stops seeing the omitted
// agents' targets as unwanted; carried is merged back into the ledger the
// finalizer persists. A target with no entries cannot exist in a validated
// ledger and is scoped.
//
// A coordinator-owned target belongs to no single agent, so the per-entry
// adapter partition cannot classify it and would report every shared-surface
// project as a MixedAdapterTargetError. ownsShared decides it instead: an
// invocation covering the whole configured selection scopes the surface and
// re-derives it, and a temporary --agent subset carries it through untouched.
func splitLedger(previous realize.Ledger, selected []string, ownsShared bool) (realize.Ledger, realize.Ledger, error) {
	chosen := make(map[string]struct{}, len(selected))
	for _, agentID := range selected {
		chosen[agentID] = struct{}{}
	}
	scoped := realize.Ledger{SchemaVersion: previous.SchemaVersion}
	carried := realize.Ledger{SchemaVersion: previous.SchemaVersion}
	for _, target := range previous.Targets {
		if target.Owner == realize.OwnerCoordinator {
			if ownsShared {
				scoped.Targets = append(scoped.Targets, target)
			} else {
				carried.Targets = append(carried.Targets, target)
			}
			continue
		}
		selectedEntries, omittedEntries := 0, 0
		for _, entry := range target.Entries {
			if _, ok := chosen[entry.Adapter]; ok {
				selectedEntries++
			} else {
				omittedEntries++
			}
		}
		if selectedEntries != 0 && omittedEntries != 0 {
			return realize.Ledger{}, realize.Ledger{}, &MixedAdapterTargetError{Path: target.Path}
		}
		if omittedEntries != 0 {
			carried.Targets = append(carried.Targets, target)
			continue
		}
		scoped.Targets = append(scoped.Targets, target)
	}
	return scoped, carried, nil
}
