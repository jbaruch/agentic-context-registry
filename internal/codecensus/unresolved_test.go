package codecensus

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The result-kind and partial-resolution cases were reported by Claude / Fable
// 5.1 (r62v10) and independently confirmed by Grok (r62r10). These fixtures and
// exact namespace/location assertions are newly authored from those behaviors.
func TestUnresolvedResultKinds(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.27\n")
	for _, kind := range []struct{ name, result, ret string }{
		{"struct", "Error", "return sink"},
		{"pointer", "*Error", "return &sink"},
		{"error", "error", "return sink"},
		{"any", "any", "return sink"},
		{"wrapper", "Wrapped", "return Wrapped{sink}"},
		{"slice", "[]Error", "return []Error{sink}"},
		{"void", "", ""},
	} {
		fn := "func(string) " + kind.result
		for _, route := range []struct {
			name, extra, body string
			finite            bool
		}{
			{"direct", "", "chosen := output; chosen(VALUE)", true},
			{"conversion", "type Fn " + fn, "Fn(output)(VALUE)", true},
			{"field", "type Box struct{F " + fn + "}", "b := Box{F:output}; b.F(VALUE)", true},
			{"interface", "type Emitter interface{ Emit(string) " + kind.result + " }; type impl struct{}; func(impl) Emit(code string) " + kind.result + " { sink=Error{Code:code}; " + kind.ret + " }", "impl{}.Emit(\"first\"); var chosen Emitter = impl{}; chosen.Emit(VALUE)", false},
			{"slice", "", "chosen := []" + fn + "{output}; chosen[0](VALUE)", false},
			{"map", "", "chosen := map[int]" + fn + "{0:output}; chosen[0](VALUE)", false},
			{"channel", "", "chosen := make(chan " + fn + ",1); chosen <- output; (<-chosen)(VALUE)", false},
			{"assertion", "", "var chosen any = output; chosen.(" + fn + ")(VALUE)", false},
			{"pointer", "", "chosen := output; p := &chosen; (*p)(VALUE)", false},
			{"destination", "func supply() (" + fn + ",bool){return output,true}", "chosen := make([]" + fn + ",1); var ok bool; chosen[0],ok=supply(); _=ok; chosen[0](VALUE)", false},
			{"variadic", "func pair() (" + fn + "," + fn + "){return output,output}; func use(chosen ..." + fn + "){chosen[0](VALUE)}", "use(pair())", false},
		} {
			for _, val := range []struct{ name, expr, actual string }{
				{"registered", `"second"`, "second"},
				{"bad", `"new_unregistered"`, "new_unregistered"},
				{"computed", `compute()`, "new_unregistered"},
			} {
				t.Run(kind.name+"/"+route.name+"/"+val.name, func(t *testing.T) {
					source := `// moved and renamed input

package sample
 type Error struct{Code string}; func(Error) Error() string{return "error"}
 type Notice struct{Code string}; type Wrapped struct{E Error}; var sink Error
 func compute() string{return "new_unregistered"}
 func output(code string) ` + kind.result + `{sink=Error{Code:code}; ` + kind.ret + `}
 func note(code string, unrelated bool){_ = Notice{Code:code}}
 ` + route.extra + `
 func emit(){note("notice_only",true); output("first")
 ` + route.body + `
 }
`
					source = strings.ReplaceAll(strings.ReplaceAll(source, "VALUE", val.expr), "chosen", "renamed")
					name := "relocated.go"
					dir := filepath.Join(root, kind.name, route.name, val.name)
					writeAssignmentFixture(t, filepath.Join(dir, name), source)
					returnedResultsRuntime(t, dir, fmt.Sprintf(`emit(); t.Logf("ACTUAL=%%s",sink.Code); if sink.Code!=%q {t.Fatal(sink.Code)}`, val.actual))
					got := analyzeReturnedResults(t, name, source)
					line := sourceLine(source, val.expr)
					want := []string{"first"}
					var diagnostics []string
					if route.finite {
						if val.name != "computed" {
							want = append(want, val.actual)
							sort.Strings(want)
						}
						if val.name == "bad" {
							diagnostics = []string{fmt.Sprintf("%s:%d: refusal: unregistered code %q", name, line, val.actual)}
						}
					}
					if !route.finite || val.name == "computed" {
						diagnostics = []string{fmt.Sprintf("%s:%d: refusal: cannot prove code expression; use a constant or a supported assignment flow", name, line)}
					}
					if !reflect.DeepEqual(got.Codes["refusal"], want) || !reflect.DeepEqual(diagnosticStrings(got), diagnostics) {
						t.Errorf("codes=%v diagnostics=%v; want %v %v", got.Codes, got.Diagnostics, want, diagnostics)
					}
					assertNoticeIsolated(t, got, []string{"notice_only"})
				})
			}
		}
	}
}

