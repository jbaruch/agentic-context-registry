package producerconvert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// pythonTestCheck mirrors the exact embedded invocation in validateProposal.
func pythonTestCheck(t *testing.T, stdin string) (string, error) {
	t.Helper()
	command := exec.Command("python3", "-I", "-S", "-c", pythonTestChecks)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	command.Stdin = strings.NewReader(stdin)
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestPythonTestCheckerExecutionContract(t *testing.T) {
	const original = "def fail(message):\n    raise AssertionError(message)\n\ndef test_ok():\n    assert 1 == 1\n    if False:\n        fail('never')\n\ntest_ok()\n"
	request := func(after string) string {
		data, err := json.Marshal(map[string]string{"before": original, "after": after})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if output, err := pythonTestCheck(t, request(original)); err != nil || output != "" {
		t.Fatalf("rejected unchanged tests: %v %s", err, output)
	}
	// Parse only: a proposal that would exit or raise if executed still passes.
	inert := "import sys\nsys.exit(99)\nraise RuntimeError('EXECUTED_PROPOSAL')\n" + original
	if output, err := pythonTestCheck(t, request(inert)); err != nil || output != "" {
		t.Fatalf("executed or rejected the proposed program: %v %s", err, output)
	}
	for name, tc := range map[string]struct{ stdin, reason string }{
		"removed test":          {request("def fail(message):\n    raise AssertionError(message)\n"), "original test function removed: test_ok"},
		"assertion loss":        {request("def fail(message):\n    raise AssertionError(message)\n\ndef test_ok():\n    pass\n\ntest_ok()\n"), "original assertion/failure checks removed from test_ok"},
		"changed collector":     {request(strings.Replace(original, "raise AssertionError(message)", "print(message)", 1)), "test failure collector must retain its behavior"},
		"removed invocation":    {request(strings.TrimSuffix(original, "test_ok()\n")), "test invocation/registration removed: test_ok"},
		"invalid proposed code": {request("def test_ok(:\n"), "SyntaxError"},
		"malformed request":     {`{"before": "def test_ok(): pass"`, "JSONDecodeError"},
		"missing field":         {`{"before": "def test_ok(): pass"}`, "KeyError"},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := pythonTestCheck(t, tc.stdin)
			if err == nil || !strings.Contains(output, tc.reason) {
				t.Fatalf("err=%v output=%s", err, output)
			}
		})
	}
}

func TestPythonTestCheckerIsImportSafe(t *testing.T) {
	directory := t.TempDir()
	put(t, directory, "acr_check_tests.py", pythonTestChecks, 0o644)
	const probe = `import ast, importlib.util, io, sys
sys.dont_write_bytecode = True
class Unreadable(io.TextIOBase):
    def readable(self):
        return True
    def read(self, size=-1):
        raise RuntimeError('IMPORT_READ_STDIN')
    def readline(self, size=-1):
        raise RuntimeError('IMPORT_READ_STDIN')
sys.stdin = Unreadable()
captured = io.StringIO()
sys.stdout = sys.stderr = captured
spec = importlib.util.spec_from_file_location('acr_check_tests', sys.argv[1])
module = importlib.util.module_from_spec(spec)
try:
    spec.loader.exec_module(module)
finally:
    sys.stdout, sys.stderr = sys.__stdout__, sys.__stderr__
if captured.getvalue():
    raise SystemExit('import wrote output: ' + captured.getvalue())
tree = ast.parse("def test_a():\n    assert 1\n    self.assertTrue(1)\n    fail('x')\ntest_a()\n")
definitions = list(module.definitions(tree).values())
if len(definitions) != 1 or definitions[0].name != 'test_a' or module.assertions(definitions[0]) != 3:
    raise SystemExit('helpers unusable after import')
kept = "def test_a():\n    assert 1\ntest_a()\n"
module.check(kept, kept)
try:
    module.check(kept, "def test_a():\n    pass\ntest_a()\n")
except ValueError as error:
    if 'assertion' not in str(error):
        raise
else:
    raise SystemExit('helper accepted assertion loss')
print('IMPORT_OK')
`
	command := exec.Command("python3", "-I", "-S", "-B", "-c", probe, filepath.Join(directory, "acr_check_tests.py"))
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "IMPORT_OK" {
		t.Fatalf("import probe: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(directory, "__pycache__")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import wrote bytecode: %v", err)
	}
}

