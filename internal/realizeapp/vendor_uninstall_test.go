package realizeapp

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
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const vendorUninstallSource = "vendor:example/orphan"

type vendorUninstallRemote struct{}

func (vendorUninstallRemote) LatestRelease(context.Context, dependency.Repository) (dependency.Release, error) {
	panic("vendor uninstall contacted GitHub")
}

func (vendorUninstallRemote) ReleaseByTag(context.Context, dependency.Repository, string) (dependency.Release, error) {
	panic("vendor uninstall contacted GitHub")
}

func (vendorUninstallRemote) ResolveCommit(context.Context, dependency.Repository, string) (string, error) {
	panic("vendor uninstall contacted GitHub")
}

func (vendorUninstallRemote) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	panic("vendor uninstall contacted GitHub")
}

func (vendorUninstallRemote) DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error) {
	panic("vendor uninstall contacted GitHub")
}

func vendorUninstallFixture(t *testing.T) (string, *Application) {
	t.Helper()
	project := t.TempDir()
	vendorRoot := filepath.Join(project, ".agents", "vendor", "example", "orphan")
	writeFixture(t, filepath.Join(vendorRoot, ".tessl-plugin", "plugin.json"), []byte(`{"name":"example/orphan","version":"legacy","rules":["rules"]}`), 0o644)
	writeFixture(t, filepath.Join(vendorRoot, "rules", "guidance.md"), []byte("Original vendored guidance.\n"), 0o644)
	hash, err := dependency.HashVendorTree(vendorRoot)
	if err != nil {
		t.Fatal(err)
	}
	state := dependency.State{
		Project: dependency.Project{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Agents:        []string{"codex"},
			Freshness:     "none",
			Dependencies:  []dependency.Declaration{{Source: vendorUninstallSource, Requested: "vendored"}},
		},
		Lock: dependency.Lockfile{
			SchemaVersion: dependency.CurrentSchemaVersion,
			Dependencies: []dependency.LockedDependency{{
				Source: vendorUninstallSource, Requested: "vendored", Kind: dependency.ResolutionVendor,
				PackageVersion: "legacy", ContentHash: hash,
			}},
		},
	}
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	application := &Application{service: NewService(dependency.NewResolver(vendorUninstallRemote{})), fallback: cli.UnavailableApplication{}}
	realizeProject(t, application, project)
	return project, application
}

func TestVendorUninstallRemovesHandEditedOwnedTreeLast(t *testing.T) {
	project, application := vendorUninstallFixture(t)
	vendorRoot := filepath.Join(project, ".agents", "vendor", "example", "orphan")
	writeFixture(t, filepath.Join(vendorRoot, "rules", "guidance.md"), []byte("Hand-edited vendored guidance.\n"), 0o600)

	stdout, stderr, exitCode := runCLI(t, application, "uninstall", vendorUninstallSource, "--project", project)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("vendor uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if _, err := os.Stat(vendorRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vendor tree survived uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".agents", "vendor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty vendor parent survived uninstall: %v", err)
	}
	state, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Project.Dependencies) != 0 || len(state.Lock.Dependencies) != 0 {
		t.Fatalf("vendor state survived uninstall: %#v", state)
	}
	if _, stderr, exitCode := runCLI(t, application, "check", "--project", project); exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("check after vendor uninstall exit = %d, stderr = %q", exitCode, stderr)
	}
}

type auxiliarySnapshot struct {
	content string
	info    os.FileInfo
}

