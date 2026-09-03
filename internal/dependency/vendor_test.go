package dependency

import (
	"fmt"
	"strings"
	"testing"
)

func TestVendorRequestedIsNotACommitPin(t *testing.T) {
	t.Parallel()

	if isCommitRequest("vendored") {
		t.Fatal("vendored was classified as a commit request")
	}
	if err := validateRequestedForScheme(SchemeVendor, "vendored"); err != nil {
		t.Fatalf("vendor request rejected: %v", err)
	}
	if err := validateRequestedForScheme(SchemeGitHub, "vendored"); err == nil {
		t.Fatal("GitHub request accepted vendored")
	}
}

func TestVendorLockRejectsCommitFields(t *testing.T) {
	t.Parallel()

	valid := LockedDependency{
		Source: "vendor:example/orphan", Requested: "vendored", Kind: ResolutionVendor,
		PackageVersion: "deadbeef", ContentHash: "sha256:" + strings.Repeat("a", 64),
	}
	project := Project{SchemaVersion: VendorSchemaVersion, Dependencies: []Declaration{{Source: valid.Source, Requested: valid.Requested}}}
	if err := validateState(project, Lockfile{SchemaVersion: VendorSchemaVersion, Dependencies: []LockedDependency{valid}}); err != nil {
		t.Fatalf("valid vendor state rejected: %v", err)
	}

	invalid := valid
	invalid.Commit = strings.Repeat("b", 40)
	if err := validateState(project, Lockfile{SchemaVersion: VendorSchemaVersion, Dependencies: []LockedDependency{invalid}}); err == nil || !strings.Contains(err.Error(), "inconsistent vendor metadata") {
		t.Fatalf("vendor lock with commit error = %v", err)
	}

	schema := compileDependencySchema(t, "registry-lock.schema.json")
	if err := validateDependencySchema(t, schema, Lockfile{SchemaVersion: VendorSchemaVersion, Dependencies: []LockedDependency{valid}}); err != nil {
		t.Fatalf("schema rejected valid vendor lock: %v", err)
	}
	if err := validateDependencySchema(t, schema, Lockfile{SchemaVersion: VendorSchemaVersion, Dependencies: []LockedDependency{invalid}}); err == nil {
		t.Fatal("schema accepted vendor lock with commit")
	}
}

func TestVendorDeclarationRejectsHold(t *testing.T) {
	t.Parallel()
	declaration := Declaration{Source: "vendor:example/orphan", Requested: "vendored", Hold: &Hold{Pin: "v1.0.0", Rejected: "v2.0.0"}}
	project := Project{SchemaVersion: VendorSchemaVersion, Dependencies: []Declaration{declaration}}
	if err := validateState(project, Lockfile{SchemaVersion: VendorSchemaVersion}); err == nil || !strings.Contains(err.Error(), "hold") {
		t.Fatalf("vendor hold error = %v", err)
	}
}

func TestForwardSchemaVersionSaysUpgradeAcr(t *testing.T) {
	t.Parallel()

	project := Project{SchemaVersion: CurrentSchemaVersion + 1}
	lock := Lockfile{SchemaVersion: BaselineSchemaVersion}
	err := migrateState(&project, &lock)
	if err == nil || !strings.Contains(err.Error(), "upgrade acr") {
		t.Fatalf("forward schema error = %v, want upgrade guidance", err)
	}
	if strings.Contains(err.Error(), "use schemaVersion") {
		t.Fatalf("forward schema error suggests editing to an older version: %v", err)
	}
}

func TestVendorSourceUnderOldSchemaVersionIsRefused(t *testing.T) {
	t.Parallel()

	vendorDeclaration := Declaration{Source: "vendor:example/orphan", Requested: "vendored"}
	vendorLock := LockedDependency{
		Source: vendorDeclaration.Source, Requested: vendorDeclaration.Requested, Kind: ResolutionVendor,
		PackageVersion: "2.0.0", ContentHash: "sha256:" + strings.Repeat("a", 64),
	}
	tests := []struct {
		name    string
		project Project
		lock    Lockfile
		want    string
	}{
		{
			name:    "project",
			project: Project{SchemaVersion: BaselineSchemaVersion, Dependencies: []Declaration{vendorDeclaration}},
			lock:    Lockfile{SchemaVersion: VendorSchemaVersion},
			want:    ProjectFilename,
		},
		{
			name:    "lock",
			project: Project{SchemaVersion: VendorSchemaVersion, Dependencies: []Declaration{vendorDeclaration}},
			lock:    Lockfile{SchemaVersion: BaselineSchemaVersion, Dependencies: []LockedDependency{vendorLock}},
			want:    LockFilename,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project, lock := test.project, test.lock
			err := migrateState(&project, &lock)
			if err == nil || !strings.Contains(err.Error(), test.want+" records a vendored dependency") || !strings.Contains(err.Error(), "set schemaVersion 3") {
				t.Fatalf("migrateState() error = %v", err)
			}
		})
	}
}

func TestSchemaVersionIsMinimalForContent(t *testing.T) {
	t.Parallel()

	vendorFree := State{Project: Project{SchemaVersion: CurrentSchemaVersion}, Lock: Lockfile{SchemaVersion: CurrentSchemaVersion}}
	projectData, lockData, err := MarshalState(vendorFree)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{ProjectFilename: projectData, LockFilename: lockData} {
		if !strings.Contains(string(data), "schemaVersion: 2\n") {
			t.Fatalf("%s = %s, want baseline schemaVersion 2", name, data)
		}
	}

	vendorProjectOnly := State{
		Project: Project{SchemaVersion: CurrentSchemaVersion, Dependencies: []Declaration{{Source: "vendor:example/orphan", Requested: "vendored"}}},
		Lock:    Lockfile{SchemaVersion: CurrentSchemaVersion},
	}
	projectData, lockData, err = MarshalState(vendorProjectOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projectData), fmt.Sprintf("schemaVersion: %d\n", VendorSchemaVersion)) {
		t.Fatalf("%s = %s, want vendor schemaVersion %d", ProjectFilename, projectData, VendorSchemaVersion)
	}
	if !strings.Contains(string(lockData), fmt.Sprintf("schemaVersion: %d\n", BaselineSchemaVersion)) {
		t.Fatalf("%s = %s, want baseline schemaVersion %d", LockFilename, lockData, BaselineSchemaVersion)
	}
}

func TestSourceSchemeKeepsGitHubParserNarrow(t *testing.T) {
	t.Parallel()

	scheme, err := SourceScheme("vendor:example/orphan")
	if err != nil || scheme != SchemeVendor {
		t.Fatalf("SourceScheme() = %q, %v", scheme, err)
	}
	identity, err := ParseVendorSource("vendor:example/orphan")
	if err != nil || identity.FullName() != "example/orphan" {
		t.Fatalf("ParseVendorSource() = %#v, %v", identity, err)
	}
	if _, err := ParseSource("vendor:example/orphan"); err == nil {
		t.Fatal("ParseSource accepted a vendor source")
	}
}