func TestCorrection14PythonObligations(t *testing.T) {
	cases := []struct {
		name, path, before, after string
		command                   []string
	}{
		{"testLogin-reduced", "tests/test_checks.py", "import unittest\nclass Checks(unittest.TestCase):\n def testLogin(self):\n  self.fail('independent failure')\nif __name__ == '__main__':\n unittest.main()\n", "import unittest\nclass Checks(unittest.TestCase):\n def testLogin(self):\n  pass\nif __name__ == '__main__':\n unittest.main()\n", []string{"python3", "-I", "-S"}},
		{"testLogin-registration", "tests/test_checks.py", "import unittest\ndef testLogin():\n raise AssertionError('independent failure')\nsuite=unittest.TestSuite([unittest.FunctionTestCase(testLogin)])\nresult=unittest.TextTestRunner().run(suite)\nraise SystemExit(not result.wasSuccessful())\n", "import unittest\ndef testLogin():\n raise AssertionError('independent failure')\nsuite=unittest.TestSuite([])\nresult=unittest.TextTestRunner().run(suite)\nraise SystemExit(not result.wasSuccessful())\n", []string{"python3", "-I", "-S"}},
		{"test_login-reduced", "tests/test_checks.py", "import unittest\nclass Checks(unittest.TestCase):\n def test_login(self):\n  self.fail('independent failure')\nif __name__ == '__main__':\n unittest.main()\n", "import unittest\nclass Checks(unittest.TestCase):\n def test_login(self):\n  pass\nif __name__ == '__main__':\n unittest.main()\n", []string{"python3", "-I", "-S"}},
		{"test_login-registration", "tests/test_checks.py", "import unittest\ndef test_login():\n raise AssertionError('independent failure')\nsuite=unittest.TestSuite([unittest.FunctionTestCase(test_login)])\nresult=unittest.TextTestRunner().run(suite)\nraise SystemExit(not result.wasSuccessful())\n", "import unittest\ndef test_login():\n raise AssertionError('independent failure')\nsuite=unittest.TestSuite([])\nresult=unittest.TextTestRunner().run(suite)\nraise SystemExit(not result.wasSuccessful())\n", []string{"python3", "-I", "-S"}},
	}
	originalCases := append(cases[:0:0], cases...)
	for _, c := range originalCases {
		c.name += "-preserved"
		c.after = c.before
		cases = append(cases, c)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, opts, p := semanticFixture(t)
			// A source-owned installed reference makes this test editable for migration.
			// Keep its comment as ordinary context after removing the obsolete root.
			prefix := "# .tessl/plugins/upstream/orbit/skills/check/check.sh\n"
			suffix := "# migrated helper location\n"
			if strings.HasPrefix(c.name, "go") {
				prefix = "// .tessl/plugins/upstream/orbit/skills/check/check.sh\n"
				suffix = "// migrated helper location\n"
			}
			c.before = prefix + c.before
			c.after = suffix + c.after
			put(t, root, c.path, c.before, 0644)
			run := func() ([]byte, error) {
				args := append(append([]string{}, c.command[1:]...), filepath.Join(root, c.path))
				cmd := exec.Command(c.command[0], args...)
				return cmd.CombinedOutput()
			}
			beforeOut, beforeErr := run()
			if beforeErr == nil {
				t.Fatalf("fixture must fail before conversion: %s", beforeOut)
			}
			p.Edits = append(p.Edits, proposedEdit{Path: c.path, BeforeDigest: digest([]byte(c.before)), Action: "replace", Content: c.after})
			before := treeAt(t, root)
			calls := 0
			plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil })
			t.Logf("accepted=%t calls=%d error=%v original_failure=%v original_output=%s", err == nil, calls, err, beforeErr, beforeOut)
			if !reflect.DeepEqual(before, treeAt(t, root)) {
				t.Fatal("planning mutated fixture")
			}
			wantRefusal := !strings.HasSuffix(c.name, "-preserved")
			if wantRefusal {
				if err == nil {
					t.Fatal("destructive test edit accepted")
				}
				if calls != 3 {
					t.Fatalf("retry count %d", calls)
				}
				assertCorrection14NoResidue(t, root)
				return
			}
			if err != nil || calls != 1 {
				t.Fatalf("harmless adaptation: %v calls=%d", err, calls)
			}
			if err == nil {
				r, e := plan.Apply()
				if e != nil || !r.Wrote {
					t.Fatalf("Apply %v %+v", e, r)
				}
				if read(t, root, c.path) != c.after {
					t.Fatal("wrong applied check")
				}
				assertCorrection14Applied(t, root, plan)
				out, e := run()
				t.Logf("applied test exit=%v output=%s", e, out)
				if strings.HasSuffix(c.name, "-preserved") {
					if e == nil {
						t.Fatal("preserved failing test became success")
					}
				} else if e != nil {
					t.Fatalf("expected no-op observed to pass: %s", out)
				}
				r2, e := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
					t.Fatal("provider called on inert rerun")
					return p, AgentRun{}, nil
				})
				if e != nil || !r2.Report.Current {
					t.Fatalf("rerun %v %+v", e, r2.Report)
				}
			}
		})
	}
}

// The complete two-class discriminator and metadata adaptation come from the
// correction14 review's name-collision-v3 public CLI probe and judge15 ruling.
func TestCorrection15DistinctPythonTests(t *testing.T) {
	testDistinctPythonTests(t, []string{"reference-only", "delete-first-class", "remove-first-failure"}, "self.fail(version)")
}

// Adopt the complete reviewer15 counterexample and judge16 nested-owner controls.
// These execute only controlled fixtures; proposal validation remains parse-only.
func TestCorrection16NestedPythonChecks(t *testing.T) {
	testDistinctPythonTests(t, []string{"nested-helper-control", "nested-helper-compensation", "called-helper-control", "called-helper-loss", "called-helper-parent-compensation"}, "self.fail(version)")
}

// Reuse reviewer16's complete two-class explicit-raise discriminator and the
// accepted metadata adaptation, including the executed called-helper control.
func TestCorrection17ExplicitRaises(t *testing.T) {
	testDistinctPythonTests(t, []string{"reference-only", "remove-first-failure", "nested-helper-control", "nested-helper-compensation", "called-helper-control", "called-helper-loss", "called-helper-parent-compensation"}, "raise AssertionError(version)")
}

