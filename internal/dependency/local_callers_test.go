package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func localCallerBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "acr")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/acr").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	return binary
}

func TestLocalReaderSourceBoundary(t *testing.T) {
	review6Binary := localCallerBinary(t)
	for _, inside := range []bool{false, true} {
		t.Run(map[bool]string{false: "external-control", true: "inside-source"}[inside], func(t *testing.T) {
			p, s, svc, _ := localFixture(t)
			ctx := context.Background()
			r, e := svc.InstallLocal(ctx, p, s, false)
			if e != nil {
				t.Fatal(e)
			}
			state, e := LoadState(p)
			if e != nil {
				t.Fatal(e)
			}
			state.Project.Agents = []string{"codex"}
			state.Project.Freshness = "none"
			if e = WriteState(p, state); e != nil {
				t.Fatal(e)
			}
			filename, _, e := localAuthorizationPath(p, r.Dependencies[0].Source)
			if e != nil {
				t.Fatal(e)
			}
			data, e := os.ReadFile(filename)
			if e != nil {
				t.Fatal(e)
			}
			if inside {
				t.Setenv("ACR_STATE_HOME", filepath.Join(s, "state"))
				filename, _, e = localAuthorizationPath(p, r.Dependencies[0].Source)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.MkdirAll(filepath.Dir(filename), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filename, data, 0600); e != nil {
					t.Fatal(e)
				}
			}
			sourceBefore := localRecoveryTree(t, s)
			projectBefore := localRecoveryTree(t, p)
			_, authErr := authorizeLocal(p, state.Project.Dependencies[0])
			statuses, listErr := svc.List(p)
			if listErr != nil || len(statuses) != 1 {
				t.Fatalf("list: %v %v", statuses, listErr)
			}
			outdated, outdatedErr := svc.Outdated(ctx, p)
			if outdatedErr != nil || len(outdated) != 1 {
				t.Fatalf("outdated: %v %v", outdated, outdatedErr)
			}
			status := "authorized on this machine"
			if inside {
				status = "not authorized on this machine; " + fmt.Sprint(authErr)
			}
			expectedNotice := fmt.Sprintf("%s is installed from local path %q (%s). Local dependency state is user/machine-specific; do not commit this local row.", state.Project.Dependencies[0].Source, state.Project.Dependencies[0].Path, status)
			for _, notice := range []string{statuses[0].LocalAuthorization, outdated[0].Notice, LocalNotice(p, state.Project.Dependencies[0])} {
				if notice != expectedNotice {
					t.Errorf("authorization notice = %q, want %q", notice, expectedNotice)
				}
			}
			_, cleanup, matErr := svc.resolver.MaterializeLockedAt(ctx, p, state.Lock.Dependencies[0])
			if cleanup != nil {
				if e := cleanup(); e != nil {
					t.Fatal(e)
				}
			}
			if inside && matErr == nil || !inside && matErr != nil {
				t.Errorf("materialize: %v", matErr)
			}
			checkOut, checkErr := exec.Command(review6Binary, "check", "--project", p).CombinedOutput()
			if inside && (checkErr == nil || !strings.Contains(string(checkOut), "local_source_unauthorized")) {
				t.Errorf("check: %s %v", checkOut, checkErr)
			}
			if !reflect.DeepEqual(projectBefore, localRecoveryTree(t, p)) {
				t.Fatal("readers changed project")
			}
			out, cliErr := exec.Command(review6Binary, "realize", "--project", p).CombinedOutput()
			t.Logf("inside=%v reader=%v realize=%v output=%s projectChanged=%v", inside, authErr, cliErr, out, !reflect.DeepEqual(projectBefore, localRecoveryTree(t, p)))
			if !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, s)) {
				t.Fatal("source changed")
			}
			if inside {
				_, writerErr := svc.InstallLocal(ctx, p, s, false)
				t.Logf("explicit writer=%v", writerErr)
				if writerErr == nil || !strings.Contains(writerErr.Error(), "outside the local source tree") {
					t.Fatal("writer control did not refuse")
				}
				if authErr == nil || cliErr == nil {
					t.Error("reader accepted plugin-contained authorization and realized it")
				}
				if !reflect.DeepEqual(projectBefore, localRecoveryTree(t, p)) {
					t.Error("refused realize changed project")
				}
			} else if authErr != nil || cliErr != nil {
				t.Fatal("external positive control failed")
			}
		})
	}
}

