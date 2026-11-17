package realizeapp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

// Realize the same package independently at A and B. pull replaces only the
// lock, just as Git does; the checkout's A outputs and receipt stay untouched.
func appliedPullFixture(t *testing.T, git bool) (string, string, *Service, *Service, func()) {
	t.Helper()
	makeVersion := func(version string) (string, *Service) {
		project, loader, service := sharedSkillFixture(t, []string{"claude-code", "codex", "cursor"}, true, firstSource)
		state, err := dependency.LoadState(project)
		if err != nil {
			t.Fatal(err)
		}
		state.Project.Freshness = "none"
		state.Lock.Dependencies[0].PackageVersion = version
		state.Lock.Dependencies[0].Tag = "v" + version
		if err := dependency.WriteState(project, state); err != nil {
			t.Fatal(err)
		}
		if version == "2.0.0" {
			root := loader.packages[firstSource].root
			writeFixture(t, filepath.Join(root, "skills/skill0/references/notes.md"), []byte("B notes\n"), 0o644)
			writeFixture(t, filepath.Join(root, "skills/skill0/run.sh"), []byte("#!/bin/sh\necho B\n"), 0o644)
		}
		if git {
			initRepository(t, project)
		}
		if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
			t.Fatalf("realize %s: %v", version, err)
		}
		return project, service
	}
	a, serviceA := makeVersion("1.0.0")
	b, serviceB := makeVersion("2.0.0")
	pull := func() {
		writeFixture(t, filepath.Join(a, dependency.LockFilename), readFile(t, filepath.Join(b, dependency.LockFilename)), 0o644)
	}
	return a, b, serviceA, serviceB, pull
}

func readApplied(t *testing.T, project string) realize.Ledger {
	t.Helper()
	ledger, err := decodeApplied(readFile(t, filepath.Join(project, appliedPath)))
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func TestAppliedReceiptSurvivesPulledLockBump(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-git", true: "git"}[git], func(t *testing.T) {
			a, b, _, serviceB, pull := appliedPullFixture(t, git)
			oldReceipt := string(readFile(t, filepath.Join(a, appliedPath)))
			pull()
			if string(readFile(t, filepath.Join(a, appliedPath))) != oldReceipt {
				t.Fatal("pull changed checkout evidence")
			}
			before := hashProjectTree(t, a)
			for _, mode := range []realize.Mode{realize.ModeDryRun, realize.ModeCheck} {
				_, err := serviceB.Run(context.Background(), a, nil, mode)
				var changes *realize.ChangesError
				if (mode == realize.ModeDryRun && err != nil) || (mode == realize.ModeCheck && !errors.As(err, &changes)) {
					t.Fatalf("%s after pull: %v", mode, err)
				}
				if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
					t.Fatalf("%s wrote files", mode)
				}
			}
			result, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply)
			if err != nil {
				t.Fatal(err)
			}
			if result.Plan.LedgerChanged {
				t.Fatal("compared the new ledger to receipt ownership instead of committed B")
			}
			assertNoAuxiliaryChanges(t, result.Plan)
			assertAppliedEqualsCommitted(t, a)
			// Compare product bytes/modes; checkout-bound receipts must differ.
			if !reflect.DeepEqual(productTreeWithoutApplied(t, a), productTreeWithoutApplied(t, b)) {
				t.Fatal("pulled checkout differs from independently realized B")
			}
			if result, err := serviceB.Run(context.Background(), a, nil, realize.ModeCheck); err != nil || result.Plan.HasChanges() {
				t.Fatalf("B did not converge: %v", err)
			}
		})
	}
}

