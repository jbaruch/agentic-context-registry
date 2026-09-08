package migrateapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"github.com/jbaruch/agentic-context-registry/internal/realizeapp"
)

// The consumer fixtures cover claude-code, so the ACR state a migration finds
// selects exactly that agent. A widened or narrowed selection is a different
// refusal, and these cases are about dependencies.
var retainedAgents = []string{"claude-code"}

const (
	retainedPinnedSource = "github:other/pinned"
	retainedHeldSource   = "github:other/held"
)

// retainedRemote serves the mapped package and both unrelated ACR packages, so
// a migration resolves what it maps and rematerializes what it retains without
// a network call.
func retainedRemote(t *testing.T) *multiRepoGitHub {
	t.Helper()
	return newMultiRepoGitHub(map[string][]byte{
		"example/alpha": hostileArchive(t, "example/alpha", map[string]string{
			"rules/always-rule.md":          "---\nalwaysApply: true\n---\n# Always\n",
			"skills/review-change/SKILL.md": "# Review\n",
		}),
		"other/pinned": hostileArchive(t, "other/pinned", map[string]string{
			"rules/pinned-rule.md": "---\nalwaysApply: true\n---\n# Pinned\n",
		}),
		"other/held": hostileArchive(t, "other/held", map[string]string{
			"rules/held-rule.md": "---\nalwaysApply: true\n---\n# Held\n",
		}),
	})
}

