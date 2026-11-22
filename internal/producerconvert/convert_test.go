package producerconvert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

const fixturePublisher = `name: Publish
permissions:
  contents: write
  pull-requests: write
on:
  push:
    branches: [main]
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: tesslio/patch-version-publish@v1
        with:
          token: ${{ secrets.TESSL_TOKEN }}
          path: plugins/orbit
`
const independentTestJob = `  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: ./tests/run.sh
`

func put(t *testing.T, root, name, body string, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) (string, Options) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","description":"Orbit","author":{"name":"Original author"},"license":"MIT","skills":["skills/inspect","skills/check"]}`+"\n", 0o644)
	put(t, root, "plugins/orbit/.tesslignore", "tests/\n", 0o644)
	put(t, root, "plugins/orbit/skills/inspect/SKILL.md", "# Inspect\nRun `.tessl/plugins/upstream/orbit/skills/check/check.sh`.\nRead [data](skills/check/data.txt).\nForeign `.tessl/plugins/foreign/tools/skills/check/check.sh`.\n", 0o644)
	put(t, root, "plugins/orbit/skills/check/SKILL.md", "# Check\nRun `skills/check/check.sh`.\n", 0o644)
	put(t, root, "plugins/orbit/skills/check/check.sh", "#!/bin/sh\nset -eu\nprintf 'orbit-ok\\n'\n", 0o751)
	put(t, root, "plugins/orbit/skills/check/data.txt", "fixed data\n", 0o640)
	put(t, root, "plugins/orbit/skills/check/LICENSE", "Original notice, verbatim.\n", 0o644)
	put(t, root, ".github/workflows/publish.yml", fixturePublisher+independentTestJob, 0o644)
	put(t, root, "AGENTS.md", "Run tessl install foreign/tools. Preserve me.\n", 0o644)
	put(t, root, "tessl.json", `{"dependencies":{"foreign/tools":{"version":"9.8.7"}}}`, 0o644)
	put(t, root, ".tessl/plugins/foreign/tools/foreign.txt", "foreign installed content\n", 0o644)
	return root, Options{PackageRoot: filepath.Join(root, "plugins/orbit"), Repository: "https://github.com/destination/nebula"}
}
func treeAt(t *testing.T, root string) tree {
	t.Helper()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	result := tree{}
	err = fs.WalkDir(r.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if excluded(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state := fileState{Mode: uint32(info.Mode().Perm()), Directory: entry.IsDir()}
		if entry.Type()&fs.ModeSymlink != 0 {
			state.Link, err = r.Readlink(name)
		} else if !entry.IsDir() {
			state, err = readState(r, name)
		}
		if err != nil {
			return err
		}
		result[name] = state
		return nil
	})
	closeErr := r.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	return result
}
func read(t *testing.T, root, name string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func absent(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or unreadable: %v", name, err)
	}
}

