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
	projectRequired, projectFeature := requiredProjectSchema(*project)
	if err := migrateSchemaVersion(ProjectFilename, &project.SchemaVersion, projectRequired, projectFeature); err != nil {
		return err
	}
	lockRequired, lockFeature := requiredLockSchema(*lock)
	return migrateSchemaVersion(LockFilename, &lock.SchemaVersion, lockRequired, lockFeature)
}

func migrateSchemaVersion(filename string, version *int, required int, feature string) error {
	if *version < MinimumSchemaVersion || *version > CurrentSchemaVersion {
		return schemaVersionError(filename, *version)
	}
	if feature == "hold" && *version < HoldSchemaVersion {
		return fmt.Errorf("%s records a rollback hold under schemaVersion %d, which has no holds; set schemaVersion %d in %s so an older ACR refuses the file instead of reinstalling the rejected release", filename, *version, HoldSchemaVersion, filename)
	}
	if feature == "vendor" && *version < VendorSchemaVersion {
		return fmt.Errorf("%s records a vendored dependency under schemaVersion %d, which has no vendor sources; set schemaVersion %d in %s so an older ACR refuses the file instead of treating it as a GitHub dependency", filename, *version, VendorSchemaVersion, filename)
	}
	*version = required
	return nil
}

func schemaVersionError(filename string, version int) error {
	if version > CurrentSchemaVersion {
		return fmt.Errorf("unsupported %s schemaVersion %d; upgrade acr to a version that supports it", filename, version)
	}
	return fmt.Errorf("unsupported %s schemaVersion %d; run 'acr install' with a supported project file", filename, version)
}

func requiredProjectSchema(project Project) (int, string) {
	for _, declaration := range project.Dependencies {
		if scheme, err := SourceScheme(declaration.Source); err == nil && scheme == SchemeVendor || declaration.Requested == "vendored" {
			return VendorSchemaVersion, "vendor"
		}
	}
	if projectRecordsHold(project) {
		return HoldSchemaVersion, "hold"
	}
	return BaselineSchemaVersion, ""
}

func requiredLockSchema(lock Lockfile) (int, string) {
	for _, dependency := range lock.Dependencies {
		if scheme, err := SourceScheme(dependency.Source); err == nil && scheme == SchemeVendor || dependency.Requested == "vendored" || dependency.Kind == ResolutionVendor {
			return VendorSchemaVersion, "vendor"
		}
	}
	if lockRecordsHold(lock) {
		return HoldSchemaVersion, "hold"
	}
	return BaselineSchemaVersion, ""
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
