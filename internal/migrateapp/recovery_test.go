package migrateapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func TestCoverageRecoveryPreservesTheInstallation(t *testing.T) {
	for _, test := range []struct {
		name         string
		inventory    migrate.Report
		code, remedy string
	}{
		{name: "uncovered agent", inventory: migrate.Report{Agents: []migrate.AgentCoverage{{ID: "gemini"}}}, code: "uncovered-agent",
			remedy: "if you no longer use gemini, remove its Tessl-generated native output outside the Tessl installation, then rerun acr migrate tessl --finalize; otherwise keep Tessl installed and do not finalize"},
		{name: "unsupported artifact", inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "check", Kind: "hook", Classification: "unsupported"}}}}}, code: "unsupported-artifact",
			remedy: "keep Tessl installed and do not finalize; to finalize, remove this unsupported artifact in the source package, reinstall or update through Tessl, then rerun acr migrate tessl --finalize"},
		{name: "lossy artifact", inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "check", Kind: "rule", Lossy: []string{"description"}}}}}}, code: "lossy-artifact",
			remedy: "keep Tessl installed and do not finalize; to finalize, remove this lossy artifact in the source package, reinstall or update through Tessl, then rerun acr migrate tessl --finalize"},
		{name: "ambiguous artifact", inventory: migrate.Report{Packages: []migrate.PackageReport{{Artifacts: []migrate.ArtifactReport{{ID: "check", Kind: "rule", Classification: "ambiguous"}}}}}, code: "ambiguous-artifact",
			remedy: "resolve the reported ambiguity in the source package or its native output, reinstall or regenerate through Tessl as needed, then rerun acr migrate tessl --finalize"},
	} {
		t.Run(test.name, func(t *testing.T) {
			blockers := coverageBlockers(test.inventory, nil, migrate.AcceptedSet{})
			if len(blockers) != 1 || blockers[0].Code != test.code || blockers[0].Remedy != test.remedy {
				t.Fatalf("blockers=%+v want %s remedy=%q", blockers, test.code, test.remedy)
			}
			if strings.Contains(blockers[0].Remedy, ".tessl/") {
				t.Fatal("remedy targets installed files")
			}
		})
	}
}

func TestNativeEntryRecoveryNamesTheEntry(t *testing.T) {
	const path = ".agents/skills/review"
	const remedy = "remove the native entry .agents/skills/review or repoint it to a retained skill tree outside the Tessl installation, then rerun acr migrate tessl --finalize"
	_, _, blockers, err := sharedSurfacePlan(nil, migrate.Report{SharedSkills: []migrate.SharedSkillEntry{{Path: path, Disposition: migrate.SharedSkillBlocked}}}, realize.Ledger{})
	if err != nil || len(blockers) != 1 || blockers[0].Remedy != remedy {
		t.Fatalf("blockers=%+v err=%v", blockers, err)
	}
	entry := retainedDependency{path: path, kind: "skill", target: ".tessl/plugins/example/alpha/skills/review"}
	if blocker := danglingBlocker(entry, "the installed tree"); blocker.Remedy != remedy {
		t.Fatalf("dangling recovery=%+v", blocker)
	}
	if blocker := unprovenBlocker(entry, "inspection failed"); blocker.Remedy != "repoint the native entry .agents/skills/review at an inspectable retained tree outside the Tessl installation without unowned links, or remove that entry, then rerun acr migrate tessl --finalize" {
		t.Fatalf("unproven recovery=%+v", blocker)
	}
}

func TestTesslDirectoryDepthRecovery(t *testing.T) {
	root := t.TempDir()
	directory := ".tessl" + strings.Repeat("/level", 65)
	if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
		t.Fatal(err)
	}
	before := hashTreeWithModes(t, root)
	project, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	_, err = prunedTesslDirectories(project, nil)
	want := fmt.Sprintf("%q nests deeper than 64 directories; shorten the directory structure in the source package and reinstall or update it through Tessl, or remove the affected package through Tessl; then rerun acr migrate tessl --finalize", directory)
	if err == nil || err.Error() != want {
		t.Fatalf("depth recovery=%v want=%q", err, want)
	}
	if !mapsEqual(before, hashTreeWithModes(t, root)) {
		t.Fatal("depth refusal changed installation")
	}
}