// seedRetainedState writes the ACR state a project already carries before any
// migration: one dependency pinned to a release and carrying an extension
// field, and one latest dependency parked behind a rollback hold. Both locks
// are produced by the real resolver, so they are resolutions the migration
// could have rebuilt — and must not.
func seedRetainedState(t *testing.T, root string, github dependency.GitHub) dependency.State {
	t.Helper()
	resolver := dependency.NewResolver(github)
	pinned, err := resolver.Resolve(context.Background(), dependency.Declaration{Source: retainedPinnedSource, Requested: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	held, err := resolver.Resolve(context.Background(), dependency.Declaration{Source: retainedHeldSource, Requested: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	pinned.Extra = map[string]any{"acceptance": "issue-107"}
	held.Hold = &dependency.LockHold{RejectedTag: "v2.0.0"}
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.BaselineSchemaVersion,
			Agents:        append([]string(nil), retainedAgents...),
			Freshness:     "none",
			Dependencies: []dependency.Declaration{
				{Source: retainedPinnedSource, Requested: "v1.0.0", Extra: map[string]any{"acceptance": "issue-107"}},
				{Source: retainedHeldSource, Requested: "latest", Hold: &dependency.Hold{Pin: "v1.0.0", Rejected: "v2.0.0", Reason: "rollback"}},
			},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.BaselineSchemaVersion,
			Dependencies:  []dependency.LockedDependency{pinned, held},
		},
	}
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	// The reported project had installed and realized these packages before it
	// ever ran a migration, so the fixture realizes them too: the migration
	// must leave their native output in place, not only their state entries.
	if _, err := realizeapp.NewService(resolver).Run(context.Background(), root, retainedAgents, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	loaded, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// acrBlock returns the realized block one package owns in a shared Markdown
// host, so a later comparison is against that package's bytes rather than the
// whole file the migration legitimately extends.
func acrBlock(t *testing.T, content, source string) string {
	t.Helper()
	const open = "<!-- acr:begin "
	for _, segment := range strings.Split(content, open)[1:] {
		header, _, found := strings.Cut(segment, " -->")
		if !found {
			t.Fatalf("unterminated ACR block header in %q", content)
		}
		if !strings.Contains(header, "source="+source+" ") {
			continue
		}
		end := strings.Index(segment, "<!-- acr:end ")
		if end < 0 {
			t.Fatalf("the ACR block for %s has no end marker", source)
		}
		closing, _, found := strings.Cut(segment[end:], "-->")
		if !found {
			t.Fatalf("unterminated ACR block end marker for %s", source)
		}
		return open + segment[:end] + closing + "-->"
	}
	t.Fatalf("no ACR block for %s in %q", source, content)
	return ""
}

// assertRetained fails unless every unrelated declaration and lock survives the
// migration exactly as it was found.
func assertRetained(t *testing.T, before dependency.State, root string) dependency.State {
	t.Helper()
	after, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{retainedPinnedSource, retainedHeldSource} {
		wantDeclaration, ok := declarationBySource(before.Project.Dependencies, source)
		if !ok {
			t.Fatalf("fixture does not declare %s", source)
		}
		gotDeclaration, ok := declarationBySource(after.Project.Dependencies, source)
		if !ok {
			t.Fatalf("migration dropped the declaration for %s: %#v", source, after.Project.Dependencies)
		}
		if !reflect.DeepEqual(wantDeclaration, gotDeclaration) {
			t.Fatalf("declaration for %s = %#v, want %#v", source, gotDeclaration, wantDeclaration)
		}
		wantLock, ok := lockBySource(before.Lock.Dependencies, source)
		if !ok {
			t.Fatalf("fixture does not lock %s", source)
		}
		gotLock, ok := lockBySource(after.Lock.Dependencies, source)
		if !ok {
			t.Fatalf("migration dropped the lock for %s: %#v", source, after.Lock.Dependencies)
		}
		if !reflect.DeepEqual(wantLock, gotLock) {
			t.Fatalf("lock for %s = %#v, want %#v", source, gotLock, wantLock)
		}
	}
	if !reflect.DeepEqual(sortedStrings(before.Project.Agents), sortedStrings(after.Project.Agents)) {
		t.Fatalf("agents = %#v, want %#v", after.Project.Agents, before.Project.Agents)
	}
	if before.Project.Freshness != after.Project.Freshness {
		t.Fatalf("freshness = %q, want %q", after.Project.Freshness, before.Project.Freshness)
	}
	return after
}

// TestMigrateRetainsUnrelatedDependenciesOnExplicitMapping is the issue #107
// regression on the explicit pinned GitHub mapping: a consumer holding ACR
// packages the Tessl mapping never names migrates instead of refusing, the
// rehearsal writes nothing, and the retained resolutions are untouched.
func TestMigrateRetainsUnrelatedDependenciesOnExplicitMapping(t *testing.T) {
	root := seedConsumer(t)
	github := retainedRemote(t)
	before := seedRetainedState(t, root, github)
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	args := []string{"migrate", "tessl", "--json", "--project", root, "--map", "example/alpha=github:example/alpha@v1.0.0"}

	rehearsalTree := hashTreeWithModes(t, root)
	stdout, stderr, exitCode := runCLI(t, application, append(append([]string(nil), args...), "--dry-run")...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("dry run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if sources := reportedDeclarationSources(t, stdout); !reflect.DeepEqual(sources, []string{"github:example/alpha", retainedHeldSource, retainedPinnedSource}) {
		t.Fatalf("dry-run declarations = %#v, want the mapped package alongside both retained ones", sources)
	}
	if after := hashTreeWithModes(t, root); !mapsEqual(rehearsalTree, after) {
		t.Fatalf("dry run wrote to the project\nbefore=%v\nafter=%v", rehearsalTree, after)
	}

	stdout, stderr, exitCode = runCLI(t, application, args...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, `"wrote":true`) {
		t.Fatalf("apply report = %s", stdout)
	}
	after := assertRetained(t, before, root)
	if _, ok := declarationBySource(after.Project.Dependencies, "github:example/alpha"); !ok {
		t.Fatalf("migration did not declare the mapped package: %#v", after.Project.Dependencies)
	}

	appliedTree := hashTreeWithModes(t, root)
	stdout, stderr, exitCode = runCLI(t, application, args...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("repeat exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, `"wrote":false`) {
		t.Fatalf("repeat claimed a write: %s", stdout)
	}
	if repeated := hashTreeWithModes(t, root); !mapsEqual(appliedTree, repeated) {
		t.Fatalf("repeat mutated the project\nbefore=%v\nafter=%v", appliedTree, repeated)
	}
	assertRetained(t, before, root)
}

// TestMigrateRetainsUnrelatedDependenciesOnVendorUnmapped is the same
// regression on the reported path: --vendor-unmapped over a consumer whose
// only Tessl package has no upstream, next to unrelated ACR packages.
func TestMigrateRetainsUnrelatedDependenciesOnVendorUnmapped(t *testing.T) {
	root := writeUnmappedConsumer(t)
	github := retainedRemote(t)
	before := seedRetainedState(t, root, github)
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	args := []string{"migrate", "tessl", "--json", "--project", root, "--vendor-unmapped"}

	rehearsalTree := hashTreeWithModes(t, root)
	stdout, stderr, exitCode := runCLI(t, application, append(append([]string(nil), args...), "--dry-run")...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("dry run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashTreeWithModes(t, root); !mapsEqual(rehearsalTree, after) {
		t.Fatalf("dry run wrote to the project\nbefore=%v\nafter=%v", rehearsalTree, after)
	}

	stdout, stderr, exitCode = runCLI(t, application, args...)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("apply exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	after := assertRetained(t, before, root)
	vendored, ok := lockBySource(after.Lock.Dependencies, "vendor:example/orphan")
	if !ok || vendored.Kind != dependency.ResolutionVendor {
		t.Fatalf("migration did not vendor the unmapped package: %#v", after.Lock.Dependencies)
	}
	// The vendored source raises the schema version; the retained state must
	// still be expressible under the stamp the migration writes.
	if after.Project.SchemaVersion < dependency.VendorSchemaVersion {
		t.Fatalf("project schemaVersion = %d, want at least %d", after.Project.SchemaVersion, dependency.VendorSchemaVersion)
	}

	appliedTree := hashTreeWithModes(t, root)
	if _, stderr, exitCode = runCLI(t, application, args...); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("repeat exit = %d, stderr = %q", exitCode, stderr)
	}
	if repeated := hashTreeWithModes(t, root); !mapsEqual(appliedTree, repeated) {
		t.Fatalf("repeat mutated the project\nbefore=%v\nafter=%v", appliedTree, repeated)
	}
	assertRetained(t, before, root)
}

// TestMigrateStillRefusesChangedRequestOnMappedSource holds the deliberate
// refusal in place: a mapping that would move a package the project already
// declares to a different request is a genuine conflict, and refusing it must
// write nothing.
func TestMigrateStillRefusesChangedRequestOnMappedSource(t *testing.T) {
	root := seedConsumer(t)
	github := retainedRemote(t)
	seedRetainedState(t, root, github)
	state, err := dependency.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	resolver := dependency.NewResolver(github)
	conflicting, err := resolver.Resolve(context.Background(), dependency.Declaration{Source: "github:example/alpha", Requested: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	state.Project.Dependencies = append(state.Project.Dependencies, dependency.Declaration{Source: "github:example/alpha", Requested: "latest"})
	state.Lock.Dependencies = append(state.Lock.Dependencies, conflicting)
	if err := dependency.WriteState(root, state); err != nil {
		t.Fatal(err)
	}

	before := hashTreeWithModes(t, root)
	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	stdout, stderr, exitCode := runCLI(t, application,
		"migrate", "tessl", "--json", "--project", root, "--map", "example/alpha=github:example/alpha@v1.0.0")
	if exitCode != cli.ExitOperational || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code":"`+cli.CodeProjectStateConflict+`"`) {
		t.Fatalf("stderr = %q, want %s", stderr, cli.CodeProjectStateConflict)
	}
	after := hashTreeWithModes(t, root)
	delete(before, ".agents/.acr-transactions/.lock")
	delete(after, ".agents/.acr-transactions/.lock")
	delete(before, ".agents/.acr-transactions")
	delete(after, ".agents/.acr-transactions")
	if !mapsEqual(before, after) {
		t.Fatalf("the refusal mutated the project\nbefore=%v\nafter=%v", before, after)
	}
}

// TestResolveStateDropsSupersededVendorSource covers the one existing entry a
// migration does not retain: the vendored tree the same run replaces with its
// upstream source.
func TestResolveStateDropsSupersededVendorSource(t *testing.T) {
	const identity = "example/alpha"
	existing := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.VendorSchemaVersion,
			Dependencies: []dependency.Declaration{
				{Source: "vendor:" + identity, Requested: "vendored"},
				{Source: retainedPinnedSource, Requested: "v1.0.0", Extra: map[string]any{"acceptance": "issue-107"}},
			},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.VendorSchemaVersion,
			Dependencies: []dependency.LockedDependency{
				{Source: "vendor:" + identity, Requested: "vendored", Kind: dependency.ResolutionVendor, PackageVersion: "1.0.0", ContentHash: "sha256:old"},
				{Source: retainedPinnedSource, Requested: "v1.0.0", Kind: dependency.ResolutionRelease, Tag: "v1.0.0", PackageVersion: "1.0.0", ContentHash: "sha256:pinned"},
			},
		},
	}
	mappings := []migrate.Mapping{{From: identity, Source: "github:" + identity, Requested: "v1.0.0", Explicit: true}}
	github := retainedRemote(t)

	desired, _, err := newService(github).resolveState(context.Background(), existing, mappings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := declarationBySource(desired.Project.Dependencies, "vendor:"+identity); ok {
		t.Fatalf("the superseded vendor source survived: %#v", desired.Project.Dependencies)
	}
	if _, ok := lockBySource(desired.Lock.Dependencies, "vendor:"+identity); ok {
		t.Fatalf("the superseded vendor lock survived: %#v", desired.Lock.Dependencies)
	}
	retained, ok := declarationBySource(desired.Project.Dependencies, retainedPinnedSource)
	if !ok || retained.Requested != "v1.0.0" || retained.Extra["acceptance"] != "issue-107" {
		t.Fatalf("retained declaration = %#v", retained)
	}
	if desired.Project.SchemaVersion != dependency.VendorSchemaVersion {
		t.Fatalf("project schemaVersion = %d, want the version the retained state was stamped with", desired.Project.SchemaVersion)
	}
}

// TestCompatibleProjectStateAcceptsAddedMappedDependencies pins the predicate
// itself: added packages are the point of a migration, dropped or re-requested
// ones are the conflict.
func TestCompatibleProjectStateAcceptsAddedMappedDependencies(t *testing.T) {
	existing := dependency.State{
		Project: dependency.Project{Agents: []string{"codex"}, Dependencies: []dependency.Declaration{{Source: "github:one/pkg", Requested: "latest"}}},
		Lock:    dependency.Lockfile{Dependencies: []dependency.LockedDependency{{Source: "github:one/pkg", Requested: "latest", Tag: "v1.0.0"}}},
	}
	desired := dependency.State{
		Project: dependency.Project{Agents: []string{"codex"}, Dependencies: []dependency.Declaration{
			{Source: "github:one/pkg", Requested: "latest"},
			{Source: "github:two/pkg", Requested: "v2.0.0"},
		}},
		Lock: dependency.Lockfile{Dependencies: []dependency.LockedDependency{
			{Source: "github:one/pkg", Requested: "latest", Tag: "v1.0.0"},
			{Source: "github:two/pkg", Requested: "v2.0.0", Tag: "v2.0.0"},
		}},
	}
	if err := compatibleProjectState(existing, desired); err != nil {
		t.Fatalf("adding a mapped package = %v, want acceptance", err)
	}

	for name, mutate := range map[string]func(*dependency.State){
		"dropped-declaration": func(state *dependency.State) { state.Project.Dependencies = state.Project.Dependencies[1:] },
		"changed-request": func(state *dependency.State) {
			state.Project.Dependencies[0].Requested = "v3.0.0"
			state.Lock.Dependencies[0].Requested = "v3.0.0"
		},
		"re-resolved-lock": func(state *dependency.State) { state.Lock.Dependencies[0].Tag = "v9.9.9" },
		"dropped-lock":     func(state *dependency.State) { state.Lock.Dependencies = state.Lock.Dependencies[1:] },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := dependency.State{
				Project: dependency.Project{Agents: desired.Project.Agents, Dependencies: append([]dependency.Declaration(nil), desired.Project.Dependencies...)},
				Lock:    dependency.Lockfile{Dependencies: append([]dependency.LockedDependency(nil), desired.Lock.Dependencies...)},
			}
			mutate(&mutated)
			var migrationErr *Error
			if err := compatibleProjectState(existing, mutated); !errors.As(err, &migrationErr) || migrationErr.Code != cli.CodeProjectStateConflict {
				t.Fatalf("error = %#v, want %s", err, cli.CodeProjectStateConflict)
			}
		})
	}
}

// reportedDeclarationSources reads the sorted declaration sources out of one
// migration report envelope.
func reportedDeclarationSources(t *testing.T, stdout string) []string {
	t.Helper()
	var envelope struct {
		Result struct {
			Project struct {
				Dependencies []struct {
					Source string `json:"source"`
				} `json:"dependencies"`
			} `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode migration report: %v: %s", err, stdout)
	}
	sources := make([]string, 0, len(envelope.Result.Project.Dependencies))
	for _, declaration := range envelope.Result.Project.Dependencies {
		sources = append(sources, declaration.Source)
	}
	return sortedStrings(sources)
}

// TestMigrateKeepsRetainedPackagesRealized covers the native side of the same
// defect: dropping the unrelated declarations would have taken their realized
// content with them. The migration adds its own block to the shared host and
// leaves both retained blocks byte for byte.
func TestMigrateKeepsRetainedPackagesRealized(t *testing.T) {
	root := seedConsumer(t)
	github := retainedRemote(t)
	before := seedRetainedState(t, root, github)
	host := filepath.Join(root, "CLAUDE.md")
	realizedBefore, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	modeBefore, err := os.Stat(host)
	if err != nil {
		t.Fatal(err)
	}

	application := &Application{service: newService(github), fallback: cli.UnavailableApplication{}}
	if _, stderr, exitCode := runCLI(t, application,
		"migrate", "tessl", "--json", "--project", root, "--map", "example/alpha=github:example/alpha@v1.0.0"); exitCode != cli.ExitSuccess {
		t.Fatalf("apply exit = %d, stderr = %q", exitCode, stderr)
	}
	assertRetained(t, before, root)

	realizedAfter, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{retainedPinnedSource, retainedHeldSource} {
		want := acrBlock(t, string(realizedBefore), source)
		if got := acrBlock(t, string(realizedAfter), source); got != want {
			t.Fatalf("realized block for %s = %q, want %q", source, got, want)
		}
	}
	if !strings.Contains(string(realizedAfter), "source=github:example/alpha ") {
		t.Fatalf("the migration realized no block for the mapped package:\n%s", realizedAfter)
	}
	modeAfter, err := os.Stat(host)
	if err != nil {
		t.Fatal(err)
	}
	if modeAfter.Mode().Perm() != modeBefore.Mode().Perm() {
		t.Fatalf("host mode = %v, want %v", modeAfter.Mode().Perm(), modeBefore.Mode().Perm())
	}
}