func snapshotAuxiliaries(t *testing.T, project string) map[string]auxiliarySnapshot {
	t.Helper()
	files := make(map[string]auxiliarySnapshot)
	for _, name := range []string{appliedPath, appliedIgnorePath} {
		info, err := os.Stat(filepath.Join(project, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = auxiliarySnapshot{content: string(readFile(t, filepath.Join(project, name))), info: info}
	}
	return files
}

func assertAuxiliariesUnchanged(t *testing.T, project string, before map[string]auxiliarySnapshot) {
	t.Helper()
	for name, after := range snapshotAuxiliaries(t, project) {
		want := before[name]
		if after.content != want.content || after.info.Mode() != want.info.Mode() || !os.SameFile(after.info, want.info) || !after.info.ModTime().Equal(want.info.ModTime()) {
			t.Fatalf("restoration wrote auxiliary %s", name)
		}
	}
}

func TestVendorUninstallRemovalFailureRestoresStateAndOutputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		absent     bool
		appendRule bool
		edit       string
		late       bool
	}{
		{name: "unchanged-ignore"},
		{name: "initially-absent", absent: true},
		{name: "append-ignore-rule", appendRule: true},
		{name: "ignore-edit-at-failure", edit: appliedIgnorePath},
		{name: "receipt-edit-at-failure", edit: appliedPath},
		{name: "ignore-edit-during-restoring-marshal", edit: appliedIgnorePath, late: true},
		{name: "receipt-edit-during-restoring-marshal", edit: appliedPath, late: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, application := vendorUninstallFixture(t)
			initial := snapshotAuxiliaries(t, project)
			if test.absent {
				for _, name := range []string{appliedPath, appliedIgnorePath} {
					if err := os.Remove(filepath.Join(project, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.appendRule {
				writeFixture(t, filepath.Join(project, appliedIgnorePath), []byte("operator-cache"), 0o600)
			}
			before := productTreeWithoutApplied(t, project)
			delete(before, appliedIgnorePath)
			injected := errors.New("injected vendor removal failure")
			var forward map[string]auxiliarySnapshot
			removing, marshaled := false, false
			edit := func() {
				writeFixture(t, filepath.Join(project, test.edit), []byte("operator edit\n"), 0o600)
			}
			application.service.removeVendorTree = func(string, realize.VendorTreeRemovalPlan) error {
				removing = true
				if test.edit != "" && !test.late {
					edit()
				}
				forward = snapshotAuxiliaries(t, project)
				return injected
			}
			application.service.marshalState = func(state dependency.State) ([]byte, []byte, error) {
				if removing && !marshaled {
					marshaled = true
					if test.late {
						edit()
						forward = snapshotAuxiliaries(t, project)
					}
				}
				return dependency.MarshalState(state)
			}
			_, err := application.service.Uninstall(context.Background(), project, vendorUninstallSource, false)
			if !errors.Is(err, injected) || err.Error() != injected.Error() || !marshaled {
				t.Fatalf("vendor removal/restoration error = %v, restored marshal = %v", err, marshaled)
			}
			after := productTreeWithoutApplied(t, project)
			delete(after, appliedIgnorePath)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("failed vendor uninstall changed product state: before=%v after=%v", before, after)
			}
			assertAuxiliariesUnchanged(t, project, forward)
			if test.name == "unchanged-ignore" && !os.SameFile(initial[appliedIgnorePath].info, forward[appliedIgnorePath].info) {
				t.Fatal("forward realization replaced an unchanged ignore file")
			}
			if test.appendRule && (forward[appliedIgnorePath].content != "operator-cache\n/.acr-applied.json\n" || forward[appliedIgnorePath].info.Mode().Perm() != 0o600) {
				t.Fatal("forward realization did not preserve ignore content and mode")
			}
			if result, err := application.service.Run(context.Background(), project, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
				t.Fatalf("check with leftover evidence = %v", err)
			}
			assertAuxiliariesUnchanged(t, project, forward)
			if _, err := application.service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
				t.Fatalf("realize with leftover evidence = %v", err)
			}
			assertAppliedEqualsCommitted(t, project)
			application.service.removeVendorTree = realize.ApplyVendorTreeRemoval
			if _, err := application.service.Uninstall(context.Background(), project, vendorUninstallSource, false); err != nil {
				t.Fatalf("retry vendor uninstall = %v", err)
			}
			state, err := dependency.LoadState(project)
			if err != nil || len(state.Lock.Dependencies) != 0 {
				t.Fatalf("retry vendor uninstall state = %#v, %v", state, err)
			}
		})
	}
}

func TestVendorUninstallFailureBeforeAppliedWriteDoesNotCompensate(t *testing.T) {
	project, application := vendorUninstallFixture(t)
	injected := errors.New("injected state preparation failure")
	want := hashProjectTree(t, project)
	edited := false
	application.service.marshalState = func(dependency.State) ([]byte, []byte, error) {
		for _, name := range []string{appliedIgnorePath, appliedPath} {
			writeFixture(t, filepath.Join(project, name), []byte("operator edit before realization wrote state\n"), 0o600)
		}
		afterEdit := hashProjectTree(t, project)
		for _, name := range []string{appliedIgnorePath, appliedPath} {
			want[name] = afterEdit[name]
		}
		edited = true
		return nil, nil, injected
	}
	application.service.removeVendorTree = func(string, realize.VendorTreeRemovalPlan) error {
		t.Fatal("vendor removal ran after state preparation failed")
		return nil
	}
	_, err := application.service.Uninstall(context.Background(), project, vendorUninstallSource, false)
	if !errors.Is(err, injected) || strings.Contains(err.Error(), "compensation") {
		t.Fatalf("preparation failure attempted compensation: %v", err)
	}
	if !edited || !reflect.DeepEqual(want, hashProjectTree(t, project)) {
		t.Fatal("failed preparation changed auxiliary edits despite never writing them")
	}
}

func TestRestoreStateLeavesForwardEvidenceAndPreservesGeneratedEdits(t *testing.T) {
	project, _, serviceA, serviceB, pull := appliedPullFixture(t, false)
	original, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	before := productTreeWithoutApplied(t, project)
	pull()
	if _, err := serviceB.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	forward := snapshotAuxiliaries(t, project)
	live, err := dependency.LoadState(project)
	if err != nil {
		t.Fatal(err)
	}
	original.Lock.Realization = live.Lock.Realization
	if _, err := serviceA.RestoreStateFrom(context.Background(), project, live, original, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, productTreeWithoutApplied(t, project)) {
		t.Fatal("restoration did not restore A product state")
	}
	assertAuxiliariesUnchanged(t, project, forward)
	for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeApply} {
		result, err := serviceA.Run(context.Background(), project, nil, mode)
		if err != nil || result.Plan.HasChanges() {
			t.Fatalf("%s with leftover B receipt = %v", mode, err)
		}
		if mode == realize.ModeCheck {
			assertAuxiliariesUnchanged(t, project, forward)
		}
	}
	assertAppliedEqualsCommitted(t, project)
	// Restoring B's bytes as an operator edit must not revive the leftover B
	// receipt, even if someone later puts that receipt back.
	writeFixture(t, filepath.Join(project, appliedPath), []byte(forward[appliedPath].content), forward[appliedPath].info.Mode())
	target := ".codex/skills/acr__example__first__skill0/references/notes.md"
	writeFixture(t, filepath.Join(project, target), []byte("B notes\n"), 0o644)
	edited := hashProjectTree(t, project)
	for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeApply} {
		_, err := serviceA.Run(context.Background(), project, nil, mode)
		var conflict *realize.ConflictError
		if !errors.As(err, &conflict) || !reflect.DeepEqual(edited, hashProjectTree(t, project)) {
			t.Fatalf("%s accepted generated edit with stale evidence: %v", mode, err)
		}
	}
}

