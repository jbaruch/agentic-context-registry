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