func TestCleanNestedPlanApplyAndRerun(t *testing.T) {
	root, opts := fixture(t)
	if err := os.Symlink("plugins/orbit/skills/inspect/SKILL.md", filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	before := treeAt(t, root)
	opts.DryRun = true
	preview, err := Convert(opts)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Wrote || preview.Current || preview.Package != "destination/nebula" || preview.Version != "2.3.4" {
		t.Fatalf("preview=%+v", preview)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("dry run mutated source")
	}
	absent(t, root, ReceiptPath)
	absent(t, root, transactionPath)
	if len(preview.Changes) == 0 || !strings.Contains(FormatText(preview), "--- a/agent-plugin.yaml") {
		t.Fatal("preview omits exact manifest diff")
	}
	var sawReceipt, sawRewrite, sawRetirement bool
	for _, c := range preview.Changes {
		switch c.Path {
		case ReceiptPath:
			sawReceipt = c.Operation == "create" && c.AfterMode == 0o600
		case "plugins/orbit/skills/inspect/SKILL.md":
			sawRewrite = strings.Contains(c.Before, ".tessl/plugins/upstream/orbit/") && strings.Contains(c.After, "plugins/orbit/skills/check/check.sh") && c.BeforeMode == c.AfterMode
		case "plugins/orbit/.tessl-plugin/plugin.json":
			sawRetirement = c.Operation == "remove" && c.After == ""
		}
	}
	if !sawReceipt || !sawRewrite || !sawRetirement {
		t.Fatal("preview omits receipt, rewrite or retirement")
	}
	opts.DryRun = false
	applied, err := Convert(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Wrote {
		t.Fatal("apply wrote nothing")
	}
	for _, c := range preview.Changes {
		if c.Operation == "remove" {
			absent(t, root, c.Path)
			continue
		}
		if got := read(t, root, c.Path); got != c.After {
			t.Fatalf("%s differs from preview", c.Path)
		}
		st, err := os.Stat(filepath.Join(root, c.Path))
		if err != nil {
			t.Fatal(err)
		}
		if uint32(st.Mode().Perm()) != c.AfterMode {
			t.Fatalf("%s mode=%o", c.Path, st.Mode().Perm())
		}
	}
	m, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := manifest.LoadListed(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, preview.PublishedFiles) {
		t.Fatalf("actual inventory %v != %v", files, preview.PublishedFiles)
	}
	if m.Source.TesslIdentity != "" || m.Name != "destination/nebula" || m.Version != "2.3.4" {
		t.Fatalf("manifest=%+v", m)
	}
	if !strings.Contains(read(t, root, manifest.Filename), `# Original author.name: "Original author"`) {
		t.Fatal("attribution lost")
	}
	if !strings.HasSuffix(read(t, root, ".github/workflows/publish.yml"), independentTestJob) {
		t.Fatal("independent test bytes changed")
	}
	for _, name := range []string{"AGENTS.md", "tessl.json", "plugins/orbit/skills/check/LICENSE", "plugins/orbit/skills/check/check.sh", "plugins/orbit/skills/check/data.txt"} {
		if got := read(t, root, name); got != string(before[name].Content) {
			t.Fatalf("unrelated %s changed", name)
		}
	}
	if read(t, root, ".tessl/plugins/foreign/tools/foreign.txt") != "foreign installed content\n" {
		t.Fatal("foreign content changed")
	}
	after := treeAt(t, root)
	repeated, err := Convert(opts)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Wrote || !repeated.Current || !matches(after, treeAt(t, root)) {
		t.Fatal("rerun was not inert")
	}
	opts.DryRun = true
	again, err := Convert(opts)
	if err != nil || !again.Current || again.Wrote {
		t.Fatalf("repeat dry-run=%+v %v", again, err)
	}
	absent(t, root, transactionPath)
}

func TestDeterministicConvertOmitsSemanticInventory(t *testing.T) {
	root, opts := fixture(t)
	body := "def test_install():\n    tessl install upstream/orbit\n"
	put(t, root, "tests/test_orbit.py", body, 0o644)
	report, err := Convert(opts)
	if err != nil {
		t.Fatalf("tests/ leaked into deterministic inventory: %v", err)
	}
	if !report.Wrote {
		t.Fatal("clean conversion did not apply")
	}
	if read(t, root, "tests/test_orbit.py") != body {
		t.Fatal("tests/ classified or rewritten")
	}
	for _, c := range report.Changes {
		if strings.HasPrefix(c.Path, "tests/") || strings.HasSuffix(c.Path, ".acr-package.json") {
			t.Fatalf("deterministic conversion leaked semantic inventory: %s", c.Path)
		}
	}
	absent(t, root, "plugins/orbit/skills/inspect/.acr-package.json")
	absent(t, root, "plugins/orbit/skills/check/.acr-package.json")
}

func TestSchema3ReceiptRoundTrip(t *testing.T) {
	root, opts := fixture(t)
	applied, err := Convert(opts)
	if err != nil || !applied.Wrote {
		t.Fatalf("initial conversion: %+v, %v", applied, err)
	}
	data := read(t, root, ReceiptPath)
	var rec receipt
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.SchemaVersion != 3 || rec.SourcePackage != "upstream/orbit" || rec.Package != "destination/nebula" || rec.Version != "2.3.4" {
		t.Fatalf("schema-3 receipt: %+v", rec)
	}
	encoded, err := json.MarshalIndent(rec, "", "  ")
	if err != nil || string(encoded)+"\n" != data {
		t.Fatalf("schema-3 round trip differs: %v", err)
	}
	before := treeAt(t, root)
	for _, dry := range []bool{false, true} {
		opts.DryRun = dry
		report, err := Convert(opts)
		if err != nil || !report.Current || report.Wrote || len(report.Changes) != 0 || report.Package != applied.Package || !reflect.DeepEqual(report.Artifacts, applied.Artifacts) || !reflect.DeepEqual(report.PublishedFiles, applied.PublishedFiles) {
			t.Fatalf("dry=%t: schema-3 rerun: %+v, %v", dry, report, err)
		}
		if !reflect.DeepEqual(before, treeAt(t, root)) || read(t, root, ReceiptPath) != data {
			t.Fatal("schema-3 rerun wrote state")
		}
		absent(t, root, transactionPath)
	}
}

func TestSchema2ReceiptCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, options, fields string
		removedAgentPath      bool
	}{
		// f9e92b73 serialized acceptAgentWidening:false even for purely
		// deterministic conversions, while omitting agent and policyChanges.
		{"base-deterministic", `"acceptAgentWidening": false,`, "", false},
		{"empty-legacy-fields", `"agent": "", "acceptAgentWidening": false,`, `"agentRuns": [], "policyChanges": [],`, false},
		{"null-legacy-lists", `"agent": "", "acceptAgentWidening": false,`, `"agentRuns": null, "policyChanges": null,`, false},
		{"agent-selection", `"agent": "codex",`, "", true},
		{"accepted-widening", `"acceptAgentWidening": true,`, "", true},
		{"agent-run", "", `"agentRuns": [{"agent":"codex","path":"skills/check"}],`, true},
		{"policy-change", "", `"policyChanges": [{"path":"review.yml","from":"score","to":"review"}],`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			if _, err := Convert(opts); err != nil {
				t.Fatal(err)
			}
			data := read(t, root, ReceiptPath)
			canonical := data
			var current struct {
				SchemaVersion int `json:"schemaVersion"`
			}
			if err := json.Unmarshal([]byte(data), &current); err != nil || current.SchemaVersion != 3 {
				t.Fatalf("new receipt schema: %d, %v", current.SchemaVersion, err)
			}
			for _, key := range []string{"agent", "acceptAgentWidening", "agentRuns", "policyChanges"} {
				if strings.Contains(data, `"`+key+`":`) {
					t.Fatalf("new receipt retains removed field %s", key)
				}
			}
			data = strings.Replace(data, `"options": {`, `"options": {`+tc.options, 1)
			data = strings.Replace(data, `"schemaVersion": 3,`, `"schemaVersion": 2,`+tc.fields, 1)
			mode := os.FileMode(0o600)
			if tc.name == "empty-legacy-fields" {
				mode = 0o644 // Git checkout mode must survive the upgrade.
			} else if tc.name == "null-legacy-lists" {
				mode = 0o440
			}
			put(t, root, ReceiptPath, data, mode)
			before := treeAt(t, root)
			var proposed []Change
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				if tc.removedAgentPath {
					var refusal *Error
					const want = "receipt came from the removed agent path; restart from the original source checkout and migrate the remaining files manually before rerunning deterministic conversion"
					if !errors.As(err, &refusal) || refusal.Code != "receipt_conflict" || refusal.Path != ReceiptPath || refusal.Reason != want || report.Current {
						t.Fatalf("dry=%t: report=%+v error=%v", dry, report, err)
					}
					if report.Wrote || !reflect.DeepEqual(before, treeAt(t, root)) || read(t, root, ReceiptPath) != data {
						t.Fatal("refused legacy receipt rerun wrote state")
					}
					absent(t, root, transactionPath)
					continue
				}
				if err != nil || !report.Current || report.Wrote == dry || report.DryRun != dry || report.ReportVersion != 1 || len(report.Changes) != 1 {
					t.Fatalf("dry=%t: legacy deterministic rerun: %+v, %v", dry, report, err)
				}
				verb, action, wantBytes := "Upgraded", "Updated receipt for", canonical
				if dry {
					verb, action, wantBytes = "Would upgrade", "Would update receipt for", data
					proposed = report.Changes
				} else if !reflect.DeepEqual(proposed, report.Changes) {
					t.Fatal("applied change differs from preview")
				}
				if !reflect.DeepEqual(report.Notes, []string{verb + " .acr-producer-migration.json from schema 2 to 3."}) {
					t.Fatalf("upgrade note: %v", report.Notes)
				}
				change := report.Changes[0]
				if change.Path != ReceiptPath || change.Operation != "modify" || change.Before != data || change.After != canonical || change.BeforeMode != uint32(mode) || change.AfterMode != uint32(mode) || !strings.HasPrefix(change.Diff, "--- a/"+ReceiptPath+"\n+++ b/"+ReceiptPath+"\n") {
					t.Fatalf("unexpected receipt change: %+v", change)
				}
				if !strings.HasPrefix(FormatText(report), action+" upstream/orbit 2.3.4 → destination/nebula 2.3.4\n") {
					t.Fatalf("unexpected text action: %s", FormatText(report))
				}
				if !reflect.DeepEqual(before, treeAt(t, root)) || read(t, root, ReceiptPath) != wantBytes {
					t.Fatal("upgrade changed more than receipt schema or preview wrote state")
				}
				info, err := os.Stat(filepath.Join(root, ReceiptPath))
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("receipt mode changed: %v, %v", info, err)
				}
				absent(t, root, transactionPath)
			}
			if tc.removedAgentPath {
				return
			}
			for _, dry := range []bool{false, true} {
				opts.DryRun = dry
				report, err := Convert(opts)
				if err != nil || !report.Current || report.Wrote || len(report.Changes) != 0 || len(report.Notes) != 0 {
					t.Fatalf("dry=%t: upgraded rerun: %+v, %v", dry, report, err)
				}
				info, statErr := os.Stat(filepath.Join(root, ReceiptPath))
				if statErr != nil || info.Mode().Perm() != mode || read(t, root, ReceiptPath) != canonical || !reflect.DeepEqual(before, treeAt(t, root)) {
					t.Fatal("upgraded rerun changed bytes or modes")
				}
				absent(t, root, transactionPath)
			}
		})
	}
}

// Keep the original v3 bytes as an independent witness of historical evidence;
// rebuilding the receipt from today's converted tree must not pass these tests.
func schema2ReceiptFixture(t *testing.T) (root string, opts Options, legacy, canonical string) {
	t.Helper()
	root, opts = fixture(t)
	if _, err := Convert(opts); err != nil {
		t.Fatal(err)
	}
	canonical = read(t, root, ReceiptPath)
	legacy = strings.Replace(canonical, `"schemaVersion": 3,`, `"schemaVersion": 2,`, 1)
	legacy = strings.Replace(legacy, `"options": {`, `"options": {"acceptAgentWidening": false,`, 1)
	put(t, root, ReceiptPath, legacy, 0o644)
	return
}

func TestSchema2ReceiptPrepareApply(t *testing.T) {
	root, opts, legacy, canonical := schema2ReceiptFixture(t)
	before := treeAt(t, root)
	opts.DryRun = true
	plan, err := Prepare(opts)
	if err != nil || !plan.Report.Current || plan.Report.Wrote || len(plan.Report.Changes) != 1 {
		t.Fatalf("prepare: %+v, %v", plan.Report, err)
	}
	if read(t, root, ReceiptPath) != legacy || !reflect.DeepEqual(before, treeAt(t, root)) {
		t.Fatal("Prepare wrote state")
	}
	absent(t, root, transactionPath)
	// Public presentation fields must not choose which private delta is applied.
	plan.Report.Current = false
	plan.Report.Changes = nil
	report, err := plan.Apply()
	if err != nil || !report.Current || !report.Wrote || report.DryRun || !reflect.DeepEqual(report.Notes, []string{"Upgraded .acr-producer-migration.json from schema 2 to 3."}) {
		t.Fatalf("Apply: %+v, %v", report, err)
	}
	if read(t, root, ReceiptPath) != canonical || !reflect.DeepEqual(before, treeAt(t, root)) {
		t.Fatal("Apply did not preserve historical evidence and converted output")
	}
	absent(t, root, transactionPath)
}

