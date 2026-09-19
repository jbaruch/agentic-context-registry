package codecensus

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Diagnostic shapes originated in the r62i8 discrimination probe and r62v8
// returned-callable survey. Same-type isolation oracles follow reviewer9
// (Claude / Fable 5.1) TestReviewer9SameTypeIsolation: seed both helpers, send
// the second compatible callable into a distinct target, and assert exact
// per-target codes and diagnostics. Those probes are evidence, not the shipped
// assertions.

const returnedResultsPrelude = `package sample
type Error struct { Code string }
type Notice struct { Code string }
func compute() string { return "returned_unregistered" }
func output(code string) Error { return Error{Code:code} }
`

func returnedResultsRuntime(t *testing.T, dir, check string) {
	t.Helper()
	writeAssignmentFixture(t, filepath.Join(dir, "runtime_test.go"), `package sample
import "testing"
func TestRuntime(t *testing.T){`+check+`}
`)
	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestRuntime$", "-v", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Logf("runtime:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
}

func analyzeReturnedResults(t *testing.T, name, source string) Result {
	t.Helper()
	got, err := Analyze([]Source{{Package: "sample", Filename: name, Content: []byte(source)}}, nil, []Target{
		{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second", "alpha_mark", "beta_mark"}},
		{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"notice_only", "notice_second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertNoticeIsolated(t *testing.T, got Result, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got.Codes["notice"], want) {
		t.Errorf("notice codes=%v want %v", got.Codes["notice"], want)
	}
	for _, d := range got.Diagnostics {
		if d.Namespace == "notice" && (want == nil || len(want) == 1 && want[0] == "notice_only") {
			t.Errorf("unrelated notice diagnostic: %s", d.String())
		}
	}
}

func containsCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

func refusalAt(got Result, filename string) []Diagnostic {
	var refusal []Diagnostic
	for _, d := range got.Diagnostics {
		if d.Namespace == "refusal" && d.Position.Filename == filename && d.Position.Line > 0 {
			refusal = append(refusal, d)
		}
	}
	return refusal
}

func diagnosticStrings(got Result) []string {
	var lines []string
	for _, d := range got.Diagnostics {
		lines = append(lines, d.String())
	}
	return lines
}

func sourceLine(source, needle string) int {
	i := strings.LastIndex(source, needle)
	if i < 0 {
		return 0
	}
	return 1 + strings.Count(source[:i], "\n")
}

// First and non-first positions, explicit/named/bare/forwarded summaries,
// short/plain/true n:1 declarations, blanks, expansion, and movement.
func TestReturnedCallableResultPositions(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	type route struct {
		name, extra, bind, invoke, seed string
	}
	routes := []route{
		{name: "direct", bind: "chosen := output", invoke: "return chosen(VALUE)"},
		{name: "single", extra: "func supply() func(string) Error { return output }", bind: "chosen := supply()", invoke: "return chosen(VALUE)"},
		{name: "tuple", extra: "func supply() (func(string) Error, bool) { return output, true }", bind: "chosen, _ := supply()", invoke: "return chosen(VALUE)"},
		{name: "tuple_unseeded", extra: "func supply() (func(string) Error, bool) { return output, true }", bind: "chosen, _ := supply()", invoke: "return chosen(VALUE)", seed: "omit"},
		{name: "tuple_second", extra: "func supply() (bool, func(string) Error) { return true, output }", bind: "_, chosen := supply()", invoke: "return chosen(VALUE)"},
		{name: "tuple_named", extra: "func supply() (f func(string) Error, ok bool) { f = output; ok = true; return f, ok }", bind: "chosen, _ := supply()", invoke: "return chosen(VALUE)"},
		{name: "tuple_bare", extra: "func supply() (f func(string) Error, ok bool) { f = output; ok = true; return }", bind: "chosen, _ := supply()", invoke: "return chosen(VALUE)"},
		{name: "tuple_forward", extra: "func pair() (func(string) Error, bool) { return output, true }\nfunc supply() (func(string) Error, bool) { return pair() }", bind: "chosen, _ := supply()", invoke: "return chosen(VALUE)"},
		{name: "tuple_decl", extra: "func supply() (func(string) Error, bool) { return output, true }", bind: "var chosen, ok = supply(); _ = ok", invoke: "return chosen(VALUE)"},
		{name: "tuple_plain", extra: "func supply() (func(string) Error, bool) { return output, true }", bind: "var chosen func(string) Error; var ok bool; chosen, ok = supply(); _ = ok", invoke: "return chosen(VALUE)"},
		{name: "tuple_expand", extra: "func pair() (func(string) Error, bool) { return output, true }\nfunc use(f func(string) Error, ok bool) Error { return f(VALUE) }", invoke: "return use(pair())"},
	}
	values := []struct{ name, expression, actual string }{
		{"registered", `"second"`, "second"},
		{"bad", `"returned_unregistered"`, "returned_unregistered"},
		{"computed", `compute()`, "returned_unregistered"},
	}
	for _, moved := range []bool{false, true} {
		for _, rt := range routes {
			if moved && rt.name != "tuple" && rt.name != "tuple_second" && rt.name != "tuple_forward" && rt.name != "tuple_decl" && rt.name != "tuple_expand" {
				continue
			}
			for _, value := range values {
				t.Run(fmt.Sprintf("moved_%t/%s/%s", moved, rt.name, value.name), func(t *testing.T) {
					seed := `_ = output("first")`
					if rt.seed == "omit" {
						seed = ""
					}
					extra := strings.ReplaceAll(rt.extra, "VALUE", value.expression)
					invoke := strings.ReplaceAll(rt.invoke, "VALUE", value.expression)
					source := returnedResultsPrelude + extra + `
func emit() Error {
 _ = Notice{Code:"notice_only"}
 ` + seed + `
 ` + rt.bind + `
 ` + invoke + `
}
`
					name := "fixture.go"
					if moved {
						name = "shifted.go"
						source = "// moved source\n\n" + strings.ReplaceAll(source, "chosen", "renamed")
					}
					dir := filepath.Join(root, fmt.Sprintf("%s_%s_%t", rt.name, value.name, moved))
					writeAssignmentFixture(t, filepath.Join(dir, name), source)
					returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatalf("got %%q",got)}`, value.actual))
					got := analyzeReturnedResults(t, name, source)
					argumentLine := 1 + strings.Count(source[:strings.LastIndex(source, value.expression)], "\n")
					t.Logf("RESULT route=%s value=%s moved=%t codes=%v diagnostics=%v argument=%s:%d", rt.name, value.name, moved, got.Codes, got.Diagnostics, name, argumentLine)
					assertNoticeIsolated(t, got, []string{"notice_only"})
					hasFirst := containsCode(got.Codes["refusal"], "first")
					hasActual := containsCode(got.Codes["refusal"], value.actual)
					if rt.seed != "omit" && !hasFirst {
						t.Error("seed vocabulary was lost")
					}
					if rt.seed == "omit" && hasFirst {
						t.Error("unseeded control acquired the seed value")
					}
					refusal := refusalAt(got, name)
					atArgument := len(refusal) == 1 && refusal[0].Position.Line == argumentLine
					if value.name == "registered" {
						if !hasActual && len(refusal) == 0 {
							t.Errorf("emitted registered value omitted without target uncertainty")
						}
						if rt.name == "direct" && (!hasActual || len(refusal) != 0) {
							t.Errorf("direct registered control: codes=%v diagnostics=%v", got.Codes["refusal"], got.Diagnostics)
						}
						return
					}
					if len(refusal) == 0 {
						t.Errorf("actual emitted %s value lacks source-located target diagnostic", value.name)
					}
					if rt.seed != "omit" && !atArgument {
						t.Errorf("diagnostic location mismatch: %v want %s:%d", got.Diagnostics, name, argumentLine)
					}
					if value.name == "bad" && hasActual {
						want := fmt.Sprintf("%s:%d: refusal: unregistered code %q", name, argumentLine, value.actual)
						if len(refusal) != 1 || refusal[0].String() != want {
							t.Errorf("diagnostics=%v want %s", got.Diagnostics, want)
						}
					}
				})
			}
		}
	}
}

func TestReturnedCallableResultConsumers(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	source := returnedResultsPrelude + `var slot = []int{0}
var supplies int
func record(e Error) int { return 0 }
func supply() (func(string) Error, int) { supplies++; return output, 1 }
func emit() Error {
 _ = Notice{Code:"notice_only"}
 _ = output("first")
 var chosen func(string) Error
 chosen, slot[record(Error{Code:"from_lhs"})] = supply()
 return chosen("second")
}
`
	dir := filepath.Join(root, "consumers")
	writeAssignmentFixture(t, filepath.Join(dir, "fixture.go"), source)
	returnedResultsRuntime(t, dir, `got:=emit().Code; t.Logf("ACTUAL=%s supplies=%d",got,supplies); if got!="second" || supplies!=1 {t.Fatalf("got %q supplies=%d",got,supplies)}`)
	got := analyzeReturnedResults(t, "fixture.go", source)
	t.Logf("consumer codes=%v diagnostics=%v", got.Codes, got.Diagnostics)
	assertNoticeIsolated(t, got, []string{"notice_only"})
	if !containsCode(got.Codes["refusal"], "first") || !containsCode(got.Codes["refusal"], "second") {
		t.Errorf("callable result lost: %v", got.Codes["refusal"])
	}
	if !containsCode(got.Codes["refusal"], "from_lhs") {
		found := false
		for _, d := range got.Diagnostics {
			if d.Namespace == "refusal" && d.Position.Filename == "fixture.go" && d.Position.Line > 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("lhs result consumer omitted without target uncertainty: %v", got)
		}
	}
}

func TestReturnedCallableDistinctTargetIsolation(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	type layout struct {
		name, extra, bind string
	}
	layouts := []layout{
		{name: "explicit", extra: "func left(code string) Error { return Error{Code:code} }\nfunc right(code string) Notice { return Notice{Code:code} }\nfunc supply() (func(string) Error, func(string) Notice) { return left, right }", bind: "takeErr, takeNote := supply()"},
		{name: "reversed", extra: "func left(code string) Error { return Error{Code:code} }\nfunc right(code string) Notice { return Notice{Code:code} }\nfunc supply() (func(string) Notice, func(string) Error) { return right, left }", bind: "takeNote, takeErr := supply()"},
		{name: "intervening", extra: "func left(code string) Error { return Error{Code:code} }\nfunc right(code string) Notice { return Notice{Code:code} }\nfunc supply() (func(string) Error, bool, func(string) Notice) { return left, true, right }", bind: "takeErr, _, takeNote := supply()"},
		{name: "named_bare", extra: "func left(code string) Error { return Error{Code:code} }\nfunc right(code string) Notice { return Notice{Code:code} }\nfunc supply() (takeErr func(string) Error, takeNote func(string) Notice) { takeErr = left; takeNote = right; return }", bind: "takeErr, takeNote := supply()"},
		{name: "forwarded", extra: "func left(code string) Error { return Error{Code:code} }\nfunc right(code string) Notice { return Notice{Code:code} }\nfunc pair() (func(string) Error, func(string) Notice) { return left, right }\nfunc supply() (func(string) Error, func(string) Notice) { return pair() }", bind: "takeErr, takeNote := supply()"},
	}
	values := []struct{ name, errExpr, errActual, noteExpr, noteActual string }{
		{"registered", `"second"`, "second", `"notice_second"`, "notice_second"},
		{"bad", `"returned_unregistered"`, "returned_unregistered", `"notice_unregistered"`, "notice_unregistered"},
		{"computed", `compute()`, "returned_unregistered", `compute()`, "returned_unregistered"},
	}
	for _, moved := range []bool{false, true} {
		for _, ly := range layouts {
			if moved && ly.name != "explicit" && ly.name != "intervening" {
				continue
			}
			for _, value := range values {
				for _, mode := range []string{"left", "right", "both"} {
					t.Run(fmt.Sprintf("moved_%t/%s/%s/%s", moved, ly.name, mode, value.name), func(t *testing.T) {
						errCall := ""
						noteCall := ""
						runtime := ""
						switch mode {
						case "left":
							errCall = "e = takeErr(" + value.errExpr + ")"
							runtime = fmt.Sprintf(`e,n:=emit(); t.Logf("ACTUAL_ERR=%%s ACTUAL_NOTE=%%s",e.Code,n.Code); if e.Code!=%q || n.Code!="" {t.Fatalf("err=%%q note=%%q",e.Code,n.Code)}`, value.errActual)
						case "right":
							noteCall = "n = takeNote(" + value.noteExpr + ")"
							runtime = fmt.Sprintf(`e,n:=emit(); t.Logf("ACTUAL_ERR=%%s ACTUAL_NOTE=%%s",e.Code,n.Code); if e.Code!="" || n.Code!=%q {t.Fatalf("err=%%q note=%%q",e.Code,n.Code)}`, value.noteActual)
						case "both":
							errCall = "e = takeErr(" + value.errExpr + ")"
							noteCall = "n = takeNote(" + value.noteExpr + ")"
							runtime = fmt.Sprintf(`e,n:=emit(); t.Logf("ACTUAL_ERR=%%s ACTUAL_NOTE=%%s",e.Code,n.Code); if e.Code!=%q || n.Code!=%q {t.Fatalf("err=%%q note=%%q",e.Code,n.Code)}`, value.errActual, value.noteActual)
						}
						source := returnedResultsPrelude + ly.extra + `
func emit() (Error, Notice) {
 _ = Notice{Code:"notice_only"}
 _ = output("first")
 ` + ly.bind + `
 _ = takeErr
 _ = takeNote
 var e Error
 var n Notice
 ` + errCall + `
 ` + noteCall + `
 return e, n
}
`
						name := "fixture.go"
						if moved {
							name = "shifted.go"
							source = "// moved source\n\n" + source
						}
						dir := filepath.Join(root, fmt.Sprintf("%s_%s_%s_%t", ly.name, mode, value.name, moved))
						writeAssignmentFixture(t, filepath.Join(dir, name), source)
						returnedResultsRuntime(t, dir, runtime)
						got := analyzeReturnedResults(t, name, source)
						t.Logf("isolation layout=%s mode=%s value=%s codes=%v diagnostics=%v", ly.name, mode, value.name, got.Codes, got.Diagnostics)
						if !containsCode(got.Codes["refusal"], "first") {
							t.Error("error seed vocabulary was lost")
						}
						if !containsCode(got.Codes["notice"], "notice_only") {
							t.Error("notice seed vocabulary was lost")
						}
						errLine := sourceLine(source, "takeErr("+value.errExpr)
						noteLine := sourceLine(source, "takeNote("+value.noteExpr)
						if mode == "left" || mode == "both" {
							if value.name == "registered" {
								if !containsCode(got.Codes["refusal"], value.errActual) {
									t.Errorf("left registered value omitted: %v", got.Codes["refusal"])
								}
							} else {
								found := false
								for _, d := range got.Diagnostics {
									if d.Namespace == "refusal" && d.Position.Filename == name && d.Position.Line == errLine {
										found = true
									}
								}
								if !found && !containsCode(got.Codes["refusal"], value.errActual) {
									t.Errorf("left %s omitted without refusal uncertainty at %s:%d", value.name, name, errLine)
								}
								if value.name == "bad" && containsCode(got.Codes["refusal"], value.errActual) {
									want := fmt.Sprintf("%s:%d: refusal: unregistered code %q", name, errLine, value.errActual)
									if !containsCode(diagnosticStrings(got), want) {
										t.Errorf("left diagnostics=%v want %s", got.Diagnostics, want)
									}
								}
							}
						}
						if mode == "right" || mode == "both" {
							if value.name == "registered" {
								if !containsCode(got.Codes["notice"], value.noteActual) {
									t.Errorf("right registered value omitted: %v", got.Codes["notice"])
								}
							} else {
								found := false
								for _, d := range got.Diagnostics {
									if d.Namespace == "notice" && d.Position.Filename == name && d.Position.Line == noteLine {
										found = true
									}
								}
								if !found && !containsCode(got.Codes["notice"], value.noteActual) {
									t.Errorf("right %s omitted without notice uncertainty at %s:%d", value.name, name, noteLine)
								}
								if value.name == "bad" && containsCode(got.Codes["notice"], value.noteActual) {
									want := fmt.Sprintf("%s:%d: notice: unregistered code %q", name, noteLine, value.noteActual)
									if !containsCode(diagnosticStrings(got), want) {
										t.Errorf("right diagnostics=%v want %s", got.Diagnostics, want)
									}
								}
							}
						}
						if mode == "left" {
							if containsCode(got.Codes["notice"], "notice_second") || containsCode(got.Codes["notice"], "notice_unregistered") || containsCode(got.Codes["notice"], "second") || containsCode(got.Codes["notice"], "returned_unregistered") {
								t.Errorf("left invoke contaminated notice: %v", got.Codes["notice"])
							}
							for _, d := range got.Diagnostics {
								if d.Namespace == "notice" && strings.Contains(d.Message, "unregistered code") {
									t.Errorf("left invoke forwarded arguments into notice: %s", d.String())
								}
							}
						}
						if mode == "right" {
							if containsCode(got.Codes["refusal"], "second") || containsCode(got.Codes["refusal"], "returned_unregistered") || containsCode(got.Codes["refusal"], "notice_second") || containsCode(got.Codes["refusal"], "notice_unregistered") {
								t.Errorf("right invoke introduced left vocabulary: %v", got.Codes["refusal"])
							}
							for _, d := range got.Diagnostics {
								if d.Namespace == "refusal" && strings.Contains(d.Message, "unregistered code") {
									t.Errorf("right invoke forwarded arguments into refusal: %s", d.String())
								}
							}
						}
					})
				}
			}
		}
	}
}

// Same-type callables with different emitting targets. alpha feeds Error.Code;
// beta feeds Probe.Code and returns a constant Error. Both helpers are seeded,
// so the unused-parameter fallback cannot stand in for isolation. Exact code
// sets and exact diagnostics discriminate any merge of the two positions.
func TestReturnedCallableSameTypeIsolation(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	const fn = "func(string) Error"
	const prelude = `package sample
type Error struct { Code string }
type Notice struct { Code string }
type Probe struct { Code string }
var sink Probe
func compute() string { return "returned_unregistered" }
func alpha(code string) Error { return Error{Code:code} }
func beta(code string) Error { sink = Probe{Code:code}; return Error{Code:"first"} }
`
	type layout struct {
		name, supply string
		names, types []string
	}
	layouts := []layout{
		{"explicit", "func supply() (" + fn + ", " + fn + ") { return alpha, beta }", []string{"a", "b"}, []string{fn, fn}},
		{"reversed", "func supply() (" + fn + ", " + fn + ") { return beta, alpha }", []string{"b", "a"}, []string{fn, fn}},
		{"intervening", "func supply() (" + fn + ", bool, " + fn + ") { return alpha, true, beta }", []string{"a", "skip", "b"}, []string{fn, "bool", fn}},
		{"named_bare", "func supply() (x " + fn + ", y " + fn + ") { x = alpha; y = beta; return }", []string{"a", "b"}, []string{fn, fn}},
		{"forwarded", "func pair() (" + fn + ", " + fn + ") { return alpha, beta }\nfunc supply() (" + fn + ", " + fn + ") { return pair() }", []string{"a", "b"}, []string{fn, fn}},
		{"swapped", "func pair() (" + fn + ", " + fn + ") { return alpha, beta }\nfunc supply() (" + fn + ", " + fn + ") { x, y := pair(); return y, x }", []string{"b", "a"}, []string{fn, fn}},
	}
	values := []struct{ name, expression, actual string }{
		{"registered", `"second"`, "second"},
		{"bad", `"returned_unregistered"`, "returned_unregistered"},
		{"computed", `compute()`, "returned_unregistered"},
	}
	for _, moved := range []bool{false, true} {
		for _, ly := range layouts {
			if moved && ly.name != "explicit" && ly.name != "intervening" {
				continue
			}
			for _, consumer := range []string{"short", "plain", "decl", "expand"} {
				for _, mode := range []string{"alpha", "beta"} {
					for _, value := range values {
						t.Run(fmt.Sprintf("moved_%t/%s/%s/%s/%s", moved, ly.name, consumer, mode, value.name), func(t *testing.T) {
							invoked := "a"
							if mode == "beta" {
								invoked = "b"
							}
							hasSkip := false
							short := make([]string, len(ly.names))
							params := make([]string, len(ly.names))
							for i, name := range ly.names {
								short[i] = name
								if name == "skip" {
									hasSkip = true
									short[i] = "_"
								}
								params[i] = name + " " + ly.types[i]
							}
							keep := ""
							if hasSkip {
								keep = "; _ = skip"
							}
							var source string
							if consumer == "expand" {
								source = prelude + ly.supply + "\nfunc use(" + strings.Join(params, ", ") + ") Error {\n _, _ = a, b" + keep + "\n return " + invoked + "(" + value.expression + ")\n}\nfunc emit() Error {\n _ = Notice{Code:\"notice_only\"}\n _ = alpha(\"first\")\n _ = beta(\"probe_seed\")\n return use(supply())\n}\n"
							} else {
								bind := strings.Join(short, ", ") + " := supply()"
								switch consumer {
								case "plain":
									bind = "var a, b " + fn + "; var skip bool; " + strings.Join(ly.names, ", ") + " = supply(); _ = skip"
								case "decl":
									bind = "var " + strings.Join(ly.names, ", ") + " = supply()" + keep
								}
								source = prelude + ly.supply + "\nfunc emit() Error {\n _ = Notice{Code:\"notice_only\"}\n _ = alpha(\"first\")\n _ = beta(\"probe_seed\")\n " + bind + "\n _, _ = a, b\n var e Error\n e = " + invoked + "(" + value.expression + ")\n return e\n}\n"
							}
							name := "fixture.go"
							if moved {
								name = "shifted.go"
								source = "// moved source\n\n" + source
							}
							actual := value.actual
							dir := filepath.Join(root, fmt.Sprintf("%s_%s_%s_%s_%t", ly.name, consumer, mode, value.name, moved))
							writeAssignmentFixture(t, filepath.Join(dir, name), source)
							wantErr, wantSink := actual, "probe_seed"
							if mode == "beta" {
								wantErr, wantSink = "first", actual
							}
							returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL_ERR=%%s ACTUAL_PROBE=%%s",got,sink.Code); if got!=%q || sink.Code!=%q {t.Fatalf("err=%%q probe=%%q",got,sink.Code)}`, wantErr, wantSink))
							got, err := Analyze([]Source{{Package: "sample", Filename: name, Content: []byte(source)}}, nil, []Target{
								{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}},
								{Package: "sample", Type: "Probe", Field: "Code", Namespace: "probe", Registered: []string{"probe_seed", "second"}},
								{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"notice_only"}},
							})
							if err != nil {
								t.Fatal(err)
							}
							wantRefusal, wantProbe := []string{"first"}, []string{"probe_seed"}
							namespace := "refusal"
							if mode == "alpha" {
								if value.name == "registered" {
									wantRefusal = append(wantRefusal, actual)
								}
							} else {
								if value.name == "registered" {
									wantProbe = append(wantProbe, actual)
								}
								namespace = "probe"
							}
							sort.Strings(wantRefusal)
							sort.Strings(wantProbe)
							var wantDiagnostics []string
							if value.name != "registered" {
								line := sourceLine(source, invoked+"("+value.expression)
								if line == 0 {
									line = sourceLine(source, value.expression)
								}
								message := "cannot prove code expression; use a constant or a supported assignment flow"
								if value.name == "bad" {
									message = fmt.Sprintf("unregistered code %q", actual)
									if mode == "alpha" {
										wantRefusal = append(wantRefusal, actual)
										sort.Strings(wantRefusal)
									} else {
										wantProbe = append(wantProbe, actual)
										sort.Strings(wantProbe)
									}
								}
								wantDiagnostics = []string{fmt.Sprintf("%s:%d: %s: %s", name, line, namespace, message)}
							}
							diagnostics := diagnosticStrings(got)
							t.Logf("RESULT refusal=%v probe=%v notice=%v diagnostics=%v", got.Codes["refusal"], got.Codes["probe"], got.Codes["notice"], diagnostics)
							assertNoticeIsolated(t, got, []string{"notice_only"})
							if !reflect.DeepEqual(got.Codes["refusal"], wantRefusal) {
								t.Errorf("refusal=%v want %v", got.Codes["refusal"], wantRefusal)
							}
							if !reflect.DeepEqual(got.Codes["probe"], wantProbe) {
								t.Errorf("probe=%v want %v", got.Codes["probe"], wantProbe)
							}
							if !reflect.DeepEqual(diagnostics, wantDiagnostics) {
								t.Errorf("diagnostics=%v want %v", diagnostics, wantDiagnostics)
							}
						})
					}
				}
			}
		}
	}
}