func testDistinctPythonTests(t *testing.T, changes []string, failure string) {
	t.Helper()
	for _, name := range []string{"testLogin", "test_login"} {
		for _, change := range changes {
			for _, dry := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/dry=%t", name, change, dry), func(t *testing.T) {
					root := t.TempDir()
					stageCheck := correctionStageCheck(t)
					defer stageCheck()
					put(t, root, ".tessl-plugin/plugin.json", "{\"name\":\"origin/demo\",\"version\":\"2.3.4\",\"skills\":[\"skills/check\"]}\n", 0o644)
					put(t, root, "skills/check/SKILL.md", "# Check\nRead ordinary data.\n", 0o644)
					preamble := "import unittest, json\nfrom pathlib import Path\nmetadata = Path(__file__).resolve().parents[1] / '.tessl-plugin/plugin.json'\nversion = json.loads(metadata.read_text())['version']\n"
					first := "class AFailing(unittest.TestCase):\n    def " + name + "(self):\n        " + failure + "\n\n"
					if strings.HasPrefix(change, "nested-helper-") {
						first = "class AFailing(unittest.TestCase):\n    def " + name + "(self):\n        def diagnostic():\n            pass\n        " + failure + "\n\n"
					} else if strings.HasPrefix(change, "called-helper-") {
						first = "class AFailing(unittest.TestCase):\n    def " + name + "(self):\n        def diagnostic():\n            " + failure + "\n        self.assertTrue(version)\n        diagnostic()\n\n"
					}
					last := "class BPassing(unittest.TestCase):\n    def " + name + "(self):\n        pass\n\n"
					end := "if __name__ == '__main__':\n    unittest.main()\n"
					original := preamble + first + last + end
					candidate := original
					switch change {
					case "delete-first-class":
						candidate = preamble + last + end
					case "remove-first-failure":
						candidate = strings.Replace(original, failure, "pass", 1)
					case "nested-helper-compensation":
						candidate = strings.Replace(original, "            pass\n        "+failure, "            "+failure, 1)
					case "called-helper-loss":
						candidate = strings.Replace(original, failure, "pass", 1)
					case "called-helper-parent-compensation":
						candidate = strings.Replace(original, "            "+failure, "            pass", 1)
						candidate = strings.Replace(candidate, "        diagnostic()", "        "+failure+"\n        diagnostic()", 1)
					}
					candidate = strings.Replace(candidate, "'.tessl-plugin/plugin.json'", "'skills' / 'check' / '.acr-package.json'", 1)
					const path = "tests/test_cases.py"
					put(t, root, path, original, 0o644)
					run := func() {
						t.Helper()
						command := exec.Command("python3", "-B", filepath.Join(root, path))
						command.Dir = root
						output, err := command.CombinedOutput()
						var failure *exec.ExitError
						if !errors.As(err, &failure) || failure.ExitCode() != 1 || !strings.Contains(string(output), "Ran 2 tests") || !strings.Contains(string(output), "FAILED (failures=1)") || !strings.Contains(string(output), "AssertionError: 2.3.4") {
							t.Fatalf("expected two tests and original version failure: %v\n%s", err, output)
						}
						t.Logf("controlled fixture exit=1, two tests, one failure: %s", output)
					}
					run()
					before := correction12Inventory(t, root)
					opts := Options{PackageRoot: root, Repository: "https://github.com/destination/demo", Agent: "claude", DryRun: dry}
					p := proposal{Edits: []proposedEdit{{Path: path, BeforeDigest: digest([]byte(original)), Action: "replace", Content: candidate}}}
					calls := 0
					plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
						calls++
						return p, AgentRun{}, nil
					})
					if !reflect.DeepEqual(before, correction12Inventory(t, root)) {
						t.Fatal("preparation changed physical input")
					}
					if change != "reference-only" && !strings.HasSuffix(change, "-control") {
						reason := "original test function removed: AFailing." + name
						if change != "delete-first-class" {
							reason = "original assertion/failure checks removed from AFailing." + name
							if strings.HasPrefix(change, "called-helper-") {
								reason += ".diagnostic"
							}
						}
						if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), reason) || calls != 3 {
							t.Fatalf("expected owner-qualified refusal, calls=%d: %v", calls, err)
						}
						assertCorrection14NoResidue(t, root)
						return
					}
					if err != nil || calls != 1 || len(plan.Report.Changes) != 5 {
						t.Fatalf("metadata-only adaptation: %v calls=%d changes=%d", err, calls, len(plan.Report.Changes))
					}
					if dry {
						assertCorrection14NoResidue(t, root)
						return
					}
					expected := tree{}
					for name, state := range before {
						expected[name] = state
					}
					for _, change := range plan.Report.Changes {
						if change.Operation == "remove" {
							delete(expected, change.Path)
						} else {
							expected[change.Path] = fileState{Content: []byte(change.After), Digest: digest([]byte(change.After)), Mode: change.AfterMode}
						}
					}
					if report, err := plan.Apply(); err != nil || !report.Wrote {
						t.Fatalf("Apply: %v %+v", err, report)
					}
					correction12Unchanged(t, root, expected)
					assertCorrection14Applied(t, root, plan)
					if read(t, root, path) != candidate {
						t.Fatal("applied program differs from proposal")
					}
					info, err := os.Stat(filepath.Join(root, ReceiptPath))
					if err != nil || info.Mode().Perm() != 0o600 {
						t.Fatalf("receipt mode: %v %v", info, err)
					}
					run()
					after := correction12Inventory(t, root)
					current, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
						t.Fatal("provider called on current rerun")
						return proposal{}, AgentRun{}, nil
					})
					if err != nil || !current.Report.Current || !reflect.DeepEqual(after, correction12Inventory(t, root)) {
						t.Fatalf("current rerun: %v %+v", err, current.Report)
					}
				})
			}
		}
	}
}

