package dependency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

func localFixture(t *testing.T) (string, string, *Service, *fakeGitHub) {
	t.Helper()
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	project, source := t.TempDir(), t.TempDir()
	if err := ExtractPackageArchive(packageArchive(t, "1.0.0", "original\n"), source); err != nil {
		t.Fatal(err)
	}
	remote := &fakeGitHub{}
	return project, source, NewService(NewResolver(remote)), remote
}

func requireLocalCode(t *testing.T, err error, code string) {
	t.Helper()
	var local *LocalSourceError
	if !errors.As(err, &local) || local.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func localStateBytes(t *testing.T, root string) []string {
	t.Helper()
	return []string{readTestFile(t, filepath.Join(root, ProjectFilename)), readTestFile(t, filepath.Join(root, LockFilename))}
}

func TestLocalInstallRefreshAndMachineBinding(t *testing.T) {
	project, source, service, remote := localFixture(t)
	ctx := context.Background()
	dry, err := service.InstallLocal(ctx, project, source, true)
	if err != nil || !dry.Changed {
		t.Fatalf("dry install: %+v %v", dry, err)
	}
	if entries, err := os.ReadDir(project); err != nil || len(entries) != 0 {
		t.Fatalf("dry run wrote project: %v %v", entries, err)
	}
	result, err := service.InstallLocal(ctx, project, source, false)
	if err != nil {
		t.Fatal(err)
	}
	locked := result.Dependencies[0]
	if !result.Changed || locked.Kind != ResolutionLocal || locked.Source != "github:owner/plugin" || locked.Path != source || locked.Commit != "" || locked.Tag != "" || locked.ReleaseID != 0 {
		t.Fatalf("local lock: %+v", locked)
	}
	before := localStateBytes(t, project)
	result, err = service.InstallLocal(ctx, project, source, false)
	if err != nil || result.Changed {
		t.Fatalf("repeat: %+v %v", result, err)
	}
	if !reflect.DeepEqual(before, localStateBytes(t, project)) {
		t.Fatal("unchanged install rewrote state")
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("undeclared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err = service.Reconcile(ctx, project, false)
	if err != nil || result.Changed {
		t.Fatalf("undeclared content changed hash: %+v %v", result, err)
	}
	if err := os.WriteFile(filepath.Join(source, "guidance.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = service.resolver.MaterializeLockedAt(ctx, project, locked)
	requireLocalCode(t, err, cli.CodeLocalSourceChanged)
	result, err = service.Reconcile(ctx, project, false)
	if err != nil || !result.Changed || result.Dependencies[0].ContentHash == locked.ContentHash {
		t.Fatalf("refresh: %+v %v", result, err)
	}
	pkg, cleanup, err := service.resolver.MaterializeLockedAt(ctx, project, result.Dependencies[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(pkg.Root, "guidance.md")); got != "edited\n" {
		t.Fatalf("materialized %q", got)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	before = localStateBytes(t, project)
	_, err = service.Reconcile(ctx, project, false)
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
	_, _, err = service.resolver.MaterializeLockedAt(ctx, project, result.Dependencies[0])
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
	if !reflect.DeepEqual(before, localStateBytes(t, project)) {
		t.Fatal("unauthorized operation wrote state")
	}
	rows, err := service.List(project)
	if err != nil || !strings.Contains(rows[0].LocalAuthorization, "not authorized") {
		t.Fatalf("list: %+v %v", rows, err)
	}
	outdated, err := service.Outdated(ctx, project)
	if err != nil || len(outdated) != 1 || outdated[0].Actionable() || outdated[0].Status != OutdatedLocal {
		t.Fatalf("outdated: %+v %v", outdated, err)
	}
	result, err = service.InstallLocal(ctx, project, source, false)
	if err != nil || result.Changed {
		t.Fatalf("authorize existing row: %+v %v", result, err)
	}
	if remote.latestCalls+remote.resolveCalls+remote.downloadCalls != 0 {
		t.Fatalf("local operations used network: %+v", remote)
	}
}

func TestLocalSnapshotInventoryAndReleaseHash(t *testing.T) {
	_, source, _, _ := localFixture(t)
	manifest := readTestFile(t, filepath.Join(source, "agent-plugin.yaml")) + "  skills:\n    - id: sample\n      path: skills/sample\n"
	writeLocalTestFile(t, source, "agent-plugin.yaml", manifest, 0o664)
	writeLocalTestFile(t, source, "skills/sample/SKILL.md", "---\nname: sample\ndescription: Sample skill\n---\n# Sample\nRead references/help.md.\n", 0o664)
	writeLocalTestFile(t, source, "skills/sample/references/help.md", "help\n", 0o644)
	writeLocalTestFile(t, source, "skills/sample/scripts/check.sh", "#!/bin/sh\nexit 0\n", 0o775)
	writeLocalTestFile(t, source, "README.md", "must not ship\n", 0o644)
	pkg, locked, cleanup, err := snapshotLocal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pkg.Root, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undeclared README included: %v", err)
	}
	for _, file := range []string{"skills/sample/references/help.md", "skills/sample/scripts/check.sh"} {
		if _, err := os.Stat(filepath.Join(pkg.Root, file)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(pkg.Root, "skills/sample/scripts/check.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode: %v %v", info, err)
	}
	writeLocalTestFile(t, source, "guidance.md", "later edit\n", 0o644)
	if readTestFile(t, filepath.Join(pkg.Root, "guidance.md")) != "original\n" {
		t.Fatal("snapshot reads live source")
	}
	files := map[string]string{}
	err = filepath.WalkDir(pkg.Root, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(pkg.Root, filename)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(relative)] = readTestFile(t, filename)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// testArchive uses 0644; compare against an actual extracted release with
	// the script's normalized executable mode restored by its archive header.
	releaseRoot := t.TempDir()
	if err := ExtractPackageArchive(testArchive(t, "release", files), releaseRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(releaseRoot, "skills/sample/scripts/check.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, err := HashPackageFiles(releaseRoot, pkg.Manifest)
	if err != nil || hash != locked.ContentHash {
		t.Fatalf("release hash %s != local %s: %v", hash, locked.ContentHash, err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pkg.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot leaked: %v", err)
	}
}

func writeLocalTestFile(t *testing.T, root, relative, body string, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSnapshotBoundsAndFailureCleanup(t *testing.T) {
	_, source, _, _ := localFixture(t)
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	for _, limits := range [][2]int64{{1, 1024}, {100, 10}} {
		_, _, _, err := snapshotLocalBounded(source, int(limits[0]), limits[1])
		if err == nil {
			t.Fatal("accepted excessive package")
		}
		entries, err := os.ReadDir(temporary)
		if err != nil || len(entries) != 0 {
			t.Fatalf("temporary leak: %v %v", entries, err)
		}
	}
	if err := os.Remove(filepath.Join(source, "guidance.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(source, "guidance.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := snapshotLocal(source); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestLocalCanonicalDirectoryAndInvalidAuthorization(t *testing.T) {
	project, source, service, _ := localFixture(t)
	ctx := context.Background()
	alias := filepath.Join(t.TempDir(), "plugin")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	result, err := service.InstallLocal(ctx, project, alias, false)
	if err != nil {
		t.Fatal(err)
	}
	filename, _, err := localAuthorizationPath(project, result.Dependencies[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"{", strings.Replace(string(original), `"schemaVersion":1`, `"schemaVersion":2`, 1), strings.Replace(string(original), `"project":"sha256:`, `"project":"wrong:`, 1)} {
		if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := service.resolver.MaterializeLockedAt(ctx, project, result.Dependencies[0])
		requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
		if got := readTestFile(t, filename); got != data {
			t.Fatal("reader rewrote bad auth")
		}
		if _, err := service.InstallLocal(ctx, project, alias, false); err != nil {
			t.Fatal(err)
		}
	}
	replacement := t.TempDir()
	if err := ExtractPackageArchive(packageArchive(t, "1.0.0", "original\n"), replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}
	_, err = service.Reconcile(ctx, project, false)
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
	if _, err := service.InstallLocal(ctx, project, alias, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	_, _, err = service.resolver.MaterializeLockedAt(ctx, project, result.Dependencies[0])
	requireLocalCode(t, err, cli.CodeLocalSourceUnavailable)
}

func TestLocalSchemaContractsAndDowngrade(t *testing.T) {
	project, source, service, _ := localFixture(t)
	result, err := service.InstallLocal(context.Background(), project, source, false)
	if err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	projectSchema := compileDependencySchema(t, "agents.schema.json")
	lockSchema := compileDependencySchema(t, "registry-lock.schema.json")
	if state.Project.SchemaVersion != 5 || state.Lock.SchemaVersion != 5 {
		t.Fatalf("grades: %+v", state)
	}
	if err := validateDependencySchema(t, projectSchema, state.Project); err != nil {
		t.Fatal(err)
	}
	if err := validateDependencySchema(t, lockSchema, state.Lock); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*State){
		func(s *State) { s.Lock.Dependencies[0].Commit = strings.Repeat("a", 40) },
		func(s *State) { s.Lock.Dependencies[0].Tag = "v1.0.0" },
		func(s *State) { s.Lock.Dependencies[0].ReleaseID = 1 },
		func(s *State) { s.Lock.Dependencies[0].Kind = ResolutionRelease },
		func(s *State) { s.Project.Dependencies[0].Path = ""; s.Lock.Dependencies[0].Path = "" },
	} {
		invalid := cloneState(state)
		mutate(&invalid)
		if err := validateState(invalid.Project, invalid.Lock); err == nil {
			t.Fatalf("Go accepted invalid local: %+v", invalid)
		}
		if err := validateDependencySchema(t, lockSchema, invalid.Lock); err == nil {
			t.Fatalf("schema accepted invalid local: %+v", invalid.Lock)
		}
	}
	for _, version := range []int{1, 2, 3, 4} {
		invalid := cloneState(state)
		invalid.Project.SchemaVersion = version
		invalid.Lock.SchemaVersion = version
		if err := migrateState(&invalid.Project, &invalid.Lock); err == nil {
			t.Fatal("old schema accepted local")
		}
		if err := validateDependencySchema(t, projectSchema, invalid.Project); err == nil {
			t.Fatal("old project schema accepted local")
		}
		if err := validateDependencySchema(t, lockSchema, invalid.Lock); err == nil {
			t.Fatal("old lock schema accepted local")
		}
	}
	pruned, _, err := PruneDependency(state, result.Dependencies[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteState(project, pruned); err != nil {
		t.Fatal(err)
	}
	state, err = LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	if state.Project.SchemaVersion != 2 || state.Lock.SchemaVersion != 2 {
		t.Fatalf("did not downgrade: %+v", state)
	}
}

func TestLocalPreservesHoldPinAndIfMissingBoundaries(t *testing.T) {
	project, source, service, remote := localFixture(t)
	ctx := context.Background()
	commit := strings.Repeat("a", 40)
	state := State{Project: Project{SchemaVersion: 2, Dependencies: []Declaration{{Source: "github:owner/plugin", Requested: "latest", Hold: &Hold{Pin: "v1.0.0", Rejected: "v2.0.0"}}}}, Lock: Lockfile{SchemaVersion: 2, Dependencies: []LockedDependency{{Source: "github:owner/plugin", Requested: "latest", Kind: ResolutionRelease, ReleaseID: 1, Tag: "v1.0.0", Commit: commit, PackageVersion: "1.0.0", ContentHash: "sha256:" + strings.Repeat("b", 64)}}}}
	if err := WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	before := localStateBytes(t, project)
	_, err := service.InstallLocal(ctx, project, source, false)
	if err == nil || !strings.Contains(err.Error(), "rollback hold") {
		t.Fatalf("local install discarded hold: %v", err)
	}
	if !reflect.DeepEqual(before, localStateBytes(t, project)) {
		t.Fatal("held refusal wrote state")
	}
	state.Project.Dependencies[0].Hold = nil
	if err := WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	if _, err := service.InstallLocal(ctx, project, source, false); err != nil {
		t.Fatal(err)
	}
	before = localStateBytes(t, project)
	if _, err := service.InstallIfMissing(ctx, project, "github:owner/plugin", "v3.0.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, project, "github:owner/plugin", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, localStateBytes(t, project)) {
		t.Fatal("if-missing/update changed local lock")
	}
	if remote.latestCalls+remote.resolveCalls+remote.downloadCalls != 0 {
		t.Fatal("local preservation used network")
	}
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	_, err = service.InstallIfMissing(ctx, project, "github:owner/plugin", "latest", false)
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
}

func TestLocalPathSchemasMatchNormalization(t *testing.T) {
	schema := compileDependencySchema(t, "agents.schema.json")
	for _, value := range []string{".", "..", "../..", "../../plugin", "plugin@dev", "/", "/absolute/plugin", "plugin/sub", "plugin/../other", ".../..", "foo./..", "/../plugin", "./plugin", "plugin/./child", "plugin//child", "plugin/"} {
		declaration := Declaration{Source: "github:owner/plugin", Requested: RequestedLocal, Path: value}
		runtimeErr := validateLocalPath(RequestedLocal, value)
		schemaErr := validateDependencySchema(t, schema, Project{SchemaVersion: 5, Dependencies: []Declaration{declaration}})
		if (runtimeErr == nil) != (schemaErr == nil) {
			t.Fatalf("path %q runtime=%v schema=%v", value, runtimeErr, schemaErr)
		}
	}
}

func TestLocalReplacementRevokesAuthorizationWithoutLockRow(t *testing.T) {
	project, source, service, remote := localFixture(t)
	ctx := context.Background()
	if _, err := service.InstallLocal(ctx, project, source, false); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	declaration := state.Project.Dependencies[0]
	state.Lock.Dependencies = nil
	if err := WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	remote.latest = Release{ID: 1, Tag: "v1.0.0"}
	remote.commits = map[string]string{"v1.0.0": commit}
	remote.archives = map[string][]byte{commit: packageArchive(t, "1.0.0", "original\n")}
	if _, err := service.Install(ctx, project, declaration.Source, "latest", DowngradeUnset, false); err != nil {
		t.Fatal(err)
	}
	_, err = authorizeLocal(project, declaration)
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
}