func TestLocalImplicitRecoveryCallers(t *testing.T) {
	if os.Getenv("REVIEW6_CRASH") == "1" {
		p := os.Getenv("REVIEW6_PROJECT")
		before, e := LoadState(p)
		if e != nil {
			t.Fatal(e)
		}
		desired, _, e := PruneDependency(before, "github:owner/plugin")
		if e != nil {
			t.Fatal(e)
		}
		pd, ld, e := MarshalState(desired)
		if e != nil {
			t.Fatal(e)
		}
		var edits []realize.FileTransactionEdit
		for i, n := range []string{ProjectFilename, LockFilename} {
			b, e := os.ReadFile(filepath.Join(p, n))
			if e != nil {
				t.Fatal(e)
			}
			a := pd
			if i == 1 {
				a = ld
			}
			edits = append(edits, realize.FileTransactionEdit{Path: n, Operation: "splice", Before: b, BeforeMode: 0644, After: a, AfterMode: 0644})
		}
		e = ChangeLocalRemoval(p, "github:owner/plugin", func() error {
			return realize.ApplyFileTransactionWithHooks(p, edits, nil, realize.FileTransactionHooks{TransactionID: func() (string, error) { return "review6-interrupted-removal", nil }, AfterEdit: func(i int, _ realize.FileTransactionEdit) error {
				if i == 1 {
					os.Exit(77)
				}
				return nil
			}})
		})
		t.Fatalf("child returned %v", e)
	}
	review6Binary := localCallerBinary(t)
	for _, mode := range []string{"pending-bare", "pending-guard", "no-pending", "reconcile", "freshness", "github", "if-missing", "dry-run", "no-change", "explicit-repair"} {
		t.Run(mode, func(t *testing.T) {
			p, s, svc, _ := localFixture(t)
			ctx := context.Background()
			if _, e := svc.InstallLocal(ctx, p, s, false); e != nil {
				t.Fatal(e)
			}
			other := t.TempDir()
			if e := ExtractPackageArchive(packageArchive(t, "1.0.0", "original\n"), other); e != nil {
				t.Fatal(e)
			}
			m := filepath.Join(other, "agent-plugin.yaml")
			b, e := os.ReadFile(m)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(m, []byte(strings.ReplaceAll(string(b), "owner/plugin", "owner/other")), 0644); e != nil {
				t.Fatal(e)
			}
			if _, e = svc.InstallLocal(ctx, p, other, false); e != nil {
				t.Fatal(e)
			}
			if mode != "no-pending" {
				child := exec.Command(os.Args[0], "-test.run=^TestLocalImplicitRecoveryCallers$")
				child.Env = append(os.Environ(), "REVIEW6_CRASH=1", "REVIEW6_PROJECT="+p)
				out, e := child.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("child: %s %v", out, e)
				}
			}
			if mode != "no-change" {
				if e = os.WriteFile(filepath.Join(other, "guidance.md"), []byte("changed B\n"), 0644); e != nil {
					t.Fatal(e)
				}
			}
			before := localRecoveryTree(t, p)
			sourceBefore := localRecoveryTree(t, s)
			otherBefore := localRecoveryTree(t, other)
			var out []byte
			switch mode {
			case "pending-guard":
				e = AuthorizeLocalRecovery(p)
			case "reconcile", "dry-run", "no-change":
				_, e = svc.Reconcile(ctx, p, mode == "dry-run")
			case "explicit-repair":
				_, e = svc.InstallLocal(ctx, p, other, false)
			case "freshness":
				out, e = exec.Command(review6Binary, "freshness", "run", "--policy", "install", "--project", p).CombinedOutput()
			case "github", "if-missing":
				commit := strings.Repeat("a", 40)
				remote := &fakeGitHub{latest: Release{ID: 7, Tag: "v1.0.0"}, commits: map[string]string{"v1.0.0": commit}, archives: map[string][]byte{commit: packageArchiveFor(t, "owner/release", "1.0.0", "release\n")}}
				remoteService := NewService(NewResolver(remote))
				if mode == "github" {
					_, e = remoteService.Install(ctx, p, "github:owner/release", "latest", DowngradeUnset, false)
				} else {
					_, e = remoteService.InstallIfMissing(ctx, p, "github:owner/release", "latest", false)
				}
			default:
				out, e = exec.Command(review6Binary, "install", "--project", p).CombinedOutput()
			}
			after := localRecoveryTree(t, p)
			changed := !reflect.DeepEqual(before, after)
			t.Logf("mode=%s err=%v out=%s changed=%v", mode, e, out, changed)
			if !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, s)) || !reflect.DeepEqual(otherBefore, localRecoveryTree(t, other)) {
				t.Fatal("source changed")
			}
			if mode == "no-pending" || mode == "explicit-repair" {
				if e != nil || !changed {
					t.Fatal("ordinary refresh control failed")
				}
				return
			}
			if mode == "dry-run" || mode == "no-change" {
				if e != nil || changed {
					t.Fatalf("read-only/no-op changed=%v err=%v", changed, e)
				}
				return
			}
			if e == nil {
				t.Fatal("pending unauthorized control unexpectedly succeeded")
			}
			code := "local_source_unauthorized"
			if mode == "freshness" {
				code = "freshness_update_failed"
			}
			if !strings.Contains(e.Error()+string(out), code) {
				t.Fatalf("wrong refusal: %v %s", e, out)
			}
			if mode == "pending-guard" {
				if changed || !strings.Contains(e.Error(), "local_source_unauthorized") {
					t.Fatal("guard control failed")
				}
				return
			}
			if changed {
				t.Error("bare install changed project/journal before refusing unauthorized recovered state")
			}
		})
	}
}