func TestPythonTestCheckerDistinctOwnersAndOccurrences(t *testing.T) {
	for _, tc := range []struct{ name, before, after, reason string }{
		{"reverse-delete-A", "class B:\n def testCase(self): pass\nclass A:\n def testCase(self): self.fail('x')\n", "class B:\n def testCase(self): pass\n", "original test function removed: A.testCase"},
		{"delete-B", "class A:\n def testCase(self): self.fail('x')\nclass B:\n def testCase(self): pass\n", "class A:\n def testCase(self): self.fail('x')\n", "original test function removed: B.testCase"},
		{"other-owner-cannot-compensate", "class A:\n def testCase(self): self.fail('x')\nclass B:\n def testCase(self): pass\n", "class A:\n def testCase(self): pass\nclass B:\n def testCase(self): self.fail('x')\n", "original assertion/failure checks removed from A.testCase"},
		{"nested-owner", "def outer():\n class A:\n  async def testCase(self): self.fail('x')\n class B:\n  async def testCase(self): pass\n", "def outer():\n class B:\n  async def testCase(self): pass\n", "original test function removed: outer.A.testCase"},
		{"conditional-owner", "if True:\n class A:\n  def testCase(self): pass\nclass B:\n def testCase(self): pass\n", "class B:\n def testCase(self): pass\n", "original test function removed: A.testCase"},
		{"repeated-test", "def testCase(): pass\ndef testCase(): pass\n", "def testCase(): pass\n", "original test function removed: testCase[2]"},
		{"repeated-owner", "class A:\n def testCase(self): pass\nclass A:\n def testCase(self): pass\n", "class A:\n def testCase(self): pass\n", "original test function removed: A[2].testCase"},
		{"repeated-async", "async def testCase(): pass\nasync def testCase(): pass\n", "async def testCase(): pass\n", "original test function removed: testCase[2]"},
		{"collector-owner", "class A:\n def fail(self, message: str) -> None: raise AssertionError(message)\nclass B:\n def fail(self, message): pass\n", "class A:\n def fail(self, message: int) -> None: raise AssertionError(message)\nclass B:\n def fail(self, message): pass\n", "test failure collector must retain its behavior"},
		{"collector-occurrence", "def fail(message): raise AssertionError(message)\ndef fail(message): pass\n", "def fail(message): pass\ndef fail(message): pass\n", "test failure collector must retain its behavior"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(after string, reason string) {
				t.Helper()
				data, err := json.Marshal(map[string]string{"before": tc.before, "after": after})
				if err != nil {
					t.Fatal(err)
				}
				output, err := pythonTestCheck(t, string(data))
				if reason == "" {
					if err != nil || output != "" {
						t.Fatalf("stable identity refused: %v %s", err, output)
					}
				} else if err == nil || !strings.Contains(output, reason) {
					t.Fatalf("expected %s: %v %s", reason, err, output)
				}
			}
			check(tc.after, tc.reason)
			check(tc.before, "")
			// Nondefinitions and differently named definitions cannot shift identities.
			check("# harmless comment\nmetadata = 'adapted'\ndef unrelated(): pass\n"+tc.before, "")
		})
	}
}

func TestPythonTestCheckerNestedCheckOwners(t *testing.T) {
	for _, tc := range []struct{ name, before, after, owner string }{
		{"parent-to-sync-helper", "def testCase():\n def helper(): pass\n self.fail('x')\n", "def testCase():\n def helper(): self.fail('x')\n", "testCase"},
		{"parent-to-async-helper", "async def testCase():\n async def helper(): pass\n fail('x')\n", "async def testCase():\n async def helper(): fail('x')\n", "testCase"},
		{"parent-to-class-body", "def testCase():\n class Helper: pass\n assert 1\n", "def testCase():\n class Helper: assert 1\n", "testCase"},
		{"parent-to-class-method", "def testCase():\n class Helper:\n  def run(self): pass\n self.assertEqual(1, 1)\n", "def testCase():\n class Helper:\n  def run(self): self.assertEqual(1, 1)\n", "testCase"},
		{"nested-check-reduction", "def testCase():\n def helper():\n  assert 1\n  self.fail('x')\n assert 2\n", "def testCase():\n def helper(): assert 1\n assert 2\n", "testCase.helper"},
		{"nested-owner-removal", "def testCase():\n def helper(): self.fail('x')\n assert 2\n", "def testCase():\n assert 2\n self.fail('x')\n", "testCase.helper"},
		{"nested-to-parent", "def testCase():\n def helper(): self.fail('x')\n assert 2\n", "def testCase():\n def helper(): pass\n assert 2\n self.fail('x')\n", "testCase.helper"},
		{"nested-to-sibling", "def testCase():\n def helper(): self.fail('x')\n def other(): pass\n assert 2\n", "def testCase():\n def helper(): pass\n def other(): self.fail('x')\n assert 2\n", "testCase.helper"},
		{"nested-to-child", "def testCase():\n def helper():\n  def child(): pass\n  fail('x')\n assert 2\n", "def testCase():\n def helper():\n  def child(): fail('x')\n assert 2\n", "testCase.helper"},
		{"async-nested-loss", "def testCase():\n async def helper(): self.fail('x')\n assert 2\n", "def testCase():\n async def helper(): pass\n assert 2\n self.fail('x')\n", "testCase.helper"},
		{"class-check-removal", "def testCase():\n class Helper: assert 1\n assert 2\n", "def testCase():\n assert 1\n assert 2\n", "testCase.Helper"},
		{"class-to-method", "def testCase():\n class Helper:\n  assert 1\n  def run(self): pass\n", "def testCase():\n class Helper:\n  def run(self): assert 1\n", "testCase.Helper"},
		{"class-method-to-child", "def testCase():\n class Helper:\n  def run(self):\n   def child(): pass\n   self.assertTrue(1)\n", "def testCase():\n class Helper:\n  def run(self):\n   def child(): self.assertTrue(1)\n", "testCase.Helper.run"},
		{"method-to-other-class", "def testCase():\n class A:\n  def run(self): self.fail('x')\n class B:\n  def run(self): pass\n", "def testCase():\n class A:\n  def run(self): pass\n class B:\n  def run(self): self.fail('x')\n", "testCase.A.run"},
		{"first-helper-occurrence", "def testCase():\n def helper(): assert 1\n def helper(): pass\n", "def testCase():\n def helper(): pass\n def helper(): assert 1\n", "testCase.helper"},
		{"second-helper-occurrence", "def testCase():\n def helper(): pass\n def helper(): assert 1\n", "def testCase():\n def helper(): assert 1\n def helper(): pass\n", "testCase.helper[2]"},
		{"empty-helper-removal", "def testCase():\n def helper(): pass\n assert 1\n", "def testCase():\n assert 1\n", ""},
		{"unrelated-helper-not-frozen", "def helper(): assert 1\ndef testCase(): assert 2\n", "def testCase(): assert 2\n", ""},
		{"ordinary-statements-retain-owner", "def testCase():\n if True:\n  assert 1\n try:\n  self.assertTrue(2)\n except ValueError:\n  fail('x')\n for item in []:\n  self.fail('y')\n", "def testCase():\n assert 1\n self.assertTrue(2)\n fail('x')\n self.fail('y')\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(after, owner string) {
				t.Helper()
				data, err := json.Marshal(map[string]string{"before": tc.before, "after": after})
				if err != nil {
					t.Fatal(err)
				}
				output, err := pythonTestCheck(t, string(data))
				if owner == "" {
					if err != nil || output != "" {
						t.Fatalf("valid owner preservation refused: %v %s", err, output)
					}
				} else if err == nil || !strings.Contains(output, "original assertion/failure checks removed from "+owner+"\n") {
					t.Fatalf("expected check loss at %s: %v %s", owner, err, output)
				}
			}
			check(tc.after, tc.owner)
			check(tc.before, "")
			check("# metadata adaptation\nversion = '2.3.4'\ndef unrelated(): pass\n"+tc.before, "")
		})
	}
}

