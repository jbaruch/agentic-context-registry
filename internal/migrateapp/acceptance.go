package migrateapp

import (
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
)

// buildAcceptance assembles the reviewed-change evidence one finalization run
// offers: what the operator would be accepting, and the exact packages the
// acceptance is bound to.
//
// Every mapped package contributes a binding, not only the ones that differ.
// A replacement re-resolved to another commit, a re-installed Tessl package,
// and a difference that appeared or disappeared each change the token, so a
// token issued by an earlier preview cannot authorize a later project state.
func buildAcceptance(inventory migrate.Report, mappings []migrate.Mapping, lock dependency.Lockfile, diffs []migrate.EffectiveDiff) (migrate.Acceptance, error) {
	bindings := make([]migrate.AcceptedBinding, 0, len(mappings))
	for _, mapping := range mappings {
		binding := migrate.AcceptedBinding{
			From:         mapping.From,
			TesslVersion: mapping.TesslVersion,
			TesslDigest:  migrate.PackageEffectiveDigest(inventory, mapping.From),
			Source:       mapping.Source,
			Requested:    mapping.Requested,
		}
		if locked, resolved := lockBySource(lock.Dependencies, mapping.Source); resolved {
			binding.Commit = locked.Commit
			binding.ContentHash = locked.ContentHash
		}
		bindings = append(bindings, binding)
	}
	return migrate.NewAcceptance(bindings, migrate.AcceptedChanges(inventory, diffs))
}
