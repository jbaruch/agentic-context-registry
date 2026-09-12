package producerconvert

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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
				put(t, root, ReceiptPath, strings.Replace(read(t, root, ReceiptPath), `"schemaVersion": 2`, `"schemaVersion": 99`, 1), 0o600)
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
			stageCheck := correctionStageCheck(t)
			defer stageCheck()
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