func TestPythonTestCheckerRaiseStatements(t *testing.T) {
	for _, tc := range []struct{ name, statement string }{
		{"class", "raise AssertionError"},
		{"instance", "raise AssertionError(version)"},
		{"qualified", "raise builtins.AssertionError(version)"},
		{"variable", "raise problem"},
		{"from-none", "raise AssertionError(version) from None"},
		{"from-cause", "raise AssertionError(version) from cause"},
		{"traceback", "raise problem.with_traceback(traceback)"},
		{"bare", "try:\n  operation()\n except Exception:\n  raise"},
		{"other-exception", "raise RuntimeError(version)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const head = "def testCase():\n self.assertTrue(1)\n "
			before := head + tc.statement + "\n"
			removed := "pass"
			if tc.name == "bare" {
				removed = strings.Replace(tc.statement, "raise", "pass", 1)
			}
			checkPythonPreservation(t, before, head+removed+"\n", "testCase")
			checkPythonPreservation(t, before, strings.ReplaceAll(before, "version", "adapted_version"), "")
			// Each explicit raise contributes one check, independent of the
			// constructor, cause, traceback or lack of an exception operand.
			checkPythonPreservation(t, before, head+"raise AssertionError(version)\n", "")
		})
	}
}

func TestPythonTestCheckerRaiseOwners(t *testing.T) {
	for _, tc := range []struct{ name, before, after, owner string }{
		{"parent-to-child", "def testCase():\n def helper(): pass\n assert 1\n raise problem\n", "def testCase():\n def helper(): raise problem\n assert 1\n", "testCase"},
		{"nested-reduction", "def testCase():\n def helper():\n  raise problem\n  raise other\n assert 1\n", "def testCase():\n def helper(): raise problem\n assert 1\n", "testCase.helper"},
		{"nested-removal", "def testCase():\n def helper(): raise problem\n assert 1\n", "def testCase():\n assert 1\n raise problem\n", "testCase.helper"},
		{"nested-to-parent", "def testCase():\n def helper(): raise problem\n assert 1\n", "def testCase():\n def helper(): pass\n assert 1\n raise problem\n", "testCase.helper"},
		{"nested-to-sibling", "def testCase():\n def helper(): raise problem\n def sibling(): assert 1\n assert 2\n", "def testCase():\n def helper(): pass\n def sibling():\n  assert 1\n  raise problem\n assert 2\n", "testCase.helper"},
		{"async-owner", "async def testCase():\n async def helper(): raise problem\n assert 1\n", "async def testCase():\n async def helper(): pass\n assert 1\n raise problem\n", "testCase.helper"},
		{"class-body-to-method", "def testCase():\n class Helper:\n  raise problem\n  def method(self): assert 1\n assert 2\n", "def testCase():\n class Helper:\n  def method(self):\n   assert 1\n   raise problem\n assert 2\n", "testCase.Helper"},
		{"class-method-to-other-class", "def testCase():\n class A:\n  def method(self): raise problem\n class B:\n  def method(self): assert 1\n", "def testCase():\n class A:\n  def method(self): pass\n class B:\n  def method(self):\n   assert 1\n   raise problem\n", "testCase.A.method"},
		{"deeper-owner", "def testCase():\n class Helper:\n  def method(self):\n   async def nested(): raise problem\n   assert 1\n", "def testCase():\n class Helper:\n  def method(self):\n   async def nested(): pass\n   assert 1\n   raise problem\n", "testCase.Helper.method.nested"},
		{"repeated-first", "def testCase(): raise problem\ndef testCase(): assert 1\n", "def testCase(): pass\ndef testCase():\n assert 1\n raise problem\n", "testCase"},
		{"repeated-second", "def testCase(): assert 1\ndef testCase(): raise problem\n", "def testCase():\n assert 1\n raise problem\ndef testCase(): pass\n", "testCase[2]"},
		{"string-comment-not-raise", "def testCase():\n # raise problem\n message = 'raise problem'\n assert 1\n", "def testCase(): assert 1\n", ""},
		{"empty-helper-freedom", "def testCase():\n def helper(): pass\n raise problem\n", "def testCase(): raise problem\n", ""},
		{"outside-footprint", "def ordinary(): raise problem\ndef testCase(): assert 1\n", "def testCase(): assert 1\n", ""},
		{"ordinary-statements", "def testCase():\n if True:\n  raise problem\n try:\n  operation()\n except Exception:\n  raise\n for item in []:\n  raise other\n", "def testCase():\n raise problem\n try:\n  operation()\n except Exception:\n  raise\n raise other\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkPythonPreservation(t, tc.before, tc.after, tc.owner)
			checkPythonPreservation(t, tc.before, tc.before, "")
			checkPythonPreservation(t, tc.before, "# harmless metadata preamble\nversion = '2.3.4'\ndef unrelated(): pass\n"+tc.before, "")
		})
	}
}