func TestAppliedReceiptRefusesEditsAndUnusableEvidenceAfterPull(t *testing.T) {
	cases := []string{"edited", "mode-only", "missing", "malformed", "unsupported", "invalid-hash"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			a, _, _, serviceB, pull := appliedPullFixture(t, true)
			receipt := filepath.Join(a, appliedPath)
			target := filepath.Join(a, ".codex/skills/acr__example__first__skill0/references/notes.md")
			switch name {
			case "edited":
				writeFixture(t, target, []byte("user edit\n"), 0o644)
			case "mode-only":
				if err := os.Chmod(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				writeFixture(t, receipt, []byte("{"), 0o644)
			case "unsupported":
				writeFixture(t, receipt, []byte(`{"schema_version":99,"ledger":{"schemaVersion":1}}`), 0o644)
			case "invalid-hash":
				bad := readApplied(t, a)
				bad.Targets[0].OutputHash = "invalid"
				data, err := marshalAppliedLedger(readFile(t, receipt), bad)
				if err != nil {
					t.Fatal(err)
				}
				writeFixture(t, receipt, data, 0o644)
			}
			pull()
			before := hashProjectTree(t, a)
			excludeBefore := string(readFile(t, filepath.Join(a, ".git/info/exclude")))
			application := &Application{service: serviceB, fallback: cli.UnavailableApplication{}}
			stdout, stderr, exit := runCLI(t, application, "realize", "--project", a, "--json")
			if exit != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) {
				t.Fatalf("refusal exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
			}
			if !strings.Contains(stderr, ".codex/skills/acr__example__first__skill0/references/notes.md") {
				t.Fatalf("refusal omitted edited path: %s", stderr)
			}
			if name != "edited" && name != "mode-only" && (!strings.Contains(stderr, "last-applied evidence") || !strings.Contains(stderr, "delete only")) {
				t.Fatalf("missing recovery diagnostic: %s", stderr)
			}
			if !reflect.DeepEqual(before, hashProjectTree(t, a)) || excludeBefore != string(readFile(t, filepath.Join(a, ".git/info/exclude"))) {
				t.Fatal("refusal changed outputs, receipt, incoming B lock, or exclusions")
			}
		})
	}
}

func TestAppliedReceiptSeedsLegacyCheckoutBeforePull(t *testing.T) {
	a, _, serviceA, serviceB, pull := appliedPullFixture(t, false)
	if err := os.Remove(filepath.Join(a, appliedPath)); err != nil {
		t.Fatal(err)
	}
	before := hashProjectTree(t, a)
	if _, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	after := hashProjectTree(t, a)
	delete(after, appliedPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("legacy seeding changed native outputs or committed state")
	}
	if got, want := readApplied(t, a), generatedLedger(loadLedger(t, a)); !reflect.DeepEqual(got, want) {
		t.Fatalf("applied receipt = %#v; want %#v", got, want)
	}
	pull()
	if _, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
}

func TestAppliedReceiptReplacesUnusableEvidenceOnlyAfterSafeApply(t *testing.T) {
	a, _, serviceA, _, _ := appliedPullFixture(t, false)
	filename := filepath.Join(a, appliedPath)
	writeFixture(t, filename, []byte(`{"schema_version":99}`), 0o644)
	before := hashProjectTree(t, a)
	if _, err := serviceA.Run(context.Background(), a, nil, realize.ModeDryRun); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
		t.Fatal("dry-run replaced unusable evidence")
	}
	result, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Notices) != 1 || result.Notices[0].Code != "applied_evidence_unavailable" {
		t.Fatalf("unusable evidence notice = %#v", result.Notices)
	}
	if got, want := readApplied(t, a), generatedLedger(loadLedger(t, a)); !reflect.DeepEqual(got, want) {
		t.Fatalf("applied receipt = %#v; want %#v", got, want)
	}
	after := hashProjectTree(t, a)
	delete(before, appliedPath)
	delete(after, appliedPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("repairing evidence changed native outputs or dependency state")
	}
}

