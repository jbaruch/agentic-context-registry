package codecensus

// Regression controls adopted from the independent correction6 tester. Every case analyzes and executes the same source.
// Runtime assertions accept only Go-legal observations and log the compiler's
// actual choice; analyzer assertions require that no legal emission is green.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const pendingWriteHeader = `package sample
type Error struct{Code string; N int}
type Other struct{Code string; N int}
type Inner struct{Text string}
type Slot struct{N int; Text string; In Inner}
type R struct{}
func (R) rec(e Error) int { return record(e) }
var slot Slot
var observed string
var calls int
func record(e Error) int { observed=e.Code; calls++; return 0 }
func dest(e Error) *Slot { record(e); return &slot }
func ptr(e Error) *int { record(e); return &slot.N }
func wrap(e Error, n int) { _ = n; record(e) }
func keep(p *string) *string { return p }
func compute() string { return "boundary_bad" }
`

func pendingWriteWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pendingWriteAnalyze(t *testing.T, filename, source string) ([]string, []string) {
	t.Helper()
	got, err := Analyze([]Source{{Package: "sample", Filename: filename, Content: []byte(source)}}, nil, []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}}})
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics []string
	for _, d := range got.Diagnostics {
		diagnostics = append(diagnostics, d.String())
	}
	return got.Codes["refusal"], diagnostics
}

func pendingWriteRuntime(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	t.Logf("same-source runtime outcomes:\n%s", out)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
}

// Family 2: pending-write shapes beyond the shipped matrix, over the value
// matrix and a relocated/comment-shifted layout. Exact sets and diagnostics.
func TestPendingWriteShapes(t *testing.T) {
	root := t.TempDir()
	pendingWriteWrite(t, filepath.Join(root, "go.mod"), "module pendingWriteb\n\ngo 1.25\n")
	for _, flow := range []struct {
		name, setup, statement string
		alsoCodes              []string
		observed               string
		calls                  int
	}{
		{"array_index", `var arr [1]int`, `c,arr[record(Error{Code:c})]="first",1; _=arr`, nil, "", 0},
		{"paren_star_call", ``, `c,(*ptr(Error{Code:c}))="first",1`, nil, "", 0},
		{"double_index_call", `m:=map[int][]int{0:{0}}`, `c,m[record(Error{Code:c})][0]="first",1`, nil, "", 0},
		{"nested_selector_chain", ``, `c,dest(Error{Code:c}).In.Text="first","x"`, nil, "", 0},
		{"method_value_callee", `var r R`, `c,slots[r.rec(Error{Code:c})]="first",1`, nil, "", 0},
		{"func_field_callee", `type H struct{fn func(Error)int}; h:=H{fn:record}`, `c,slots[h.fn(Error{Code:c})]="first",1`, nil, "", 0},
		{"conversion_selector", `e:=Error{Code:"first"}`, `c,(*Other)(&e).Code="first",c; record(e)`, nil, "", 0},
		{"struct_field_forward", `s:=Slot{}`, `c,s.Text="first",c; record(Error{Code:s.Text})`, nil, "", 0},
		{"string_slice_index", `codes:=[]string{"second"}`, `c,codes[record(Error{Code:c})]="first",c; _=codes`, nil, "", 0},
		{"swap", `b:="first"`, `c,b=b,c; record(Error{Code:b})`, nil, "", 0},
		{"duplicate_lhs", ``, `c,c="first",c; record(Error{Code:c})`, nil, "", 0},
		{"short_decl_redeclare", ``, `c,n:="first",slots[record(Error{Code:c})]; _=n`, nil, "", 0},
		{"closure_operand_reads_pre_write", ``, `c,slots[func()int{return record(Error{Code:c})}()]="first",1`, nil, "", 0},
		{"two_emitting_operands", ``, `c,slots[record(Error{Code:c})],slot.N="first",1,record(Error{Code:"second"})`, []string{"second"}, "second", 2},
		{"declaration_operands", ``, `var out,n=c,record(Error{Code:c}); _=out; _=n`, nil, "", 0},
	} {
		for _, input := range []struct{ name, expr, actual string }{
			{"registered", `"second"`, "second"}, {"bad", `"boundary_bad"`, "boundary_bad"}, {"computed", `compute()`, "boundary_bad"},
		} {
			name := flow.name + "_" + input.name
			t.Run(name, func(t *testing.T) {
				source := pendingWriteHeader + "func emit() {\nc := " + input.expr + "\nslots:=[]int{0}\n" + flow.setup + "\n" + flow.statement + "\n_=c; _=slots; _=Error{Code:\"first\"}\n}\n"
				for _, moved := range []bool{false, true} {
					filename, in := "probe.go", source
					if moved {
						filename, in = "relocated/probe.go", "// moved source\n\n"+source
					}
					codes, diagnostics := pendingWriteAnalyze(t, filename, in)
					want := append([]string{"first"}, flow.alsoCodes...)
					if input.name != "computed" {
						want = append(want, input.actual)
					}
					slices.Sort(want)
					want = slices.Compact(want)
					if !reflect.DeepEqual(codes, want) {
						t.Errorf("moved=%v codes=%q, want %q", moved, codes, want)
					}
					var wantDiagnostics []string
					if input.name != "registered" {
						offset := strings.LastIndex(in, input.expr)
						message := `unregistered code "boundary_bad"`
						if input.name == "computed" {
							message = "cannot prove code expression; use a constant or a supported assignment flow"
						}
						wantDiagnostics = []string{fmt.Sprintf("%s:%d: refusal: %s", filename, 1+strings.Count(in[:offset], "\n"), message)}
					}
					if !reflect.DeepEqual(diagnostics, wantDiagnostics) {
						t.Errorf("moved=%v diagnostics=%q, want %q", moved, diagnostics, wantDiagnostics)
					}
				}
				observed, calls := input.actual, 1
				if flow.observed != "" {
					observed, calls = flow.observed, flow.calls
				}
				dir := filepath.Join(root, name)
				pendingWriteWrite(t, filepath.Join(dir, "source.go"), source)
				pendingWriteWrite(t, filepath.Join(dir, "source_test.go"), fmt.Sprintf(`package sample
import "testing"
func TestRuntime(t *testing.T){ emit(); t.Logf("OBSERVED case=%s observed=%%q calls=%%d", observed, calls); if observed!=%q || calls!=%d { t.Fatalf("observed=%%q calls=%%d", observed, calls) } }
`, name, observed, calls))
			})
		}
	}
	pendingWriteRuntime(t, root)
}