func TestConversionDoesNotUsePublicCurrentForReceiptUpgrade(t *testing.T) {
	root, opts := fixture(t)
	plan, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	plan.Report.Current = true
	if report, err := plan.Apply(); err != nil || !report.Wrote {
		t.Fatalf("public current flag changed private conversion: wrote=%v error=%v", report.Wrote, err)
	}
	if read(t, root, "plugins/orbit/skills/inspect/SKILL.md") != string(plan.after["plugins/orbit/skills/inspect/SKILL.md"].Content) {
		t.Fatal("conversion edits were not applied")
	}
	absent(t, root, "plugins/orbit/.tessl-plugin/plugin.json")
	absent(t, root, transactionPath)
}

func TestSchema2ReceiptFailures(t *testing.T) {
	for _, kind := range []string{"stage-fault", "stage-collision", "publication-fault", "receipt-bytes", "receipt-mode", "receipt-symlink", "receipt-directory", "receipt-missing", "output-before", "output-after", "commit-fault"} {
		t.Run(kind, func(t *testing.T) {
			root, opts, legacy, canonical := schema2ReceiptFixture(t)
			plan, err := Prepare(opts)
			if err != nil {
				t.Fatal(err)
			}
			before := treeAt(t, root)
			fault := errors.New("injected receipt fault")
			report, err := plan.apply(transactionHooks{Before: func(phase, name string) error {
				if phase == "stage" {
					switch kind {
					case "stage-fault":
						return fault
					case "stage-collision":
						put(t, root, transactionPath+"/receipt.json", "occupied", 0o600)
					}
				}
				if phase == "validate" && kind == "output-before" || phase == "write" && kind == "output-after" {
					put(t, root, "plugins/orbit/skills/check/data.txt", "racing output\n", 0o640)
				}
				if phase == "write" {
					switch kind {
					case "publication-fault":
						return os.Remove(filepath.Join(root, transactionPath, "receipt.json"))
					case "receipt-bytes":
						put(t, root, ReceiptPath, legacy+"\n", 0o644)
					case "receipt-mode":
						return os.Chmod(filepath.Join(root, ReceiptPath), 0o600)
					case "receipt-symlink", "receipt-directory", "receipt-missing":
						if err := os.Remove(filepath.Join(root, ReceiptPath)); err != nil {
							return err
						}
						if kind == "receipt-symlink" {
							return os.Symlink("AGENTS.md", filepath.Join(root, ReceiptPath))
						}
						if kind == "receipt-directory" {
							return os.Mkdir(filepath.Join(root, ReceiptPath), 0o755)
						}
					}
				}
				if phase == "commit" && kind == "commit-fault" {
					return fault
				}
				return nil
			}})
			if err == nil || report.Wrote != (kind == "commit-fault") || !report.Current {
				t.Fatalf("fault outcome: wrote=%v current=%v error=%v", report.Wrote, report.Current, err)
			}
			if strings.HasPrefix(kind, "receipt-") || strings.HasPrefix(kind, "output-") {
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.Code != "source_changed" {
					t.Fatalf("changed input error: %v", err)
				}
			}
			wantBytes, wantMode := legacy, os.FileMode(0o644)
			switch kind {
			case "receipt-bytes":
				wantBytes += "\n"
			case "receipt-mode":
				wantMode = 0o600
			case "commit-fault":
				wantBytes = canonical
				if !errors.Is(err, fault) || !reflect.DeepEqual(report.Notes, []string{"Upgraded .acr-producer-migration.json from schema 2 to 3."}) {
					t.Fatalf("post-publication error lost truthful result: %v, %v", report.Notes, err)
				}
			}
			info, statErr := os.Lstat(filepath.Join(root, ReceiptPath))
			switch kind {
			case "receipt-symlink":
				if statErr != nil || info.Mode()&os.ModeSymlink == 0 || read(t, root, "AGENTS.md") != "Run tessl install foreign/tools. Preserve me.\n" {
					t.Fatal("overwrote racing symlink or target")
				}
			case "receipt-directory":
				if statErr != nil || !info.IsDir() {
					t.Fatal("overwrote racing directory")
				}
			case "receipt-missing":
				if !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatal("recreated removed receipt")
				}
			default:
				if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != wantMode || read(t, root, ReceiptPath) != wantBytes {
					t.Fatal("lost original or published receipt bytes/mode")
				}
			}
			if strings.HasPrefix(kind, "output-") {
				if read(t, root, "plugins/orbit/skills/check/data.txt") != "racing output\n" {
					t.Fatal("overwrote racing output")
				}
			} else if !reflect.DeepEqual(before, treeAt(t, root)) {
				t.Fatal("receipt transaction changed conversion output")
			}
			absent(t, root, transactionPath)
		})
	}
}

func TestSchema2ReceiptInterruption(t *testing.T) {
	// The child exits at an actual publication boundary, bypassing all defers.
	if phase := os.Getenv("ACR_RECEIPT_INTERRUPT_PHASE"); phase != "" {
		plan, err := Prepare(Options{PackageRoot: filepath.Join(os.Getenv("ACR_RECEIPT_INTERRUPT_ROOT"), "plugins/orbit"), Repository: "https://github.com/destination/nebula"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = plan.apply(transactionHooks{Before: func(current, name string) error {
			if current == phase && name == ReceiptPath {
				os.Exit(23)
			}
			return nil
		}})
		t.Fatalf("did not interrupt at %s: %v", phase, err)
	}
	for _, phase := range []string{"write", "commit"} {
		t.Run(phase, func(t *testing.T) {
			root, opts, legacy, canonical := schema2ReceiptFixture(t)
			before := treeAt(t, root)
			child := exec.Command(os.Args[0], "-test.run=^TestSchema2ReceiptInterruption$")
			child.Env = append(os.Environ(), "ACR_RECEIPT_INTERRUPT_PHASE="+phase, "ACR_RECEIPT_INTERRUPT_ROOT="+root)
			out, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child did not interrupt: %v\n%s", err, out)
			}
			want := legacy
			if phase == "commit" {
				want = canonical
			}
			info, err := os.Lstat(filepath.Join(root, ReceiptPath))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 || read(t, root, ReceiptPath) != want || !reflect.DeepEqual(before, treeAt(t, root)) {
				t.Fatal("interruption left missing, partial or changed receipt/output")
			}
			if phase == "write" {
				if read(t, root, transactionPath+"/receipt.json") != canonical {
					t.Fatal("staged receipt is incomplete")
				}
			}
			for _, dry := range []bool{false, true} {
				opts.DryRun = dry
				report, err := Convert(opts)
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.Code != "transaction_conflict" || report.Wrote || read(t, root, ReceiptPath) != want {
					t.Fatalf("interrupted claim not preserved: %v", err)
				}
			}
			// Simulate inspection and recovery: output and the complete live
			// receipt above are intact, so only the abandoned claim is removed.
			if err := os.RemoveAll(filepath.Join(root, transactionPath)); err != nil {
				t.Fatal(err)
			}
			opts.DryRun = false
			if report, err := Convert(opts); err != nil || !report.Current || report.Wrote != (phase == "write") || read(t, root, ReceiptPath) != canonical {
				t.Fatalf("recovered rerun: wrote=%v error=%v", report.Wrote, err)
			}
			if report, err := Convert(opts); err != nil || !report.Current || report.Wrote || len(report.Changes) != 0 || len(report.Notes) != 0 {
				t.Fatalf("settled rerun: %+v, %v", report, err)
			}
			absent(t, root, transactionPath)
		})
	}
}

func TestMappingFailureIncludesHookReferences(t *testing.T) {
	for _, skills := range []string{`["skills/inspect","skills/check"]`, `"skills"`, `{"inspect":{"path":"skills/inspect/SKILL.md"},"check":{"path":"skills/check/SKILL.md"}}`} {
		t.Run(skills, func(t *testing.T) {
			root, opts := fixture(t)
			put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","skills":`+skills+`,"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/stop.sh"]}]}]}}}`+"\n", 0o644)
			put(t, root, "plugins/orbit/hooks/stop.sh", "#!/bin/sh\nexit 0\n", 0o755)
			put(t, root, "plugins/orbit/skills/check/hook.md", "Run `hooks/stop.sh`.\n", 0o644)
			before := treeAt(t, root)
			for _, dry := range []bool{false, true} {
				opts.DryRun = dry
				report, err := Convert(opts)
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" {
					t.Fatalf("dry=%t: missing combined refusal: %v", dry, err)
				}
				want := []Blocker{
					{Path: "plugins/orbit/hooks/stop.sh", Reason: "nativeHooks for hooks/stop.sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion"},
					{Path: "plugins/orbit/skills/check/hook.md", Reason: "owned reference \"hooks/stop.sh`.\" is not a complete declared file path; missing destinations, dynamic suffixes or unsupported punctuation require manual migration"},
				}
				if !reflect.DeepEqual(report.Blockers, want) {
					t.Fatalf("dry=%t: incomplete hook inventory: %+v", dry, report.Blockers)
				}
				if report.Wrote || !reflect.DeepEqual(before, treeAt(t, root)) {
					t.Fatal("hook mapping refusal wrote state")
				}
			}
		})
	}
}

