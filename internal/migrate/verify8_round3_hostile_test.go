package migrate

import (
	"reflect"
	"testing"
)

// TestVerify8Round3SortMigrationReportOrdersRemovalsByPathKindID pins tester
// advisory 3 where the guarantee is observable. The finalization pipeline
// cannot currently produce an out-of-order removal list — plan.Edits is already
// path-ordered and every reachable removal path sorts before the forced-last
// "tessl.json" — so an end-to-end row cannot distinguish the sort from the
// discovery order. This row feeds SortMigrationReport a scrambled list and
// asserts the canonical order directly, including the tie-breaks on kind and on
// id that a path-only comparison would leave untouched.
func TestVerify8Round3SortMigrationReportOrdersRemovalsByPathKindID(t *testing.T) {
	t.Parallel()

	report := MigrationReport{Removed: []RemovalRecord{
		{Path: "tessl.json", Kind: "manifest"},
		{Path: ".claude/settings.json", Kind: "structured-entry", ID: "tessl.hooks.example/orphan"},
		{Path: ".claude/settings.json", Kind: "structured-entry", ID: "tessl.hooks.example/alpha"},
		{Path: ".claude/settings.json", Kind: "managed-span", ID: "tessl-managed"},
		{Path: ".claude/skills/tessl__review", Kind: "skill", ID: "review"},
	}}

	SortMigrationReport(&report)

	got := make([]string, 0, len(report.Removed))
	for _, removal := range report.Removed {
		got = append(got, removal.Path+"|"+removal.Kind+"|"+removal.ID)
	}
	want := []string{
		".claude/settings.json|managed-span|tessl-managed",
		".claude/settings.json|structured-entry|tessl.hooks.example/alpha",
		".claude/settings.json|structured-entry|tessl.hooks.example/orphan",
		".claude/skills/tessl__review|skill|review",
		"tessl.json|manifest|",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed order = %#v, want %#v", got, want)
	}
}