func checkPythonPreservation(t *testing.T, before, after, owner string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"before": before, "after": after})
	if err != nil {
		t.Fatal(err)
	}
	output, err := pythonTestCheck(t, string(data))
	if owner == "" {
		if err != nil || output != "" {
			t.Fatalf("valid check preservation refused: %v %s", err, output)
		}
	} else if err == nil || !strings.Contains(output, "original assertion/failure checks removed from "+owner+"\n") {
		t.Fatalf("expected explicit failure loss at %s: %v %s", owner, err, output)
	}
}

// pythonRequest encodes one checker request exactly as validateProposal does.
func pythonRequest(t *testing.T, before, after string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"before": before, "after": after})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// pythonRefusal asserts the checker refused a proposal: a non-zero exit from a
// checker ValueError, never a syntax or request error, while the unchanged
// program and the proposal itself both still parse under the production checks.
func pythonRefusal(t *testing.T, before, after string) string {
	t.Helper()
	if output, err := pythonTestCheck(t, pythonRequest(t, before, before)); err != nil || output != "" {
		t.Fatalf("unchanged program refused: %v %s", err, output)
	}
	if err := syntaxCheck(context.Background(), "tests/test_proposal.py", []byte(after)); err != nil {
		t.Fatalf("proposal must be a valid program for its refusal to count: %v", err)
	}
	output, err := pythonTestCheck(t, pythonRequest(t, before, after))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("bypass accepted: exit=%v output=%s", err, output)
	}
	if !strings.Contains(output, "ValueError: ") || strings.Contains(output, "SyntaxError") || strings.Contains(output, "KeyError") {
		t.Fatalf("refusal is not a checker finding: exit=%d output=%s", exit.ExitCode(), output)
	}
	t.Logf("refused exit=%d: %s", exit.ExitCode(), strings.TrimSpace(output[strings.LastIndex(output, "ValueError: "):]))
	return output
}

const bypassCollector = "def fail(message):\n    raise AssertionError(message)\n\n"
const bypassTest = "def test_thing():\n    assert 1 == 1\n    if 2 != 2:\n        fail(\"bad\")\n\ntest_thing()\n"
const bypassSuite = "import unittest\n\n" + bypassCollector + "class Suite(unittest.TestCase):\n    def test_thing(self):\n        assert 1 == 1\n        if 2 != 2:\n            fail(\"bad\")\n"