func TestMappingFailuresCollectRulesHooksAndPrivatePolicy(t *testing.T) {
	root, opts := fixture(t)
	put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","private":true,"rules":["rules/one.md","rules/two.md","rules/scoped.md"],"skills":["skills/check"],"nativeHooks":{"claude-code":{"Stop":[{"hooks":[{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/one.sh","tessl.json"]},{"type":"command","command":"bash","args":["${TESSL_PLUGIN_DIR}/hooks/two.sh"]}]}]}}}`, 0o644)
	put(t, root, "LICENSE", "Root license must remain published.\n", 0o644)
	put(t, root, "NOTICE", "Unpackaged notice.\n", 0o644)
	put(t, root, "plugins/orbit/LICENSE", "Unpackaged package license.\n", 0o644)
	put(t, root, "plugins/orbit/rules/scoped.md", "---\nalwaysApply: false\napplyTo: tessl.json — Tessl config\n---\n# Scoped\n", 0o644)
	for _, name := range []string{"one", "two"} {
		put(t, root, "plugins/orbit/rules/"+name+".md", "# Rule\n", 0o644)
		put(t, root, "plugins/orbit/hooks/"+name+".sh", "#!/bin/sh\nexit 0\n", 0o755)
	}
	want := []Blocker{{Path: "plugins/orbit/.tessl-plugin/plugin.json", Reason: "private: true cannot be published through public GitHub releases; set private to false or drop it"}}
	const noticeReason = "required license/notice file is outside the published inventory; place it at a supported root license path or inside a declared artifact before clean conversion"
	want = append(want, Blocker{Path: "NOTICE", Reason: noticeReason}, Blocker{Path: "plugins/orbit/LICENSE", Reason: noticeReason})
	want = append(want, Blocker{Path: "plugins/orbit/rules/scoped.md", Reason: "Tessl configuration/manifest reference requires manual migration; state, pins and rollback cannot be inferred"})
	for _, name := range []string{"one", "two"} {
		want = append(want, Blocker{Path: "plugins/orbit/hooks/" + name + ".sh", Reason: "nativeHooks for hooks/" + name + ".sh on claude-code run on claude-code; converting would also fire on claude-code, codex, cursor. Move the entry into consensus hooks before conversion"})
	}
	for _, name := range []string{"one", "two"} {
		want = append(want, Blocker{Path: "plugins/orbit/rules/" + name + ".md", Reason: "rule rules/" + name + ".md has no YAML frontmatter; set alwaysApply and convert again"})
	}
	sort.SliceStable(want, func(i, j int) bool { return want[i].Path < want[j].Path })
	before := treeAt(t, root)
	for _, dry := range []bool{true, false} {
		opts.DryRun = dry
		report, err := Convert(opts)
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Code != "unsupported_semantic_conversion" || !reflect.DeepEqual(report.Blockers, want) {
			t.Fatalf("dry=%t: blockers=%+v error=%v; want %+v", dry, report.Blockers, err, want)
		}
		if report.Wrote || !reflect.DeepEqual(before, treeAt(t, root)) {
			t.Fatal("combined mapping refusal wrote state")
		}
	}
}

func TestMappingOperationalErrorsAreNotManualMigration(t *testing.T) {
	for _, kind := range []string{"invalid-path", "missing-file", "invalid-version", "invalid-source", "unpublishable-content", "mixed-validation", "malformed-frontmatter"} {
		t.Run(kind, func(t *testing.T) {
			root, opts := fixture(t)
			switch kind {
			case "invalid-path":
				put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","skills":["../escape"]}`, 0o644)
			case "missing-file":
				if err := os.Remove(filepath.Join(opts.PackageRoot, "skills/check/SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "invalid-version":
				opts.PackageVersion = "invalid"
			case "invalid-source":
				opts.Repository = "https://elsewhere.test/upstream/orbit"
			case "unpublishable-content":
				put(t, root, "plugins/orbit/skills/check/cache.pyc", "bytecode", 0o644)
			case "mixed-validation":
				put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","rules":["rules/scoped.md"],"skills":["skills/missing"]}`, 0o644)
				put(t, root, "plugins/orbit/rules/scoped.md", "---\nalwaysApply: false\napplyTo: /absolute — scope\n---\n# Scoped\n", 0o644)
			case "malformed-frontmatter":
				put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","rules":["rules/bad.md"]}`, 0o644)
				put(t, root, "plugins/orbit/rules/bad.md", "---\nalwaysApply: [\n---\n# Bad YAML\n", 0o644)
			}
			_, _, want := tesslplugin.Map(tesslplugin.Options{PackageRoot: opts.PackageRoot, DryRun: true}, opts.Repository, opts.PackageVersion)
			if want == nil {
				t.Fatal("fixture must fail mapping")
			}
			before := treeAt(t, root)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				if err == nil || err.Error() != want.Error() || len(report.Blockers) != 0 || report.Wrote || !reflect.DeepEqual(before, treeAt(t, root)) {
					t.Fatalf("dry=%t: report=%+v err=%v; want ordinary error %v with zero writes", dry, report, err, want)
				}
			}
		})
	}
}

func TestLicenseInventoryCollisionRemainsInvalidPath(t *testing.T) {
	// MapFS preserves Linux-like case-sensitive file/directory occupancy on all hosts.
	image := fstest.MapFS{
		"LICENSE":                           {Data: []byte("MIT\n")},
		"license/pkg/skills/check/SKILL.md": {Data: []byte("# Check\n")},
	}
	value := manifest.Manifest{
		SchemaVersion: manifest.CurrentSchemaVersion, Name: "example/manual", Version: "1.2.3",
		Source:    manifest.Source{Repository: "https://github.com/example/manual"},
		Artifacts: manifest.Artifacts{Skills: []manifest.SkillArtifact{{ID: "check", Path: "license/pkg/skills/check"}}},
	}
	_, err := manifest.PlannedPackageFilesFS(image, value)
	const want = `manifest validation failed: invalid_path at license/pkg/skills/check/SKILL.md: distribution path "license/pkg/skills/check/SKILL.md" occupies "license", which conflicts with "LICENSE" under canonical normalization and Unicode full case folding (including file/directory occupancy); rename the conflicting or unsafe source path and update its declarations before retrying`
	var invalid *manifest.ValidationErrors
	if !errors.As(err, &invalid) || len(invalid.Issues) != 1 || invalid.Issues[0].Code != manifest.CodeInvalidPath || err.Error() != want {
		t.Fatalf("error=%v; want invalid_path: %s", err, want)
	}
}

