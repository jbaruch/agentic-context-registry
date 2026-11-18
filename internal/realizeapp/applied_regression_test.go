package realizeapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func writeApplied(t *testing.T, project string, ledger realize.Ledger) {
	t.Helper()
	data, err := encodeApplied(project, ledger)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(project, appliedPath), data, 0o644)
}

func appliedHash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertAppliedEqualsCommitted(t *testing.T, project string) {
	t.Helper()
	if got, want := readApplied(t, project), generatedLedger(loadLedger(t, project)); !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt does not exactly record the applied generated ledger: got %#v, want %#v", got, want)
	}
	bound, err := boundApplied(project, readFile(t, filepath.Join(project, appliedPath)))
	if err != nil || !reflect.DeepEqual(bound, readApplied(t, project)) {
		t.Fatalf("receipt does not bind the final live files: %v", err)
	}
}

func assertNoAuxiliaryChanges(t *testing.T, plan realize.Plan) {
	t.Helper()
	for _, op := range plan.Operations {
		if op.Path == appliedPath || op.Path == appliedIgnorePath {
			t.Fatalf("auxiliary maintenance escaped into realization plan: %#v", op)
		}
	}
}

func TestAppliedReceiptAcceptsCommittedBytesWithOldEvidence(t *testing.T) {
	for _, trackedPull := range []bool{false, true} {
		t.Run(map[bool]string{false: "older-cli-realized-B", true: "git-fast-forward-updates-generated-files"}[trackedPull], func(t *testing.T) {
			a, b, serviceA, serviceB, pull := appliedPullFixture(t, trackedPull)
			if trackedPull {
				// Record generated outputs in each actual Git index, so both
				// authoritative ledgers carry the correct exclusion flags.
				for _, project := range []string{a, b} {
					for _, target := range loadLedger(t, project).Targets {
						gitFixtureCommand(t, project, "add", "-f", "--", target.Path)
					}
				}
				for project, service := range map[string]*Service{a: serviceA, b: serviceB} {
					if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
						t.Fatal(err)
					}
				}
				gitFixtureCommand(t, a, "add", "--all")
				gitFixtureCommand(t, a, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Record A")
				gitFixtureCommand(t, a, "checkout", "-qb", "incoming")
			}
			oldReceipt := string(readFile(t, filepath.Join(a, appliedPath)))
			pull()
			for _, target := range loadLedger(t, b).Targets {
				writeFixture(t, filepath.Join(a, target.Path), readFile(t, filepath.Join(b, target.Path)), os.FileMode(target.Mode))
			}
			if trackedPull {
				gitFixtureCommand(t, a, "add", "--all")
				gitFixtureCommand(t, a, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Record B")
				gitFixtureCommand(t, a, "checkout", "-q", "-")
				gitFixtureCommand(t, a, "merge", "--ff-only", "incoming")
			}
			if got := string(readFile(t, filepath.Join(a, appliedPath))); got != oldReceipt {
				t.Fatal("Git changed the checkout-local A receipt")
			}
			before := hashProjectTree(t, a)
			for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun, realize.ModeApply} {
				result, err := serviceB.Run(context.Background(), a, nil, mode)
				if err != nil || result.Plan.HasChanges() || changeCount(result.Plan) != 0 || result.Plan.LedgerChanged {
					t.Fatalf("%s rejected committed B with receipt A or counted false changes: %v, %#v", mode, err, result.Plan)
				}
				assertNoAuxiliaryChanges(t, result.Plan)
				after := hashProjectTree(t, a)
				if mode == realize.ModeApply {
					delete(before, appliedPath)
					delete(after, appliedPath)
					assertAppliedEqualsCommitted(t, a)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("%s rewrote current native output or committed state", mode)
				}
			}
		})
	}
}

func TestAppliedReceiptCannotOwnPrivateNotes(t *testing.T) {
	a, _, serviceA, _, _ := appliedPullFixture(t, false)
	content := []byte("Private project notes\n")
	writeFixture(t, filepath.Join(a, "private-notes.txt"), content, 0o600)
	applied := readApplied(t, a)
	planted := applied.Targets[0]
	planted.Path, planted.Owner, planted.Mode = "private-notes.txt", "", 0o600
	planted.Entries = append([]realize.Entry(nil), planted.Entries...)
	planted.OutputHash, planted.Entries[0].ManagedHash = appliedHash(content), appliedHash(content)
	applied.Targets = append(applied.Targets, planted)
	writeApplied(t, a, applied)
	before := hashProjectTree(t, a)
	result, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range result.Plan.Operations {
		if op.Path == planted.Path {
			t.Fatalf("receipt authorized an operation on private notes: %#v", op)
		}
	}
	if ledgerHasTarget(loadLedger(t, a), planted.Path) || ledgerHasTarget(readApplied(t, a), planted.Path) {
		t.Fatal("planted claim acquired or retained ownership/evidence")
	}
	after := hashProjectTree(t, a)
	delete(before, appliedPath)
	delete(after, appliedPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("receipt-only claim changed user bytes, modes or product state")
	}
}

