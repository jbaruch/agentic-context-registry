package realize

import (
	"strings"
	"testing"
)

// sharedTarget is a coordinator-owned ledger target. Its adapter identity
// and version mirror adapter.SharedSkillsAdapter and
// adapter.SharedSkillsVersion, which this package cannot reference:
// internal/adapter imports internal/realize, so reading the constants here
// would close an import cycle. This package validates adapterVersion only
// for non-emptiness, so the literals are a readable stand-in, not an
// oracle; move them when the shared surface's own version moves.
func sharedTarget(path string) Target {
	return Target{
		Path: path, Owner: OwnerCoordinator, Mode: 0o644, Ownership: OwnershipGenerated,
		OutputHash: contentHash([]byte("managed\n")),
		Entries: []Entry{{
			Source: "vendor:example/pkg", ArtifactID: "review", ArtifactKind: ArtifactFile,
			SourcePath: "skills/review/SKILL.md", Adapter: "coordinator", AdapterVersion: "2",
			ManagedHash: contentHash([]byte("managed\n")),
		}},
	}
}

// TestSharedSurfacePathPredicate proves the adapter boundary keeps refusing
// every .agents path while the realization engine accepts exactly the
// coordinator-owned entries.
func TestSharedSurfacePathPredicate(t *testing.T) {
	t.Parallel()

	accepted := []string{".agents/skills/acr__example__pkg__review", ".agents/skills/acr__example__pkg__review/SKILL.md"}
	for _, path := range accepted {
		if err := ValidateSharedSurfacePath(path); err != nil {
			t.Fatalf("ValidateSharedSurfacePath(%q) = %v, want acceptance", path, err)
		}
		if err := ValidateRealizationPath(path); err != nil {
			t.Fatalf("ValidateRealizationPath(%q) = %v, want acceptance", path, err)
		}
		if err := ValidateTargetPath(path); err == nil {
			t.Fatalf("ValidateTargetPath(%q) accepted a shared-surface path; the adapter boundary must keep refusing it", path)
		}
	}
	refused := []string{
		".agents/registry.lock", ".agents/vendor/example/pkg/SKILL.md", ".agents/.acr-transactions/x",
		".agents/skills", ".agents/skills/tessl__review", ".agents/skills/acr__", "agents.yaml", ".git/config",
	}
	for _, path := range refused {
		if err := ValidateRealizationPath(path); err == nil {
			t.Fatalf("ValidateRealizationPath(%q) accepted a reserved path", path)
		}
	}
	for _, ancestor := range []string{".agents", ".agents/skills"} {
		if err := ValidateRealizationDirectoryPath(ancestor); err != nil {
			t.Fatalf("ValidateRealizationDirectoryPath(%q) = %v, want acceptance of a created ancestor", ancestor, err)
		}
	}
}

// TestLedgerVersionIsGraded is D32 and D33: version 1 loads unchanged and is
// rewritten as 1, a coordinator target requires version 2, and an unknown
// version is refused loudly rather than partially read.
func TestLedgerVersionIsGraded(t *testing.T) {
	t.Parallel()

	plain := testLedger(testTarget("docs/guide.md", "managed\n", OwnershipGenerated))
	plain.SchemaVersion = BaselineLedgerSchemaVersion
	encoded, err := EncodeLedger(plain)
	if err != nil {
		t.Fatal(err)
	}
	if encoded["schemaVersion"] != BaselineLedgerSchemaVersion {
		t.Fatalf("encoded schemaVersion = %v, want %d", encoded["schemaVersion"], BaselineLedgerSchemaVersion)
	}
	decoded, err := DecodeLedger(encoded)
	if err != nil || decoded.SchemaVersion != BaselineLedgerSchemaVersion {
		t.Fatalf("DecodeLedger() = %#v, %v", decoded, err)
	}

	shared := Ledger{SchemaVersion: BaselineLedgerSchemaVersion, Targets: []Target{sharedTarget(".agents/skills/acr__example__pkg__review/SKILL.md")}}
	if err := ValidateLedger(shared); err == nil || !strings.Contains(err.Error(), "schemaVersion 2") {
		t.Fatalf("ValidateLedger(v1 with a coordinator target) = %v, want the required version named", err)
	}
	shared.SchemaVersion = SharedSurfaceLedgerSchemaVersion
	if err := ValidateLedger(shared); err != nil {
		t.Fatalf("ValidateLedger(v2 shared ledger) = %v", err)
	}
	sharedEncoded, err := EncodeLedger(shared)
	if err != nil {
		t.Fatal(err)
	}
	if sharedEncoded["schemaVersion"] != SharedSurfaceLedgerSchemaVersion {
		t.Fatalf("encoded schemaVersion = %v, want %d", sharedEncoded["schemaVersion"], SharedSurfaceLedgerSchemaVersion)
	}

	future := plain
	future.SchemaVersion = SharedSurfaceLedgerSchemaVersion + 1
	if err := ValidateLedger(future); err == nil || !strings.Contains(err.Error(), "unsupported realization schemaVersion 3") {
		t.Fatalf("ValidateLedger(future) = %v, want a loud refusal", err)
	}
}

// TestLedgerOwnerAndPathMustAgree keeps the discriminator and the path from
// drifting apart: neither alone may authorize a write to the shared surface.
func TestLedgerOwnerAndPathMustAgree(t *testing.T) {
	t.Parallel()

	misplaced := sharedTarget(".agents/skills/acr__example__pkg__review/SKILL.md")
	misplaced.Path = "docs/guide.md"
	if err := ValidateLedger(Ledger{SchemaVersion: SharedSurfaceLedgerSchemaVersion, Targets: []Target{misplaced}}); err == nil ||
		!strings.Contains(err.Error(), "outside") {
		t.Fatalf("ValidateLedger(coordinator target outside the surface) = %v", err)
	}

	unowned := sharedTarget(".agents/skills/acr__example__pkg__review/SKILL.md")
	unowned.Owner = ""
	if err := ValidateLedger(Ledger{SchemaVersion: SharedSurfaceLedgerSchemaVersion, Targets: []Target{unowned}}); err == nil ||
		!strings.Contains(err.Error(), "records no owner") {
		t.Fatalf("ValidateLedger(shared path with no owner) = %v", err)
	}
}