func TestVersionOverrideAndReceiptBinding(t *testing.T) {
	for _, change := range []string{"options", "content", "mode", "addition", "receipt-version", "receipt-json", "receipt-trailing"} {
		t.Run(change, func(t *testing.T) {
			root, opts := fixture(t)
			opts.PackageVersion = "7.8.9"
			result, err := Convert(opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Version != "7.8.9" {
				t.Fatal("override ignored")
			}
			switch change {
			case "options":
				opts.PackageVersion = "7.8.10"
			case "content":
				put(t, root, "plugins/orbit/skills/check/data.txt", "edited", 0o640)
			case "mode":
				if err := os.Chmod(filepath.Join(root, "plugins/orbit/skills/check/check.sh"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "addition":
				put(t, root, "plugins/orbit/skills/check/new.txt", "new", 0o644)
			case "receipt-version":
				put(t, root, ReceiptPath, strings.Replace(read(t, root, ReceiptPath), `"schemaVersion": 3`, `"schemaVersion": 99`, 1), 0o600)
			case "receipt-trailing":
				put(t, root, ReceiptPath, read(t, root, ReceiptPath)+"{}", 0o600)
			case "receipt-json":
				put(t, root, ReceiptPath, "invalid", 0o600)
			}
			before := treeAt(t, root)
			receiptBefore := read(t, root, ReceiptPath)
			_, err = Convert(opts)
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Code != "receipt_conflict" {
				t.Fatalf("error=%v", err)
			}
			var want string
			switch change {
			case "receipt-version":
				want = "receipt schema version 99 is not supported; current schema version is 3. Restore the original receipt or source checkout."
			case "options":
				want = "receipt conversion options or required fields differ; restore the original source for a different migration"
			}
			if want != "" && refusal.Reason != want {
				t.Fatalf("reason=%q; want %q", refusal.Reason, want)
			}
			if !matches(before, treeAt(t, root)) || read(t, root, ReceiptPath) != receiptBefore {
				t.Fatal("refusal wrote state")
			}
		})
	}
}

func TestRefuseUnsupportedBeforeWrites(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"configuration", "plugins/orbit/skills/check/custom.py", "open('tessl.json', 'w').write(data)\n"},
		{"dependency", "plugins/orbit/skills/check/SKILL.md", "Run `tessl install other/package`.\n"},
		{"dynamic", "plugins/orbit/skills/check/check.sh", "#!/bin/sh\nROOT=\".tessl/plugins/upstream/orbit\"\n\"$ROOT/$HELPER\"\n"},
		{"dynamic-suffix", "plugins/orbit/skills/check/SKILL.md", "Run `skills/check/check.sh${SUFFIX}`.\n"},
		{"nested-dynamic", "plugins/orbit/skills/check/check.sh", "#!/bin/sh\nplugins/orbit/skills/check/${HELPER}\n"},
		{"unknown-command", "plugins/orbit/skills/check/check.sh", "#!/bin/sh\ntessl frobnicate --custom\n"},
		{"subprocess-command", "plugins/orbit/skills/check/custom.py", "subprocess.run(['tessl', 'custom'])\n"},
		{"compact-json", "plugins/orbit/skills/check/paths.json", `{"path":".tessl/plugins/upstream/orbit/skills/check/check.sh"}`},
		{"opaque-call", "plugins/orbit/skills/check/custom.py", `open("skills/check/data.txt")`},
		{"missing", "plugins/orbit/skills/check/SKILL.md", "Run `skills/check/missing.sh`.\n"},
		{"directory", "plugins/orbit/skills/check/SKILL.md", "Read `.tessl/plugins/upstream/orbit/skills/check/`.\n"},
		{"review", ".github/workflows/review.yml", "on: pull_request\njobs:\n  review:\n    steps:\n      - run: tessl review run --threshold 85\n"},
		{"mixed", ".github/workflows/publish.yml", strings.Replace(fixturePublisher, "      - uses: tesslio/patch-version-publish@v1", "      - run: ./custom.sh\n      - uses: tesslio/patch-version-publish@v1", 1)},
		{"notice", "NOTICE", "Upstream notice must be distributed.\n"},
		{"nested-license", "plugins/orbit/LICENSE", "Upstream license must be distributed.\n"},
		{"collision", "agent-plugin.yaml", "existing manifest\n"},
		{"workflow-collision", publishWorkflowPath, "existing publish\n"},
		{"another-package", "other/.tessl-plugin/plugin.json", `{"name":"other/one","version":"1.0.0"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, opts := fixture(t)
			put(t, root, tc.path, tc.body, 0o644)
			before := treeAt(t, root)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				if err == nil || report.Wrote {
					t.Fatalf("unsupported source accepted: %+v %v", report, err)
				}
				found := false
				for _, b := range report.Blockers {
					if b.Path == tc.path {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing actionable path %s: %+v %v", tc.path, report.Blockers, err)
				}
				if !matches(before, treeAt(t, root)) {
					t.Fatal("refusal changed source")
				}
				absent(t, root, ReceiptPath)
				absent(t, root, transactionPath)
			}
		})
	}
}

func TestCleanConversionRootLicenses(t *testing.T) {
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md", "COPYING.txt", "UNLICENSE", "UNLICENSE.md", "UNLICENSE.txt"} {
		t.Run(name, func(t *testing.T) {
			root, opts := fixture(t)
			const notice = "Original notice, verbatim.\x00\r\n"
			put(t, root, name, notice, 0o644)
			want := []string{name, manifest.Filename, "plugins/orbit/skills/check/LICENSE", "plugins/orbit/skills/check/SKILL.md", "plugins/orbit/skills/check/check.sh", "plugins/orbit/skills/check/data.txt", "plugins/orbit/skills/inspect/SKILL.md"}
			sort.Strings(want)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				if err != nil || report.Wrote != !dry || len(report.Blockers) != 0 || !reflect.DeepEqual(report.PublishedFiles, want) {
					t.Fatalf("dry=%v: report=%+v, err=%v; want inventory %v", dry, report, err, want)
				}
				if got := read(t, root, name); got != notice {
					t.Fatalf("license bytes = %q; want %q", got, notice)
				}
			}
			_, files, err := manifest.LoadListed(root, nil)
			if err != nil || !reflect.DeepEqual(files, want) {
				t.Fatalf("converted inventory = %v, %v; want %v", files, err, want)
			}
		})
	}
}

func TestRootSelectedConversionRefusesLicenseRewrites(t *testing.T) {
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md", "COPYING.txt", "UNLICENSE", "UNLICENSE.md", "UNLICENSE.txt"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, ".tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","description":"Orbit","skills":["skills/check"]}`+"\n", 0o644)
			put(t, root, "skills/check/SKILL.md", "# Check\n", 0o644)
			put(t, root, "skills/check/check.sh", "#!/bin/sh\nexit 0\n", 0o755)
			const notice = "Original attribution: [helper](.tessl/plugins/upstream/orbit/skills/check/check.sh).\r\n"
			put(t, root, name, notice, 0o644)
			before := treeAt(t, root)
			want := []Blocker{{Path: name, Reason: "owned reference in license/notice text cannot be rewritten while preserving its bytes"}}
			for _, dry := range []bool{true, false} {
				report, err := Convert(Options{PackageRoot: root, Repository: "https://github.com/destination/nebula", DryRun: dry})
				if err == nil || report.Wrote || !reflect.DeepEqual(report.Blockers, want) {
					t.Fatalf("dry=%v: blockers=%+v, wrote=%v, err=%v; want %+v", dry, report.Blockers, report.Wrote, err, want)
				}
				if got := read(t, root, name); got != notice {
					t.Fatalf("license bytes = %q; want %q", got, notice)
				}
				if !matches(before, treeAt(t, root)) {
					t.Fatal("refused license rewrite changed source")
				}
				assertCorrection14NoResidue(t, root)
			}
		})
	}
}

func TestCleanConversionRefusesUnpackagedNotices(t *testing.T) {
	for _, name := range []string{"NOTICE", "AUTHORS", "LICENSE-MIT", "LICENSE.APACHE", "plugins/orbit/LICENSE", "plugins/orbit/LICENSE-MIT", "plugins/orbit/LICENSE.APACHE", "plugins/orbit/other/COPYING.txt"} {
		t.Run(name, func(t *testing.T) {
			root, opts := fixture(t)
			put(t, root, name, "Original notice.\n", 0o644)
			before := treeAt(t, root)
			want := []Blocker{{Path: name, Reason: "required license/notice file is outside the published inventory; place it at a supported root license path or inside a declared artifact before clean conversion"}}
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				report, err := Convert(opts)
				if err == nil || report.Wrote || !reflect.DeepEqual(report.Blockers, want) {
					t.Fatalf("dry=%v: blockers=%+v, wrote=%v, err=%v; want %+v", dry, report.Blockers, report.Wrote, err, want)
				}
				if !matches(before, treeAt(t, root)) {
					t.Fatal("refusal changed source")
				}
			}
		})
	}
}

