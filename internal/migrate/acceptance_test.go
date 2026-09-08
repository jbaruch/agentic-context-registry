package migrate

import (
	"strings"
	"testing"
)

func reviewedInventory() Report {
	return Report{
		Packages: []PackageReport{{
			Name: "example/alpha", TesslIdentity: "example/alpha", Version: "1.0.0",
			Artifacts: []ArtifactReport{
				{ID: "always-rule", Kind: kindRule, Classification: classMigratable, Digest: "sha256:rule", Lossy: []string{"description"}},
				{ID: "review-change", Kind: kindSkill, Classification: classMigratable, Digest: "sha256:skill"},
				{ID: "unstable", Kind: kindSkill, Classification: classAmbiguous, Digest: "sha256:unstable"},
			},
		}},
	}
}

func reviewedDiffs() []EffectiveDiff {
	return []EffectiveDiff{
		{EffectiveKey: EffectiveKey{Package: "example/alpha", Kind: kindRule, ID: "always-rule"}, Reason: DiffLossy, TesslDigest: "sha256:rule", ACRDigest: "sha256:rule"},
		{EffectiveKey: EffectiveKey{Package: "example/alpha", Kind: kindSkill, ID: "review-change"}, Reason: DiffBody, TesslDigest: "sha256:skill", ACRDigest: "sha256:other"},
	}
}

func reviewedBindings() []AcceptedBinding {
	return []AcceptedBinding{{
		From: "example/alpha", TesslVersion: "1.0.0", TesslDigest: "sha256:package",
		Source: "github:example/alpha", Requested: "latest", Commit: strings.Repeat("a", 40), ContentHash: "sha256:content",
	}}
}

func TestAcceptedChangesSelectReviewableDifferencesOnly(t *testing.T) {
	t.Parallel()

	inventory := reviewedInventory()
	diffs := append(reviewedDiffs(),
		EffectiveDiff{EffectiveKey: EffectiveKey{Package: "example/alpha", Kind: kindSkill, ID: "retired"}, Reason: DiffMissingInACR},
		EffectiveDiff{EffectiveKey: EffectiveKey{Package: "example/alpha", Kind: kindSkill, ID: "unstable"}, Reason: DiffBody},
	)

	changes := AcceptedChanges(inventory, diffs)

	got := make([]string, 0, len(changes))
	for _, change := range changes {
		got = append(got, change.Kind+"/"+change.ID+"/"+change.Reason)
	}
	want := []string{"rule/always-rule/lossy", "skill/review-change/body"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("acceptable changes = %q, want %q", got, want)
	}
	if changes[0].Detail != "description" {
		t.Fatalf("lossy detail = %q, want the reported loss", changes[0].Detail)
	}
}

func TestAcceptableDiffRejectsAMissingReplacement(t *testing.T) {
	t.Parallel()

	for reason, want := range map[string]bool{
		DiffBody: true, DiffActivation: true, DiffEvent: true, DiffLossy: true,
		DiffMissingInTessl: true, DiffMissingInACR: false, "invented": false,
	} {
		if got := AcceptableDiff(reason); got != want {
			t.Errorf("AcceptableDiff(%q) = %t, want %t", reason, got, want)
		}
	}
}

