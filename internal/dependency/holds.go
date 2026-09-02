package dependency

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// NoticeCodeHoldResumable identifies a candidate published beyond a rollback
// barrier, which only an explicit acr resume may adopt. It replaces #16's
// placeholder dependency_hold code, which no decision ever emitted.
const NoticeCodeHoldResumable = "dependency_hold_resumable"

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

// projectHolds enforces the rollback barrier declared in agents.yaml. It never
// clears a hold: the only exit is an explicit acr resume.
type projectHolds struct {
	resolver *Resolver
}

// NewHoldPolicy returns the rollback-hold policy backed by one resolver.
func NewHoldPolicy(resolver *Resolver) HoldPolicy {
	return projectHolds{resolver: resolver}
}

// Resolve preserves the held known-good release for every candidate while the
// barrier stands. A candidate strictly newer than the barrier adds a resume
// suggestion and changes nothing else.
func (policy projectHolds) Resolve(ctx context.Context, declaration Declaration, existing *LockedDependency, candidate Release) (HoldDecision, error) {
	hold := declaration.Hold
	if hold == nil {
		return HoldDecision{}, nil
	}
	decision := HoldDecision{}
	if beyondBarrier(candidate, existing, hold) {
		decision.Notice = resumeSuggestion(declaration.Source, hold, candidate.Tag)
	}
	if existing != nil && lockMatchesReference(*existing, hold.Pin) {
		repaired, needsRewrite := repairHeldLock(declaration, *existing)
		if !needsRewrite {
			decision.Skip = true
			return decision, nil
		}
		decision.Pin = &repaired
		return decision, nil
	}
	pinned, err := policy.resolveHeldPin(ctx, declaration, hold)
	if err != nil {
		return HoldDecision{}, err
	}
	decision.Pin = pinned
	return decision, nil
}

// repairHeldLock reconciles a lock that already resolves the held pin with the
// current declaration, without contacting the remote. It reports whether the
// lock needs rewriting; an unchanged lock is preserved byte-identically.
func repairHeldLock(declaration Declaration, existing LockedDependency) (LockedDependency, bool) {
	repaired := existing
	repaired.Hold = cloneLockHold(existing.Hold)
	// A re-declared dependency must not keep a stale requested policy on its
	// preserved lock, which lock validation would reject.
	repaired.Requested = declaration.Requested
	if repaired.Hold == nil {
		// The barrier tag is repairable offline; its release identity is not,
		// and stays empty until the pin is resolved again.
		repaired.Hold = &LockHold{RejectedTag: declaration.Hold.Rejected}
	}
	return repaired, repaired.Requested != existing.Requested || existing.Hold == nil
}

func lockMatchesReference(locked LockedDependency, reference string) bool {
	if isCommitRequest(reference) {
		return locked.Kind == ResolutionCommit && strings.HasPrefix(locked.Commit, strings.ToLower(reference))
	}
	return locked.Kind == ResolutionRelease && locked.Tag == reference
}

// resolveHeldPin resolves the known-good reference through the ordinary
// resolver, then restores the declaration's latest policy and the barrier
// record the lock must carry.
func (policy projectHolds) resolveHeldPin(ctx context.Context, declaration Declaration, hold *Hold) (*LockedDependency, error) {
	pinnedDeclaration := Declaration{Source: declaration.Source, Requested: hold.Pin}
	locked, err := policy.resolver.Resolve(ctx, pinnedDeclaration)
	if err != nil {
		return nil, fmt.Errorf("resolve held %s for %s: %w; run 'acr resume %s' if the held release is gone", hold.Pin, declaration.Source, err, declaration.Source)
	}
	locked.Requested = declaration.Requested
	locked.Hold = policy.barrierRecord(ctx, declaration.Source, hold)
	return &locked, nil
}

// barrierRecord repopulates the rejected release identity. A barrier tag that
// no longer exists degrades to tag-only matching rather than failing the
// resolution, because the barrier itself still stands on the tag.
func (policy projectHolds) barrierRecord(ctx context.Context, source string, hold *Hold) *LockHold {
	record := &LockHold{RejectedTag: hold.Rejected}
	repository, err := ParseSource(source)
	if err != nil {
		return record
	}
	release, err := policy.resolver.github.ReleaseByTag(ctx, repository, hold.Rejected)
	if err != nil || release.Tag != hold.Rejected || release.ID <= 0 {
		return record
	}
	record.RejectedReleaseID = release.ID
	commit, err := policy.resolver.github.ResolveCommit(ctx, repository, hold.Rejected)
	if err == nil && fullCommitPattern.MatchString(commit) {
		record.RejectedCommit = commit
	}
	return record
}

// beyondBarrier reports whether a candidate is a stable release strictly newer
// than the rejected one. Tag equality is tested first, so a retag can never
// clear the barrier however its release identity changed.
func beyondBarrier(candidate Release, existing *LockedDependency, hold *Hold) bool {
	if candidate.Tag == "" || candidate.Draft || candidate.Prerelease || candidate.Tag == hold.Rejected {
		return false
	}
	return strictlyNewer(candidate, existing, hold.Rejected)
}

// strictlyNewer orders two release tags. Comparable semver decides; otherwise
// GitHub's creation-ordered release IDs decide when the barrier's ID is known;
// otherwise the barrier stands. The comparison gates a notice, never locked
// state, so a mis-ordered pair costs at most a missing or premature suggestion.
func strictlyNewer(candidate Release, existing *LockedDependency, rejected string) bool {
	if semverComparable(candidate.Tag, rejected) {
		return tagStrictlyNewer(candidate.Tag, rejected)
	}
	if existing == nil || existing.Hold == nil || existing.Hold.RejectedReleaseID <= 0 {
		return false
	}
	return candidate.ID > existing.Hold.RejectedReleaseID
}

// tagStrictlyNewer orders two release tags by semver. A semver prerelease is
// never newer than a stable release, whatever GitHub's prerelease flag says.
func tagStrictlyNewer(left, right string) bool {
	if !semverComparable(left, right) {
		return false
	}
	return semver.Prerelease(semverForm(left)) == "" && semver.Compare(semverForm(left), semverForm(right)) > 0
}

// tagProvenNotNewer reports whether semver proves left is not after right. An
// incomparable pair is never proven either way, and a prerelease orders exactly
// where semver puts it, so a prerelease of a later version is not proven older
// than an earlier stable release.
func tagProvenNotNewer(left, right string) bool {
	return semverComparable(left, right) && semver.Compare(semverForm(left), semverForm(right)) <= 0
}

func semverComparable(left, right string) bool {
	return semver.IsValid(semverForm(left)) && semver.IsValid(semverForm(right))
}

func semverForm(tag string) string {
	return "v" + strings.TrimPrefix(tag, "v")
}

func resumeSuggestion(source string, hold *Hold, candidateTag string) string {
	return fmt.Sprintf("%s is held at %s after %s was rejected; %s is newer than the barrier. Review it, then run 'acr resume %s' to resume latest.",
		source, hold.Pin, hold.Rejected, candidateTag, source)
}
