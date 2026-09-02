package dependency

import (
	"fmt"
	"strings"
)

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
// declaration is ambiguous enough to need an operator choice. Only a strictly
// newer release is an ordinary upgrade; equality and an incomparable pair both
// count, because either would otherwise turn latest into a permanent pin with
// no choice made.
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
		// is acr resume, and only acr resume, that resumes latest.
		if previous.Hold != nil && declaration.Requested == "latest" {
			declaration.Hold = cloneHold(previous.Hold)
		}
		return declaration, false, nil
	}
	if previous.Hold != nil && !provenNotNewerThanLock(existing, declaration.Requested) {
		return Declaration{}, false, fmt.Errorf("%s is held at %s behind the %s barrier, and %s is not proven older than the held release; a held dependency moves forward only through 'acr resume %s', so review the barrier and resume before installing %s",
			declaration.Source, previous.Hold.Pin, previous.Hold.Rejected, declaration.Requested, declaration.Source, declaration.Requested)
	}
	switch choice {
	case DowngradeUnset:
		return Declaration{}, false, &DowngradeRequiredError{Source: declaration.Source, CurrentTag: currentReference(existing, previous.Hold), RequestedRef: declaration.Requested}
	case DowngradePin:
		return declaration, true, nil
	}
	rejected := advanceBarrier(previous.Hold, existing)
	if rejected == "" {
		return Declaration{}, false, fmt.Errorf("cannot roll %s back temporarily: its lock records no release tag to reject; rerun with --pin to pin %s permanently",
			declaration.Source, declaration.Requested)
	}
	if rejectsRequestedRelease(existing, declaration.Requested, rejected) {
		return Declaration{}, false, fmt.Errorf("cannot roll %s back to %s: it is the release the barrier would reject, so the rollback has nothing to pin; rerun with --pin to stop tracking latest at %s",
			declaration.Source, declaration.Requested, declaration.Requested)
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

// rejectsRequestedRelease reports whether a rollback would pin the very release
// its barrier rejects, which is what an explicit reference equal to the current
// resolution asks for. The lock is consulted as well as the tag, so a commit
// naming the locked release is caught alongside its tag.
func rejectsRequestedRelease(existing *LockedDependency, requested, rejected string) bool {
	if requested == rejected {
		return true
	}
	return existing != nil && rejected == lockedTag(existing) && lockResolvesReference(*existing, requested)
}

// provenNotNewerThanLock reports whether an explicit reference is proven not to
// move a lock forward. The proof is the resolution the lock already carries, or
// semver ordering; an unorderable pair is never proven, so a standing hold can
// only ever be re-pointed at a release it already sits ahead of.
func provenNotNewerThanLock(existing *LockedDependency, requested string) bool {
	if existing == nil {
		return false
	}
	if lockResolvesReference(*existing, requested) {
		return true
	}
	if isCommitRequest(requested) || existing.Kind != ResolutionRelease {
		return false
	}
	return tagProvenNotNewer(requested, existing.Tag)
}

// lockResolvesReference reports whether an explicit reference names the exact
// resolution a lock already carries. A commit matches the locked commit however
// the lock was requested, which is what distinguishes this from the held-pin
// test in lockMatchesReference.
func lockResolvesReference(locked LockedDependency, reference string) bool {
	if isCommitRequest(reference) {
		return locked.Commit != "" && strings.HasPrefix(locked.Commit, strings.ToLower(reference))
	}
	return locked.Kind == ResolutionRelease && locked.Tag == reference
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

// advanceBarrier returns the barrier a new rollback must carry: the release the
// lock resolves now, but only where that release is proven newer than the
// barrier already standing. Semver decides a comparable pair; otherwise the
// release identities the lock already records decide, at no network cost, since
// GitHub numbers releases in creation order. An unprovable pair keeps the
// standing barrier, so deepening a rollback never re-exposes a release an
// earlier one rejected.
func advanceBarrier(existingHold *Hold, existing *LockedDependency) string {
	rejected := lockedTag(existing)
	if existingHold == nil || existingHold.Rejected == "" {
		return rejected
	}
	if rejected == "" {
		return existingHold.Rejected
	}
	if semverComparable(rejected, existingHold.Rejected) {
		if tagStrictlyNewer(rejected, existingHold.Rejected) {
			return rejected
		}
		return existingHold.Rejected
	}
	if recordedRejectionIsNewer(existing) {
		return rejected
	}
	return existingHold.Rejected
}

// recordedRejectionIsNewer reports whether the release a lock resolves was
// created after the barrier the same lock records. Both identities are already
// on disk, so ordering an unorderable pair of tags costs no network call.
func recordedRejectionIsNewer(existing *LockedDependency) bool {
	if existing == nil || existing.Hold == nil {
		return false
	}
	return existing.ReleaseID > 0 && existing.Hold.RejectedReleaseID > 0 && existing.ReleaseID > existing.Hold.RejectedReleaseID
}