func TestAcceptanceTokenIsDeterministicAndOrderIndependent(t *testing.T) {
	t.Parallel()

	changes := AcceptedChanges(reviewedInventory(), reviewedDiffs())
	first, err := NewAcceptance(reviewedBindings(), changes)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []AcceptedChange{changes[1], changes[0]}
	second, err := NewAcceptance(reviewedBindings(), reversed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == "" || first.Token != second.Token {
		t.Fatalf("tokens = %q and %q, want one stable value", first.Token, second.Token)
	}
	if !strings.HasPrefix(first.Token, AcceptanceTokenPrefix) {
		t.Fatalf("token %q does not carry the acceptance prefix", first.Token)
	}
	if first.SchemaVersion != AcceptanceSchemaVersion {
		t.Fatalf("schema version = %d, want %d", first.SchemaVersion, AcceptanceSchemaVersion)
	}
}

func TestAcceptanceTokenChangesWithEveryBoundInput(t *testing.T) {
	t.Parallel()

	changes := AcceptedChanges(reviewedInventory(), reviewedDiffs())
	base, err := NewAcceptance(reviewedBindings(), changes)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AcceptedBinding){
		"tesslVersion": func(binding *AcceptedBinding) { binding.TesslVersion = "1.0.1" },
		"tesslDigest":  func(binding *AcceptedBinding) { binding.TesslDigest = "sha256:reinstalled" },
		"source":       func(binding *AcceptedBinding) { binding.Source = "github:other/alpha" },
		"requested":    func(binding *AcceptedBinding) { binding.Requested = "v1.0.0" },
		"commit":       func(binding *AcceptedBinding) { binding.Commit = strings.Repeat("b", 40) },
		"contentHash":  func(binding *AcceptedBinding) { binding.ContentHash = "sha256:rebuilt" },
	} {
		t.Run(name, func(t *testing.T) {
			bindings := reviewedBindings()
			mutate(&bindings[0])
			mutated, err := NewAcceptance(bindings, changes)
			if err != nil {
				t.Fatal(err)
			}
			if mutated.Token == base.Token {
				t.Fatalf("%s did not change the token", name)
			}
		})
	}

	for name, mutate := range map[string]func([]AcceptedChange) []AcceptedChange{
		"digest":  func(values []AcceptedChange) []AcceptedChange { values[0].TesslDigest = "sha256:edited"; return values },
		"reason":  func(values []AcceptedChange) []AcceptedChange { values[1].Reason = DiffActivation; return values },
		"detail":  func(values []AcceptedChange) []AcceptedChange { values[0].Detail = "apply-to-prose"; return values },
		"dropped": func(values []AcceptedChange) []AcceptedChange { return values[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			mutated, err := NewAcceptance(reviewedBindings(), mutate(AcceptedChanges(reviewedInventory(), reviewedDiffs())))
			if err != nil {
				t.Fatal(err)
			}
			if mutated.Token == base.Token {
				t.Fatalf("%s did not change the token", name)
			}
		})
	}
}