func TestAppliedReceiptRejectsMismatchedArtifactIdentity(t *testing.T) {
	for _, mismatch := range []string{"source", "artifactId", "artifactKind", "sourcePath", "adapter", "owner", "entry-set"} {
		t.Run(mismatch, func(t *testing.T) {
			a, _, _, serviceB, pull := appliedPullFixture(t, true)
			applied := readApplied(t, a)
			for i := range applied.Targets {
				target := &applied.Targets[i]
				if target.Path != ".codex/skills/acr__example__first__skill0/references/notes.md" {
					continue
				}
				content := []byte("Local edit matching fabricated evidence\n")
				writeFixture(t, filepath.Join(a, target.Path), content, 0o644)
				target.OutputHash, target.Entries[0].ManagedHash = appliedHash(content), appliedHash(content)
				switch mismatch {
				case "source":
					target.Entries[0].Source = secondSource
				case "artifactId":
					target.Entries[0].ArtifactID = "other-artifact"
				case "artifactKind":
					target.Entries[0].ArtifactKind = realize.ArtifactManagedBlock
				case "sourcePath":
					target.Entries[0].SourcePath = "other/notes.md"
				case "adapter":
					target.Entries[0].Adapter = "cursor"
				case "owner":
					target.Owner = realize.OwnerCoordinator
				case "entry-set":
					entry := target.Entries[0]
					entry.ArtifactID = "extra-entry"
					target.Entries = append(target.Entries, entry)
				}
			}
			writeApplied(t, a, applied)
			pull()
			assertAppliedConflictUnchanged(t, a, serviceB, false)
		})
	}
}

func TestAppliedReceiptSameIdentityForgeryLimitation(t *testing.T) {
	// Documented limitation: unauthenticated same-identity evidence cannot be
	// distinguished from genuine history. This pins acceptance of a forged hash,
	// not a claim that edited-file preservation is secure against receipt writers.
	a, _, _, serviceB, pull := appliedPullFixture(t, false)
	applied := readApplied(t, a)
	for i := range applied.Targets {
		target := &applied.Targets[i]
		if strings.HasSuffix(target.Path, "/notes.md") {
			content := []byte("An edit with forged same-identity history\n")
			writeFixture(t, filepath.Join(a, target.Path), content, os.FileMode(target.Mode))
			target.OutputHash, target.Entries[0].ManagedHash = appliedHash(content), appliedHash(content)
		}
	}
	writeApplied(t, a, applied)
	pull()
	if _, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply); err != nil {
		t.Fatalf("same-identity forged evidence behavior changed: %v", err)
	}
	if got := string(readFile(t, filepath.Join(a, ".codex/skills/acr__example__first__skill0/references/notes.md"))); got != "B notes\n" {
		t.Fatalf("forged-evidence limitation no longer reproduces: %q", got)
	}
	assertAppliedEqualsCommitted(t, a)
}

