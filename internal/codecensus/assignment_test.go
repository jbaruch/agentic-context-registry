package codecensus

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Rename tokens, not substrings: candidate and observed must survive renaming c.
func renameAssignmentLocal(src string) string {
	fset := token.NewFileSet()
	file := fset.AddFile("input.go", -1, len(src))
	var scan scanner.Scanner
	scan.Init(file, []byte(src), nil, 0)
	var edits []int
	for {
		pos, tok, lit := scan.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT && lit == "c" {
			edits = append(edits, file.Offset(pos))
		}
	}
	for i := len(edits) - 1; i >= 0; i-- {
		at := edits[i]
		src = src[:at] + "candidate" + src[at+1:]
	}
	return src
}

func writeAssignmentFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every runtime program is the exact source passed to Analyze, including the
// moved/renamed layout. Runtime writes happen after all destination operands.
func TestAssignmentBoundaryAgainstRuntime(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module boundary\n\ngo 1.25\n")
	const header = `package sample
 type Error struct{Code string}
 type Other struct{Code string}
 type Slot struct{N int; Text string}
 var slot Slot
 var observed string
 var calls int
 func record(e Error) int { observed=e.Code; calls++; return 0 }
 func dest(e Error) *Slot { record(e); return &slot }
 func pointer(e Error) *int { record(e); return &slot.N }
 func other(e Other) int { return 0 }
 func compute() string { return "boundary_bad" }
 `
	for _, flow := range []struct {
		name, setup, statement string
		separate               bool
	}{
		{"slice", `slots:=[]int{0}`, `c,slots[record(Error{Code:c})]="first",1`, false},
		{"map", `slots:=map[int]int{}`, `c,slots[record(Error{Code:c})]="first",1`, false},
		{"parenthesized", `slots:=[]int{0}`, `(c),(slots[record(Error{Code:c})])="first",1`, false},
		{"selector_int", ``, `c,dest(Error{Code:c}).N="first",1`, false},
		{"selector_string", ``, `c,dest(Error{Code:c}).Text="first","x"`, false},
		{"dereference", ``, `c,*pointer(Error{Code:c})="first",1`, false},
		{"earlier_target", `slots:=[]int{0}`, `slots[record(Error{Code:c})],c=1,"first"`, false},
		{"split", `slots:=[]int{0}`, `slots[record(Error{Code:c})]=1; c="first"`, false},
		{"capture", `slots:=[]int{0}; read:=func()string{return c}; _=read`, `c,slots[record(Error{Code:c})]="first",1`, false},
		{"function_alias", `slots:=[]int{0}; alias:=record`, `c,slots[alias(Error{Code:c})]="first",1`, false},
		{"alias_rebind", `slots:=[]int{0}; alias:=func(text string)int{return record(Error{Code:text})}; discard:=func(text string)int{return 0}`, `alias,slots[alias(c)]=discard,1; _=alias`, false},
		{"compound_operand", ``, `dest(Error{Code:c}).Text += "x"`, false},
		{"compound_index", `slots:=[]int{0}`, `slots[record(Error{Code:c})] += 1`, false},
		{"rhs_observation", `slots:=[]int{0}`, `c,slots[0]="first",record(Error{Code:c})`, false},
		{"short_declaration", ``, `c,next:="first",c; record(Error{Code:next})`, false},
		{"simultaneous_rhs", `next:="first"`, `c,next="first",c; record(Error{Code:next})`, false},
		{"shared_field", `e:=Other{Code:c}`, `e.Code,slot.N="first",record(Error{Code:e.Code})`, false},
		{"lhs_and_rhs", `slots:=[]int{0}`, `c,slots[record(Error{Code:c})]="first",func()int{return 1}()`, false},
		{"namespace", `slots:=[]int{0}`, `c,slots[other(Other{Code:c})]="first",1; observed="first"`, true},
	} {
		for _, input := range []struct{ name, expr, actual string }{
			{"registered", `"second"`, "second"}, {"bad", `"boundary_bad"`, "boundary_bad"}, {"computed", `compute()`, "boundary_bad"},
		} {
			for _, moved := range []bool{false, true} {
				name := fmt.Sprintf("%s_%s_%t", flow.name, input.name, moved)
				t.Run(name, func(t *testing.T) {
					source := header + "func emit() {\nc := " + input.expr + "\n" + flow.setup + "\n" + flow.statement + "\n_=c; _=Error{Code:\"first\"}\n}\n"
					filename := "boundary.go"
					if moved {
						source = "// moved source\n\n" + renameAssignmentLocal(source)
						filename = "relocated/assignment.go"
					}
					actual, count := input.actual, 1
					if flow.separate {
						actual = "first"
						count = 0
					}
					dir := filepath.Join(root, name)
					writeAssignmentFixture(t, filepath.Join(dir, "source.go"), source)
					writeAssignmentFixture(t, filepath.Join(dir, "source_test.go"), fmt.Sprintf(`package sample
import "testing"
func TestRuntime(t *testing.T){emit();if observed!=%q || calls!=%d { t.Fatalf("observed=%%q calls=%%d",observed,calls) }}
`, actual, count))
					got, err := Analyze([]Source{{Package: "sample", Filename: filename, Content: []byte(source)}}, nil, []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}}})
					if err != nil {
						t.Fatal(err)
					}
					codes := []string{"first"}
					if !flow.separate && input.name != "computed" {
						codes = append(codes, input.actual)
						slices.Sort(codes)
					}
					if !reflect.DeepEqual(got.Codes["refusal"], codes) {
						t.Errorf("codes=%q, want %q", got.Codes["refusal"], codes)
					}
					var want []string
					if !flow.separate && input.name != "registered" {
						offset := strings.LastIndex(source, input.expr)
						message := `unregistered code "boundary_bad"`
						if input.name == "computed" {
							message = "cannot prove code expression; use a constant or a supported assignment flow"
						}
						want = []string{fmt.Sprintf("%s:%d: refusal: %s", filename, 1+strings.Count(source[:offset], "\n"), message)}
					}
					var diagnostics []string
					for _, d := range got.Diagnostics {
						diagnostics = append(diagnostics, d.String())
					}
					if !reflect.DeepEqual(diagnostics, want) {
						t.Errorf("diagnostics=%v, want %v", diagnostics, want)
					}
				})
			}
		}
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	t.Logf("same-source runtime outcomes:\n%s", out)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
}

