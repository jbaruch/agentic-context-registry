package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
	"github.com/jbaruch/agentic-context-registry/internal/codecensus"
)

// Analyze the complete production inventory with real registries, then execute
// the exact changed source through namedError and migrateCLIError. Go overlays
// keep production files untouched and put all fixtures in the test temp root.
func TestCodeCensusAssignmentProductionBoundary(t *testing.T) {
	root := commandDocsRoot(t)
	sources, imports, err := codecensus.Repository(root)
	if err != nil {
		t.Fatal(err)
	}
	targets := []codecensus.Target{
		{Package: "github.com/jbaruch/agentic-context-registry/internal/cli", Type: "Error", Field: "Code", Namespace: "refusal", Registered: cli.RefusalCodes, AllowEmpty: true},
		{Package: "github.com/jbaruch/agentic-context-registry/internal/cli", Type: "Notice", Field: "Code", Namespace: "notice", Registered: append(append([]string(nil), cli.NoticeCodes...), cli.RefusalCodes...)},
	}
	baseline, err := codecensus.Analyze(sources, imports, targets)
	if err != nil || len(baseline.Diagnostics) != 0 {
		t.Fatalf("baseline: %v %v", baseline, err)
	}
	t.Logf("production files=%d refusals=%d notices=%d", len(sources), len(baseline.Codes["refusal"]), len(baseline.Codes["notice"]))
	const filename = "internal/migrateapp/service.go"
	const anchor = `return &Error{Code: code, Message: message, Cause: cause}`
	for _, flow := range []struct{ name, assignment string }{
		{"multi", `candidate,slots[record(Error{Code:candidate,Message:message,Cause:cause})]="migrate_failed",1`},
		{"earlier", `slots[record(Error{Code:candidate,Message:message,Cause:cause})],candidate=1,"migrate_failed"`},
		{"split", `slots[record(Error{Code:candidate,Message:message,Cause:cause})]=1; candidate="migrate_failed"`},
	} {
		for _, value := range []struct{ name, expression, actual, diagnostic string }{
			{"registered", `"usage"`, "usage", ""},
			{"bad", `"assignment_unregistered"`, "assignment_unregistered", `unregistered code "assignment_unregistered"`},
			{"computed", `fmt.Sprint("assignment", "_unregistered")`, "assignment_unregistered", "cannot prove code expression; use a constant or a supported assignment flow"},
		} {
			t.Run(flow.name+"/"+value.name, func(t *testing.T) {
				changed := append([]codecensus.Source(nil), sources...)
				var content string
				line := 0
				for i, s := range changed {
					if s.Filename != filename {
						continue
					}
					if strings.Count(string(s.Content), anchor) != 1 {
						t.Fatal("namedError return anchor moved")
					}
					replacement := `_ = code
 candidate:=` + value.expression + `
 result:=&Error{Code:"migrate_failed",Message:message,Cause:cause}
 record:=func(e Error)int{result=&e;return 0}
 slots:=[]int{0}
 ` + flow.assignment + `
 _=candidate
 return result`
					content = "// Source movement must shift diagnostics.\n\n" + strings.Replace(string(s.Content), anchor, replacement, 1)
					offset := strings.Index(content, "candidate:="+value.expression) + len("candidate:=")
					line = 1 + strings.Count(content[:offset], "\n")
					changed[i].Content = []byte(content)
				}
				if line == 0 {
					t.Fatal("production namedError source missing")
				}
				// Write runtime inputs before analysis assertions, so a red census still
				// executes its causal oracle and preserves the working controls.
				temp := t.TempDir()
				service := filepath.Join(temp, "service.go")
				testfile := filepath.Join(temp, "boundary_test.go")
				runtimeTest := fmt.Sprintf(`package migrateapp
import("testing";"github.com/jbaruch/agentic-context-registry/internal/cli")
func TestAssignmentBoundaryRuntime(t *testing.T){
 got,ok:=migrateCLIError(namedError("migrate_failed","boundary",nil)).(*cli.Error)
 if !ok || got.Code!=%q {t.Fatalf("CLI error=%%#v",got)}
}
`, value.actual)
				overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{
					filepath.Join(root, filename): service,
					filepath.Join(root, "internal/migrateapp/assignment_boundary_runtime_test.go"): testfile,
				}})
				if err != nil {
					t.Fatal(err)
				}
				for name, data := range map[string][]byte{service: []byte(content), testfile: []byte(runtimeTest), filepath.Join(temp, "overlay.json"): overlay} {
					if err := os.WriteFile(name, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command("go", "test", "-overlay", filepath.Join(temp, "overlay.json"), "-count=1", "-run", "^TestAssignmentBoundaryRuntime$", "-v", "./internal/migrateapp")
				cmd.Dir = root
				output, err := cmd.CombinedOutput()
				t.Logf("actual production runtime:\n%s", output)
				if err != nil {
					t.Fatalf("runtime: %v", err)
				}
				got, err := codecensus.Analyze(changed, imports, targets)
				if err != nil {
					t.Fatal(err)
				}
				if value.diagnostic == "" {
					if len(got.Diagnostics) != 0 || !slices.Contains(got.Codes["refusal"], value.actual) {
						t.Errorf("registered flow: %v", got)
					}
				} else {
					want := fmt.Sprintf("%s:%d: refusal: %s", filename, line, value.diagnostic)
					if len(got.Diagnostics) != 1 || got.Diagnostics[0].String() != want {
						t.Errorf("diagnostics=%v, want %s", got.Diagnostics, want)
					}
				}
				if !reflect.DeepEqual(got.Codes["notice"], baseline.Codes["notice"]) {
					t.Errorf("unrelated notice vocabulary changed: %v", got.Codes["notice"])
				}
			})
		}
	}
}