// The lead's reproduction: every footprint count survives while the test no
// longer runs. Each form is refused by what its decorator or statement names,
// so an imported alias, a re-exported module and a local shadow all count.
func TestPythonTestCheckerRefusesTestBypasses(t *testing.T) {
	plain := bypassCollector + bypassTest
	skipped := "import unittest\n" + bypassCollector + "@unittest.skip(\"flaky\")\n" + bypassTest
	for _, tc := range []struct{ name, before, after string }{
		{"from-unittest-import-skip", plain, "from unittest import skip\n" + bypassCollector + "@skip(\"migration\")\n" + bypassTest},
		{"import-unittest-skipIf", plain, "import unittest\n" + bypassCollector + "@unittest.skipIf(True, \"migration\")\n" + bypassTest},
		{"import-unittest-as-alias", plain, "import unittest as u\n" + bypassCollector + "@u.skipUnless(False, \"migration\")\n" + bypassTest},
		{"from-unittest-import-skip-as-s", plain, "from unittest import skip as s\n" + bypassCollector + "@s(\"migration\")\n" + bypassTest},
		{"from-unittest-case", plain, "from unittest.case import skipIf as when\n" + bypassCollector + "@when(True, \"migration\")\n" + bypassTest},
		{"expectedFailure-alias", plain, "from unittest import expectedFailure as ok\n" + bypassCollector + "@ok\n" + bypassTest},
		{"pytest-mark-skip", plain, "import pytest\n" + bypassCollector + "@pytest.mark.skip(reason=\"migration\")\n" + bypassTest},
		{"pytest-mark-skipif", plain, "import pytest\n" + bypassCollector + "@pytest.mark.skipif(True, reason=\"migration\")\n" + bypassTest},
		{"pytest-mark-alias-xfail", plain, "from pytest import mark as m\n" + bypassCollector + "@m.xfail\n" + bypassTest},
		{"pytest-fixture", plain, "import pytest\n" + bypassCollector + "@pytest.fixture\n" + bypassTest},
		{"local-decorator", plain, "def skip(function):\n    return lambda: None\n\n" + bypassCollector + "@skip\n" + bypassTest},
		{"dynamic-decorator", plain, "import unittest\n" + bypassCollector + "@getattr(unittest, \"skip\")(\"migration\")\n" + bypassTest},
		{"shadowed-import", plain, "import unittest\nfrom unittest import mock\nmock = unittest\n" + bypassCollector + "@mock.skip(\"migration\")\n" + bypassTest},
		{"rebound-import", plain, "import unittest\nimport unittest.mock as patch\n" + bypassCollector + "@patch(\"migration\")\n" + bypassTest},
		{"unresolvable-decorator", plain, "decorators = []\n" + bypassCollector + "@decorators[0]\n" + bypassTest},
		{"class-level-skip", bypassSuite, strings.Replace(bypassSuite, "class Suite", "@unittest.skip(\"migration\")\nclass Suite", 1)},
		{"enclosing-function-skip", "def outer():\n    " + strings.ReplaceAll(bypassTest, "\n", "\n    "), "from unittest import skip\n@skip(\"migration\")\ndef outer():\n    " + strings.ReplaceAll(bypassTest, "\n", "\n    ")},
		{"pytestmark", plain, "import pytest\npytestmark = pytest.mark.skip(reason=\"migration\")\n" + plain},
		{"pytestmark-list", plain, "import pytest\npytestmark = [pytest.mark.slow, pytest.mark.skipif(True, reason=\"migration\")]\n" + plain},
		{"pytestmark-augmented", "import pytest\npytestmark = [pytest.mark.slow]\n" + plain, "import pytest\npytestmark = [pytest.mark.slow]\npytestmark += [pytest.mark.skip]\n" + plain},
		{"changed-skip-condition", "import os\nimport unittest\n" + bypassCollector + "@unittest.skipIf(os.name == \"nt\", \"windows\")\n" + bypassTest, "import os\nimport unittest\n" + bypassCollector + "@unittest.skipIf(os.name != \"nt\", \"windows\")\n" + bypassTest},
		{"respelled-skip", skipped, "from unittest import skip\n" + bypassCollector + "@skip(\"flaky\")\n" + bypassTest},
		{"removed-decorator", skipped, "import unittest\n" + plain},
		{"reordered-decorators", "import unittest\nfrom unittest import mock\n" + bypassCollector + "@mock.patch(\"os.getcwd\")\n@unittest.skipIf(False, \"never\")\n" + bypassTest, "import unittest\nfrom unittest import mock\n" + bypassCollector + "@unittest.skipIf(False, \"never\")\n@mock.patch(\"os.getcwd\")\n" + bypassTest},
		{"patch-target-becomes-skip", "from unittest import mock\n" + bypassCollector + "@mock.patch(\"os.getcwd\")\n" + bypassTest, "from unittest import mock, skip\n" + bypassCollector + "@skip(\"migration\")\n" + bypassTest},
		{"self-skipTest", bypassSuite, strings.Replace(bypassSuite, "        assert 1 == 1\n", "        self.skipTest(\"migration\")\n        assert 1 == 1\n", 1)},
		{"setUp-skipTest", bypassSuite, strings.Replace(bypassSuite, "    def test_thing", "    def setUp(self):\n        self.skipTest(\"migration\")\n\n    def test_thing", 1)},
		{"raise-SkipTest", plain, "from unittest import SkipTest\n" + strings.Replace(plain, "    assert 1 == 1\n", "    raise SkipTest(\"migration\")\n    assert 1 == 1\n", 1)},
		{"raise-case-SkipTest-alias", plain, "from unittest.case import SkipTest as Later\n" + strings.Replace(plain, "    assert 1 == 1\n", "    raise Later\n    assert 1 == 1\n", 1)},
		{"pytest-skip-call", plain, "import pytest\n" + strings.Replace(plain, "    assert 1 == 1\n", "    pytest.skip(\"migration\")\n    assert 1 == 1\n", 1)},
		{"pytest-importorskip", plain, "from pytest import importorskip\n" + strings.Replace(plain, "    assert 1 == 1\n", "    importorskip(\"acr_missing\")\n    assert 1 == 1\n", 1)},
		{"module-level-skip", plain, "import pytest\npytest.skip(\"migration\", allow_module_level=True)\n" + plain},
		{"helper-skip", plain, "import unittest\n" + strings.Replace(plain, "def test_thing():\n", "def helper():\n    raise unittest.SkipTest(\"migration\")\n\ndef test_thing():\n    helper()\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pythonRefusal(t, tc.before, tc.after)
		})
	}
}