func assertAppliedConflictUnchanged(t *testing.T, project string, service *Service, unusable bool) {
	t.Helper()
	before := hashProjectTree(t, project)
	exclude := string(readFile(t, filepath.Join(project, ".git/info/exclude")))
	application := &Application{service: service, fallback: cli.UnavailableApplication{}}
	stdout, stderr, exit := runCLI(t, application, "realize", "--project", project, "--json")
	if exit != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"code":"realization_conflict"`) || !strings.Contains(stderr, ".codex/skills/acr__example__first__skill0/references/notes.md") {
		t.Fatalf("expected exact generated-path refusal: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if unusable && !strings.Contains(stderr, "unusable last-applied evidence") {
		t.Fatalf("invalid receipt did not use conservative fallback: %s", stderr)
	}
	if !reflect.DeepEqual(before, hashProjectTree(t, project)) || exclude != string(readFile(t, filepath.Join(project, ".git/info/exclude"))) {
		t.Fatal("refusal changed output bytes/modes, incoming lock, receipt or exclusions")
	}
}

func TestAppliedReceiptCannotReplaceCommittedSharedTarget(t *testing.T) {
	project, root, state, value := realizationFixture(t)
	state.Project.Agents, state.Project.Freshness = []string{"codex"}, "none"
	if err := dependency.WriteState(project, state); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixtureLoader{root: root, manifest: value})
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(project, "AGENTS.md")
	content := append([]byte("# Local guidance\n\n"), readFile(t, filename)...)
	writeFixture(t, filename, content, 0o644)
	if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	applied := readApplied(t, project)
	for _, target := range loadLedger(t, project).Targets {
		if target.Path == "AGENTS.md" {
			target.Path, target.Ownership = "agents.MD", realize.OwnershipGenerated
			target.OutputHash = appliedHash(content)
			applied.Targets = append(applied.Targets, target)
		}
	}
	writeApplied(t, project, applied)
	result, err := service.Run(context.Background(), project, nil, realize.ModeApply)
	if err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filename)) != string(content) {
		t.Fatal("shared guidance was changed by a receipt alias")
	}
	for _, op := range result.Plan.Operations {
		if strings.EqualFold(op.Path, "AGENTS.md") && op.Kind != realize.OperationPreserve {
			t.Fatalf("receipt induced a whole-file shared operation: %#v", op)
		}
	}
	if ledgerHasTarget(readApplied(t, project), "agents.MD") || ledgerHasTarget(readApplied(t, project), "AGENTS.md") {
		t.Fatal("shared alias retained generated evidence")
	}
}

func TestAppliedReceiptConvergedEvidenceIsNotDrift(t *testing.T) {
	for _, evidence := range []string{"missing", "stale", "invalid", "wrong-mode"} {
		t.Run(evidence, func(t *testing.T) {
			a, _, serviceA, _, _ := appliedPullFixture(t, true)
			switch evidence {
			case "missing":
				if err := os.Remove(filepath.Join(a, appliedPath)); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(a, appliedIgnorePath)); err != nil {
					t.Fatal(err)
				}
			case "stale":
				prior := readApplied(t, a)
				for i := range prior.Targets {
					prior.Targets[i].OutputHash = appliedHash([]byte("Old version\n"))
					prior.Targets[i].Entries[0].ManagedHash = prior.Targets[i].OutputHash
				}
				writeApplied(t, a, prior)
			case "invalid":
				writeFixture(t, filepath.Join(a, appliedPath), []byte(`{"schema_version":99}`), 0o644)
			case "wrong-mode":
				if err := os.Chmod(filepath.Join(a, appliedPath), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := hashProjectTree(t, a)
			for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun} {
				result, err := serviceA.Run(context.Background(), a, nil, mode)
				if err != nil || result.Plan.HasChanges() || changeCount(result.Plan) != 0 || len(result.Notices) != 0 {
					t.Fatalf("%s reported evidence-only drift: %v, %#v", mode, err, result)
				}
				assertNoAuxiliaryChanges(t, result.Plan)
				if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
					t.Fatalf("%s wrote evidence", mode)
				}
			}
			result, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply)
			if err != nil || result.Plan.HasChanges() || changeCount(result.Plan) != 0 {
				t.Fatalf("evidence-only apply counted realization changes: %v, %#v", err, result)
			}
			assertNoAuxiliaryChanges(t, result.Plan)
			assertAppliedEqualsCommitted(t, a)
			after := hashProjectTree(t, a)
			for _, filename := range []string{appliedPath, appliedIgnorePath} {
				delete(before, filename)
				delete(after, filename)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("auxiliary refresh changed product state")
			}
			auxiliaries := snapshotAuxiliaries(t, a)
			for rerun := 0; rerun < 2; rerun++ {
				result, err := serviceA.Run(context.Background(), a, nil, realize.ModeApply)
				if err != nil || result.Plan.HasChanges() || changeCount(result.Plan) != 0 || len(result.Notices) != 0 {
					t.Fatalf("converged rerun reported drift: %v, %#v", err, result)
				}
				assertAuxiliariesUnchanged(t, a, auxiliaries)
				for name, current := range snapshotAuxiliaries(t, a) {
					was, supported := appliedMetadata(auxiliaries[name].info)
					now, _ := appliedMetadata(current.info)
					if supported && was != now {
						t.Fatalf("converged rerun changed auxiliary metadata: %s", name)
					}
				}
			}
		})
	}
}

func TestAppliedIgnoreLayoutsConvergeWithoutRewriting(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	for _, test := range []struct {
		name          string
		before        string
		want          string
		ignoredBefore bool
	}{
		{name: "trailing-blank-lines", before: "/.acr-applied.json\n\n\n", want: "/.acr-applied.json\n\n\n", ignoredBefore: true},
		{name: "CRLF", before: "operator-rule\r\n/.acr-applied.json\r\n\r\n", want: "operator-rule\r\n/.acr-applied.json\r\n\r\n", ignoredBefore: true},
		{name: "trailing-CR", before: "/.acr-applied.json\r\n", want: "/.acr-applied.json\r\n", ignoredBefore: true},
		{name: "trailing-spaces", before: "/.acr-applied.json   \n", want: "/.acr-applied.json   \n", ignoredBefore: true},
		{name: "trailing-spaces-CR", before: "/.acr-applied.json   \r\n", want: "/.acr-applied.json   \r\n", ignoredBefore: true},
		{name: "trailing-tab", before: "/.acr-applied.json\t\n", want: "/.acr-applied.json\t\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-space-tab-CR", before: "/.acr-applied.json \t\r\n", want: "/.acr-applied.json \t\r\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-VT", before: "/.acr-applied.json\v\n", want: "/.acr-applied.json\v\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-FF", before: "/.acr-applied.json\f\n", want: "/.acr-applied.json\f\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-NBSP", before: "/.acr-applied.json\u00a0\n", want: "/.acr-applied.json\u00a0\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-backslash-space", before: "/.acr-applied.json\\ \n", want: "/.acr-applied.json\\ \n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-backslash-spaces", before: "/.acr-applied.json\\   \n", want: "/.acr-applied.json\\   \n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-CR-CR", before: "/.acr-applied.json\r\r\n", want: "/.acr-applied.json\r\r\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "trailing-CR-space", before: "/.acr-applied.json\r \n", want: "/.acr-applied.json\r \n/.acr-applied.json\n", ignoredBefore: false},
		{name: "middle-rule", before: "operator-rule\n/.acr-applied.json\nother-rule\n", want: "operator-rule\n/.acr-applied.json\nother-rule\n", ignoredBefore: true},
		{name: "unterminated-rule", before: "/.acr-applied.json", want: "/.acr-applied.json", ignoredBefore: true},
		{name: "empty", before: "", want: "/.acr-applied.json\n", ignoredBefore: false},
		{name: "absent-rule", before: "operator-rule\n", want: "operator-rule\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "unterminated-other-rule", before: "operator-rule", want: "operator-rule\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "comment-only", before: "# /.acr-applied.json\n", want: "# /.acr-applied.json\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "negation-only", before: "!/.acr-applied.json\n", want: "!/.acr-applied.json\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "negation-after", before: "/.acr-applied.json\n!/.acr-applied.json\n", want: "/.acr-applied.json\n!/.acr-applied.json\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "negation-before", before: "!/.acr-applied.json\n/.acr-applied.json\n", want: "!/.acr-applied.json\n/.acr-applied.json\n", ignoredBefore: true},
		{name: "unrelated-negation-after", before: "/.acr-applied.json\n!/notes.txt\n", want: "/.acr-applied.json\n!/notes.txt\n/.acr-applied.json\n", ignoredBefore: true},
		{name: "escaped-negation-after", before: "/.acr-applied.json\n\\!/notes.txt\n", want: "/.acr-applied.json\n\\!/notes.txt\n", ignoredBefore: true},
		{name: "broad-negation-after", before: "/.acr-applied.json\n!*\n", want: "/.acr-applied.json\n!*\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "bare-negation-after", before: "/.acr-applied.json\n!\n", want: "/.acr-applied.json\n!\n/.acr-applied.json\n", ignoredBefore: true},
		{name: "negation-then-canonical", before: "/.acr-applied.json\n!*\n/.acr-applied.json\n", want: "/.acr-applied.json\n!*\n/.acr-applied.json\n", ignoredBefore: true},
		{name: "substring-only", before: "/.acr-applied.json.backup\n", want: "/.acr-applied.json.backup\n/.acr-applied.json\n", ignoredBefore: false},
		{name: "leading-whitespace", before: " /.acr-applied.json\n", want: " /.acr-applied.json\n/.acr-applied.json\n", ignoredBefore: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, _, service, _, _ := appliedPullFixture(t, true)
			filename := filepath.Join(project, appliedIgnorePath)
			writeFixture(t, filename, []byte(test.before), 0o600)
			auxiliaries := snapshotAuxiliaries(t, project)
			for run, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun, realize.ModeApply, realize.ModeApply, realize.ModeApply} {
				result, err := service.Run(context.Background(), project, nil, mode)
				if err != nil || result.Plan.HasChanges() || changeCount(result.Plan) != 0 || (mode == realize.ModeApply && len(result.Notices) != 0) {
					t.Fatalf("%s reported ignore-only drift: %v, %#v", mode, err, result)
				}
				assertNoAuxiliaryChanges(t, result.Plan)
				want := test.before
				if mode == realize.ModeApply {
					want = test.want
				}
				if got := string(readFile(t, filename)); got != want {
					t.Fatalf("%s ignore bytes = %q, want %q", mode, got, want)
				}
				info, err := os.Stat(filename)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("ignore mode changed: %v, %v", info, err)
				}
				if run == 2 && test.before != test.want {
					// The first apply appends a canonical rule; all later applies
					// must preserve the resulting bytes and file identities.
					auxiliaries = snapshotAuxiliaries(t, project)
				}
				assertAuxiliariesUnchanged(t, project, auxiliaries)
				// Fixed expectations above follow Git's pattern syntax, not
				// the recognizer's whitespace handling.
				command := exec.Command("git", "-C", project, "-c", "core.excludesFile=/dev/null", "check-ignore", "--no-index", "-q", "--", appliedPath)
				output, err := command.CombinedOutput()
				if err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 1 {
						t.Fatalf("git check-ignore: %v: %s", err, output)
					}
				}
				if got, want := err == nil, mode == realize.ModeApply || test.ignoredBefore; got != want {
					t.Fatalf("%s git ignored receipt = %t, want %t", mode, got, want)
				}
			}
		})
	}
}

func TestOversizedAppliedReceiptIsUnusableEvidence(t *testing.T) {
	for _, pulled := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "pulled-lock"}[pulled], func(t *testing.T) {
			project, _, serviceA, serviceB, pull := appliedPullFixture(t, true)
			service := serviceA
			if pulled {
				pull()
				service = serviceB
			}
			// Fixed oversized regular file: the read limit, not JSON decoding,
			// must make this receipt unusable. Do not derive size from the code.
			writeFixture(t, filepath.Join(project, appliedPath), []byte(strings.Repeat(" ", (32<<20)+1)), 0o644)
			before := hashProjectTree(t, project)
			auxiliaries := snapshotAuxiliaries(t, project)
			for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun} {
				result, err := service.Run(context.Background(), project, nil, mode)
				if pulled {
					var conflict *realize.ConflictError
					if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "unusable last-applied evidence") || strings.Contains(err.Error(), "missing last-applied evidence") {
						t.Fatalf("%s trusted oversized evidence or misclassified it: %v", mode, err)
					}
				} else if err != nil || result.Plan.HasChanges() || changeCount(result.Plan) != 0 {
					t.Fatalf("%s rejected current state with oversized receipt: %v", mode, err)
				}
				if !reflect.DeepEqual(before, hashProjectTree(t, project)) {
					t.Fatalf("%s wrote with oversized evidence", mode)
				}
				assertAuxiliariesUnchanged(t, project, auxiliaries)
			}
			_, err := service.Run(context.Background(), project, nil, realize.ModeApply)
			var oversized *realize.FileTooLargeError
			if !errors.As(err, &oversized) || oversized.Path != appliedPath || !strings.Contains(err.Error(), "back up/remove") || !strings.Contains(err.Error(), "preserve .agents/registry.lock") {
				t.Fatalf("apply oversized before-image refusal = %v", err)
			}
			if !reflect.DeepEqual(before, hashProjectTree(t, project)) {
				t.Fatal("oversized apply refusal changed outputs, lock, receipt, ignore file or created a journal")
			}
			assertAuxiliariesUnchanged(t, project, auxiliaries)
		})
	}
}

func TestOversizedAppliedIgnoreRemainsARefusal(t *testing.T) {
	project, _, service, _, _ := appliedPullFixture(t, false)
	writeFixture(t, filepath.Join(project, appliedIgnorePath), []byte(strings.Repeat(" ", (32<<20)+1)), 0o644)
	before := hashProjectTree(t, project)
	auxiliaries := snapshotAuxiliaries(t, project)
	for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun, realize.ModeApply} {
		_, err := service.Run(context.Background(), project, nil, mode)
		var oversized *realize.FileTooLargeError
		if !errors.As(err, &oversized) || oversized.Path != appliedIgnorePath {
			t.Fatalf("%s oversized ignore refusal = %v", mode, err)
		}
		if !reflect.DeepEqual(before, hashProjectTree(t, project)) {
			t.Fatalf("%s wrote with an oversized ignore file", mode)
		}
		assertAuxiliariesUnchanged(t, project, auxiliaries)
	}
}

func TestAppliedReceiptCannotMixHashAndModeTuples(t *testing.T) {
	a, b, _, serviceB, pull := appliedPullFixture(t, true)
	path := ".codex/skills/acr__example__first__skill0/references/notes.md"
	// B bytes with A's mode are neither B(0600) nor A(0644).
	state, err := dependency.LoadState(b)
	if err != nil {
		t.Fatal(err)
	}
	ledger := loadLedger(t, b)
	for i := range ledger.Targets {
		if ledger.Targets[i].Path == path {
			ledger.Targets[i].Mode = 0o600
		}
	}
	state.Lock.Realization, err = realize.EncodeLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := dependency.WriteState(b, state); err != nil {
		t.Fatal(err)
	}
	pull()
	writeFixture(t, filepath.Join(a, path), []byte("B notes\n"), 0o644)
	assertAppliedConflictUnchanged(t, a, serviceB, false)
}

func TestAppliedReceiptDroppedUnownedLeafDoesNotAuthorizeRemoval(t *testing.T) {
	a, b, _, serviceB, _ := appliedPullFixture(t, false)
	// B independently stops shipping run.sh. Re-realize B while its committed
	// ledger still owns the old leaf, then bring only B's ledger into A.
	loader := serviceB.loader.(*multiPackageLoader)
	if err := os.Remove(filepath.Join(loader.packages[firstSource].root, "skills/skill0/run.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceB.Run(context.Background(), b, nil, realize.ModeApply); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(a, dependency.LockFilename), readFile(t, filepath.Join(b, dependency.LockFilename)), 0o644)
	before := hashProjectTree(t, a)
	_, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply)
	// Native validation refuses the now-unowned leaf. Receipt evidence cannot
	// whitelist it or turn it into a deletion target.
	var nativeErr *adapter.NativeValidationError
	if !errors.As(err, &nativeErr) || nativeErr.Code != adapter.CodeInvalidSkillTree {
		t.Fatalf("apply error = %v, want invalid_skill_tree native validation error", err)
	}
	const want = `adapter "claude-code" validate: invalid_skill_tree: skill tree ".claude/skills/acr__example__first__skill0" contains unexpected file ".claude/skills/acr__example__first__skill0/run.sh"`
	if err.Error() != want {
		t.Fatalf("apply error = %v, want %q", err, want)
	}
	if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
		t.Fatal("unowned leaf refusal changed product bytes, modes or the receipt")
	}
}

func TestAppliedReceiptTrackedSpellingsRefuseBeforeMutation(t *testing.T) {
	for _, linked := range []bool{false, true} {
		for _, spelling := range []string{appliedPath, ".agents/.ACR-APPLIED.JSON", ".AGENTS/.acr-applied.json"} {
			t.Run(map[bool]string{false: "standard", true: "linked"}[linked]+"/"+spelling, func(t *testing.T) {
				a, b, serviceA, serviceB, _ := appliedPullFixture(t, true)
				if linked {
					gitFixtureCommand(t, a, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "Initialize fixture")
					worktree := filepath.Join(t.TempDir(), "linked")
					gitFixtureCommand(t, a, "worktree", "add", "-qb", "linked", worktree)
					state, err := dependency.LoadState(a)
					if err != nil {
						t.Fatal(err)
					}
					if err := dependency.WriteState(worktree, state); err != nil {
						t.Fatal(err)
					}
					if _, err := serviceA.Run(context.Background(), worktree, nil, realize.ModeApply); err != nil {
						t.Fatal(err)
					}
					a = worktree
				}
				if spelling != appliedPath {
					canonical := filepath.Join(a, appliedPath)
					alias := filepath.Join(a, spelling)
					if _, err := os.Stat(alias); err == nil {
						if err := os.Rename(canonical, alias); err != nil {
							t.Fatal(err)
						}
					} else if errors.Is(err, os.ErrNotExist) {
						// SameFile exercises alias handling on sensitive filesystems
						// too; insensitive storage uses the actual renamed spelling.
						if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.Link(canonical, alias); err != nil {
							t.Fatal(err)
						}
					} else {
						t.Fatal(err)
					}
				}
				gitFixtureCommand(t, a, "add", "-f", "--", spelling)
				writeFixture(t, filepath.Join(a, dependency.LockFilename), readFile(t, filepath.Join(b, dependency.LockFilename)), 0o644)
				before := hashProjectTree(t, a)
				index := gitFixtureCommand(t, a, "ls-files", "--stage", "-z")
				result, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply)
				if err == nil || !strings.Contains(err.Error(), "git rm --cached -- "+spelling) {
					t.Fatalf("tracked %s not refused with indexed spelling: %v, %#v", spelling, err, result)
				}
				if !reflect.DeepEqual(before, hashProjectTree(t, a)) || index != gitFixtureCommand(t, a, "ls-files", "--stage", "-z") {
					t.Fatal("tracked receipt refusal changed outputs, state, evidence or Git index")
				}
			})
		}
	}
}

func TestAppliedReceiptOrdinaryRunDoesNotInspectFullIndex(t *testing.T) {
	project, _, service, _, _ := appliedPullFixture(t, true)
	writeFixture(t, filepath.Join(project, "unrelated", "tracked.txt"), []byte("unrelated\n"), 0o644)
	gitFixtureCommand(t, project, "add", "--", "unrelated/tracked.txt")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "index-queries")
	// Reject any full-index query before Git can return unrelated paths for
	// metadata inspection. Planner queries for specific output paths still run.
	writeFixture(t, filepath.Join(bin, "git"), []byte(`#!/bin/bash
set -euo pipefail
audit() {
    local indexed=0 paths=0 count=0 arg
    for arg in "$@"; do
        if [[ "$arg" == ls-files ]]; then indexed=1; fi
        if [[ "$indexed" == 1 && "$paths" == 1 ]]; then
            if [[ "$arg" == . ]]; then
                echo "full-index query would inspect unrelated tracked paths" >&2
                exit 81
            fi
            printf '%s\n' "$arg" >> "$ACR_TEST_INDEX_QUERIES"
            count=$((count + 1))
        fi
        if [[ "$indexed" == 1 && "$arg" == -- ]]; then paths=1; fi
    done
    if [[ "$indexed" == 1 && "$count" == 0 ]]; then
        echo "unbounded index query" >&2
        exit 82
    fi
}
audit "$@"
exec "$ACR_TEST_REAL_GIT" "$@"
`), 0o755)
	t.Setenv("ACR_TEST_REAL_GIT", realGit)
	t.Setenv("ACR_TEST_INDEX_QUERIES", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mode := range []realize.Mode{realize.ModeCheck, realize.ModeDryRun, realize.ModeApply} {
		if _, err := service.Run(context.Background(), project, nil, mode); err != nil {
			t.Fatalf("ordinary %s run: %v", mode, err)
		}
	}
	queries := string(readFile(t, log))
	if strings.Count(queries, ":(icase,literal)"+appliedPath+"\n") != 4 || strings.Contains(queries, "unrelated/tracked.txt") {
		t.Fatalf("receipt checks did not limit index candidates: %q", queries)
	}
}

func TestAppliedReceiptDoesNotExpandUninstallCoverage(t *testing.T) {
	a, _, serviceA := sharedSkillFixture(t, []string{"codex", "cursor"}, true, firstSource)
	b, _, serviceB := sharedSkillFixture(t, []string{"codex"}, true, firstSource)
	for project, service := range map[string]*Service{a: serviceA, b: serviceB} {
		state, err := dependency.LoadState(project)
		if err != nil {
			t.Fatal(err)
		}
		state.Project.Freshness = "none"
		if err := dependency.WriteState(project, state); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{dependency.ProjectFilename, dependency.LockFilename} {
		writeFixture(t, filepath.Join(a, path), readFile(t, filepath.Join(b, path)), 0o644)
	}
	before := hashProjectTree(t, a)
	result, err := serviceB.Uninstall(context.Background(), a, firstSource, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Agents, []string{"codex"}) {
		t.Fatalf("receipt-only Cursor expanded uninstall coverage: %v", result.Agents)
	}
	for _, op := range result.Plan.Operations {
		if strings.HasPrefix(op.Path, ".cursor/") {
			t.Fatalf("receipt-only Cursor output was planned: %#v", op)
		}
	}
	after := hashProjectTree(t, a)
	cursorFiles := 0
	for path, fingerprint := range before {
		if strings.HasPrefix(path, ".cursor/") {
			cursorFiles++
			if after[path] != fingerprint {
				t.Fatalf("uninstall changed receipt-only Cursor file %s", path)
			}
		}
	}
	if cursorFiles != 3 {
		t.Fatalf("fixture Cursor files = %d, want 3", cursorFiles)
	}
	for _, target := range readApplied(t, a).Targets {
		if strings.HasPrefix(target.Path, ".cursor/") {
			t.Fatal("successful realization retained ineligible Cursor evidence")
		}
	}
}

func TestAppliedReceiptCompilerUsesPriorEntryHashes(t *testing.T) {
	makeVersion := func(version string) (string, *Service) {
		project, loader, application := uninstallFixtureWithHooks(t, []string{"claude-code", "codex"}, firstSource)
		state, err := dependency.LoadState(project)
		if err != nil {
			t.Fatal(err)
		}
		state.Project.Freshness = "none"
		state.Lock.Dependencies[0].Tag = "v" + version
		state.Lock.Dependencies[0].PackageVersion = version
		if err := dependency.WriteState(project, state); err != nil {
			t.Fatal(err)
		}
		if version == "2.0.0" {
			pkg := loader.packages[firstSource]
			writeFixture(t, filepath.Join(pkg.root, "rules/guidance.md"), []byte("# Updated guidance\n"), 0o644)
			pkg.manifest.Artifacts.Hooks[0].Args = []string{"--new-argument"}
			loader.packages[firstSource] = pkg
		}
		if _, err := application.service.Run(context.Background(), project, nil, realize.ModeApply); err != nil {
			t.Fatal(err)
		}
		return project, application.service
	}
	for _, scenario := range []string{"old-A", "current-B", "edited-block", "edited-entry"} {
		t.Run(scenario, func(t *testing.T) {
			a, _ := makeVersion("1.0.0")
			b, serviceB := makeVersion("2.0.0")
			writeFixture(t, filepath.Join(a, dependency.LockFilename), readFile(t, filepath.Join(b, dependency.LockFilename)), 0o644)
			switch scenario {
			case "current-B":
				for _, target := range loadLedger(t, b).Targets {
					writeFixture(t, filepath.Join(a, target.Path), readFile(t, filepath.Join(b, target.Path)), os.FileMode(target.Mode))
				}
			case "edited-block":
				path := filepath.Join(a, "AGENTS.md")
				writeFixture(t, path, []byte(strings.Replace(string(readFile(t, path)), "# Guidance", "# Edited guidance", 1)), 0o644)
			case "edited-entry":
				path := filepath.Join(a, ".claude/settings.json")
				writeFixture(t, path, []byte(strings.Replace(string(readFile(t, path)), "session.sh", "edited-session.sh", 1)), 0o644)
			}
			before := hashProjectTree(t, a)
			result, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply)
			if strings.HasPrefix(scenario, "edited-") {
				path := "AGENTS.md"
				if scenario == "edited-entry" {
					path = ".claude/settings.json"
				}
				if err == nil || !strings.Contains(err.Error(), path) || !reflect.DeepEqual(before, hashProjectTree(t, a)) {
					t.Fatalf("edited compiler input was accepted or mutated: %v", err)
				}
				return
			}
			if err != nil || result.Plan.LedgerChanged {
				t.Fatalf("compiler comparison with %s: %v, %#v", scenario, err, result.Plan)
			}
			if scenario == "current-B" && result.Plan.HasChanges() {
				t.Fatal("old receipt caused rewrites of committed Markdown/config bytes")
			}
			if !reflect.DeepEqual(productTreeWithoutApplied(t, a), productTreeWithoutApplied(t, b)) {
				t.Fatal("compiler did not converge to independent B output/entry hashes")
			}
			assertAppliedEqualsCommitted(t, a)
		})
	}
}

func TestAppliedReceiptIgnoresVersionAndExclusionClaims(t *testing.T) {
	a, _, _, serviceB, pull := appliedPullFixture(t, true)
	evidence := readApplied(t, a)
	for i := range evidence.Targets {
		evidence.Targets[i].Excluded = false
		for j := range evidence.Targets[i].Entries {
			evidence.Targets[i].Entries[j].AdapterVersion = "historical-version"
		}
	}
	writeApplied(t, a, evidence)
	pull()
	result, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply)
	if err != nil || result.Plan.LedgerChanged {
		t.Fatalf("comparison version or exclusion claims changed ownership: %v, %#v", err, result.Plan)
	}
	assertAppliedEqualsCommitted(t, a)
}