// These overlays exercise the four distinct graph boundaries through the real
// migration error conversion. None of the probe globals are production source.
func TestCodeCensusDependencyProductionBoundary(t *testing.T) {
	root := commandDocsRoot(t)
	sources, imports, err := codecensus.Repository(root)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "github.com/jbaruch/agentic-context-registry/"
	targets := []codecensus.Target{
		{Package: prefix + "internal/cli", Type: "Error", Field: "Code", Namespace: "refusal", Registered: cli.RefusalCodes, AllowEmpty: true},
		{Package: prefix + "internal/cli", Type: "Notice", Field: "Code", Namespace: "notice", Registered: append(append([]string(nil), cli.NoticeCodes...), cli.RefusalCodes...)},
	}
	baseline, err := codecensus.Analyze(sources, imports, targets)
	if err != nil || len(baseline.Diagnostics) != 0 {
		t.Fatalf("baseline: %v %v", baseline, err)
	}
	t.Logf("production files=%d refusals=%d notices=%d", len(sources), len(baseline.Codes["refusal"]), len(baseline.Codes["notice"]))
	const filename = "internal/migrateapp/service.go"
	const anchor = `return &Error{Code: code, Message: message, Cause: cause}`
	for _, flow := range []struct {
		name, body, appendix, extra, marker string
		escaped, unordered                  bool
	}{
		{name: "direct", body: `candidate:=VALUE; return &Error{Code:candidate,Message:message,Cause:cause}`, marker: "candidate:="},
		{name: "return", body: `result,_:=censusBuild(message,cause);return result`, appendix: `
func censusBuild(message string,cause error)(*Error,int){
 candidate:="migrate_failed"
 return &Error{Code:candidate,Message:message,Cause:cause},func()int{candidate=VALUE;return 0}()
}
`, marker: "candidate=", unordered: true},
		{name: "global", body: `censusMutate();return &Error{Code:censusGlobal,Message:message,Cause:cause}`, appendix: `
var censusGlobal="migrate_failed"
func censusMutate(){f:=func(){censusGlobal=VALUE};f()}
`, marker: "censusGlobal="},
		{name: "qualified", body: `cli.CensusGlobal=VALUE; return &Error{Code:cli.CensusEmit().Code,Message:message,Cause:cause}`, extra: `package cli
var CensusGlobal="migrate_failed"
func CensusEmit()*Error{return &Error{Code:CensusGlobal}}
`, marker: "cli.CensusGlobal="},
		{name: "escaped_plain", body: `candidate:="migrate_failed"
 alias:=&candidate
 candidate="migrate_failed"
 *alias=VALUE
 return &Error{Code:candidate,Message:message,Cause:cause}`, marker: "&candidate", escaped: true},
		{name: "escaped_tuple", body: `candidate:="migrate_failed"
 var alias *string
 candidate,alias="migrate_failed",&candidate
 *alias=VALUE
 return &Error{Code:candidate,Message:message,Cause:cause}`, marker: "&candidate", escaped: true},
		{name: "escaped_helper", body: `candidate:="migrate_failed"
 alias:=censusKeep(&candidate)
 candidate="migrate_failed"
 *alias=VALUE
 return &Error{Code:candidate,Message:message,Cause:cause}`, appendix: `
func censusKeep(p *string)*string{return p}
`, marker: "&candidate", escaped: true},
	} {
		for _, value := range []struct{ name, expression, actual string }{{"registered", `"usage"`, "usage"}, {"bad", `"dependency_unregistered"`, "dependency_unregistered"}, {"computed", `fmt.Sprint("dependency","_unregistered")`, "dependency_unregistered"}} {
			t.Run(flow.name+"/"+value.name, func(t *testing.T) {
				changed := append([]codecensus.Source(nil), sources...)
				var content string
				for i, s := range changed {
					if s.Filename == filename {
						if strings.Count(string(s.Content), anchor) != 1 {
							t.Fatal("namedError return anchor moved")
						}
						body := strings.ReplaceAll(flow.body, "VALUE", value.expression)
						appendix := strings.ReplaceAll(flow.appendix, "VALUE", value.expression)
						content = "// Source movement must shift diagnostics.\n\n" + strings.Replace(string(s.Content), anchor, "_ = Error{Code:code,Message:message,Cause:cause}\n"+body, 1) + appendix
						changed[i].Content = []byte(content)
					}
				}
				marker := flow.marker
				if !flow.escaped {
					marker += value.expression
				}
				if strings.Count(content, marker) != 1 {
					t.Fatalf("missing/ambiguous diagnostic marker %q", marker)
				}
				line := 1 + strings.Count(content[:strings.Index(content, marker)], "\n")
				temp := t.TempDir()
				replace := map[string]string{}
				write := func(rel, body string) {
					local := filepath.Join(temp, strings.ReplaceAll(rel, "/", "_"))
					if err := os.WriteFile(local, []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
					replace[filepath.Join(root, rel)] = local
				}
				write(filename, content)
				if flow.extra != "" {
					const extra = "internal/cli/census_dependency_probe.go"
					write(extra, flow.extra)
					changed = append(changed, codecensus.Source{Package: prefix + "internal/cli", Filename: extra, Content: []byte(flow.extra)})
				}
				write("internal/migrateapp/census_dependency_runtime_test.go", fmt.Sprintf(`package migrateapp
import("testing";"github.com/jbaruch/agentic-context-registry/internal/cli")
func TestDependencyBoundaryRuntime(t *testing.T){
 got,ok:=migrateCLIError(namedError("migrate_failed","boundary",nil)).(*cli.Error)
 if !ok {t.Fatalf("CLI error=%%#v",got)}
 t.Logf("OBSERVED=%%s",got.Code)
 if got.Code!=%q && !(%t && got.Code=="migrate_failed"){t.Fatalf("CLI code=%%q",got.Code)}
}
`, value.actual, flow.unordered))
				overlay, err := json.Marshal(map[string]any{"Replace": replace})
				if err != nil {
					t.Fatal(err)
				}
				overlayFile := filepath.Join(temp, "overlay.json")
				if err := os.WriteFile(overlayFile, overlay, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("go", "test", "-overlay", overlayFile, "-count=1", "-run", "^TestDependencyBoundaryRuntime$", "-v", "./internal/migrateapp")
				cmd.Dir = root
				output, err := cmd.CombinedOutput()
				t.Logf("actual production runtime:\n%s", output)
				if err != nil {
					t.Fatal(err)
				}
				got, err := codecensus.Analyze(changed, imports, targets)
				if err != nil {
					t.Fatal(err)
				}
				var wantDiagnostics []string
				if flow.escaped || value.name != "registered" {
					message := `unregistered code "dependency_unregistered"`
					if flow.escaped || value.name == "computed" {
						message = "cannot prove code expression; use a constant or a supported assignment flow"
					}
					wantDiagnostics = []string{fmt.Sprintf("%s:%d: refusal: %s", filename, line, message)}
				}
				var diagnostics []string
				for _, d := range got.Diagnostics {
					diagnostics = append(diagnostics, d.String())
				}
				t.Logf("diagnostics=%v want=%v", diagnostics, wantDiagnostics)
				if !reflect.DeepEqual(diagnostics, wantDiagnostics) {
					t.Errorf("diagnostics=%v want=%v", diagnostics, wantDiagnostics)
				}
				// Exact real-registry sets prevent an unrelated registered seed from
				// substituting for the dependency checks above.
				wantCodes := append([]string(nil), baseline.Codes["refusal"]...)
				if !flow.escaped && value.name == "bad" {
					wantCodes = append(wantCodes, value.actual)
					slices.Sort(wantCodes)
				}
				if !reflect.DeepEqual(got.Codes["refusal"], wantCodes) {
					t.Errorf("refusals=%v want=%v", got.Codes["refusal"], wantCodes)
				}
				if !reflect.DeepEqual(got.Codes["notice"], baseline.Codes["notice"]) {
					t.Errorf("notice namespace changed: %v", got.Codes["notice"])
				}
			})
		}
	}
}