func TestSuffixedSourceRootNoticesRefuseCleanConversion(t *testing.T) {
	for _, selected := range []string{".", "plugins/orbit"} {
		for _, name := range []string{"LICENSE-MIT", "LICENSE.APACHE"} {
			for _, ownedReference := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/owned=%v", selected, name, ownedReference), func(t *testing.T) {
					root := t.TempDir()
					put(t, root, ".git/HEAD", "ref: refs/heads/main\n", 0o644)
					put(t, root, filepath.Join(selected, ".tessl-plugin/plugin.json"), `{"name":"upstream/orbit","version":"2.3.4","skills":["skills/check"]}`+"\n", 0o644)
					put(t, root, filepath.Join(selected, "skills/check/SKILL.md"), "# Check\n", 0o644)
					put(t, root, filepath.Join(selected, "skills/check/check.sh"), "#!/bin/sh\nexit 0\n", 0o755)
					notice := "Original notice.\r\n"
					reason := "required license/notice file is outside the published inventory; place it at a supported root license path or inside a declared artifact before clean conversion"
					if ownedReference {
						notice = "Original attribution: [helper](.tessl/plugins/upstream/orbit/skills/check/check.sh).\r\n"
						reason = "owned reference in license/notice text cannot be rewritten while preserving its bytes"
					}
					source := filepath.ToSlash(filepath.Join(selected, name))
					put(t, root, source, notice, 0o644)
					before := treeAt(t, root)
					want := []Blocker{{Path: source, Reason: reason}}
					for _, dry := range []bool{true, false} {
						report, err := Convert(Options{PackageRoot: filepath.Join(root, selected), Repository: "https://github.com/destination/nebula", DryRun: dry})
						if err == nil || report.Wrote || !reflect.DeepEqual(report.Blockers, want) {
							t.Fatalf("dry=%v: %+v, %v", dry, report, err)
						}
						if !matches(before, treeAt(t, root)) || read(t, root, source) != notice {
							t.Fatal("refusal changed notice bytes or source")
						}
					}
				})
			}
		}
	}
}

