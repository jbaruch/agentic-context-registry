package producerconvert

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
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
	result, err := snapshot(r)
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
	files, err := manifest.PackageFiles(root, m)
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

func TestVersionOverrideAndReceiptBinding(t *testing.T) {
	for _, change := range []string{"options", "content", "mode", "addition", "receipt-version", "receipt-json", "receipt-mode", "receipt-trailing"} {
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
				put(t, root, ReceiptPath, strings.Replace(read(t, root, ReceiptPath), `"schemaVersion": 1`, `"schemaVersion": 99`, 1), 0o600)
			case "receipt-mode":
				if err := os.Chmod(filepath.Join(root, ReceiptPath), 0o644); err != nil {
					t.Fatal(err)
				}
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
		{"license", "LICENSE", "Upstream license must be distributed.\n"},
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
