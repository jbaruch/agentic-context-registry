package dependency

import (
	"fmt"
	"strings"
)

// Hold is the reviewable rollback intent recorded on a latest declaration. A
// held dependency keeps requesting latest while ACR resolves Pin instead, and
// Rejected is the resume barrier no automatic reconcile may cross.
type Hold struct {
	Pin      string `yaml:"pin" json:"pin"`
	Rejected string `yaml:"rejected" json:"rejected"`
	Reason   string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// LockHold records the resolved identity of the rejected release. agents.yaml
// carries tag names only, so the lock is where a retag becomes detectable.
type LockHold struct {
	RejectedTag       string `yaml:"rejectedTag" json:"rejectedTag"`
	RejectedReleaseID int64  `yaml:"rejectedReleaseId,omitempty" json:"rejectedReleaseId,omitempty"`
	RejectedCommit    string `yaml:"rejectedCommit,omitempty" json:"rejectedCommit,omitempty"`
}

func validateHold(hold *Hold, requested string) error {
	if hold == nil {
		return nil
	}
	if requested != "latest" {
		return fmt.Errorf("hold is only valid on a latest declaration, not %q; run 'acr install SOURCE@%s --pin' for a permanent pin", requested, requested)
	}
	if err := validateHeldReference("pin", hold.Pin); err != nil {
		return err
	}
	if err := validateHeldReference("rejected", hold.Rejected); err != nil {
		return err
	}
	if isCommitRequest(hold.Rejected) {
		return fmt.Errorf("hold.rejected must be a release tag, not a commit SHA; run 'acr resume SOURCE' to clear the hold and reinstall")
	}
	if hold.Pin == hold.Rejected {
		return fmt.Errorf("hold.pin and hold.rejected are both %q; a rollback must pin a different reference than the one it rejected", hold.Pin)
	}
	if strings.TrimSpace(hold.Reason) != hold.Reason {
		return fmt.Errorf("hold.reason must not begin or end with whitespace; trim the reason and retry")
	}
	return nil
}

func validateHeldReference(field, value string) error {
	if value == "latest" {
		return fmt.Errorf("hold.%s must name a fixed release tag or commit, not latest", field)
	}
	if err := validateRequested(value); err != nil {
		return fmt.Errorf("hold.%s: %w", field, err)
	}
	return nil
}

func validateLockHold(source string, hold *Hold, lockHold *LockHold) error {
	if hold == nil {
		if lockHold != nil {
			return fmt.Errorf("locked dependency %q records rejected release %q that %s does not declare; restore the hold in %s, or delete the lock entry and run 'acr install' to accept that release", source, lockHold.RejectedTag, ProjectFilename, ProjectFilename)
		}
		return nil
	}
	if lockHold == nil {
		return nil
	}
	// Every command that could repair these loads the same state first, so the
	// recovery has to be an edit the operator makes before running one.
	if lockHold.RejectedTag != hold.Rejected {
		return fmt.Errorf("locked dependency %q records rejected release %q but %s rejects %q; delete this dependency's entry from %s and rerun 'acr install' to rebuild it from the declared barrier, or edit %s so hold.rejected names %q",
			source, lockHold.RejectedTag, ProjectFilename, hold.Rejected, LockFilename, ProjectFilename, lockHold.RejectedTag)
	}
	if lockHold.RejectedCommit != "" && !fullCommitPattern.MatchString(lockHold.RejectedCommit) {
		return fmt.Errorf("locked dependency %q has an invalid rejectedCommit; delete the rejectedCommit line from its hold in %s, or replace it with the full 40-character commit, then rerun 'acr install'",
			source, LockFilename)
	}
	return nil
}

func cloneHold(hold *Hold) *Hold {
	if hold == nil {
		return nil
	}
	copied := *hold
	return &copied
}

func cloneLockHold(hold *LockHold) *LockHold {
	if hold == nil {
		return nil
	}
	copied := *hold
	return &copied
}
