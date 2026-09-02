package dependency

import "fmt"

// DowngradeChoice records how an install below the locked release resolves.
type DowngradeChoice string

const (
	// DowngradeUnset means the caller has not chosen yet.
	DowngradeUnset DowngradeChoice = ""
	// DowngradeHold keeps requesting latest behind a rollback barrier.
	DowngradeHold DowngradeChoice = "hold"
	// DowngradePin replaces latest with a permanent pin.
	DowngradePin DowngradeChoice = "pin"
)

// DowngradeRequiredError reports that installing an explicit reference over a
// latest declaration would move it backwards, which needs an explicit choice.
// The interactive prompt in #13 turns this into the three-option menu.
type DowngradeRequiredError struct {
	Source       string
	CurrentTag   string
	RequestedRef string
}

func (err *DowngradeRequiredError) Error() string {
	return fmt.Sprintf("%s@%s is a rollback from the locked %s; pass --hold to roll back temporarily until a newer stable release exists, or --pin to replace latest with a permanent pin",
		err.Source, err.RequestedRef, err.CurrentTag)
}

// requiresDowngradeChoice reports whether an explicit reference over a latest
// declaration is ambiguous enough to need an operator choice. An incomparable
// pair counts: requiring a choice is conservative, and the operator can still
// choose the permanent pin.
func requiresDowngradeChoice(declaration Declaration, existing *LockedDependency, requested string) bool {
	if declaration.Requested != "latest" || requested == "latest" {
		return false
	}
	if declaration.Hold != nil {
		// A standing hold is retired only by acr resume or an explicit --pin,
		// so every explicit reference over one is a choice, never a silent
		// conversion of the hold into a permanent pin.
		return true
	}
	if existing == nil {
		return false
	}
	if lockMatchesReference(*existing, requested) {
		return false
	}
	if isCommitRequest(requested) || existing.Kind != ResolutionRelease {
		return true
	}
	return !tagStrictlyNewer(requested, existing.Tag)
}

// applyDowngradeChoice turns a requested reference plus an operator choice into
// the declaration to persist. It reports whether the declaration must be
// re-resolved even though its requested policy may be unchanged.
func applyDowngradeChoice(previous Declaration, existing *LockedDependency, declaration Declaration, choice DowngradeChoice) (Declaration, bool, error) {
	if !requiresDowngradeChoice(previous, existing, declaration.Requested) {
		if choice != DowngradeUnset {
			return Declaration{}, false, fmt.Errorf("--%s applies only to a rollback, and %s@%s does not roll %s back from its current resolution; rerun without --%s",
				choice, declaration.Source, declaration.Requested, declaration.Source, choice)
		}
		// Re-declaring latest re-affirms the policy a hold already carries; it
		// is acr resume, and only acr resume, that retires the barrier.
		if previous.Hold != nil && declaration.Requested == "latest" {
			declaration.Hold = cloneHold(previous.Hold)
		}
		return declaration, false, nil
	}
	switch choice {
	case DowngradeUnset:
		return Declaration{}, false, &DowngradeRequiredError{Source: declaration.Source, CurrentTag: currentReference(existing, previous.Hold), RequestedRef: declaration.Requested}
	case DowngradePin:
		return declaration, true, nil
	}
	rejected := advanceBarrier(previous.Hold, lockedTag(existing))
	if rejected == "" {
		return Declaration{}, false, fmt.Errorf("cannot roll %s back temporarily: its lock records no release tag to reject; rerun with --pin to pin %s permanently",
			declaration.Source, declaration.Requested)
	}
	held := Declaration{Source: declaration.Source, Requested: previous.Requested, Hold: &Hold{Pin: declaration.Requested, Rejected: rejected}}
	// A reason written against an earlier barrier does not describe a new one.
	if previous.Hold != nil && previous.Hold.Rejected == rejected {
		held.Hold.Reason = previous.Hold.Reason
	}
	return held, true, nil
}

// lockedTag returns the release tag a lock resolves, or empty for a commit lock
// or a declaration that has never been locked.
func lockedTag(existing *LockedDependency) string {
	if existing == nil || existing.Kind != ResolutionRelease {
		return ""
	}
	return existing.Tag
}

func currentReference(existing *LockedDependency, hold *Hold) string {
	switch {
	case existing != nil && existing.Tag != "":
		return existing.Tag
	case existing != nil:
		return existing.Commit
	case hold != nil:
		return hold.Pin
	default:
		return "latest"
	}
}

// advanceBarrier returns the barrier a new rollback must carry: the newer of
// the release being rejected now and any barrier already standing. Deepening a
// rollback therefore never re-exposes a release an earlier one rejected.
func advanceBarrier(existingHold *Hold, rejected string) string {
	if existingHold == nil || existingHold.Rejected == "" {
		return rejected
	}
	if tagStrictlyNewer(existingHold.Rejected, rejected) {
		return existingHold.Rejected
	}
	return rejected
}