func TestAppliedReceiptRetainsUnselectedEvidenceAfterPull(t *testing.T) {
	a, b, _, serviceB, pull := appliedPullFixture(t, true)
	pull()
	for _, agent := range []string{"codex", "cursor", "claude-code"} {
		prior := readApplied(t, a)
		_, expectedCarried, err := splitLedger(prior, []string{agent}, false)
		if err != nil {
			t.Fatal(err)
		}
		committedBefore := loadLedger(t, a)
		_, committedCarried, err := splitLedger(committedBefore, []string{agent}, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := serviceB.Run(context.Background(), a, []string{agent}, realize.ModeApply); err != nil {
			t.Fatalf("sequential --agent %s: %v", agent, err)
		}
		_, actualCarried, err := splitLedger(readApplied(t, a), []string{agent}, false)
		if err != nil || !reflect.DeepEqual(expectedCarried, actualCarried) {
			t.Fatalf("--agent %s invented evidence for omitted agents/shared surface: %v", agent, err)
		}
		_, actualCommitted, err := splitLedger(loadLedger(t, a), []string{agent}, false)
		if err != nil || !reflect.DeepEqual(committedCarried, actualCommitted) {
			t.Fatalf("--agent %s changed unselected committed entries: %v", agent, err)
		}
	}
	if _, err := serviceB.Run(context.Background(), a, []string{"codex", "cursor", "claude-code"}, realize.ModeApply); err != nil {
		t.Fatalf("full run after subsets: %v", err)
	}
	if !reflect.DeepEqual(productTreeWithoutApplied(t, a), productTreeWithoutApplied(t, b)) {
		t.Fatal("sequential subsets did not converge to B")
	}
}

func TestAppliedReceiptGitIgnoreAndTrackedRefusal(t *testing.T) {
	// Evidence created before Git initialization must still resist staging.
	a, _, serviceA, _, _ := appliedPullFixture(t, false)
	initRepository(t, a)
	ignore := filepath.Join(a, appliedIgnorePath)
	writeFixture(t, ignore, []byte("# user rule\n/local\n!/.acr-applied.json"), 0o640)
	if _, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, ignore)); got != "# user rule\n/local\n!/.acr-applied.json\n/.acr-applied.json\n" {
		t.Fatalf("ignore content = %q", got)
	}
	if info, err := os.Stat(ignore); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("ignore mode changed: %v, %v", info, err)
	}
	gitFixtureCommand(t, a, "add", "--all")
	if tracked := gitFixtureCommand(t, a, "ls-files", "--", appliedPath); tracked != "" {
		t.Fatalf("ordinary staging included receipt: %s", tracked)
	}
	gitFixtureCommand(t, a, "add", "-f", "--", appliedPath)
	before := hashProjectTree(t, a)
	if _, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply); err == nil || !strings.Contains(err.Error(), "git rm --cached") {
		t.Fatalf("tracked receipt refusal = %v", err)
	}
	if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
		t.Fatal("tracked receipt refusal wrote files")
	}
}

func gitFixtureCommand(t *testing.T, project string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", project}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func TestAppliedReceiptsAreIndependentInLinkedWorktrees(t *testing.T) {
	a, _, serviceA, _, _ := appliedPullFixture(t, true)
	gitFixtureCommand(t, a, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "Initialize fixture")
	linked := filepath.Join(t.TempDir(), "linked")
	gitFixtureCommand(t, a, "worktree", "add", "-qb", "linked", linked)
	state, err := dependency.LoadState(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := dependency.WriteState(linked, state); err != nil {
		t.Fatal(err)
	}
	before := string(readFile(t, filepath.Join(a, appliedPath)))
	if _, err := serviceA.Run(context.Background(), linked, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(linked, appliedPath)); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(a, appliedPath))) != before {
		t.Fatal("linked worktree shared last-applied evidence")
	}
}

