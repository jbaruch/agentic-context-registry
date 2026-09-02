package dependency

import "context"

// NoticeCodeHold identifies a dependency decision made by HoldPolicy.
const NoticeCodeHold = "dependency_hold"

// HoldPolicy is consulted before a latest declaration is re-resolved.
type HoldPolicy interface {
	// Resolve returns the action for one latest candidate. Skip preserves the
	// existing lock, Pin substitutes a held known-good lock, and Notice is
	// surfaced verbatim by the caller.
	Resolve(context.Context, Declaration, *LockedDependency, Release) (HoldDecision, error)
}

// HoldDecision controls one latest-candidate resolution.
type HoldDecision struct {
	Skip   bool
	Pin    *LockedDependency
	Notice string
}

type noHolds struct{}

func (noHolds) Resolve(context.Context, Declaration, *LockedDependency, Release) (HoldDecision, error) {
	return HoldDecision{}, nil
}