func TestVendorUninstallDryRunPlansTreeAndWritesNothing(t *testing.T) {
	project, application := vendorUninstallFixture(t)
	before := hashProjectTree(t, project)
	stdout, stderr, exitCode := runCLI(t, application, "uninstall", vendorUninstallSource, "--dry-run", "--json", "--project", project)
	if exitCode != cli.ExitSuccess || stderr != "" {
		t.Fatalf("vendor dry-run exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	if after := hashProjectTree(t, project); !reflect.DeepEqual(after, before) {
		t.Fatalf("vendor dry-run changed tree:\n before %#v\n after  %#v", before, after)
	}
	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Result  struct {
			Changed           bool `json:"changed"`
			VendorTreeRemoval struct {
				Path  string `json:"path"`
				Files int    `json:"files"`
			} `json:"vendorTreeRemoval"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Command != "uninstall" || !envelope.Result.Changed || envelope.Result.VendorTreeRemoval.Path != ".agents/vendor/example/orphan" || envelope.Result.VendorTreeRemoval.Files != 2 {
		t.Fatalf("vendor dry-run envelope = %#v", envelope)
	}
}

func TestVendorUninstallAcceptsAbsentOrEmptyTree(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, root string)
	}{
		{
			name: "removed by hand",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "emptied by hand",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project, application := vendorUninstallFixture(t)
			vendorRoot := filepath.Join(project, ".agents", "vendor", "example", "orphan")
			test.prepare(t, vendorRoot)

			stdout, stderr, exitCode := runCLI(t, application, "uninstall", vendorUninstallSource, "--json", "--project", project)
			if exitCode != cli.ExitSuccess || stderr != "" {
				t.Fatalf("vendor uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
			}
			var envelope struct {
				Result UninstallResult `json:"result"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Result.VendorTreeRemoval == nil || envelope.Result.VendorTreeRemoval.Files != 0 {
				t.Fatalf("vendor removal = %#v, want zero files", envelope.Result.VendorTreeRemoval)
			}
			if _, err := os.Stat(filepath.Join(project, ".agents", "vendor")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("empty vendor parent survived uninstall: %v", err)
			}
			state, err := dependency.LoadState(project)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Project.Dependencies) != 0 || len(state.Lock.Dependencies) != 0 {
				t.Fatalf("vendor state survived uninstall: %#v", state)
			}
			if _, checkStderr, checkExit := runCLI(t, application, "check", "--project", project); checkExit != cli.ExitSuccess || checkStderr != "" {
				t.Fatalf("check after vendor uninstall exit = %d, stderr = %q", checkExit, checkStderr)
			}

			secondStdout, secondStderr, secondExit := runCLI(t, application, "uninstall", vendorUninstallSource, "--json", "--project", project)
			if secondExit != cli.ExitUsage || secondStdout != "" || !strings.Contains(secondStderr, `"code":"dependency_not_declared"`) || !strings.Contains(secondStderr, "acr list") {
				t.Fatalf("second uninstall exit = %d, stdout = %q, stderr = %q", secondExit, secondStdout, secondStderr)
			}
		})
	}
}

func TestVendorUninstallJSONEnvelope(t *testing.T) {
	project, application := vendorUninstallFixture(t)
	stdout, stderr, exitCode := runCLI(t, application, "uninstall", vendorUninstallSource, "--json", "--project", project)
	if exitCode != cli.ExitSuccess || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("vendor JSON uninstall exit = %d, stdout = %q, stderr = %q", exitCode, stdout, stderr)
	}
	var envelope struct {
		OK      bool            `json:"ok"`
		Command string          `json:"command"`
		Result  UninstallResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Command != "uninstall" || !envelope.Result.Changed || envelope.Result.Removed == nil || envelope.Result.Removed.Source != vendorUninstallSource || envelope.Result.VendorTreeRemoval == nil || envelope.Result.VendorTreeRemoval.Path != ".agents/vendor/example/orphan" {
		t.Fatalf("vendor uninstall envelope = %#v", envelope)
	}
}
