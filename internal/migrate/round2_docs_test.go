package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stage 1 runs the inventory with a mapping already supplied, so a reader whose
// package carries no repository evidence meets unmapped_package there. The
// stage has to name the code and forward to stage 2.
func TestMigrationGuideStageOneForwardsUnmappedPackages(t *testing.T) {
	t.Parallel()

	stage := migrationGuideStage(t, 1)
	for _, clause := range []string{"`unmapped_package`", "(#stage-2-resolve-missing-mappings)"} {
		if !strings.Contains(stage, clause) {
			t.Errorf("migration guide stage 1 does not state %q", clause)
		}
	}
}

// A mapped repository whose producer is unpublished 404s during release
// resolution and reports migrate_failed, not source_not_a_package. Stage 0 and
// stage 2 both have to predict that and send the reader back to stage 0.
func TestMigrationGuideStatesTheMissingReleaseFailure(t *testing.T) {
	t.Parallel()

	for _, stage := range []int{0, 2} {
		text := migrationGuideStage(t, stage)
		for _, clause := range []string{"release", "`migrate_failed`", "404"} {
			if !strings.Contains(text, clause) {
				t.Errorf("migration guide stage %d does not state %q", stage, clause)
			}
		}
		if !strings.Contains(text, "gh auth login") {
			t.Errorf("migration guide stage %d does not exclude gh auth login as the remedy", stage)
		}
	}
}

// The derived issue-URL check replaced three literal issue assertions, so
// deleting a deferral bullet outright now reds nothing. Keep the three v1
// deferrals named until the issues that track them close.
func TestReadmeRetainsTheThreeDeferredCapabilityIssues(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	bullets := strings.Join(collectBulletText(deferredCapabilityBullets(t, string(content))), "\n")
	for _, issue := range []string{"issues/14", "issues/13", "issues/4"} {
		if !strings.Contains(bullets, issue) {
			t.Errorf("README deferred capabilities no longer name %s", issue)
		}
	}
}

func collectBulletText(bullets []readmeBullet) []string {
	text := make([]string, 0, len(bullets))
	for _, bullet := range bullets {
		text = append(text, bullet.text)
	}
	return text
}

// migrationGuideStage returns the prose of one numbered stage, ending at the
// next stage heading so a clause cannot satisfy the stage it was moved out of.
func migrationGuideStage(t *testing.T, stage int) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "migration-guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "## Stage " + string(rune('0'+stage)) + ":"
	lines := strings.Split(string(content), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, prefix) {
			start = index
			break
		}
	}
	if start < 0 {
		t.Fatalf("migration guide has no %q heading", prefix)
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		if strings.HasPrefix(lines[index], "## Stage ") {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}
