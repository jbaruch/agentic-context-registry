package codecensus

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Diagnostic shapes were taken from r62i8/overlays/discrimination_test.go and
// r62v8/reviewer-evidence/returned_shapes_test.go. Those probes are evidence,
// not the shipped assertions: this file requires finite values or source-located
// affected-target uncertainty, true n:1 declarations, and isolated callables.

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
					if rt.name == "direct" && !atArgument {
						t.Errorf("direct diagnostic location mismatch: %v want %s:%d", got.Diagnostics, name, argumentLine)
					}
					if value.name == "bad" && !hasActual && len(refusal) == 0 {
						t.Errorf("unregistered emission missing from codes without a diagnostic")
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
						if mode == "left" || mode == "both" {
							if value.name == "registered" {
								if !containsCode(got.Codes["refusal"], value.errActual) {
									t.Errorf("left registered value omitted: %v", got.Codes["refusal"])
								}
							} else {
								found := false
								for _, d := range got.Diagnostics {
									if d.Namespace == "refusal" && d.Position.Filename == name && d.Position.Line > 0 {
										found = true
									}
								}
								if !found && !containsCode(got.Codes["refusal"], value.errActual) {
									t.Errorf("left %s omitted without refusal uncertainty", value.name)
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
									if d.Namespace == "notice" && d.Position.Filename == name && d.Position.Line > 0 {
										found = true
									}
								}
								if !found && !containsCode(got.Codes["notice"], value.noteActual) {
									t.Errorf("right %s omitted without notice uncertainty", value.name)
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

func TestReturnedCallableSameTypeIsolation(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	type layout struct {
		name, extra, bind string
	}
	const sameTypeFns = "func alpha(code string) Error { return Error{Code:code} }\nfunc beta(code string) Error { return Error{Code:code} }\n"
	layouts := []layout{
		{name: "order_ab", extra: sameTypeFns + "func supply() (func(string) Error, func(string) Error) { return alpha, beta }", bind: "slot0, slot1 := supply()"},
		{name: "order_ba", extra: sameTypeFns + "func supply() (func(string) Error, func(string) Error) { return beta, alpha }", bind: "slot0, slot1 := supply()"},
		{name: "intervening", extra: sameTypeFns + "func supply() (func(string) Error, bool, func(string) Error) { return alpha, true, beta }", bind: "slot0, _, slot1 := supply()"},
		{name: "named_bare", extra: sameTypeFns + "func supply() (slot0 func(string) Error, slot1 func(string) Error) { slot0 = alpha; slot1 = beta; return }", bind: "slot0, slot1 := supply()"},
		{name: "forwarded", extra: sameTypeFns + "func pair() (func(string) Error, func(string) Error) { return alpha, beta }\nfunc supply() (func(string) Error, func(string) Error) { return pair() }", bind: "slot0, slot1 := supply()"},
	}
	values := []struct{ name, expression, actual string }{
		{"registered", `"second"`, "second"},
		{"bad", `"returned_unregistered"`, "returned_unregistered"},
		{"computed", `compute()`, "returned_unregistered"},
	}
	for _, moved := range []bool{false, true} {
		for _, ly := range layouts {
			if moved && ly.name != "order_ab" && ly.name != "intervening" {
				continue
			}
			for _, value := range values {
				for _, mode := range []string{"first", "second", "both"} {
					t.Run(fmt.Sprintf("moved_%t/%s/%s/%s", moved, ly.name, mode, value.name), func(t *testing.T) {
						call := ""
						runtime := ""
						switch mode {
						case "first":
							call = "e = slot0(" + value.expression + ")"
							runtime = fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatalf("got %%q",got)}`, value.actual)
						case "second":
							call = "e = slot1(" + value.expression + ")"
							runtime = fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatalf("got %%q",got)}`, value.actual)
						case "both":
							call = "e = slot0(" + value.expression + "); _ = slot1(" + value.expression + ")"
							runtime = fmt.Sprintf(`got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatalf("got %%q",got)}`, value.actual)
						}
						source := returnedResultsPrelude + ly.extra + `
func emit() Error {
 _ = Notice{Code:"notice_only"}
 _ = output("first")
 ` + ly.bind + `
 _ = slot0
 _ = slot1
 var e Error
 ` + call + `
 return e
}
`
						name := "fixture.go"
						if moved {
							name = "shifted.go"
							source = "// moved source\n\n" + strings.ReplaceAll(source, "slot0", "renamed0")
							source = strings.ReplaceAll(source, "slot1", "renamed1")
						}
						dir := filepath.Join(root, fmt.Sprintf("%s_%s_%s_%t", ly.name, mode, value.name, moved))
						writeAssignmentFixture(t, filepath.Join(dir, name), source)
						returnedResultsRuntime(t, dir, runtime)
						got := analyzeReturnedResults(t, name, source)
						t.Logf("same-type layout=%s mode=%s value=%s codes=%v diagnostics=%v", ly.name, mode, value.name, got.Codes, got.Diagnostics)
						assertNoticeIsolated(t, got, []string{"notice_only"})
						if !containsCode(got.Codes["refusal"], "first") {
							t.Error("seed vocabulary was lost")
						}
						alphaLine := 1 + strings.Count(source[:strings.Index(source, "func alpha")], "\n")
						betaLine := 1 + strings.Count(source[:strings.Index(source, "func beta")], "\n")
						invokedLine, otherLine := alphaLine, betaLine
						if ly.name == "order_ba" {
							invokedLine, otherLine = betaLine, alphaLine
						}
						if mode == "second" {
							invokedLine, otherLine = otherLine, invokedLine
						}
						if value.name == "registered" {
							if !containsCode(got.Codes["refusal"], "second") {
								t.Errorf("registered argument omitted: %v", got.Codes["refusal"])
							}
						} else {
							found := false
							for _, d := range got.Diagnostics {
								if d.Namespace == "refusal" && d.Position.Filename == name && d.Position.Line > 0 {
									found = true
								}
							}
							if !found && !containsCode(got.Codes["refusal"], value.actual) {
								t.Errorf("argument %s omitted without target uncertainty", value.name)
							}
						}
						if mode != "both" && value.name == "bad" {
							for _, d := range got.Diagnostics {
								if d.Namespace == "refusal" && d.Position.Line == otherLine && strings.Contains(d.Message, `unregistered code "returned_unregistered"`) {
									t.Errorf("uninvoked same-type slot at %s:%d received the argument: %s (invoked %s:%d)", name, otherLine, d.String(), name, invokedLine)
								}
							}
						}
					})
				}
			}
		}
	}
}
