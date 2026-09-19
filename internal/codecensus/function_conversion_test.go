package codecensus

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// A registered direct call used to hide the dependency lost by conversion.
// Compare the same named function type with assignment, both with and without
// that seed, so an empty-parameter fallback cannot satisfy the diagnostic oracle.
func TestNamedFunctionConversionAssignmentContrasts(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.27\n")
	for _, seeded := range []bool{false, true} {
		for _, route := range []struct{ name, binding string }{
			{"conversion", "chosen := Handler(output)"},
			{"typed_assignment", "var chosen Handler = output"},
		} {
			for _, value := range []struct{ name, expression, actual string }{
				{"registered", `"second"`, "second"},
				{"bad", `"returned_unregistered"`, "returned_unregistered"},
				{"computed", "compute()", "returned_unregistered"},
			} {
				t.Run(fmt.Sprintf("seed_%t/%s/%s", seeded, route.name, value.name), func(t *testing.T) {
					seed := ""
					if seeded {
						seed = `_ = output("first")`
					}
					source := returnedResultsPrelude + fmt.Sprintf(`
type Handler func(string) Error
func emit() Error {
 _ = Notice{Code:"notice_only"}
 %s
 %s
 return chosen(%s)
}
`, seed, route.binding, value.expression)
					const name = "fixture.go"
					dir := filepath.Join(root, fmt.Sprintf("seed_%t", seeded), route.name, value.name)
					writeAssignmentFixture(t, filepath.Join(dir, name), source)
					returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatal(got)}`, value.actual))
					got := analyzeReturnedResults(t, name, source)
					var wantCodes, wantDiagnostics []string
					if seeded {
						wantCodes = append(wantCodes, "first")
					}
					if value.name != "computed" {
						wantCodes = append(wantCodes, value.actual)
					}
					sort.Strings(wantCodes)
					line := sourceLine(source, "return chosen("+value.expression+")")
					switch value.name {
					case "bad":
						wantDiagnostics = []string{fmt.Sprintf("%s:%d: refusal: unregistered code %q", name, line, value.actual)}
					case "computed":
						wantDiagnostics = []string{fmt.Sprintf("%s:%d: refusal: cannot prove code expression; use a constant or a supported assignment flow", name, line)}
					}
					if !reflect.DeepEqual(got.Codes["refusal"], wantCodes) {
						t.Errorf("codes=%v want %v", got.Codes["refusal"], wantCodes)
					}
					if diagnostics := diagnosticStrings(got); !reflect.DeepEqual(diagnostics, wantDiagnostics) {
						t.Errorf("diagnostics=%v want %v", diagnostics, wantDiagnostics)
					}
					assertNoticeIsolated(t, got, []string{"notice_only"})
				})
			}
		}
	}
}