func TestUnresolvedSideEffectsAndAlternatives(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.27\n")
	for _, route := range []struct {
		name, bind, invoke string
		partial, finite    bool
	}{
		{"side_effect", `fns:=[]func(string) Error{beta}; chosen:=fns[0]`, `chosen(VALUE)`, false, false},
		{"partial_other", `fns:=[]func(string) Error{beta}; chosen:=alpha; if flag {chosen=fns[0]}`, `chosen(VALUE)`, true, false},
		{"partial_returned", `factories:=[]func()func(string)Error{func()func(string)Error{return beta}}; chosen:=alpha; if flag {chosen=factories[0]()}`, `chosen(VALUE)`, true, false},
		{"known_other", `chosen:=beta`, `chosen(VALUE)`, false, true},
		{"argless", `var chosen Loader=impl{}`, `chosen.Load()`, false, true},
	} {
		for _, val := range []struct{ name, expr, actual string }{{"registered", `"second"`, "second"}, {"bad", `"new_unregistered"`, "new_unregistered"}, {"computed", `compute()`, "new_unregistered"}} {
			t.Run(route.name+"/"+val.name, func(t *testing.T) {
				source := `// shifted source

package sample
 type Error struct{Code string}; type Probe struct{Code string}; type Notice struct{Code string}
 var sink Probe
 func compute()string{return "new_unregistered"}
 func alpha(code string) Error{return Error{Code:code}}
 func beta(code string) Error{sink=Probe{Code:code};return Error{Code:"first"}}
 type Loader interface{Load()Error};type impl struct{};func(impl)Load()Error{return Error{Code:"first"}}
 func emit(flag bool){alpha("first");beta("probe_seed"); _=Notice{Code:"notice_only"}
 ` + route.bind + `
 ` + route.invoke + `
 }
`
				// For the side-effect-only row alpha has a distinct signature, so the
				// unknown compatible callee can affect Probe but not Error's parameter.
				if route.name == "side_effect" {
					source = strings.ReplaceAll(source, "alpha(code string)", "alpha(code string, control bool)")
					source = strings.ReplaceAll(source, `alpha("first")`, `alpha("first",true)`)
				}
				source = strings.ReplaceAll(strings.ReplaceAll(source, "VALUE", val.expr), "chosen", "renamed")
				dir := filepath.Join(root, route.name, val.name)
				writeAssignmentFixture(t, filepath.Join(dir, "relocated.go"), source)
				actual := val.actual
				if route.name == "argless" {
					actual = "probe_seed"
				}
				returnedResultsRuntime(t, dir, fmt.Sprintf(`emit(true);t.Logf("ACTUAL_PROBE=%%s",sink.Code);if sink.Code!=%q{t.Fatal(sink.Code)}`, actual))
				got, err := Analyze([]Source{{Package: "sample", Filename: "relocated.go", Content: []byte(source)}}, nil, []Target{
					{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}},
					{Package: "sample", Type: "Probe", Field: "Code", Namespace: "probe", Registered: []string{"probe_seed", "second"}},
					{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"notice_only"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				wantErr, wantProbe := []string{"first"}, []string{"probe_seed"}
				line := sourceLine(source, val.expr)
				var wantDiagnostics []string
				if route.partial || route.name == "known_other" {
					if val.name != "computed" {
						if route.partial {
							wantErr = append(wantErr, val.actual)
						} else {
							wantProbe = append(wantProbe, val.actual)
						}
					}
					if val.name != "registered" {
						ns := "probe"
						if route.partial {
							ns = "refusal"
						}
						msg := fmt.Sprintf("unregistered code %q", val.actual)
						if val.name == "computed" {
							msg = "cannot prove code expression; use a constant or a supported assignment flow"
						}
						wantDiagnostics = append(wantDiagnostics, fmt.Sprintf("relocated.go:%d: %s: %s", line, ns, msg))
					}
				}
				if !route.finite {
					wantDiagnostics = append(wantDiagnostics, fmt.Sprintf("relocated.go:%d: probe: cannot prove code expression; use a constant or a supported assignment flow", line))
					if route.partial {
						d := fmt.Sprintf("relocated.go:%d: refusal: cannot prove code expression; use a constant or a supported assignment flow", line)
						if !containsCode(wantDiagnostics, d) {
							wantDiagnostics = append(wantDiagnostics, d)
						}
					}
				}
				sort.Strings(wantErr)
				sort.Strings(wantProbe)
				sort.Strings(wantDiagnostics)
				if !reflect.DeepEqual(got.Codes["refusal"], wantErr) || !reflect.DeepEqual(got.Codes["probe"], wantProbe) || !reflect.DeepEqual(diagnosticStrings(got), wantDiagnostics) {
					t.Errorf("got=%v want refusal=%v probe=%v diagnostics=%v", got, wantErr, wantProbe, wantDiagnostics)
				}
				assertNoticeIsolated(t, got, []string{"notice_only"})
			})
		}
	}
}

func TestMethodExpressionArguments(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.27\n")
	for _, route := range []struct{ name, call string }{
		{"direct", `output(VALUE)`},
		{"method_value", `kind("alpha_mark").Emit(VALUE)`},
		{"method_expression_string", `kind.Emit("alpha_mark",VALUE)`},
		{"method_expression_struct", `impl.Emit(impl{},VALUE)`},
		{"stored_expression", `func() Error {chosen:=kind.Emit; return chosen("alpha_mark",VALUE)}()`},
		{"returned_expression", `func() Error {chosen,_:=supply();return chosen("alpha_mark",VALUE)}()`},
		{"unresolved_expression", `func() Error {chosen:=[]func(kind,string)Error{kind.Emit};return chosen[0]("alpha_mark",
VALUE)}()`},
	} {
		for _, val := range []struct{ name, expr, actual string }{{"registered", `"second"`, "second"}, {"bad", `"new_unregistered"`, "new_unregistered"}, {"computed", `compute()`, "new_unregistered"}} {
			t.Run(route.name+"/"+val.name, func(t *testing.T) {
				source := `// relocated method source

package sample
 type Error struct{Code string}; type Notice struct{Code string};type kind string;type impl struct{}
 func output(code string)Error{return Error{Code:code}}
 func(kind) Emit(code string)Error{return Error{Code:code}}
 func(impl) Emit(code string)Error{return Error{Code:code}}
 func compute()string{return "new_unregistered"}
 func supply()(func(kind,string)Error,bool){return kind.Emit,true}
 func emit()Error{output("first");kind("alpha_mark").Emit("first");impl{}.Emit("first");_=Notice{Code:"notice_only"}
 return ` + strings.ReplaceAll(route.call, "VALUE", val.expr) + `
}
`
				dir := filepath.Join(root, route.name, val.name)
				writeAssignmentFixture(t, filepath.Join(dir, "renamed.go"), source)
				returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code;t.Logf("ACTUAL=%%s",got);if got!=%q{t.Fatal(got)}`, val.actual))
				got := analyzeReturnedResults(t, "renamed.go", source)
				want := []string{"first"}
				var diagnostics []string
				if val.name != "computed" && route.name != "unresolved_expression" {
					want = append(want, val.actual)
					sort.Strings(want)
				}
				if val.name != "registered" || route.name == "unresolved_expression" {
					msg := fmt.Sprintf("unregistered code %q", val.actual)
					if val.name == "computed" || route.name == "unresolved_expression" {
						msg = "cannot prove code expression; use a constant or a supported assignment flow"
					}
					diagnostics = []string{fmt.Sprintf("renamed.go:%d: refusal: %s", sourceLine(source, val.expr), msg)}
				}
				if !reflect.DeepEqual(got.Codes["refusal"], want) || !reflect.DeepEqual(diagnosticStrings(got), diagnostics) {
					t.Errorf("got=%v want=%v %v", got, want, diagnostics)
				}
				assertNoticeIsolated(t, got, []string{"notice_only"})
			})
		}
	}
}