func TestAcceptanceWithoutChangesIssuesNoToken(t *testing.T) {
	t.Parallel()

	acceptance, err := NewAcceptance(reviewedBindings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if acceptance.Token != "" {
		t.Fatalf("token = %q, want none when there is nothing to accept", acceptance.Token)
	}
	if !acceptance.Grants().Empty() {
		t.Fatal("a bundle with no token granted an acceptance")
	}
}

func TestGrantsCoverOnlyTheListedArtifacts(t *testing.T) {
	t.Parallel()

	acceptance, err := NewAcceptance(reviewedBindings(), AcceptedChanges(reviewedInventory(), reviewedDiffs()))
	if err != nil {
		t.Fatal(err)
	}
	granted := acceptance.Grants()
	if !granted.Covers("example/alpha", kindRule, "always-rule") || !granted.Covers("example/alpha", kindSkill, "review-change") {
		t.Fatal("the listed artifacts are not covered")
	}
	for _, absent := range [][3]string{
		{"example/alpha", kindSkill, "unstable"},
		{"example/alpha", kindRule, "review-change"},
		{"other/alpha", kindRule, "always-rule"},
	} {
		if granted.Covers(absent[0], absent[1], absent[2]) {
			t.Fatalf("%v is covered by an acceptance that never listed it", absent)
		}
	}
	if (AcceptedSet{}).Covers("example/alpha", kindRule, "always-rule") {
		t.Fatal("the zero set covers an artifact")
	}
}

// packageDigest fails the test rather than returning an error, so every
// caller below reads as the single value the assertion is about.
func packageDigest(t *testing.T, report Report, identity string) string {
	t.Helper()
	digest, err := PackageEffectiveDigest(report, identity)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestPackageEffectiveDigestTracksTheReportedEvidence(t *testing.T) {
	t.Parallel()

	base := packageDigest(t, reviewedInventory(), "example/alpha")
	if base == "" {
		t.Fatal("digest is empty")
	}
	if packageDigest(t, reviewedInventory(), "example/alpha") != base {
		t.Fatal("digest is not deterministic")
	}
	if packageDigest(t, reviewedInventory(), "example/beta") == base {
		t.Fatal("an unrelated package shares the digest")
	}
	for name, mutate := range map[string]func(*Report){
		"body":  func(report *Report) { report.Packages[0].Artifacts[0].Digest = "sha256:rewritten" },
		"lossy": func(report *Report) { report.Packages[0].Artifacts[0].Lossy = nil },
		"added": func(report *Report) {
			report.Packages[0].Artifacts = append(report.Packages[0].Artifacts, ArtifactReport{ID: "extra", Kind: kindSkill, Digest: "sha256:extra"})
		},
		"classification": func(report *Report) { report.Packages[0].Artifacts[1].Classification = classUnsupported },
		"event":          func(report *Report) { report.Packages[0].Artifacts[1].Event = "session-start" },
		"activationMode": func(report *Report) {
			report.Packages[0].Artifacts[0].Activation = &ActivationReport{Mode: "paths", Paths: []string{"docs/**"}}
		},
		"activationPaths": func(report *Report) {
			report.Packages[0].Artifacts[2].Activation = &ActivationReport{Mode: "paths", Paths: []string{"secrets/**"}}
		},
		"droppedProse": func(report *Report) {
			report.Packages[0].Artifacts[0].Lossy = []string{droppedProse("when editing production credentials")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			inventory := reviewedInventory()
			mutate(&inventory)
			if packageDigest(t, inventory, "example/alpha") == base {
				t.Fatalf("%s did not change the package digest", name)
			}
		})
	}
}

// TestPackageEffectiveDigestSeparatesEveryFieldBoundary pins the encoding
// against the values that a delimiter-joined record cannot tell apart. The
// discarded applyTo clause is free text an operator wrote, so a comma, a NUL
// separator or an array boundary inside it must never move one field's value
// into another.
func TestPackageEffectiveDigestSeparatesEveryFieldBoundary(t *testing.T) {
	t.Parallel()

	scoped := func(paths, lossy []string) Report {
		return Report{Packages: []PackageReport{{
			TesslIdentity: "example/alpha",
			Artifacts: []ArtifactReport{{
				ID: "scope", Kind: kindRule, Classification: classMigratable, Digest: "sha256:body",
				Activation: &ActivationReport{Mode: "paths", Paths: paths}, Lossy: lossy,
			}},
		}}}
	}
	for name, pair := range map[string][2]Report{
		"path array shape": {
			scoped([]string{"a,b", "c"}, []string{"description"}),
			scoped([]string{"a", "b,c"}, []string{"description"}),
		},
		"lossy array shape": {
			scoped([]string{"docs/**"}, []string{"description,extra"}),
			scoped([]string{"docs/**"}, []string{"description", "extra"}),
		},
		"empty against absent path": {
			scoped([]string{""}, []string{"description"}),
			scoped(nil, []string{"description"}),
		},
		"prose carrying a comma": {
			scoped([]string{"docs/**"}, []string{droppedProse("when editing docs, and only then")}),
			scoped([]string{"docs/**"}, []string{droppedProse("when editing docs"), "and only then"}),
		},
		"prose carrying the record separator": {
			scoped([]string{"docs/**"}, []string{droppedProse("when editing docs\x00sha256:body")}),
			scoped([]string{"docs/**"}, []string{droppedProse("when editing docs")}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			left := packageDigest(t, pair[0], "example/alpha")
			right := packageDigest(t, pair[1], "example/alpha")
			if left == right {
				t.Fatalf("%s collides: both encode to %s", name, left)
			}
		})
	}

	// Path and lossy order is normalized, so a reordered array is the same
	// evidence and must not move the token.
	sorted := packageDigest(t, scoped([]string{"a", "b"}, []string{"one", "two"}), "example/alpha")
	if reversed := packageDigest(t, scoped([]string{"b", "a"}, []string{"two", "one"}), "example/alpha"); reversed != sorted {
		t.Fatalf("reordered evidence changed the digest: %s vs %s", sorted, reversed)
	}
}
