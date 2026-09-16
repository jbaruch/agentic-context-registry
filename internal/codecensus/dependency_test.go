package codecensus

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Every fixture is legal Go executed as well as analyzed. Independent calls
// may run before or after ordinary reads; both observations are valid oracles.
func TestDependencyBoundariesAgainstRuntime(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module boundaries\n\ngo 1.25\n")
	const header = `package sample
 type Error struct { Code string; N int }
 type Other struct { Code string }
 var observed string
 var calls int
 func record(e Error) { observed=e.Code; calls++ }
 func compute() string { return "boundary_bad" }
 func keep(p *string) *string { return p }
 `
	const mutation = `func() int { candidate=VALUE; effects++; return 0 }()`
	for _, tc := range []struct {
		name, declarations, body      string
		escaped, replacement, ordered bool
	}{
		{name: "assignment", body: `candidate:="first"; e,n:=Error{Code:candidate}, MUTATE; _=n; record(e)`},
		{name: "declaration", body: `candidate:="first"; var e,n=Error{Code:candidate}, MUTATE; _=n; record(e)`},
		{name: "call", body: `candidate:="first"; func(e Error,n int){record(e)}(Error{Code:candidate}, MUTATE)`},
		{name: "return", declarations: `func build(effects *int)(Error,int){candidate:="first"; return Error{Code:candidate},func()int{candidate=VALUE; *effects++;return 0}()}`, body: `e,_:=build(&effects); record(e)`},
		{name: "send", body: `candidate:="first"; ch:=make(chan Error,1); ch<-Error{Code:candidate,N:MUTATE}; record(<-ch)`},
		{name: "range", body: `candidate:="first"; for i,e:=range []Error{{Code:candidate},{N:MUTATE}} {if i==0 {record(e)}}`},
		{name: "if", body: `candidate:="first"; if (Error{Code:candidate,N:MUTATE}).Code=="first" {observed="first"} else {observed=ACTUAL}; calls++`},
		{name: "for", body: `candidate:="first"; for (Error{Code:candidate,N:MUTATE}).Code=="first" {observed="first"; break}; if observed=="" {observed=ACTUAL}; calls++`},
		{name: "switch_tag", body: `candidate:="first"; switch (Error{Code:candidate,N:MUTATE}).Code {case "first":observed="first"; default:observed=ACTUAL}; calls++`},
		{name: "switch_case", body: `candidate:="first"; switch "first" {case (Error{Code:candidate,N:MUTATE}).Code: observed="first"; default:observed=ACTUAL}; calls++`},
		{name: "receive_expression", body: `candidate:="first"; ch:=make(chan int,1); ch<-1; <-[]chan int{ch}[func(e Error,n int)int{record(e);return 0}(Error{Code:candidate},MUTATE)]`},
		{name: "global_closure", declarations: `var candidate="first"; func emit(){record(Error{Code:candidate})}; func mutate(){f:=func(){candidate=VALUE};f()}`, body: `mutate(); effects++; emit()`, ordered: true},
		{name: "global_direct", declarations: `var candidate="first"; func emit(){record(Error{Code:candidate})}; func mutate(){candidate=VALUE}`, body: `mutate(); effects++; emit()`, ordered: true},
		{name: "global_function", declarations: `var handler=func(s string){}; func output(s string){record(Error{Code:s})}; func emit(){output("first");calls=0;handler(VALUE)}; func rebind(){f:=func(){handler=output};f()}`, body: `rebind();emit(); effects++`, ordered: true},
		{name: "escaped_plain", body: `candidate:="first"; alias:=&candidate; candidate="first"; *alias=VALUE; effects++; record(Error{Code:candidate})`, escaped: true, ordered: true},
		{name: "escaped_tuple", body: `candidate:="first"; var alias *string; candidate,alias="first",&candidate; *alias=VALUE; effects++; record(Error{Code:candidate})`, escaped: true, ordered: true},
		{name: "escaped_helper", body: `candidate:="first"; alias:=keep(&candidate); candidate="first"; *alias=VALUE; effects++; record(Error{Code:candidate})`, escaped: true, ordered: true},
		{name: "escaped_captured", body: `candidate:="first"; f:=func(){_=candidate};f(); alias:=&candidate; candidate="first"; *alias=VALUE; effects++; record(Error{Code:candidate})`, escaped: true, ordered: true},
		{name: "ordinary_replacement", body: `candidate:=VALUE; candidate="first"; effects++; record(Error{Code:candidate})`, replacement: true, ordered: true},
	} {
		for _, input := range []struct{ name, expr, actual string }{{"registered", `"second"`, "second"}, {"bad", `"boundary_bad"`, "boundary_bad"}, {"computed", `compute()`, "boundary_bad"}} {
			t.Run(tc.name+"/"+input.name, func(t *testing.T) {
				source := header + tc.declarations + "\nfunc run(){effects:=0; _=Other{Code:\"unrelated\"};\n" + tc.body + "\nif effects!=1 {panic(effects)}\n}\n"
				source = strings.ReplaceAll(source, "MUTATE", mutation)
				source = strings.ReplaceAll(source, "VALUE", input.expr)
				source = strings.ReplaceAll(source, "ACTUAL", fmt.Sprintf("%q", input.actual))
				actual := input.actual
				if tc.replacement {
					actual = "first"
				}
				// Run both the original and safely renamed/comment-shifted sources.
				for _, moved := range []bool{false, true} {
					name := "source.go"
					in := source
					if moved {
						name = "moved.go"
						in = "// source relocation\n\n" + strings.NewReplacer("candidate", "renamedCandidate", "handler", "renamedHandler").Replace(source)
					}
					dir := filepath.Join(root, fmt.Sprintf("%s_%s_%v", tc.name, input.name, moved))
					writeAssignmentFixture(t, filepath.Join(dir, name), in)
					writeAssignmentFixture(t, filepath.Join(dir, "runtime_test.go"), fmt.Sprintf(`package sample
import "testing"
func TestRuntime(t *testing.T){run();t.Logf("OBSERVED=%%s",observed);if calls!=1 || (observed!=%q && !(%t && observed=="first")){t.Fatalf("observed=%%s calls=%%d",observed,calls)}}
`, actual, !tc.ordered))
					got, err := Analyze([]Source{{Package: "sample", Filename: name, Content: []byte(in)}}, nil, []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}}})
					if err != nil {
						t.Fatal(err)
					}
					wantCodes := []string{"first"}
					if !tc.escaped && !tc.replacement && input.name != "computed" {
						wantCodes = append(wantCodes, input.actual)
						slices.Sort(wantCodes)
					}
					if !reflect.DeepEqual(got.Codes["refusal"], wantCodes) {
						t.Errorf("moved=%v codes=%v want %v", moved, got.Codes, wantCodes)
					}
					var wantDiags []string
					if tc.escaped || (!tc.replacement && input.name != "registered") {
						marker := input.expr
						if tc.escaped {
							marker = "&candidate"
							if moved {
								marker = "&renamedCandidate"
							}
						}
						offset := strings.LastIndex(in, marker)
						if offset < 0 {
							t.Fatal("missing marker", marker)
						}
						message := `unregistered code "boundary_bad"`
						if tc.escaped || input.name == "computed" {
							message = "cannot prove code expression; use a constant or a supported assignment flow"
						}
						wantDiags = []string{fmt.Sprintf("%s:%d: refusal: %s", name, 1+strings.Count(in[:offset], "\n"), message)}
					}
					var diags []string
					for _, d := range got.Diagnostics {
						diags = append(diags, d.String())
					}
					if !reflect.DeepEqual(diags, wantDiags) {
						t.Errorf("moved=%v diagnostics=%v want %v", moved, diags, wantDiags)
					}
				}
			})
		}
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	t.Logf("same-source runtime:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