func TestRejectUnsafeAndAmbiguousSources(t *testing.T) {
	for _, kind := range []string{"leaf-symlink", "directory-symlink", "escape", "disagree", "transaction-collision", "receipt-symlink", "invalid-version"} {
		t.Run(kind, func(t *testing.T) {
			root, opts := fixture(t)
			switch kind {
			case "leaf-symlink":
				if err := os.Symlink("data.txt", filepath.Join(root, "plugins/orbit/skills/check/link")); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				if err := os.Symlink("orbit", filepath.Join(root, "plugins/link")); err != nil {
					t.Fatal(err)
				}
				opts.PackageRoot = filepath.Join(root, "plugins/link")
			case "escape":
				put(t, root, "plugins/orbit/.tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"1.0.0","skills":["../../outside"]}`, 0o644)
			case "disagree":
				put(t, root, "plugins/orbit/tile.json", `{"name":"another/name","version":"1.0.0","skills":[]}`, 0o644)
			case "transaction-collision":
				put(t, root, transactionPath, "do not remove", 0o644)
			case "receipt-symlink":
				if err := os.Symlink("AGENTS.md", filepath.Join(root, ReceiptPath)); err != nil {
					t.Fatal(err)
				}
			case "invalid-version":
				opts.PackageVersion = "not-semver"
			}
			_, err := Convert(opts)
			if err == nil {
				t.Fatal("unsafe source accepted")
			}
			if read(t, root, "AGENTS.md") != "Run tessl install foreign/tools. Preserve me.\n" {
				t.Fatal("unrelated file touched")
			}
			absent(t, root, "agent-plugin.yaml")
		})
	}
}

func TestSourceRacesAndRollback(t *testing.T) {
	for _, kind := range []string{"before", "mode", "edit", "write", "commit", "collision", "rollback-fault"} {
		t.Run(kind, func(t *testing.T) {
			root, opts := fixture(t)
			plan, err := Prepare(opts)
			if err != nil {
				t.Fatal(err)
			}
			before := treeAt(t, root)
			fault := errors.New("injected filesystem fault")
			hooks := transactionHooks{Before: func(phase, name string) error {
				switch kind {
				case "before":
					if phase == "validate" {
						put(t, root, "plugins/orbit/skills/check/data.txt", "racing edit", 0o640)
					}
				case "mode":
					if phase == "validate" {
						if err := os.Chmod(filepath.Join(root, "plugins/orbit/skills/check/check.sh"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				case "edit":
					if phase == "edit" && name == "agent-plugin.yaml" {
						return fault
					}
				case "write", "rollback-fault":
					if phase == "write" && name == "plugins/orbit/skills/inspect/SKILL.md" {
						return fault
					}
				case "commit":
					if phase == "commit" {
						return fault
					}
				case "collision":
					if phase == "write" && name == "agent-plugin.yaml" {
						put(t, root, name, "racing creation", 0o644)
					}
				}
				if kind == "rollback-fault" && phase == "rollback" && name == "plugins/orbit/skills/inspect/SKILL.md" {
					return errors.New("injected restore fault")
				}
				return nil
			}}
			report, err := plan.apply(hooks)
			if err == nil || report.Wrote {
				t.Fatalf("fault accepted: %+v %v", report, err)
			}
			switch kind {
			case "before":
				if read(t, root, "plugins/orbit/skills/check/data.txt") != "racing edit" {
					t.Fatal("overwrote racing source")
				}
			case "mode":
				info, e := os.Stat(filepath.Join(root, "plugins/orbit/skills/check/check.sh"))
				if e != nil || info.Mode().Perm() != 0o644 {
					t.Fatal("overwrote racing mode")
				}
			case "collision":
				if read(t, root, "agent-plugin.yaml") != "racing creation" {
					t.Fatal("overwrote collision")
				}
			case "rollback-fault":
				if !strings.Contains(err.Error(), "rollback incomplete") || !strings.Contains(err.Error(), "injected restore fault") {
					t.Fatalf("rollback error hidden: %v", err)
				}
				if _, e := os.Stat(filepath.Join(root, transactionPath)); e != nil {
					t.Fatal("recovery evidence discarded")
				}
				return
			default:
				if !matches(before, treeAt(t, root)) {
					t.Fatal("rollback failed to restore bytes and modes")
				}
			}
			absent(t, root, ReceiptPath)
			absent(t, root, transactionPath)
		})
	}
}

func TestWriteBoundaryRejectsParentSymlinkRace(t *testing.T) {
	root, opts := fixture(t)
	outside := t.TempDir()
	put(t, outside, "sentinel", "unchanged", 0o644)
	plan, err := Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = plan.apply(transactionHooks{Before: func(phase, name string) error {
		if phase == "write" && name == publishWorkflowPath {
			if err := os.Rename(filepath.Join(root, ".github/workflows"), filepath.Join(root, ".github/original-workflows")); err != nil {
				return err
			}
			return os.Symlink(outside, filepath.Join(root, ".github/workflows"))
		}
		return nil
	}})
	if err == nil {
		t.Fatal("followed a racing parent symlink")
	}
	if read(t, outside, "sentinel") != "unchanged" {
		t.Fatal("outside content changed")
	}
	absent(t, outside, "acr-publish.yml")
	absent(t, root, ReceiptPath)
	absent(t, root, transactionPath)
}

func TestStandalonePackageAndReadOnlyMapping(t *testing.T) {
	root, opts := fixture(t)
	// Without a checkout marker the selected directory is the source boundary.
	if err := os.Remove(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	before := treeAt(t, root)
	opts.DryRun = true
	report, err := Convert(opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.RepositoryRoot != opts.PackageRoot || report.Version != "2.3.4" {
		t.Fatalf("standalone=%+v", report)
	}
	if !matches(before, treeAt(t, root)) {
		t.Fatal("standalone preview wrote source")
	}
}

func assertCorrection14NoResidue(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{ReceiptPath, transactionPath, manifest.Filename} {
		absent(t, root, name)
	}
}

func assertCorrection14Applied(t *testing.T, root string, plan Plan) {
	t.Helper()
	for _, change := range plan.Report.Changes {
		if change.Operation == "remove" {
			absent(t, root, change.Path)
			continue
		}
		if read(t, root, change.Path) != change.After {
			t.Fatalf("advertised bytes differ: %s", change.Path)
		}
		info, err := os.Stat(filepath.Join(root, change.Path))
		if err != nil || uint32(info.Mode().Perm()) != change.AfterMode {
			t.Fatalf("advertised mode differs: %s %v", change.Path, err)
		}
	}
	absent(t, root, transactionPath)
}

func TestCorrection14ModeZeroRepresentation(t *testing.T) {
	for _, body := range [][]byte{nil, {}, []byte("retained")} {
		p := Plan{before: tree{"file": {Content: []byte("before"), Mode: 0}}, after: tree{}}
		p.change("file", body, 0)
		if p.changes[0].Operation != "modify" {
			t.Fatalf("write inferred as deletion: %+v", p.changes[0])
		}
		state, exists := p.after["file"]
		if !exists || state.Mode != 0 || string(state.Content) != string(body) {
			t.Fatal("zero-mode write lost")
		}
	}
}

func TestCorrection14ApplyRejectsInternalModeZeroBeforeClaim(t *testing.T) {
	for _, operation := range []string{"create", "modify"} {
		t.Run(operation, func(t *testing.T) {
			root, opts := fixture(t)
			p, err := Prepare(opts)
			if err != nil {
				t.Fatal(err)
			}
			original := treeAt(t, root)
			p.changes = append(p.changes, Change{Path: "unsafe", Operation: operation, After: "data", AfterMode: 0})
			// The public report is deliberately innocent; internal operations govern Apply.
			report, err := p.apply(transactionHooks{Before: func(phase, name string) error {
				if phase == "edit" || phase == "write" {
					t.Fatal("unsafe operation reached edits")
				}
				return nil
			}})
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Code != "unsupported_file_mode" || refusal.Path != "unsafe" || report.Wrote {
				t.Fatalf("mode refusal: %+v %v", report, err)
			}
			if !matches(original, treeAt(t, root)) {
				t.Fatal("refusal changed input")
			}
			assertCorrection14NoResidue(t, root)
		})
	}
}

func TestCorrection14SelectedAncestorAlias(t *testing.T) {
	for _, inGit := range []bool{false, true} {
		for _, relative := range []bool{false, true} {
			t.Run(fmt.Sprintf("git=%t/relative=%t", inGit, relative), func(t *testing.T) {
				base := t.TempDir()
				real := filepath.Join(base, "real")
				selected := filepath.Join(real, "selected")
				put(t, selected, ".tessl-plugin/plugin.json", `{"name":"origin/demo","version":"1.2.3","skills":["skills/check"]}`, 0644)
				put(t, selected, "skills/check/SKILL.md", "# Check\nOrdinary content.\n", 0644)
				if inGit {
					if err := os.Mkdir(filepath.Join(real, ".git"), 0755); err != nil {
						t.Fatal(err)
					}
				}
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(real, alias); err != nil {
					t.Fatal(err)
				}
				put(t, base, "outside.txt", "outside sentinel", 0640)
				target := filepath.Join(alias, "selected")
				if relative {
					t.Chdir(base)
					target = "alias/selected"
				}
				original := treeAt(t, real)
				for _, dry := range []bool{true, false} {
					report, err := Convert(Options{PackageRoot: target, Repository: "https://github.com/destination/demo", DryRun: dry})
					var refusal *Error
					if !errors.As(err, &refusal) || refusal.Code != "unsafe_path" || report.Wrote {
						t.Fatalf("custom ancestor alias accepted: %+v %v", report, err)
					}
					if !matches(original, treeAt(t, real)) || read(t, base, "outside.txt") != "outside sentinel" {
						t.Fatal("alias refusal changed inventory")
					}
				}
				opts := Options{PackageRoot: selected, Repository: "https://github.com/destination/demo"}
				p, err := Prepare(opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = p.Apply(); err != nil {
					t.Fatal(err)
				}
				assertCorrection14Applied(t, p.root, p)
				current, err := Prepare(opts)
				if err != nil || !current.Report.Current {
					t.Fatalf("regular path rerun: %v", err)
				}
			})
		}
	}
}

func TestCorrection14ExplicitRemovalAndEmptyWrites(t *testing.T) {
	for _, mode := range []uint32{0, 0444, 0640, 0644} {
		t.Run(fmt.Sprintf("%03o", mode), func(t *testing.T) {
			p := Plan{before: tree{"file": {Content: []byte("before"), Mode: mode}}, after: tree{}}
			p.change("file", nil, mode)
			if p.changes[0].Operation != "modify" {
				t.Fatal("empty write became removal")
			}
			if _, exists := p.after["file"]; !exists {
				t.Fatal("empty output missing")
			}
			p.remove("file")
			if p.changes[0].Operation != "remove" || p.changes[0].After != "" {
				t.Fatal("explicit removal missing")
			}
			if _, exists := p.after["file"]; exists {
				t.Fatal("explicitly removed output retained")
			}
		})
	}
}

func correction14ReadACL(t *testing.T, filename string) string {
	t.Helper()
	username, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chmod", "+a", "user:"+strings.TrimSpace(string(username))+" allow read", filename).CombinedOutput(); err != nil {
		t.Fatalf("ACL setup: %v %s", err, out)
	}
	if err := os.Chmod(filename, 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("mode-000 setup: %v", err)
	}
	if _, err := os.ReadFile(filename); err != nil {
		t.Fatalf("ACL-readable fixture is not readable: %v", err)
	}
	return correction14ACL(t, filename)
}

func correction14ACL(t *testing.T, filename string) string {
	t.Helper()
	out, err := exec.Command("ls", "-lde", filename).Output()
	if err != nil {
		t.Fatal(err)
	}
	_, acl, _ := strings.Cut(string(out), "\n")
	if !strings.Contains(acl, "allow read") {
		t.Fatalf("missing actual read ACL: %s", out)
	}
	return acl
}

func TestCorrection14NativeReadableZeroModes(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getuid() == 0 {
		t.Skip("native ACL controls require nonroot macOS; portable mode guards run separately")
	}
	for _, scenario := range []string{"rewrite", "unchanged", "metadata-delete", "rollback", "rollback-failure"} {
		t.Run(scenario, func(t *testing.T) {
			root, opts := fixture(t)
			name := "plugins/orbit/.tessl-plugin/plugin.json"
			if scenario == "rewrite" {
				name = "plugins/orbit/skills/inspect/SKILL.md"
			}
			if scenario == "unchanged" {
				name = "plugins/orbit/skills/check/data.txt"
			}
			filename := filepath.Join(root, name)
			acl := correction14ReadACL(t, filename)
			info, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			old := read(t, root, name)
			before := treeAt(t, root)
			for _, dry := range []bool{true, false} {
				opts.DryRun = dry
				p, err := Prepare(opts)
				if !matches(before, treeAt(t, root)) {
					t.Fatal("planning mutated source")
				}
				if scenario == "rewrite" {
					var refusal *Error
					if !errors.As(err, &refusal) || refusal.Code != "unsupported_file_mode" || refusal.Path != name || p.receipt != nil {
						t.Fatalf("mode refusal: %v", err)
					}
					assertCorrection14NoResidue(t, root)
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if dry {
					continue
				}
				if strings.HasPrefix(scenario, "rollback") {
					moved := false
					_, err = p.apply(transactionHooks{Before: func(phase, current string) error {
						if phase == "commit" {
							absent(t, root, name)
							moved = true
							return errors.New("later edit fault")
						}
						if scenario == "rollback-failure" && phase == "rollback" && current == name {
							return errors.New("recovery fault")
						}
						return nil
					}})
					if err == nil || !moved {
						t.Fatal("later deletion fault was not exercised")
					}
					if scenario == "rollback-failure" {
						if !strings.Contains(err.Error(), "rollback incomplete") {
							t.Fatal(err)
						}
						found := false
						entries, e := os.ReadDir(filepath.Join(root, transactionPath))
						if e != nil {
							t.Fatal(e)
						}
						for _, entry := range entries {
							if !strings.HasPrefix(entry.Name(), "original-") {
								continue
							}
							saved := filepath.Join(root, transactionPath, entry.Name())
							savedInfo, e := os.Stat(saved)
							if e != nil {
								t.Fatal(e)
							}
							if os.SameFile(info, savedInfo) {
								found = true
								if stringMustRead(t, saved) != old || correction14ACL(t, saved) != acl {
									t.Fatal("original backup lost")
								}
							}
						}
						if !found {
							t.Fatal("recoverable original absent")
						}
						return
					}
					if !matches(before, treeAt(t, root)) {
						t.Fatal("rollback did not restore full inventory")
					}
					assertCorrection14NoResidue(t, root)
				} else {
					if _, err = p.Apply(); err != nil {
						t.Fatal(err)
					}
					assertCorrection14Applied(t, root, p)
					current, err := Prepare(opts)
					if err != nil || !current.Report.Current {
						t.Fatalf("current rerun: %v", err)
					}
					if scenario == "metadata-delete" {
						absent(t, root, name)
						return
					}
				}
			}
			currentInfo, err := os.Stat(filename)
			if err != nil || !os.SameFile(info, currentInfo) || read(t, root, name) != old || correction14ACL(t, filename) != acl {
				t.Fatalf("original inode/bytes/ACL changed: %v", err)
			}
			t.Logf("nonroot uid=%d: %s retained original inode, bytes, mode000 and ACL", os.Getuid(), scenario)
		})
	}
}

func stringMustRead(t *testing.T, filename string) string {
	t.Helper()
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCorrection14PlatformAnchorSpelling(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS fixed filesystem anchors")
	}
	for _, name := range []string{"/var", "/tmp", "/etc"} {
		if err := validateSelectedPath(name); err != nil {
			t.Fatalf("ordinary platform anchor %s: %v", name, err)
		}
	}
}

func TestNestedLicenseLikeFilesRemainOrdinaryConversionInputs(t *testing.T) {
	root := t.TempDir()
	put(t, root, ".tessl-plugin/plugin.json", `{"name":"upstream/orbit","version":"2.3.4","description":"Orbit","skills":["skills/x"]}`+"\n", 0o644)
	put(t, root, "skills/x/SKILL.md", "# Check\n", 0o644)
	put(t, root, "skills/x/helper.sh", "#!/bin/sh\nexit 0\n", 0o755)
	names := []string{"skills/x/licenses.md", "skills/x/license_check.py", "skills/x/licensing.md", "tools/license_check.py"}
	const original = "# Read `.tessl/plugins/upstream/orbit/skills/x/helper.sh`.\n"
	const rewritten = "# Read `skills/x/helper.sh`.\n"
	for _, name := range names {
		put(t, root, name, original, 0o644)
	}
	before := treeAt(t, root)
	for _, dry := range []bool{true, false} {
		report, err := Convert(Options{PackageRoot: root, Repository: "https://github.com/destination/nebula", DryRun: dry})
		if err != nil || report.Wrote != !dry || len(report.Blockers) != 0 {
			t.Fatalf("dry=%v: %+v, %v", dry, report, err)
		}
		for _, name := range names {
			want := rewritten
			if dry {
				want = original
			}
			if got := read(t, root, name); got != want {
				t.Fatalf("dry=%v %s = %q; want %q", dry, name, got, want)
			}
		}
		if dry && !matches(before, treeAt(t, root)) {
			t.Fatal("dry-run changed source")
		}
	}
}

func TestDistributionNoticeKeepsHistoricalNestedAndNoticeMatching(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice bool
	}{
		{"LICENSEfoo", false}, {"UNLICENSEfoo", false}, {"license_check.py", false}, {"COPYING-extra", true}, {"LICENSE", true},
		{"LICENSE-MIT", true}, {"LICENSE.APACHE", true}, {"LICENCE-extra", true}, {"UNLICENSE-extra", true},
		{"skills/x/LICENSE", true}, {"skills/x/LICENSE.md", true}, {"skills/x/LICENSE-MIT", true},
		{"skills/x/licenses.md", false}, {"skills/x/license_check.py", false}, {"skills/x/licensing.md", false},
		{"NOTICE", true}, {"NOTICE.md", true}, {"NOTICE-extra", true}, {"notices.md", false},
		{"COPYRIGHT-extra", true}, {"AUTHORS.md", true}, {"authors_extra.md", false},
		{"nested/NOTICE", true}, {"nested/notices.md", false}, {"nested/COPYRIGHT.md", true}, {"nested/AUTHORS-extra", true},
	} {
		got, err := distributionNotice(tc.name, ".", func(string) (fs.FileMode, error) { return 0o644, nil })
		if err != nil || got != tc.notice {
			t.Errorf("notice(%q) = %v, %v; want %v", tc.name, got, err, tc.notice)
		}
	}
}

func TestSourcePackageRootLicenseBoundary(t *testing.T) {
	for _, selected := range []string{".", "plugins/orbit"} {
		for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md", "COPYING.txt", "UNLICENSE", "UNLICENSE.md", "UNLICENSE.txt"} {
			t.Run(selected+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				put(t, root, filepath.Join(selected, ".tessl-plugin/plugin.json"), `{"name":"upstream/orbit","version":"2.3.4","description":"Orbit","repository":"https://github.com/upstream/orbit","skills":["skills/x"]}`+"\n", 0o644)
				put(t, root, filepath.Join(selected, "skills/x/SKILL.md"), "# Check\n", 0o644)
				const original = "Original notice.\x00\r\n"
				put(t, root, filepath.Join(selected, name), original, 0o644)
				before := treeAt(t, root)
				packageRoot := filepath.Join(root, selected)
				ordinary, err := tesslplugin.Convert(tesslplugin.Options{PackageRoot: packageRoot, DryRun: true})
				if err != nil || !slices.Contains(ordinary.PublishedFiles, name) {
					t.Fatalf("default inventory: %+v, %v", ordinary, err)
				}
				for _, dry := range []bool{true, false} {
					clean, err := Convert(Options{PackageRoot: packageRoot, Repository: "https://github.com/destination/nebula", DryRun: dry})
					if selected == "." {
						if err != nil || !slices.Contains(clean.PublishedFiles, name) {
							t.Fatalf("clean inventory: %+v, %v", clean, err)
						}
					} else if err == nil || clean.Wrote || len(clean.Blockers) != 1 || clean.Blockers[0].Path != selected+"/"+name || !matches(before, treeAt(t, root)) {
						t.Fatalf("unpackaged source notice was not refused without writes: %+v, %v", clean, err)
					}
					if got := read(t, root, filepath.Join(selected, name)); got != original {
						t.Fatalf("notice changed: %q", got)
					}
				}
			})
		}
	}
}

func TestSourceRootPrefixFilesAreOrdinaryInputs(t *testing.T) {
	for _, selected := range []string{".", "plugins/orbit"} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		put(t, root, filepath.Join(selected, ".tessl-plugin/plugin.json"), `{"name":"upstream/orbit","version":"2.3.4","description":"Orbit","repository":"https://github.com/upstream/orbit","skills":["skills/x"]}`+"\n", 0o644)
		put(t, root, filepath.Join(selected, "skills/x/SKILL.md"), "# Check\n", 0o644)
		put(t, root, filepath.Join(selected, "skills/x/helper.sh"), "#!/bin/sh\nexit 0\n", 0o755)
		for _, name := range []string{"LICENSEfoo", "UNLICENSEfoo"} {
			put(t, root, filepath.Join(selected, name), "# Read `.tessl/plugins/upstream/orbit/skills/x/helper.sh`.\n", 0o644)
		}
		ordinary, err := tesslplugin.Convert(tesslplugin.Options{PackageRoot: filepath.Join(root, selected), DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"LICENSEfoo", "UNLICENSEfoo"} {
			if slices.Contains(ordinary.PublishedFiles, name) {
				t.Fatalf("default implicitly selected %s", name)
			}
		}
		clean, err := Convert(Options{PackageRoot: filepath.Join(root, selected), Repository: "https://github.com/destination/nebula"})
		if err != nil || !clean.Wrote {
			t.Fatalf("ordinary conversion: %+v, %v", clean, err)
		}
		for _, name := range []string{"LICENSEfoo", "UNLICENSEfoo"} {
			if got := read(t, root, filepath.Join(selected, name)); got != "# Read `"+strings.TrimPrefix(selected+"/", "./")+"skills/x/helper.sh`.\n" {
				t.Fatalf("ordinary %s: %q", name, got)
			}
			if slices.Contains(clean.PublishedFiles, strings.TrimPrefix(selected+"/", "./")+name) {
				t.Fatalf("clean implicitly selected %s", name)
			}
		}
	}
}
