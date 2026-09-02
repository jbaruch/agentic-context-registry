package dependency

import "fmt"

// migrateState upgrades loaded project and lock state to CurrentSchemaVersion.
// internal/dependency owns both files and is their sole migrator: readers in
// other packages consume already-upgraded state and never rewrite a version.
//
// Version 1 -> 2 is purely additive. Version 1 had no rollback holds, so the
// upgrade only stamps the version; the next mutating command persists it and
// read-only commands leave the on-disk files untouched.
func migrateState(project *Project, lock *Lockfile) error {
	if err := migrateSchemaVersion(ProjectFilename, &project.SchemaVersion); err != nil {
		return err
	}
	return migrateSchemaVersion(LockFilename, &lock.SchemaVersion)
}

func migrateSchemaVersion(filename string, version *int) error {
	if *version < MinimumSchemaVersion || *version > CurrentSchemaVersion {
		return fmt.Errorf("unsupported %s schemaVersion %d; use schemaVersion %d", filename, *version, CurrentSchemaVersion)
	}
	*version = CurrentSchemaVersion
	return nil
}
