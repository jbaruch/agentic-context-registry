package adaptertest

import (
	"testing"
)

func TestFollowupsPreservationFixturesStillPlanIdentically(t *testing.T) {
	t.Run("reference-skill-shared-digest", func(t *testing.T) {
		RunGoldenFixture(t, "testdata/skill-shared-digest", "testdata/skill-shared-digest/want", NewReferenceAdapter("1.0.0"), NewCompiler())
	})
	t.Run("reference-rule-and-script", func(t *testing.T) {
		RunGoldenFixture(t, "testdata/rule-and-script", "testdata/rule-and-script/want", NewReferenceAdapter("1.0.0"), NewCompiler())
	})
	t.Run("native-all-agents", func(t *testing.T) {
		runNativeGoldenMatrix(t, "all-agents")
	})
	t.Run("native-freshness-session-start", func(t *testing.T) {
		runNativeGoldenMatrix(t, "freshness-session-start")
	})
}
