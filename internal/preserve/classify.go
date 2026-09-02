package preserve

import (
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const CodeOwnershipConflict = "ownership_conflict"

func classifyTarget(target adapter.SharedTarget, unmanaged [][]byte) (realize.Ownership, bool, error) {
	hasUnmanaged := nonEmptyFragments(unmanaged)
	if target.Observed == nil {
		if target.Previous != nil {
			return "", false, conflict(CodeOwnershipConflict, target.Path, "ledger-owned target is missing")
		}
		return realize.OwnershipGenerated, false, nil
	}
	if target.Previous == nil {
		if len(target.Observed.Content) == 0 {
			return "", false, conflict(CodeOwnershipConflict, target.Path, "existing zero-byte target cannot provide a preservation proof")
		}
		return realize.OwnershipShared, false, nil
	}
	switch target.Previous.Ownership {
	case realize.OwnershipGenerated:
		if hasUnmanaged {
			return realize.OwnershipShared, true, nil
		}
		return realize.OwnershipGenerated, false, nil
	case realize.OwnershipShared:
		if target.ExplicitDemotion {
			if hasUnmanaged {
				return "", false, conflict(CodeOwnershipConflict, target.Path, "explicit demotion requires zero unmanaged bytes or entries")
			}
			return realize.OwnershipGenerated, false, nil
		}
		return realize.OwnershipShared, false, nil
	default:
		return "", false, fmt.Errorf("%s: %s: unsupported previous ownership %q", CodeOwnershipConflict, target.Path, target.Previous.Ownership)
	}
}

func nonEmptyFragments(fragments [][]byte) bool {
	for _, fragment := range fragments {
		if len(fragment) != 0 {
			return true
		}
	}
	return false
}
