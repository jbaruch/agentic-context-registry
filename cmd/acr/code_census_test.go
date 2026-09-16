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