// Graph cells for captures/globals/fields deliberately join writes. These
// tests do not mistake that conservative vocabulary for a runtime store.
func TestAssignmentEffectsAgainstRuntime(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module effects\n\ngo 1.25\n")
	for _, tc := range []struct {
		name, body, assertion string
		codes                 []string
		unknown               bool
	}{
		{"unused_late", `c:="first"; a:=[]int{0}; c,a[record(Error{Code:c})]="unused_bad",1; _=c`, `observed=="first" && calls==1`, []string{"first"}, false},
		{"shared_rhs", `c:="second"; _=func(){_=c}; next:="first"; c,next="first",c; record(Error{Code:next})`, `observed=="second" && calls==1`, []string{"first", "second"}, false},
		{"global_rhs", `global,a[record(Error{Code:global})]="first",1`, `observed=="second" && calls==1`, []string{"first", "second"}, false},
		{"captured_effect", `c:="first"; set:=func()int{c="second";return 1}; read:=func()int{return record(Error{Code:c})}; slots:=[]int{0,0}; slots[set()],slot.N=1,read(); _=slots`, `observed=="second" && calls==1`, []string{"first", "second"}, false},
		{"effect_order", `slots:=[]int{0}; slots[func()int{order+="L";record(Error{Code:"first"});return 0}()]=func()int{order+="R";record(Error{Code:"second"});return 1}()`, `order=="LR" && calls==2 && observed=="second"`, []string{"first", "second"}, false},
		{"compound_once", `dest:=func()*Slot{order+="L";record(Error{Code:"first"});return &slot}; dest().Text += func()string{order+="R";return "x"}()`, `order=="LR" && calls==1 && slot.Text=="x"`, []string{"first"}, false},
		{"address_once", `p:=&func()*Slot{order+="L";record(Error{Code:"first"});return &slot}().Text; _=p`, `order=="L" && calls==1`, []string{"first"}, false},
		{"map_key_effect", `m:=map[int]int{record(Error{Code:"second"}):0}; _=m`, `observed=="second" && calls==1`, []string{"second"}, false},
		{"nested_call_order", `factory:=func()func(int)int{order+="F";return func(n int)int{return n}}; slots:=[]int{0}; slots[factory()(func()int{order+="A";record(Error{Code:"second"});return 0}())]=1`, `order=="FA" && calls==1`, []string{"second"}, false},
		// The read of global relative to change() is unspecified by Go. Accept
		// either runtime result, but require the conservative analysis to keep both.
		{"unspecified_read", `change:=func()int{global="first";return 0}; c,n:=global,change(); _=n; record(Error{Code:c})`, `(observed=="first" || observed=="second") && calls==1`, []string{"first", "second"}, false},
		// Both a local read and a callee read can precede the first capture
		// syntactically without being ordered before its operand effects by Go.
		{"late_local_capture", `c:="first"; out,n:=c,func()int{c="second";return 0}(); _=n; record(Error{Code:out})`, `(observed=="first" || observed=="second") && calls==1`, []string{"first", "second"}, false},
		{"late_callee_capture", `output:=func(s string)int{return record(Error{Code:s})}; output("first"); f:=func(s string)int{return 0}; f(func()string{f=output;return "bad"}())`, `(observed=="first" && calls==1) || (observed=="bad" && calls==2)`, []string{"first"}, true},
		{"compound_target_unknown", `e:=Error{Code:"first"}; e.Code += "suffix"; record(e)`, `observed=="firstsuffix" && calls==1`, []string{"first"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := `package sample
type Error struct{Code string}
type Slot struct{N int; Text string}
var slot Slot
var global="second"
var a=[]int{0}
var observed,order string
var calls int
func record(e Error)int{observed=e.Code;calls++;return 0}
func emit(){` + tc.body + "}\n"
			dir := filepath.Join(root, tc.name)
			writeAssignmentFixture(t, filepath.Join(dir, "source.go"), source)
			writeAssignmentFixture(t, filepath.Join(dir, "source_test.go"), `package sample
import "testing"
func TestRuntime(t *testing.T){emit();if !(`+tc.assertion+`){t.Fatalf("observed=%q order=%q calls=%d",observed,order,calls)}}`)
			got, err := Analyze([]Source{{Package: "sample", Filename: "effects.go", Content: []byte(source)}}, nil, []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Codes["refusal"], tc.codes) {
				t.Errorf("codes=%v, want %v", got.Codes, tc.codes)
			}
			if tc.unknown {
				want := "effects.go:10: refusal: cannot prove code expression; use a constant or a supported assignment flow"
				if len(got.Diagnostics) != 1 || got.Diagnostics[0].String() != want {
					t.Errorf("diagnostics=%v want %s", got.Diagnostics, want)
				}
			} else if len(got.Diagnostics) != 0 {
				t.Error(got.Diagnostics)
			}
		})
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	t.Logf("same-source effect outcomes:\n%s", out)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
}