func TestLocalReleaseReplacementWithoutAuthorization(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-parent", true: "existing-parent"}[parent], func(t *testing.T) {
			p, s, svc, remote := localFixture(t)
			ctx := context.Background()
			result, err := svc.InstallLocal(ctx, p, s, false)
			if err != nil {
				t.Fatal(err)
			}
			file, _, err := localAuthorizationPath(p, result.Dependencies[0].Source)
			if err != nil {
				t.Fatal(err)
			}
			if parent {
				err = os.Remove(file)
			} else {
				err = os.RemoveAll(os.Getenv("ACR_STATE_HOME"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(s); err != nil {
				t.Fatal(err)
			}
			commit := strings.Repeat("a", 40)
			remote.latest = Release{ID: 1, Tag: "v1.0.0"}
			remote.commits = map[string]string{"v1.0.0": commit}
			remote.archives = map[string][]byte{commit: packageArchive(t, "1.0.0", "original\n")}
			if _, err := svc.Install(ctx, p, "github:owner/plugin", "latest", DowngradeUnset, false); err != nil {
				t.Fatal(err)
			}
			state, err := LoadState(p)
			if err != nil {
				t.Fatal(err)
			}
			if state.Project.Dependencies[0].Requested == RequestedLocal || state.Lock.Dependencies[0].Kind == ResolutionLocal {
				t.Fatal("replacement retained local row")
			}
			if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("grant retained: %v", err)
			}
			if !parent {
				if _, err := os.Lstat(os.Getenv("ACR_STATE_HOME")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("replacement created store: %v", err)
				}
			}
		})
	}
}

func TestLocalCopiedGrantLocationControls(t *testing.T) {
	for _, mode := range []string{"external", "inside-plugin", "pending", "wrong-root", "inside-project"} {
		t.Run(mode, func(t *testing.T) {
			p, s, svc, _ := localFixture(t)
			ctx := context.Background()
			result, err := svc.InstallLocal(ctx, p, s, false)
			if err != nil {
				t.Fatal(err)
			}
			file, _, err := localAuthorizationPath(p, result.Dependencies[0].Source)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "external" {
				base := filepath.Join(s, "state")
				if mode == "inside-project" {
					base = filepath.Join(p, "state")
				}
				t.Setenv("ACR_STATE_HOME", base)
				var record localAuthorization
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				if mode == "pending" {
					record.Pending = true
				}
				if mode == "wrong-root" {
					record.SourceRoot = localDigest("wrong-root")
				}
				data, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				// Copy a real grant even for the rejected project location. Keep the
				// original relative project/source keys, rather than asking the rejecting helper.
				file = filepath.Join(base, "local", filepath.Base(filepath.Dir(file)), filepath.Base(file))
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			projectBefore, sourceBefore, storeBefore := localRecoveryTree(t, p), localRecoveryTree(t, s), localRecoveryTree(t, os.Getenv("ACR_STATE_HOME"))
			declaration := Declaration{Source: result.Dependencies[0].Source, Requested: RequestedLocal, Path: s}
			_, err = authorizeLocal(p, declaration)
			if mode == "external" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireLocalCode(t, err, "local_source_unauthorized")
			}
			if !reflect.DeepEqual(projectBefore, localRecoveryTree(t, p)) || !reflect.DeepEqual(sourceBefore, localRecoveryTree(t, s)) || !reflect.DeepEqual(storeBefore, localRecoveryTree(t, os.Getenv("ACR_STATE_HOME"))) {
				t.Fatal("reader changed project, source, or authorization store")
			}
		})
	}
}

func TestLocalInstallAfterCompletedRemoval(t *testing.T) {
	p, s, svc, _ := localFixture(t)
	ctx := context.Background()
	if _, err := svc.InstallLocal(ctx, p, s, false); err != nil {
		t.Fatal(err)
	}
	before, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	pruned, _, err := PruneDependency(before, "github:owner/plugin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(os.Getenv("ACR_STATE_HOME")); err != nil {
		t.Fatal(err)
	}
	if err := ChangeLocalRemoval(p, "github:owner/plugin", func() error { return WriteState(p, pruned) }); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InstallLocal(ctx, p, s, false); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := svc.resolver.MaterializeLockedAt(ctx, p, before.Lock.Dependencies[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}
