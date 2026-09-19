package codecensus

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAnalyzeCodeFlows(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body  string
		codes       []string
		diagnostics []string
	}{
		{name: "named literal", body: `func namedError(code string) Error { return Error{Code: code} }
func emit() Error { return namedError("first") }`, codes: []string{"first"}},
		{name: "misspelled named literal", body: `func namedError(code string) Error { return Error{Code: code} }
func emit() Error { return namedError("frist") }`, codes: []string{"frist"}, diagnostics: []string{`fixture.go:4: refusal: unregistered code "frist"`}},
		{name: "arbitrary code local", body: `func compute() string { return "first" }
func emit() Error {
 code := compute()
 return Error{Code: code}
}`, diagnostics: []string{"fixture.go:5: refusal: cannot prove code expression; use a constant or a supported assignment flow"}},
		{name: "constant and renamed locals", body: `const chosen = "first"
func emit() Error { renamed := chosen; next := renamed; return Error{Code: next} }`, codes: []string{"first"}},
		{name: "typed constant conversion", body: `type Code string
const chosen Code = "first"
func emit() Error { renamed := chosen; return Error{Code: string(renamed)} }`, codes: []string{"first"}},
		{name: "branch assignment", body: `func emit(flag bool) Error {
 var renamed string
 if flag { renamed = "first" } else { renamed = "second" }
 return Error{Code: renamed}
}`, codes: []string{"first", "second"}},
		{name: "switch assignment", body: `func emit(flag int) Error {
 var renamed string
 switch flag {case 1: renamed = "first"; default: renamed = "second"}
 return Error{Code: renamed}
}`, codes: []string{"first", "second"}},
		{name: "shadow remains separate", body: `func emit() Error {
 code := "first"
 { code := "unrelated"; _ = code }
 return Error{Code: code}
}`, codes: []string{"first"}},
		{name: "shadow emission rejected", body: `func emit() Error {
 code := "first"; _ = code
 { code := "bad"; return Error{Code: code} }
}`, codes: []string{"bad"}, diagnostics: []string{`fixture.go:5: refusal: unregistered code "bad"`}},
		{name: "assignment replaces value", body: `func emit() Error {
 code := "unused"
 code = "first"
 return Error{Code: code}
}`, codes: []string{"first"}},
		{name: "unrelated nested code", body: `type Blocker struct { Code string }
func emit() Error { _ = Blocker{Code: "nested-only"}; return Error{Code: "first"} }`, codes: []string{"first"}},
		{name: "field assignment", body: `func emit() Error {
 result := Error{Code: "first"}
 result.Code = "bad"
 return result
}`, codes: []string{"bad", "first"}, diagnostics: []string{`fixture.go:5: refusal: unregistered code "bad"`}},
		{name: "forwarded application field", body: `type ApplicationError struct { Code string }
func upstream() ApplicationError { return ApplicationError{Code: "first"} }
func emit(up ApplicationError) Error { return Error{Code: up.Code} }`, codes: []string{"first"}},
		{name: "unknown upstream field", body: `type ApplicationError struct { Code string }
func compute() string { return "first" }
func upstream() ApplicationError { return ApplicationError{Code: compute()} }
func emit(up ApplicationError) Error { return Error{Code: up.Code} }`, diagnostics: []string{"fixture.go:5: refusal: cannot prove code expression; use a constant or a supported assignment flow"}},
		{name: "callback assignment flow", body: `func check(add func(string)) { add("first") }
func emit() Error {
 var result Error
 add := func(code string) { result.Code = code }
 check(add)
 return result
}`, codes: []string{"first"}},
		{name: "callback bad argument", body: `func check(add func(string)) { add("bad") }
func emit() Error {
 var result Error
 add := func(renamed string) { result.Code = renamed }
 check(add)
 return result
}`, codes: []string{"bad"}, diagnostics: []string{`fixture.go:3: refusal: unregistered code "bad"`}},
		{name: "loop includes later values", body: `func emit() Error {
 code := "first"
 next := "bad"
 for i:=0;i<2;i++ { _ = Error{Code: code}; code = next }
 return Error{Code: "first"}
}`, codes: []string{"bad", "first"}, diagnostics: []string{`fixture.go:5: refusal: unregistered code "bad"`}},
		{name: "captured variable mutation", body: `func emit() Error {
 code := "first"
 closure := func() Error { return Error{Code: code} }
 code = "bad"
 return closure()
}`, codes: []string{"bad", "first"}, diagnostics: []string{`fixture.go:6: refusal: unregistered code "bad"`}},
		{name: "parameter branch assignment", body: `func helper(code string, flag bool) Error {
 if flag { code = "second" }
 return Error{Code: code}
}
func emit() Error { return helper("first",true) }`, codes: []string{"first", "second"}},
		{name: "loop break keeps exit value", body: `func emit(flag bool) Error {
 code := "first"
 for flag {code = "bad"; if flag { break }; code = "first"}
 return Error{Code: code}
}`, codes: []string{"bad", "first"}, diagnostics: []string{`fixture.go:5: refusal: unregistered code "bad"`}},
		{name: "switch fallthrough keeps value", body: `func emit(flag int) Error {
 code := "first"
 switch flag {case 1: code = "bad"; fallthrough
 case 2: return Error{Code: code}}
 return Error{Code: "first"}
}`, codes: []string{"bad", "first"}, diagnostics: []string{`fixture.go:5: refusal: unregistered code "bad"`}},
		{name: "unresolved source cycle", body: `func emit(e Error) Error { return Error{Code: e.Code} }`, diagnostics: []string{"fixture.go:2: refusal: cannot prove code expression; use a constant or a supported assignment flow"}},
		{name: "goto fails closed", body: `func emit() Error {
 code := "bad"
 goto output
 code = "first"
 output: return Error{Code: code}
}`, codes: []string{"first"}, diagnostics: []string{"fixture.go:5: refusal: cannot prove code expression; use a constant or a supported assignment flow"}},
		{name: "continue post overwrites before condition", body: `func check(e Error) bool { return e.Code != "" }
func compute() string { return "bad" }
func emit() Error {
 code := "first"; flag := true
 for i:=0; check(Error{Code:code}) && i<1; code="first" {
  i++; code=compute()
  if flag { continue }
  code="first"
 }
 return Error{Code:"first"}
}`, codes: []string{"first"}},
		{name: "function alias", body: `func namedError(code string) Error { return Error{Code: code} }
func emit() Error { alias := namedError; return alias("bad") }`, codes: []string{"bad"}, diagnostics: []string{`fixture.go:4: refusal: unregistered code "bad"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := "package sample\ntype Error struct { Code string }\n" + test.body + "\n"
			got, err := Analyze([]Source{{Package: "sample", Filename: "fixture.go", Content: []byte(source)}}, nil, []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Codes["refusal"], test.codes) {
				t.Errorf("codes = %q, want %q", got.Codes["refusal"], test.codes)
			}
			var diagnostics []string
			for _, d := range got.Diagnostics {
				diagnostics = append(diagnostics, d.String())
			}
			if !reflect.DeepEqual(diagnostics, test.diagnostics) {
				t.Errorf("diagnostics = %q, want %q", diagnostics, test.diagnostics)
			}
		})
	}
}

func TestAnalyzeNamespacesAndInputOrder(t *testing.T) {
	t.Parallel()
	sources := []Source{
		{Package: "sample", Filename: "types.go", Content: []byte("package sample\ntype Error struct{ Code string }; type Notice struct{ Code string }\n")},
		{Package: "sample", Filename: "b.go", Content: []byte("package sample\nvar _ = Notice{Code: \"refuse\"}\n")},
		{Package: "sample", Filename: "a.go", Content: []byte("package sample\nvar _ = Error{Code: \"observe\"}\n")},
	}
	targets := []Target{
		{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"refuse"}},
		{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"observe"}},
	}
	first, err := Analyze(sources, nil, targets)
	if err != nil {
		t.Fatal(err)
	}
	sources[0], sources[2] = sources[2], sources[0]
	second, err := Analyze(sources, nil, targets)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("input order changed result: %v / %v", first, second)
	}
	if len(first.Diagnostics) != 2 || first.Diagnostics[0].String() != `a.go:2: refusal: unregistered code "observe"` || first.Diagnostics[1].String() != `b.go:2: notice: unregistered code "refuse"` {
		t.Fatalf("diagnostics = %v", first.Diagnostics)
	}
}

func TestAnalyzeRejectsInvalidSourceAndMissingTarget(t *testing.T) {
	for _, source := range []string{"package sample\nfunc {", "package sample\nvar x = undefined"} {
		if _, err := Analyze([]Source{{Package: "sample", Filename: "bad.go", Content: []byte(source)}}, nil, nil); err == nil || !strings.Contains(err.Error(), "bad.go:") {
			t.Fatalf("error = %v, want bad.go diagnostic", err)
		}
	}
	if _, err := Analyze(nil, nil, []Target{{Package: "missing", Type: "Error", Field: "Code"}}); err == nil {
		t.Fatal("missing target accepted")
	}
}

// Analyze and execute the same fixed inputs: the runtime result is an independent
// oracle for the source paths, including the second loop-condition evaluation.
func TestAnalyzeCorrectedFlowsAgainstRuntime(t *testing.T) {
	root := t.TempDir()
	identByte := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
	}
	renameIdent := func(src, from, to string) string {
		var out strings.Builder
		for i := 0; i < len(src); {
			if strings.HasPrefix(src[i:], from) {
				end := i + len(from)
				if (i == 0 || !identByte(src[i-1])) && (end == len(src) || !identByte(src[end])) {
					out.WriteString(to)
					i = end
					continue
				}
			}
			out.WriteByte(src[i])
			i++
		}
		return out.String()
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "go.mod"), "module fixtures\n\ngo 1.25\n")
	for _, flow := range []struct {
		name, body, result string
		registered         []string
		separate           bool
	}{
		{"local", `func emit() Error {
 code := "first"
 (code) = VALUE
 return Error{Code:code}
}`, "emit().Code", []string{"second"}, false},
		{"field", `func emit() Error {
 e := Error{Code:"first"}
 (e.Code) = VALUE
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"condition", `var observed string
var count int
func check(e Error) bool { observed=e.Code; count++; return count<2 }
func emit() Error {
 code := "first"
 for check(Error{Code:code}) {
  code = VALUE
 }
 return Error{Code:"first"}
}`, "func() string { emit(); return observed }()", []string{"first", "second"}, false},
		{"post_condition", `var observed string
var count int
func check(e Error) bool { observed=e.Code; count++; return count<2 }
func emit() Error {
 for code := "first"; check(Error{Code:code}); code = VALUE {}
 return Error{Code:"first"}
}`, "func() string { emit(); return observed }()", []string{"first", "second"}, false},
		{"switch", `func emit() Error {
 code := "first"; flag := true
 switch { default:
  code = VALUE
  if flag { break }
  code = "first"
 }
 return Error{Code:code}
}`, "emit().Code", []string{"first", "second"}, false},
		{"conversion", `type Other struct { Code string }
func emit() Error {
 _ = Error{Code:"first"}
 candidate := Other{Code: VALUE}
 return Error(candidate)
}`, "emit().Code", []string{"first", "second"}, false},
		{"anonymous_conversion", `func emit() Error {
 _ = Error{Code:"first"}
 candidate := struct{ Code string }{Code: VALUE}
 return Error(candidate)
}`, "emit().Code", []string{"first", "second"}, false},
		{"pointer_conversion", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 (*Other)(&e).Code = VALUE
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"continue_post", `var observed string
func record(e Error) { observed = e.Code }
func emit() Error {
 code := "first"; flag := true
 for i:=0; i<1; record(Error{Code:code}) {
  i++
  code = VALUE
  if flag { continue }
  code = "first"
 }
 return Error{Code:"first"}
}`, "func() string { emit(); return observed }()", []string{"first", "second"}, false},
		{"continue_nearest_loop", `var observed string
func record(e Error) { observed = e.Code }
func emit() Error {
 code := "first"; flag := true
 for i:=0; i<1; record(Error{Code:code}) {
  i++
  for j:=0; j<1; j++ {
   code = "nested-only"
   if flag { continue }
   code = "first"
  }
  code = VALUE
 }
 return Error{Code:"first"}
}`, "func() string { emit(); return observed }()", []string{"first", "second"}, false},
		{"continue_range", `var observed string
func record(e Error) { observed = e.Code }
func emit() Error {
 code := "first"; flag := true
 for range []int{0,1} {
  record(Error{Code:code})
  code = VALUE
  if flag { continue }
  code = "first"
 }
 return Error{Code:"first"}
}`, "func() string { emit(); return observed }()", []string{"first", "second"}, false},
		{"slice_array", `type A [1]struct{ Code string }
func emit() Error {
 _ = A{{Code:"first"}}
 code := []struct{Code string}{{Code:VALUE}}
 a := A(code)
 return Error{Code:a[0].Code}
}`, "emit().Code", []string{"first", "second"}, false},
		{"slice_array_pointer", `type A [1]struct{ Code string }
func emit() Error {
 _ = A{{Code:"first"}}
 code := []struct{Code string}{{Code:VALUE}}
 a := (*A)(code)
 return Error{Code:a[0].Code}
}`, "emit().Code", []string{"first", "second"}, false},
		{"slice_array_pointer_shared_write", `type A [1]struct{ Code string }
func emit() Error {
 code := []struct{Code string}{{Code:"first"}}
 a := (*A)(code)
 a[0].Code = VALUE
 return Error{Code:code[0].Code}
}`, "emit().Code", []string{"first", "second"}, false},
		{"slice_array_copy_separate_write", `type A [1]struct{ Code string }
func emit() Error {
 code := []struct{Code string}{{Code:VALUE}}
 a := A(code)
 a[0].Code = "nested-only"
 return Error{Code:code[0].Code}
}`, "emit().Code", []string{"second"}, false},
		{"post_without_continue", `var observed string
func record(e Error) { observed = e.Code }
func emit() Error {
 code := "first"
 for i:=0; i<1; record(Error{Code:code}) {
  i++
  code = VALUE
 }
 return Error{Code:"first"}
}`, "func() string { emit(); return observed }()", []string{"first", "second"}, false},
		{"array_array_control", `type A [1]struct{ Code string }; type B [1]struct{ Code string }
func emit() Error {
 _ = A{{Code:"first"}}
 code := B{{Code:VALUE}}
 a := A(code)
 return Error{Code:a[0].Code}
}`, "emit().Code", []string{"first", "second"}, false},
		{"slice_slice_control", `type A []struct{ Code string }; type B []struct{ Code string }
func emit() Error {
 _ = A{{Code:"first"}}
 code := B{{Code:VALUE}}
 a := A(code)
 return Error{Code:a[0].Code}
}`, "emit().Code", []string{"first", "second"}, false},
		{"inline_converted_lhs", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 *(*Other)(&e) = Other{Code: VALUE}
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"named_pointer_aggregate", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 p := (*Other)(&e)
 *p = Other{Code: VALUE}
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"direct_aggregate_conversion", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 e = Error(Other{Code: VALUE})
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"inline_converted_lhs_paren", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 *((*Other)(&e)) = Other{Code: VALUE}
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"inline_converted_index", `type Other struct { Code string }
func emit() Error {
 e := []Error{{Code:"first"}}
 *(*Other)(&e[0]) = Other{Code: VALUE}
 return e[0]
}`, "emit().Code", []string{"first", "second"}, false},
		{"inline_converted_lhs_rhs_convert", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 *(*Other)(&e) = Other(Error{Code: VALUE})
 return e
}`, "emit().Code", []string{"first", "second"}, false},
		{"unrelated_aggregate", `type Other struct { Code string }
func emit() Error {
 e := Error{Code:"first"}
 _ = Other{Code: VALUE}
 return e
}`, "emit().Code", []string{"first"}, true},
	} {
		for _, value := range []struct {
			name, expression, runtime string
			unknown                   bool
		}{
			{"registered", `"second"`, "second", false},
			{"unregistered", `"bad"`, "bad", false},
			{"computed", `compute()`, "bad", true},
		} {
			name := flow.name + "_" + value.name
			t.Run(name, func(t *testing.T) {
				header := "package sample\ntype Error struct{ Code string }\nfunc compute() string { return \"bad\" }\n"
				source := header + strings.ReplaceAll(flow.body, "VALUE", value.expression) + "\n"
				// Comment insertion and a local rename must preserve vocabulary and move
				// diagnostics to the actual expression, without coupling to app layout.
				for _, refactor := range []bool{false, true} {
					input := source
					if refactor {
						input = "// Harmless source movement.\n" + strings.ReplaceAll(input, "code", "renamed")
						input = renameIdent(input, "e", "renamed")
					}
					got, err := Analyze([]Source{{Package: "sample", Filename: "fixture.go", Content: []byte(input)}}, nil, []Target{{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}}})
					if err != nil {
						t.Fatal(err)
					}
					if value.name == "registered" || flow.separate {
						if len(got.Diagnostics) != 0 || !reflect.DeepEqual(got.Codes["refusal"], flow.registered) {
							t.Fatalf("registered result: %v", got)
						}
					} else {
						offset := strings.LastIndex(input, value.expression)
						line := 1 + strings.Count(input[:offset], "\n")
						message := `unregistered code "bad"`
						if value.unknown {
							message = "cannot prove code expression; use a constant or a supported assignment flow"
						}
						want := fmt.Sprintf("fixture.go:%d: refusal: %s", line, message)
						if len(got.Diagnostics) != 1 || got.Diagnostics[0].String() != want {
							t.Fatalf("diagnostics = %v, want %s", got.Diagnostics, want)
						}
					}
				}
				runtime := value.runtime
				if flow.separate {
					runtime = "first"
				}
				write(filepath.Join(root, name, "fixture.go"), source)
				write(filepath.Join(root, name, "fixture_test.go"), fmt.Sprintf("package sample\nimport \"testing\"\nfunc TestRuntime(t *testing.T) { if got := %s; got != %q { t.Fatalf(\"code = %%q\",got) } }\n", flow.result, runtime))
			})
		}
	}
	command := exec.Command("go", "test", "./...", "-count=1")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("runtime fixtures: %v\n%s", err, output)
	}
}

func TestRepositorySelectsProductionSources(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":           "module inventory\n\ngo 1.25\n",
		"selected.go":      "package inventory\ntype Error struct{ Code string }; var emitted = Error{Code: \"first\"}\n",
		"ignored.go":       "//go:build ignore\n\npackage inventory\nvar ignored = doesNotCompile\n",
		"selected_test.go": "package inventory\nvar testOnly = doesNotCompile\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sources, imports, err := Repository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Filename != "selected.go" || sources[0].Package != "inventory" {
		t.Fatalf("build-selected inventory = %v", sources)
	}
	got, err := Analyze(sources, imports, []Target{{Package: "inventory", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diagnostics) != 0 || !reflect.DeepEqual(got.Codes["refusal"], []string{"first"}) {
		t.Fatalf("selected production result = %v", got)
	}
}

// Seeded ordinary returned callables must keep the invoked Code dependency.
// Direct and unseeded routes are controls; a registered seed must not hide them.
func TestReturnedCallableTargetUncertainty(t *testing.T) {
	root := t.TempDir()
	writeAssignmentFixture(t, filepath.Join(root, "go.mod"), "module sample\n\ngo 1.25\n")
	for _, moved := range []bool{false, true} {
		for _, route := range []string{"direct", "returned", "returned_unseeded"} {
			for _, value := range []struct{ name, expression, actual string }{
				{"registered", `"second"`, "second"},
				{"bad", `"returned_unregistered"`, "returned_unregistered"},
				{"computed", `compute()`, "returned_unregistered"},
			} {
				t.Run(fmt.Sprintf("moved_%t/%s/%s", moved, route, value.name), func(t *testing.T) {
					seed := `_ = output("first")`
					callee := "supply()"
					if route == "direct" {
						callee = "output"
					}
					if route == "returned_unseeded" {
						seed = ""
					}
					source := `package sample
type Error struct { Code string }
type Notice struct { Code string }
func compute() string { return "returned_unregistered" }
func output(code string) Error { return Error{Code:code} }
func supply() func(string) Error { return output }
func emit() Error {
 _ = Notice{Code:"notice_only"}
 ` + seed + `
 chosen := ` + callee + `
 return chosen(` + value.expression + `)
}
`
					name := "fixture.go"
					if moved {
						name = "shifted.go"
						source = "// moved source\n\n" + strings.ReplaceAll(source, "chosen", "renamed")
					}
					dir := filepath.Join(root, fmt.Sprintf("%s_%s_%t", route, value.name, moved))
					writeAssignmentFixture(t, filepath.Join(dir, name), source)
					writeAssignmentFixture(t, filepath.Join(dir, "runtime_test.go"), fmt.Sprintf(`package sample
import "testing"
func TestRuntime(t *testing.T){ got:=emit().Code; t.Logf("ACTUAL=%%s",got); if got!=%q {t.Fatalf("got %%q",got)} }
`, value.actual))
					cmd := exec.Command("go", "test", "-count=1", "-run", "^TestRuntime$", "-v", ".")
					cmd.Dir = dir
					out, err := cmd.CombinedOutput()
					t.Logf("runtime:\n%s", out)
					if err != nil {
						t.Fatal(err)
					}
					got, err := Analyze([]Source{{Package: "sample", Filename: name, Content: []byte(source)}}, nil, []Target{
						{Package: "sample", Type: "Error", Field: "Code", Namespace: "refusal", Registered: []string{"first", "second"}},
						{Package: "sample", Type: "Notice", Field: "Code", Namespace: "notice", Registered: []string{"notice_only"}},
					})
					if err != nil {
						t.Fatal(err)
					}
					argumentLine := 1 + strings.Count(source[:strings.LastIndex(source, value.expression)], "\n")
					t.Logf("codes=%v diagnostics=%v argument=%s:%d", got.Codes, got.Diagnostics, name, argumentLine)
					if !reflect.DeepEqual(got.Codes["notice"], []string{"notice_only"}) {
						t.Errorf("unrelated namespace changed: %v", got.Codes["notice"])
					}
					hasFirst := false
					hasActual := false
					for _, code := range got.Codes["refusal"] {
						if code == "first" {
							hasFirst = true
						}
						if code == value.actual {
							hasActual = true
						}
					}
					if route != "returned_unseeded" && !hasFirst {
						t.Error("seed vocabulary was lost")
					}
					if route == "returned_unseeded" && hasFirst {
						t.Error("unseeded control acquired the seed value")
					}
					var refusal []Diagnostic
					for _, d := range got.Diagnostics {
						if d.Namespace == "notice" {
							t.Errorf("unrelated diagnostic: %s", d.String())
						}
						if d.Namespace == "refusal" && d.Position.Filename == name && d.Position.Line > 0 {
							refusal = append(refusal, d)
						}
					}
					atArgument := len(refusal) == 1 && refusal[0].Position.Line == argumentLine
					if value.name == "registered" {
						if !hasActual && len(refusal) == 0 {
							t.Errorf("emitted registered value omitted without target uncertainty")
						}
						if route == "direct" && (hasActual != true || len(refusal) != 0) {
							t.Errorf("direct registered control: codes=%v diagnostics=%v", got.Codes["refusal"], got.Diagnostics)
						}
						return
					}
					if len(refusal) == 0 {
						t.Errorf("actual emitted %s value lacks source-located target diagnostic", value.name)
					}
					if route == "direct" && !atArgument {
						t.Errorf("direct diagnostic location mismatch: %v want %s:%d", got.Diagnostics, name, argumentLine)
					}
					if !atArgument && len(refusal) != 0 {
						t.Logf("non-direct diagnostic at %v; argument is %s:%d", refusal, name, argumentLine)
					}
					if value.name == "bad" && !hasActual && len(refusal) == 0 {
						t.Errorf("unregistered emission missing from codes without a diagnostic")
					}
				})
			}
		}
	}
}
