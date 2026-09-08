package migrateapp

import (
	"fmt"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

// Coverage blocker codes. Each names one gate finalizationReady consults, so
// exit 4 reports which one fired instead of leaving the operator to diff two
// runs.
const (
	blockerUncoveredAgent   = "uncovered-agent"
	blockerAmbiguousPath    = "ambiguous-path"
	blockerAmbiguousArtifac = "ambiguous-artifact"
	blockerLossyArtifact    = "lossy-artifact"
	blockerEffectiveDiff    = "effective-diff"
	blockerAcceptanceStale  = "acceptance-stale"

	// Refusals raised after planning, each naming the gate that actually fired.
	blockerPendingCoexistence = "pending-coexistence"
	blockerUntrackedState     = "untracked-finalization-state"
	blockerFinalizationFailed = "finalization-failed"
	blockerPlanFailed         = "finalization-plan-failed"
	blockerRecoveryConflict   = "finalization-recovery-conflict"
)

// coverageBlockers turns every condition finalizationReady refuses on into a
// named blocker carrying its remedy.
//
// accepted suppresses exactly the two blockers a reviewed-change acceptance
// answers, artifact by artifact: the artifact's own lossy report and its
// effective difference. Every other gate here is untouched by acceptance.
func coverageBlockers(inventory migrate.Report, diffs []migrate.EffectiveDiff, accepted migrate.AcceptedSet) []migrate.Blocker {
	var blockers []migrate.Blocker
	for _, agent := range inventory.Agents {
		if agent.Covered {
			continue
		}
		blockers = append(blockers, migrate.Blocker{
			Code: blockerUncoveredAgent, ID: agent.ID, Detail: "evidence: " + strings.Join(agent.Evidence, ", "),
			Remedy: fmt.Sprintf("ACR has no adapter for %s; remove its Tessl tree yourself if you no longer use it, then re-run 'acr migrate tessl --finalize'", agent.ID),
		})
	}
	for _, record := range inventory.Ambiguous {
		blockers = append(blockers, migrate.Blocker{
			Code: blockerAmbiguousPath, Path: record.Path, Detail: record.Reason,
			Remedy: fmt.Sprintf("resolve %s by hand so ACR can prove who owns it, then re-run 'acr migrate tessl --finalize'", record.Path),
		})
	}
	for _, pkg := range inventory.Packages {
		for _, artifact := range pkg.Artifacts {
			id := pkg.TesslIdentity + "/" + artifact.Kind + "/" + artifact.ID
			if len(artifact.Lossy) != 0 && !accepted.Covers(pkg.TesslIdentity, artifact.Kind, artifact.ID) {
				blockers = append(blockers, migrate.Blocker{
					Code: blockerLossyArtifact, Kind: artifact.Kind, ID: id, Detail: strings.Join(artifact.Lossy, ", "),
					Remedy: "the ACR equivalent would drop the listed behaviour; keep Tessl installed for this artifact or remove it before finalizing",
				})
				continue
			}
			if artifact.Classification == "ambiguous" {
				blockers = append(blockers, migrate.Blocker{
					Code: blockerAmbiguousArtifac, Kind: artifact.Kind, ID: id,
					Remedy: "resolve the reported ambiguity for this artifact, then re-run 'acr migrate tessl --finalize'",
				})
			}
		}
	}
	for _, diff := range diffs {
		if accepted.Covers(diff.Package, diff.Kind, diff.ID) {
			continue
		}
		blockers = append(blockers, migrate.Blocker{
			Code: blockerEffectiveDiff, Kind: diff.Kind, ID: diff.Package + "/" + diff.Kind + "/" + diff.ID, Detail: diff.Reason,
			Remedy: acceptanceRemedy(diff.Reason),
		})
	}
	return blockers
}

// acceptanceRemedy names the acceptance path for a difference an operator may
// review and accept, and keeps the reconcile-only remedy for one that has no
// replacement to review.
func acceptanceRemedy(reason string) string {
	const reconcile = "ACR's realization differs from Tessl's for this artifact; reconcile the difference before finalizing"
	if !migrate.AcceptableDiff(reason) {
		return reconcile
	}
	return reconcile + ", or accept the reviewed change with 'acr migrate tessl --finalize --accept-reviewed-changes <token>' using the token this report's acceptance block carries"
}

// staleAcceptanceBlocker refuses evidence that does not describe this project.
// offered is the bundle the run itself computed, so an empty token means the
// project has nothing to accept and any token supplied against it is stale.
func staleAcceptanceBlocker(offered migrate.Acceptance) migrate.Blocker {
	detail := "the reviewed evidence changed after this token was issued"
	if offered.Token == "" {
		detail = "this project has no reviewed difference to accept"
	}
	return migrate.Blocker{
		Code: blockerAcceptanceStale, Detail: detail,
		Remedy: "re-run 'acr migrate tessl --finalize --dry-run' to review the current changes, then pass the acceptance token it reports to --accept-reviewed-changes",
	}
}