// Decorators that name the migrated source may adapt their arguments; every
// unchanged decorator, skip and mark is retained as ordinary test content.
func TestPythonTestCheckerRetainsAdaptedDecorators(t *testing.T) {
	plain := bypassCollector + bypassTest
	for _, tc := range []struct{ name, before, after string }{
		{"unittest-mock-patch", "from unittest import mock\n" + bypassCollector + "@mock.patch(\"tessl.client\")\n" + bypassTest, "from unittest import mock\n" + bypassCollector + "@mock.patch(\"acr.client\")\n" + bypassTest},
		{"imported-patch", "from unittest.mock import patch\n" + bypassCollector + "@patch(\"tessl.client\")\n" + bypassTest, "from unittest.mock import patch\n" + bypassCollector + "@patch(\"acr.client\", autospec=True)\n" + bypassTest},
		{"patch-object", "from unittest.mock import patch as p\n" + bypassCollector + "@p.object(p, \"tessl\")\n" + bypassTest, "from unittest.mock import patch as p\n" + bypassCollector + "@p.object(p, \"acr\")\n" + bypassTest},
		{"standalone-mock", "import mock\n" + bypassCollector + "@mock.patch.dict(\"os.environ\", {\"TESSL\": \"1\"})\n" + bypassTest, "import mock\n" + bypassCollector + "@mock.patch.dict(\"os.environ\", {\"ACR\": \"1\"})\n" + bypassTest},
		{"unchanged-skip", "import unittest\n" + bypassCollector + "@unittest.skip(\"flaky\")\n" + bypassTest, "# migrated\nimport unittest\n" + bypassCollector + "@unittest.skip(\"flaky\")\n" + bypassTest},
		{"unchanged-pytestmark", "import pytest\npytestmark = pytest.mark.slow\n" + plain, "import pytest\npytestmark = pytest.mark.slow\nversion = 'adapted'\n" + plain},
		{"unchanged-skipTest", strings.Replace(bypassSuite, "        assert 1 == 1\n", "        self.skipTest(\"needs tessl\")\n        assert 1 == 1\n", 1), strings.Replace(bypassSuite, "        assert 1 == 1\n", "        self.skipTest(\"needs acr\")\n        assert 1 == 1\n", 1)},
		{"removed-skipTest", strings.Replace(bypassSuite, "        assert 1 == 1\n", "        self.skipTest(\"needs tessl\")\n        assert 1 == 1\n", 1), bypassSuite},
		{"new-helper-without-skip", plain, strings.Replace(plain, "def test_thing():\n", "def helper():\n    return 1\n\ndef test_thing():\n    helper()\n", 1)},
		{"metadata-preamble", plain, "# harmless metadata preamble\nversion = '2.3.4'\n" + plain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if output, err := pythonTestCheck(t, pythonRequest(t, tc.before, tc.after)); err != nil || output != "" {
				t.Fatalf("adaptation refused: %v %s", err, output)
			}
		})
	}
}

// The differential through the production invocation: a decorated bypass turns
// the executed fixture green, and the semantic path refuses it without residue,
// while the reference adaptation beside it still applies and reruns inertly.
func TestSemanticProposalRefusesDecoratedTestBypass(t *testing.T) {
	const path = "tests/test_checks.py"
	const reference = "# .tessl/plugins/upstream/orbit/skills/check/check.sh\n"
	const adapted = "# migrated helper location\n"
	failing := "import unittest\n\nclass Checks(unittest.TestCase):\n    def test_login(self):\n        self.fail('independent failure')\n\nif __name__ == '__main__':\n    unittest.main()\n"
	for _, tc := range []struct{ name, after string }{
		{"aliased-skip", strings.Replace(strings.Replace(failing, "import unittest\n", "import unittest\nfrom unittest import skip\n", 1), "    def test_login", "    @skip('migration')\n    def test_login", 1)},
		{"class-skipIf", strings.Replace(failing, "class Checks", "@unittest.skipIf(True, 'migration')\nclass Checks", 1)},
		{"setUp-skipTest", strings.Replace(failing, "    def test_login", "    def setUp(self):\n        self.skipTest('migration')\n\n    def test_login", 1)},
		{"preserved", failing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, opts, p := semanticFixture(t)
			before, after := reference+failing, adapted+tc.after
			put(t, root, path, before, 0o644)
			run := func(program string) ([]byte, error) {
				t.Helper()
				directory := t.TempDir()
				put(t, directory, "test_checks.py", program, 0o644)
				command := exec.Command("python3", "-I", "-S", "-B", filepath.Join(directory, "test_checks.py"))
				return command.CombinedOutput()
			}
			if output, err := run(before); err == nil {
				t.Fatalf("fixture must fail before conversion: %s", output)
			}
			// Executed only as a controlled fixture here; validation stays parse-only.
			output, err := run(after)
			bypass := tc.name != "preserved"
			if bypass && err != nil {
				t.Fatalf("proposal is not a bypass: it still fails: %s", output)
			}
			if !bypass && err == nil {
				t.Fatalf("preserved failing test became success: %s", output)
			}
			t.Logf("proposed program exit=%v output=%s", err, strings.TrimSpace(string(output)))
			p.Edits = append(p.Edits, proposedEdit{Path: path, BeforeDigest: digest([]byte(before)), Action: "replace", Content: after})
			original := treeAt(t, root)
			calls := 0
			plan, err := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) { calls++; return p, AgentRun{}, nil })
			if !reflect.DeepEqual(original, treeAt(t, root)) {
				t.Fatal("planning mutated fixture")
			}
			if bypass {
				if err == nil {
					t.Fatal("decorated bypass accepted")
				}
				if calls != 3 || !strings.Contains(err.Error(), path+": test preservation:") {
					t.Fatalf("refusal must come from the test checker after retries: calls=%d %v", calls, err)
				}
				assertCorrection14NoResidue(t, root)
				return
			}
			if err != nil || calls != 1 {
				t.Fatalf("reference adaptation refused: %v calls=%d", err, calls)
			}
			if report, e := plan.Apply(); e != nil || !report.Wrote {
				t.Fatalf("Apply %v %+v", e, report)
			}
			if read(t, root, path) != after {
				t.Fatal("wrong applied check")
			}
			rerun, e := prepareWithProvider(context.Background(), opts, func(context.Context, string, string) (proposal, AgentRun, error) {
				t.Fatal("provider called on inert rerun")
				return p, AgentRun{}, nil
			})
			if e != nil || !rerun.Report.Current {
				t.Fatalf("rerun %v %+v", e, rerun.Report)
			}
		})
	}
}
