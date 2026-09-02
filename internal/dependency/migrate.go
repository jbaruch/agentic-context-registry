package dependency

import "fmt"

// migrateState upgrades loaded project and lock state to CurrentSchemaVersion.
// internal/dependency owns both files and is their sole migrator: readers in
// other packages consume already-upgraded state and never rewrite a version.
//
// Version 1 -> 2 adds rollback holds, so the upgrade only stamps the version;
// the next mutating command persists it and read-only commands leave the
// on-disk files untouched. A file that already records a hold under version 1
// is refused instead: an older ACR reads that stamp as a file it understands
// and would resolve latest straight over the barrier.
func migrateState(project *Project, lock *Lockfile) error {
	if err := migrateSchemaVersion(ProjectFilename, &project.SchemaVersion, projectRecordsHold(*project)); err != nil {
		return err
	}
	return migrateSchemaVersion(LockFilename, &lock.SchemaVersion, lockRecordsHold(*lock))
}

func migrateSchemaVersion(filename string, version *int, recordsHold bool) error {
	if *version < MinimumSchemaVersion || *version > CurrentSchemaVersion {
		return fmt.Errorf("unsupported %s schemaVersion %d; use schemaVersion %d", filename, *version, CurrentSchemaVersion)
	}
	if recordsHold && *version < HoldSchemaVersion {
		return fmt.Errorf("%s records a rollback hold under schemaVersion %d, which has no holds; set schemaVersion %d in %s so an older ACR refuses the file instead of reinstalling the rejected release", filename, *version, HoldSchemaVersion, filename)
	}
	*version = CurrentSchemaVersion
	return nil
}

func projectRecordsHold(project Project) bool {
	for _, declaration := range project.Dependencies {
		if declaration.Hold != nil {
			return true
		}
	}
	return false
}

func lockRecordsHold(lock Lockfile) bool {
	for _, dependency := range lock.Dependencies {
		if dependency.Hold != nil {
			return true
		}
	}
	return false
}