func TestAppliedReceiptStrictValidation(t *testing.T) {
	a, _, _, _, _ := appliedPullFixture(t, false)
	valid := readFile(t, filepath.Join(a, appliedPath))
	for name, corrupt := range map[string]func([]byte) []byte{
		"missing-version": func(data []byte) []byte { return []byte(strings.Replace(string(data), `"schema_version": 2,`, "", 1)) },
		"missing-ledger":  func([]byte) []byte { return []byte(`{"schema_version":1}`) },
		"trailing":        func(data []byte) []byte { return append(data, []byte(" {}")...) },
		"unknown-wrapper": func(data []byte) []byte { return append([]byte(`{"unknown":true,`), data[1:]...) },
		"unknown-target": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"mode":`, `"unknown":true,"mode":`, 1))
		},
		"duplicate-path": func(data []byte) []byte {
			ledger, err := decodeApplied(data)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Targets = append(ledger.Targets, ledger.Targets[0])
			encoded, err := marshalAppliedLedger(data, ledger)
			if err != nil {
				t.Fatal(err)
			}
			return encoded
		},
		"shared": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"generated-only"`, `"shared"`, 1))
		},
		"invalid-ledger-version": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"schemaVersion": 2`, `"schemaVersion": 99`, 1))
		},
		"hash-mismatch": func(data []byte) []byte {
			ledger, err := decodeApplied(data)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Targets[0].OutputHash = "sha256:" + strings.Repeat("e", 64)
			encoded, err := marshalAppliedLedger(data, ledger)
			if err != nil {
				t.Fatal(err)
			}
			return encoded
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeApplied(corrupt(append([]byte(nil), valid...))); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}

func TestAppliedReceiptDropsPromotedSharedTarget(t *testing.T) {
	project, root, state, value := realizationFixture(t)
	state.Project.Agents = []string{"codex"}
	state.Project.Freshness = "none"
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixtureLoader{root: root, manifest: value})
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	if !ledgerHasTarget(readApplied(t, project), "AGENTS.md") {
		t.Fatal("generated instruction host has no applied evidence")
	}
	filename := filepath.Join(project, "AGENTS.md")
	writeFixture(t, filename, append([]byte("# Local guidance\n\n"), readFile(t, filename)...), 0o644)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	if ledgerHasTarget(readApplied(t, project), "AGENTS.md") {
		t.Fatal("promoted shared host retained generated-only evidence")
	}
	for _, target := range loadLedger(t, project).Targets {
		if target.Path == "AGENTS.md" && target.Ownership != realize.OwnershipShared {
			t.Fatal("host was not promoted to shared ownership")
		}
	}
}

type appliedChangingLoader struct {
	packageLoader
	change func()
}

func (loader appliedChangingLoader) MaterializeLocked(ctx context.Context, locked dependency.LockedDependency) (dependency.MaterializedPackage, func() error, error) {
	materialized, cleanup, err := loader.packageLoader.MaterializeLocked(ctx, locked)
	loader.change()
	return materialized, cleanup, err
}

func TestAppliedReceiptRejectsStalePreparation(t *testing.T) {
	for _, name := range []string{appliedPath, appliedIgnorePath} {
		for _, missing := range []bool{false, true} {
			t.Run(name+map[bool]string{true: "/created", false: "/changed"}[missing], func(t *testing.T) {
				a, _, serviceA, _, _ := appliedPullFixture(t, false)
				filename := filepath.Join(a, name)
				original := readFile(t, filename)
				if missing {
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
				}
				var afterConcurrent map[string]string
				serviceA.loader = appliedChangingLoader{packageLoader: serviceA.loader, change: func() {
					writeFixture(t, filename, append(original, '\n'), 0o644)
					afterConcurrent = hashProjectTree(t, a)
				}}
				_, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply)
				if err == nil || !strings.Contains(err.Error(), "changed while realization was preparing") {
					t.Fatalf("stale preparation error = %v", err)
				}
				if !reflect.DeepEqual(afterConcurrent, hashProjectTree(t, a)) {
					t.Fatal("stale preparation overwrote concurrent state or outputs")
				}
			})
		}
	}
}

func TestAppliedReceiptRefusesUnsafeStateFiles(t *testing.T) {
	unsafeStates := []string{"symlink", "mode-000", "mode-200"}
	if os.Geteuid() != 0 {
		// Other-read bits do not make a file readable by its non-root owner.
		// This exercises the kernel denial after the no-read-bits guard.
		unsafeStates = append(unsafeStates, "mode-004")
	}
	for _, name := range []string{appliedPath, appliedIgnorePath} {
		for _, unsafe := range unsafeStates {
			t.Run(name+"/"+unsafe, func(t *testing.T) {
				a, _, serviceA, _, _ := appliedPullFixture(t, false)
				filename := filepath.Join(a, name)
				before := hashProjectTree(t, a)
				auxiliaries := snapshotAuxiliaries(t, a)
				wantError := "restore readable permissions"
				if unsafe == "symlink" {
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("registry.lock", filename); err != nil {
						t.Fatal(err)
					}
					wantError = "regular file"
				} else {
					permissions := map[string]os.FileMode{"mode-000": 0, "mode-200": 0o200, "mode-004": 0o004}[unsafe]
					if err := os.Chmod(filename, permissions); err != nil {
						t.Fatal(err)
					}
					if os.Geteuid() != 0 {
						file, err := os.Open(filename)
						if err == nil {
							file.Close()
						}
						if !errors.Is(err, os.ErrPermission) {
							t.Fatalf("non-root ordinary open = %v, want permission denial", err)
						}
						t.Logf("verified EACCES at effective UID %d", os.Geteuid())
					}
				}
				unsafeBefore, err := os.Lstat(filename)
				if err != nil {
					t.Fatal(err)
				}
				for _, mode := range []realize.Mode{realize.ModeApply, realize.ModeCheck, realize.ModeDryRun} {
					_, err := serviceA.Run(context.Background(), a, nil, mode)
					if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), wantError) {
						t.Fatalf("%s unsafe state refusal = %v", mode, err)
					}
					if unsafe == "mode-004" && !errors.Is(err, os.ErrPermission) {
						t.Fatalf("%s permission error lost its cause: %v", mode, err)
					}
					after, err := os.Lstat(filename)
					if err != nil || !os.SameFile(unsafeBefore, after) || after.Mode() != unsafeBefore.Mode() || !after.ModTime().Equal(unsafeBefore.ModTime()) {
						t.Fatalf("%s replaced or changed unsafe state: %v", mode, err)
					}
				}
				if unsafe == "symlink" {
					if err := os.Remove(filename); err != nil {
						t.Fatal(err)
					}
					writeFixture(t, filename, []byte(auxiliaries[name].content), auxiliaries[name].info.Mode().Perm())
				} else {
					if err := os.Chmod(filename, auxiliaries[name].info.Mode().Perm()); err != nil {
						t.Fatal(err)
					}
					assertAuxiliariesUnchanged(t, a, auxiliaries)
				}
				if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
					t.Fatal("unsafe state refusal wrote native or auxiliary files")
				}
			})
		}
	}
}

func TestAppliedIgnoreRejectsEditDuringStateMarshaling(t *testing.T) {
	project, _, service, _, _ := appliedPullFixture(t, false)
	marshal := service.marshalState
	var injected map[string]string
	service.marshalState = func(state dependency.State) ([]byte, []byte, error) {
		writeFixture(t, filepath.Join(project, appliedIgnorePath), []byte("operator-rule\n"), 0o600)
		injected = hashProjectTree(t, project)
		delete(injected, ".agents/.acr-transactions/.lock")
		return marshal(state)
	}
	_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err == nil || !strings.Contains(err.Error(), appliedIgnorePath+" changed") {
		t.Fatalf("stale ignore error = %v", err)
	}
	if !reflect.DeepEqual(injected, hashProjectTree(t, project)) {
		t.Fatal("marshaling-time edit or other outputs changed")
	}
	info, err := os.Stat(filepath.Join(project, appliedIgnorePath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ignore mode changed: %v, %v", info, err)
	}
}

func TestAppliedIgnoreDefaultsOnlyWhenAbsent(t *testing.T) {
	for _, exists := range []bool{false, true} {
		before := realize.StateFile{Path: appliedIgnorePath, BeforeExists: exists}
		got := appliedIgnore(before, exists)
		want := uint32(0)
		if !exists {
			want = 0o644
		}
		if got.Mode != want || got.BeforeExists != exists {
			t.Fatalf("exists=%t: %#v", exists, got)
		}
	}
}
