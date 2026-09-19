package codecensus

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOmittedCodeAgainstRuntime(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		for _, allowEmpty := range []bool{false, true} {
			for _, shape := range []struct{ name, expr, actual string }{
				{"omitted", `Error{}`, ""},
				{"message_only", `Error{Message:"detail"}`, ""},
				{"explicit_empty", `Error{Code:""}`, ""},
				{"registered", `Error{Code:"first"}`, "first"},
			} {
				t.Run(fmt.Sprintf("seed_%t/allow_%t/%s", seeded, allowEmpty, shape.name), func(t *testing.T) {
					dir := t.TempDir()
					writeAssignmentFixture(t, filepath.Join(dir, "go.mod"), "module sample\n\ngo 1.25\n")
					seed := ""
					if seeded {
						seed = `var seed=Error{Code:"first"}` + "\n"
					}
					source := "package sample\ntype Error struct{Code,Message string}\ntype Notice struct{Code string}\nvar notice=Notice{Code:\"notice_only\"}\n" + seed + "func emit() Error{return " + shape.expr + "}\n"
					source += "func normalized() string{code:=emit().Code;if code==\"\"{return \"first\"};return code}\n"
					writeAssignmentFixture(t, filepath.Join(dir, "fixture.go"), source)
					returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code;t.Logf("ACTUAL=%%q NORMALIZED=%%q",got,normalized());if got!=%q||normalized()!="first"{t.Fatal(got,normalized())}`, shape.actual))
					for _, moved := range []bool{false, true} {
						input := source
						filename := "fixture.go"
						if moved {
							input = "// Source movement control.\n\n" + input
							filename = "moved.go"
						}
						got, err := Analyze([]Source{{Package: "sample", Filename: filename, Content: []byte(input)}}, nil, []Target{
							{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first"}, AllowEmpty: allowEmpty},
							{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"notice_only"}},
						})
						if err != nil {
							t.Fatal(err)
						}
						t.Logf("moved=%t codes=%v diagnostics=%v", moved, got.Codes, got.Diagnostics)
						var wantCodes []string
						if shape.actual == "" && !allowEmpty {
							wantCodes = append(wantCodes, "")
						}
						if seeded || shape.actual != "" {
							wantCodes = append(wantCodes, "first")
						}
						if !reflect.DeepEqual(got.Codes["refusal"], wantCodes) || !reflect.DeepEqual(got.Codes["notice"], []string{"notice_only"}) {
							t.Errorf("codes=%v want refusal=%v notice=[notice_only]", got.Codes, wantCodes)
						}
						var want []string
						if shape.actual == "" && !allowEmpty {
							line := 1 + strings.Count(input[:strings.Index(input, shape.expr)], "\n")
							want = []string{fmt.Sprintf("%s:%d: refusal: unregistered code %q", filename, line, "")}
						}
						var diagnostics []string
						for _, d := range got.Diagnostics {
							diagnostics = append(diagnostics, d.String())
						}
						if !reflect.DeepEqual(diagnostics, want) {
							t.Errorf("diagnostics=%v want=%v", diagnostics, want)
						}
					}
				})
			}
		}
	}
}
