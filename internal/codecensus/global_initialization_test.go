package codecensus

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Each fixture is compiled and run before its census is checked. File order and
// package spelling must not turn a registered global into an unknown source.
func TestGlobalInitializationAgainstRuntime(t *testing.T) {
	for _, layout := range []string{"before", "later", "cross_file", "cross_package"} {
		for _, val := range []struct {
			name, expr, actual string
			unknown            bool
		}{
			{"registered", `"first"`, "first", false},
			{"invalid", `"bad"`, "bad", false},
			{"unresolved", `compute()`, "bad", true},
		} {
			t.Run(layout+"/"+val.name, func(t *testing.T) {
				dir := t.TempDir()
				writeAssignmentFixture(t, filepath.Join(dir, "go.mod"), "module sample\n\ngo 1.25\n")
				header := "package sample\ntype Error struct{ Code string }\nfunc compute() string {return \"bad\"}\n"
				init := "var defaultCode = " + val.expr + "\n"
				use := "var emitted = Error{Code:defaultCode}\nfunc emit() Error{return emitted}\n"
				sources := []Source{{Package: "sample", Filename: "fixture.go", Content: []byte(header + init + use)}}
				location := "fixture.go"
				if layout == "later" {
					sources[0].Content = []byte(header + use + init)
				}
				if layout == "cross_file" {
					sources[0].Content = []byte(header + use)
					location = "z_global.go"
					sources = append(sources, Source{Package: "sample", Filename: location, Content: []byte("package sample\n" + init)})
				}
				if layout == "cross_package" {
					sources[0].Content = []byte("package sample\nimport \"sample/zdep\"\ntype Error struct{Code string}\nvar emitted=Error{Code:zdep.Emitted.Code}\nfunc emit() Error{return emitted}\n")
					location = "zdep/global.go"
					sources = append(sources, Source{Package: "sample/zdep", Filename: location, Content: []byte("package zdep\ntype Output struct{Code string}\nvar Emitted=Output{Code:defaultCode}\n" + init + "func compute() string{return \"bad\"}\n")})
				}
				for _, s := range sources {
					writeAssignmentFixture(t, filepath.Join(dir, s.Filename), string(s.Content))
				}
				returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code;t.Logf("ACTUAL=%%q",got);if got!=%q{t.Fatal(got)}`, val.actual))
				target := []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first"}}}
				got, err := Analyze(sources, nil, target)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("codes=%v diagnostics=%v", got.Codes, got.Diagnostics)
				reversed := slices.Clone(sources)
				slices.Reverse(reversed)
				again, err := Analyze(reversed, nil, target)
				if err != nil || !reflect.DeepEqual(got, again) {
					t.Fatalf("input order changed result: %v / %v (%v)", got, again, err)
				}
				var wantCodes []string
				if !val.unknown {
					wantCodes = []string{val.actual}
				}
				if !reflect.DeepEqual(got.Codes["refusal"], wantCodes) {
					t.Errorf("codes=%v want=%v", got.Codes, wantCodes)
				}
				var diagnostics []string
				for _, d := range got.Diagnostics {
					diagnostics = append(diagnostics, d.String())
				}
				var want []string
				if val.name != "registered" {
					var input string
					for _, s := range sources {
						if s.Filename == location {
							input = string(s.Content)
						}
					}
					offset := strings.Index(input, "var defaultCode = "+val.expr)
					if offset < 0 {
						t.Fatal("missing initializer")
					}
					message := `unregistered code "bad"`
					if val.unknown {
						message = "cannot prove code expression; use a constant or a supported assignment flow"
					}
					want = []string{fmt.Sprintf("%s:%d: refusal: %s", location, 1+strings.Count(input[:offset], "\n"), message)}
				}
				if !reflect.DeepEqual(diagnostics, want) {
					t.Errorf("diagnostics=%v want=%v", diagnostics, want)
				}
			})
		}
	}
}

func TestGlobalInitializationPreservesZeroAndResultSlots(t *testing.T) {
	for _, tc := range []struct {
		name, body, actual string
		codes              []string
	}{
		{"zero", `var emitted=Error{Code:code};var code string;func emit() Error{return emitted}`, "", []string{""}},
		{"separate_values", `var emitted=Error{Code:second};var first,second="unused","first";func emit() Error{return emitted}`, "first", []string{"first"}},
		{"callable_dependency", `var emitted=chosen("first");var chosen=output;func output(code string) Error{return Error{Code:code}};func emit() Error{return emitted}`, "first", []string{"first"}},
		{"tuple_slots", `var chosen,_,other=supply();func supply()(func(string)Error,bool,func(string)Notice){return output,true,observe};func output(code string)Error{return Error{Code:code}};func observe(code string)Notice{return Notice{Code:code}};func emit()Error{other("notice_only");return chosen("first")}`, "first", []string{"first"}},
		{"later_write", `var code="first";func emit()Error{code="second";return Error{Code:code}}`, "second", []string{"first", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAssignmentFixture(t, filepath.Join(dir, "go.mod"), "module sample\n\ngo 1.25\n")
			source := "package sample\ntype Error struct{Code string}\ntype Notice struct{Code string}\nvar _=Notice{Code:\"notice_only\"}\n" + tc.body + "\n"
			writeAssignmentFixture(t, filepath.Join(dir, "fixture.go"), source)
			returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code;t.Logf("ACTUAL=%%q",got);if got!=%q{t.Fatal(got)}`, tc.actual))
			got, err := Analyze([]Source{{Package: "sample", Filename: "fixture.go", Content: []byte(source)}}, nil, []Target{
				{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"", "first", "second"}},
				{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"notice_only"}},
			})
			if err != nil || len(got.Diagnostics) != 0 || !reflect.DeepEqual(got.Codes["refusal"], tc.codes) || !reflect.DeepEqual(got.Codes["notice"], []string{"notice_only"}) {
				t.Fatalf("result=%v err=%v want refusal=%q notice=[notice_only]", got, err, tc.codes)
			}
		})
	}
}
