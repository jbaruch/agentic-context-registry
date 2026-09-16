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

func TestCombinedDependencyBoundaries(t *testing.T) {
	for _, value := range []struct{ name, expr, actual string }{
		{"registered", `"second"`, "second"}, {"bad", `"bad"`, "bad"}, {"computed", `compute()`, "bad"},
	} {
		for _, moved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/moved=%v", value.name, moved), func(t *testing.T) {
				a := `package a
type X struct {Code string}
var Default = "first"
func Emit() X { return X{Code:Default} }
`
				b := `package b
import "combo/a"
type R struct {Code string}
type P struct {Code string}
type GError struct {Code string}
type Other struct {Code string}
var Global = "first"
func EmitGlobal() GError { return GError{Code:Global} }
func compute() string { return "bad" }
func Run() (R,P,int) {
 x:="first"
 p:=&x
 x="first"
 *p=VALUE
 _=Other{Code:"unrelated"}
 c:="first"
 return R{Code:c},P{Code:x},func() int {
  c=VALUE
  Global=VALUE
  a.Default=VALUE
  return 0
 }()
}
`
				b = strings.ReplaceAll(b, "VALUE", value.expr)
				name := "b/b.go"
				if moved {
					b = "// moved\n\n" + renameAssignmentLocal(b)
					name = "b/moved.go"
				}
				dir := t.TempDir()
				writeAssignmentFixture(t, filepath.Join(dir, "go.mod"), "module combo\n\ngo 1.25\n")
				writeAssignmentFixture(t, filepath.Join(dir, "a/a.go"), a)
				writeAssignmentFixture(t, filepath.Join(dir, name), b)
				runtime := fmt.Sprintf(`package b
import("testing";"combo/a")
func TestRuntime(t *testing.T){r,p,_:=Run();g:=EmitGlobal();x:=a.Emit();t.Logf("OBSERVED r=%%q p=%%q g=%%q x=%%q",r.Code,p.Code,g.Code,x.Code); if (r.Code!="first"&&r.Code!=%q)||p.Code!=%q||g.Code!=%q||x.Code!=%q {t.Fatal(r,p,g,x)}}`, value.actual, value.actual, value.actual, value.actual)
				writeAssignmentFixture(t, filepath.Join(dir, "b/runtime_test.go"), runtime)
				cmd := exec.Command("go", "test", "-count=1", "-v", "./...")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				t.Logf("runtime:\n%s", out)
				if err != nil {
					t.Fatal(err)
				}
				var targets []Target
				for _, x := range []struct{ pkg, typ, ns string }{{"combo/b", "R", "return"}, {"combo/b", "P", "address"}, {"combo/b", "GError", "global"}, {"combo/a", "X", "qualified"}} {
					targets = append(targets, Target{Package: x.pkg, Type: x.typ, Field: "Code", Namespace: x.ns, Registered: []string{"first", "second"}})
				}
				got, err := Analyze([]Source{{Package: "combo/a", Filename: "a/a.go", Content: []byte(a)}, {Package: "combo/b", Filename: name, Content: []byte(b)}}, nil, targets)
				if err != nil {
					t.Fatal(err)
				}
				unknown := "cannot prove code expression; use a constant or a supported assignment flow"
				line := func(marker string) int {
					if moved && strings.HasPrefix(marker, "c=") {
						marker = "candidate=" + strings.TrimPrefix(marker, "c=")
					}
					idx := strings.Index(b, marker)
					if idx < 0 {
						t.Fatal(marker)
					}
					return 1 + strings.Count(b[:idx], "\n")
				}
				want := []string{fmt.Sprintf("%s:%d: address: %s", name, line("p:=&x"), unknown)}
				if value.name != "registered" {
					message := `unregistered code "bad"`
					if value.name == "computed" {
						message = unknown
					}
					for _, x := range []struct{ ns, marker string }{{"return", "c=" + value.expr}, {"global", "Global=" + value.expr}, {"qualified", "a.Default=" + value.expr}} {
						want = append(want, fmt.Sprintf("%s:%d: %s: %s", name, line(x.marker), x.ns, message))
					}
				}
				var actual []string
				for _, d := range got.Diagnostics {
					actual = append(actual, d.String())
				}
				slices.Sort(want)
				slices.Sort(actual)
				t.Logf("codes=%v diagnostics=%v want=%v", got.Codes, actual, want)
				if !reflect.DeepEqual(actual, want) {
					t.Errorf("diagnostics mismatch")
				}
				for _, ns := range []string{"return", "global", "qualified"} {
					codes := []string{"first"}
					if value.name != "computed" {
						codes = append(codes, value.actual)
						slices.Sort(codes)
					}
					if !reflect.DeepEqual(got.Codes[ns], codes) {
						t.Errorf("%s codes=%v want %v", ns, got.Codes[ns], codes)
					}
				}
			})
		}
	}
}