func TestUnresolvedCalleeAffectedTargetUncertainty(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	const fn = "func(string) Error"
	const impl = "type Emitter interface { Emit(string) Error }\ntype impl struct{}\nfunc (impl) Emit(code string) Error { return Error{Code:code} }\n"
	const supply = "func supply() (" + fn + ", bool) { return output, true }\n"
	shapes := []struct{ name, extra, body string }{
		{"conversion", "type Fn " + fn + "\n", "return Fn(output)(VALUE)"},
		{"interface_dispatch", impl, "_ = impl{}.Emit(\"first\"); var e Emitter = impl{}; return e.Emit(VALUE)"},
		{"struct_literal_field", "type holder struct{ f " + fn + " }\n", "h := holder{f: output}; return h.f(VALUE)"},
		{"slice_literal", "", "fns := []" + fn + "{output}; return fns[0](VALUE)"},
		{"map_literal", "", "m := map[string]" + fn + "{\"k\": output}; return m[\"k\"](VALUE)"},
		{"channel", "", "ch := make(chan " + fn + ", 1); ch <- output; return (<-ch)(VALUE)"},
		{"type_assertion", "", "var a any = output; return a.(" + fn + ")(VALUE)"},
		{"pointer_deref", "", "chosen := output; p := &chosen; return (*p)(VALUE)"},
		{"slice_element_dest", supply, "fns := make([]" + fn + ", 1); var ok bool; fns[0], ok = supply(); _ = ok; return fns[0](VALUE)"},
		{"variadic_expand", "func pair2() (" + fn + ", " + fn + ") { return output, output }\nfunc use(fs ..." + fn + ") Error { return fs[0](VALUE) }\n", "return use(pair2())"},
	}
	values := []struct{ name, expression, actual string }{
		{"registered", `"second"`, "second"},
		{"bad", `"returned_unregistered"`, "returned_unregistered"},
		{"computed", `compute()`, "returned_unregistered"},
	}
	for _, moved := range []bool{false, true} {
		for _, shape := range shapes {
			if moved && shape.name != "conversion" && shape.name != "interface_dispatch" && shape.name != "slice_literal" {
				continue
			}
			for _, value := range values {
				t.Run(fmt.Sprintf("moved_%t/%s/%s", moved, shape.name, value.name), func(t *testing.T) {
					extra := strings.ReplaceAll(shape.extra, "VALUE", value.expression)
					body := strings.ReplaceAll(shape.body, "VALUE", value.expression)
					source := returnedResultsPrelude + extra + `
func emit() Error {
 _ = Notice{Code:"notice_only"}
 _ = output("first")
 ` + body + `
}
`
					name := "fixture.go"
					if moved {
						name = "shifted.go"
						source = "// moved source\n\n" + source
					}
					dir := filepath.Join(root, fmt.Sprintf("%s_%s_%t", shape.name, value.name, moved))
					writeAssignmentFixture(t, filepath.Join(dir, name), source)
					returnedResultsRuntime(t, dir, fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatalf("got %%q",got)}`, value.actual))
					got := analyzeReturnedResults(t, name, source)
					argumentLine := sourceLine(source, value.expression)
					t.Logf("RESULT shape=%s value=%s codes=%v diagnostics=%v argument=%s:%d", shape.name, value.name, got.Codes, got.Diagnostics, name, argumentLine)
					assertNoticeIsolated(t, got, []string{"notice_only"})
					if !containsCode(got.Codes["refusal"], "first") {
						t.Error("seed vocabulary was lost")
					}
					hasActual := containsCode(got.Codes["refusal"], value.actual)
					refusal := refusalAt(got, name)
					if value.name == "registered" {
						if !hasActual && len(refusal) == 0 {
							t.Errorf("emitted registered value omitted without target uncertainty")
						}
						return
					}
					if len(refusal) == 0 {
						t.Errorf("runtime emits %s; census lists %v with no refusal diagnostic", value.actual, got.Codes["refusal"])
					}
					atArgument := false
					for _, d := range refusal {
						if d.Position.Line == argumentLine {
							atArgument = true
						}
					}
					if !atArgument {
						t.Errorf("diagnostic location mismatch: %v want %s:%d", got.Diagnostics, name, argumentLine)
					}
					if value.name == "bad" && hasActual {
						want := fmt.Sprintf("%s:%d: refusal: unregistered code %q", name, argumentLine, value.actual)
						if !containsCode(diagnosticStrings(got), want) {
							t.Errorf("diagnostics=%v want %s", got.Diagnostics, want)
						}
					}
					if !hasActual {
						want := fmt.Sprintf("%s:%d: refusal: cannot prove code expression; use a constant or a supported assignment flow", name, argumentLine)
						if !containsCode(diagnosticStrings(got), want) {
							t.Errorf("diagnostics=%v want %s", got.Diagnostics, want)
						}
					}
				})
			}
		}
	}
}
