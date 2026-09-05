package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/dependencytest"
)

const ffaCommit = "769950e1ab14ad5df4ac2bed45efa6f353a97674"
const ffaSource = "github:jbaruch/ffa-acr-dogfood"
const ffaRoot = "jbaruch-ffa-acr-dogfood-769950e"
const ffaHook = "#!/usr/bin/env bash\nset -euo pipefail\nprintf 'FFA hook\\n'\n"
const ffaScript = "#!/usr/bin/env bash\nset -euo pipefail\nprintf 'FFA companion\\n'\n"

// Raw records are intentional: tar.Writer consumes/rejects explicit TypeXHeader
// records. Building the framing here gives the resolver actual g/x bytes, with
// a short placeholder name replaced by the per-file PAX path at read time.
type ffaTarEntry struct {
	name string
	kind byte
	body string
	mode int64
	link string
	size int64
}

func ffaPAX(key, value string) string {
	record := key + "=" + value + "\n"
	n := len(record) + 2
	for {
		next := len(fmt.Sprint(n)) + 1 + len(record)
		if next == n {
			return fmt.Sprintf("%d %s", n, record)
		}
		n = next
	}
}

func ffaArchive(t *testing.T, entries []ffaTarEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	for _, entry := range entries {
		var header [512]byte
		if len(entry.name) > 100 || len(entry.link) > 100 {
			t.Fatal("fixture name exceeds raw USTAR field")
		}
		copy(header[:100], entry.name)
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(entry.body))
		if entry.size != 0 {
			size = entry.size
		}
		copy(header[100:108], fmt.Sprintf("%07o\x00", mode))
		copy(header[108:116], "0000000\x00")
		copy(header[116:124], "0000000\x00")
		copy(header[124:136], fmt.Sprintf("%011o\x00", size))
		copy(header[136:148], "00000000000\x00")
		copy(header[148:156], "        ")
		header[156] = entry.kind
		copy(header[157:257], entry.link)
		copy(header[257:263], "ustar\x00")
		copy(header[263:265], "00")
		sum := 0
		for _, b := range header {
			sum += int(b)
		}
		copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
		raw.Write(header[:])
		raw.WriteString(entry.body)
		// Deliberately oversized/truncated payloads omit their claimed body.
		raw.Write(make([]byte, (512-len(entry.body)%512)%512))
	}
	raw.Write(make([]byte, 1024))
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func ffaPackageEntries(global, extended bool) []ffaTarEntry {
	manifest := "schemaVersion: 1\nname: jbaruch/ffa-acr-dogfood\nversion: 0.9.38\nsource:\n  repository: https://github.com/jbaruch/ffa-acr-dogfood\nartifacts:\n  rules:\n    - id: boundaries\n      path: rules/boundaries.md\n      activation:\n        mode: always\n  skills:\n    - id: advocate\n      path: skills/advocate\n  hooks:\n    - id: session-start\n      event: session-start\n      path: hooks/session-start.sh\n"
	var entries []ffaTarEntry
	if global {
		entries = append(entries, ffaTarEntry{name: "pax_global_header", kind: tar.TypeXGlobalHeader, body: ffaPAX("comment", ffaCommit)})
	}
	if extended {
		entries = append(entries, ffaTarEntry{name: "PaxHeaders/root", kind: tar.TypeXHeader, body: ffaPAX("path", ffaRoot+"/")})
	}
	entries = append(entries, ffaTarEntry{name: ffaRoot + "/", kind: tar.TypeDir, mode: 0o755})
	for _, file := range []ffaTarEntry{
		{name: "agent-plugin.yaml", body: manifest},
		{name: "rules/boundaries.md", body: "# FFA boundary\nUse only verified flight facts.\n"},
		{name: "hooks/session-start.sh", body: ffaHook, mode: 0o755},
		{name: "skills/advocate/SKILL.md", body: "---\nname: advocate\ndescription: Review a flight complaint.\n---\nRead references/guide.md and run scripts/check.sh.\n"},
		{name: "skills/advocate/references/guide.md", body: "# Companion\nVerify the flight facts.\n"},
		{name: "skills/advocate/scripts/check.sh", body: ffaScript, mode: 0o755},
	} {
		file.name = ffaRoot + "/" + file.name
		file.kind = tar.TypeReg
		if extended {
			entries = append(entries, ffaTarEntry{name: "PaxHeaders/file", kind: tar.TypeXHeader, body: ffaPAX("path", file.name)})
			file.name = "placeholder"
		}
		entries = append(entries, file)
	}
	return entries
}

func ffaRemote(archive []byte) *dependencytest.Remote {
	remote := dependencytest.NewRemote()
	release := dependency.Release{ID: 42, Tag: "v0.9.38"}
	remote.Latest[ffaSource] = release
	remote.Releases[ffaSource+"@v0.9.38"] = release
	remote.Commits[ffaSource+"@v0.9.38"] = ffaCommit
	remote.Archives[ffaSource+"@"+ffaCommit] = archive
	return remote
}

func TestFFAPAXReaderSemantics(t *testing.T) {
	archive := ffaArchive(t, ffaPackageEntries(true, true))
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	first, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Typeflag != tar.TypeXGlobalHeader || first.Name != "pax_global_header" || first.PAXRecords["comment"] != ffaCommit {
		t.Fatalf("global header = %#v", first)
	}
	root, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if root.Typeflag != tar.TypeDir || root.Name != ffaRoot+"/" {
		t.Fatalf("per-file header not consumed: %#v", root)
	}
	count := 0
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(h.Name, ffaRoot+"/") {
			t.Fatalf("effective header = %#v", h)
		}
	}
	if count != 6 {
		t.Fatalf("files = %d, want 6", count)
	}
}

func TestFFAGitHubPAXResolver(t *testing.T) {
	baseline, err := dependency.NewResolver(ffaRemote(ffaArchive(t, ffaPackageEntries(false, false)))).Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		global, extended bool
	}{{"global", true, false}, {"per-file", false, true}, {"both", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := dependency.NewResolver(ffaRemote(ffaArchive(t, ffaPackageEntries(tc.global, tc.extended))))
			locked, err := resolver.Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"})
			if err != nil {
				t.Fatalf("GitHub source archive rejected: %v", err)
			}
			if !reflect.DeepEqual(locked, baseline) {
				t.Fatalf("PAX changed package identity: got %#v, want %#v", locked, baseline)
			}
			if err := resolver.VerifyLocked(context.Background(), locked); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func ffaRun(t *testing.T, remote dependency.Remote, root string, want int, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append(args, "--project", root)
	code := runWith(remote, strings.NewReader(""), &stdout, &stderr, args)
	if code != want {
		t.Fatalf("%v exit=%d want=%d stdout=%q stderr=%q", args, code, want, stdout.String(), stderr.String())
	}
	return stdout.String() + stderr.String()
}

func ffaAssertFile(t *testing.T, root, path, body string, mode os.FileMode) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(path))
	actual, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != body {
		t.Errorf("%s bytes=%q want=%q", path, actual, body)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("%s mode=%o want=%o", path, info.Mode().Perm(), mode)
	}
}

func TestFFAGitHubPAXLifecycle(t *testing.T) {
	// Plain controls reach downstream assertions before the PAX fix.
	for _, tc := range []struct{ pax, shared bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("pax=%t/shared=%t", tc.pax, tc.shared), func(t *testing.T) {
			root := t.TempDir()
			git := exec.Command("git", "init", "--quiet", root)
			if out, err := git.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			reverify2Put(t, root, "user-notes.md", "# Keep my notes\n", 0o644)
			if tc.shared {
				reverify2Put(t, root, "AGENTS.md", "# User guidance\n", 0o644)
				reverify2Put(t, root, "CLAUDE.md", "# User Claude guidance\n", 0o644)
			}
			remote := ffaRemote(ffaArchive(t, ffaPackageEntries(tc.pax, tc.pax)))
			ffaRun(t, remote, root, 0, "init", "--agent", "claude-code", "--agent", "codex", "--freshness", "none", "--non-interactive")
			ffaRun(t, remote, root, 0, "install", ffaSource, "--non-interactive")
			state, err := dependency.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Project.Dependencies) != 1 || len(state.Lock.Dependencies) != 1 || state.Lock.Dependencies[0].Commit != ffaCommit {
				t.Fatalf("installed state=%#v", state)
			}
			ffaRun(t, remote, root, 0, "realize")
			var owned []string
			for _, agent := range []string{".claude", ".codex"} {
				base := agent + "/skills/acr__jbaruch__ffa-acr-dogfood__advocate/"
				ffaAssertFile(t, root, base+"references/guide.md", "# Companion\nVerify the flight facts.\n", 0o644)
				ffaAssertFile(t, root, base+"scripts/check.sh", ffaScript, 0o755)
				skill, err := os.ReadFile(filepath.Join(root, base, "SKILL.md"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(skill, []byte("references/guide.md")) {
					t.Errorf("skill companion reference lost: %s", skill)
				}
				hook := agent + "/hooks/acr__jbaruch__ffa-acr-dogfood__session-start/session-start.sh"
				ffaAssertFile(t, root, hook, ffaHook, 0o755)
				owned = append(owned, base+"SKILL.md", base+"references/guide.md", base+"scripts/check.sh", hook)
			}
			for _, path := range []string{"AGENTS.md", "CLAUDE.md"} {
				body, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(body, []byte("FFA boundary")) {
					t.Fatalf("%s does not load package rule: %s", path, body)
				}
			}
			for _, path := range []string{".claude/settings.json", ".codex/config.toml"} {
				body, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(body, []byte("acr__jbaruch__ffa-acr-dogfood__session-start")) {
					t.Fatalf("%s missing hook registration: %s", path, body)
				}
			}
			before := reverify2HashTree(t, root)
			ffaRun(t, remote, root, 0, "check")
			ffaRun(t, remote, root, 0, "install", ffaSource, "--non-interactive")
			ffaRun(t, remote, root, 0, "check")
			if after := reverify2HashTree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("repeat install/check changed project: before=%v after=%v", before, after)
			}
			for _, agent := range []string{".claude", ".codex"} {
				ffaAssertFile(t, root, agent+"/hooks/acr__jbaruch__ffa-acr-dogfood__session-start/session-start.sh", ffaHook, 0o755)
			}
			ffaRun(t, remote, root, 0, "uninstall", ffaSource)
			for _, path := range owned {
				if _, err := os.Lstat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("uninstall retained %s: %v", path, err)
				}
			}
			ffaAssertFile(t, root, "user-notes.md", "# Keep my notes\n", 0o644)
			if tc.shared {
				ffaAssertFile(t, root, "AGENTS.md", "# User guidance\n", 0o644)
				ffaAssertFile(t, root, "CLAUDE.md", "# User Claude guidance\n", 0o644)
			} else {
				for _, path := range []string{"AGENTS.md", "CLAUDE.md"} {
					if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("uninstall retained rule target %s: %v", path, err)
					}
				}
			}
			state, err = dependency.LoadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Project.Dependencies) != 0 || len(state.Lock.Dependencies) != 0 {
				t.Fatalf("uninstall retained dependency: %#v", state)
			}
			before = reverify2HashTree(t, root)
			output := ffaRun(t, remote, root, 2, "uninstall", ffaSource, "--json")
			if !strings.Contains(output, "dependency_not_declared") {
				t.Fatalf("second uninstall=%s", output)
			}
			if !reflect.DeepEqual(before, reverify2HashTree(t, root)) {
				t.Fatal("second uninstall changed files")
			}
		})
	}
}

func TestFFAPAXArchiveGuards(t *testing.T) {
	for _, pax := range []bool{false, true} {
		t.Run(fmt.Sprintf("pax=%t", pax), func(t *testing.T) {
			good := ffaPackageEntries(false, false)
			global := []ffaTarEntry{}
			if pax {
				global = append(global, ffaTarEntry{name: "pax_global_header", kind: tar.TypeXGlobalHeader, body: ffaPAX("comment", ffaCommit)})
			}
			tests := []struct {
				name    string
				entries []ffaTarEntry
				want    string
			}{
				{"multiple-roots", append(append([]ffaTarEntry{}, good...), ffaTarEntry{name: "second-root/file", kind: tar.TypeReg, body: "bad"}), "multiple roots"},
				{"extended-traversal", []ffaTarEntry{{name: "PaxHeaders/file", kind: tar.TypeXHeader, body: ffaPAX("path", "../../escape")}, {name: "safe", kind: tar.TypeReg, body: "escape"}}, "unsafe path"},
				{"duplicate-effective-path", append(append([]ffaTarEntry{}, good...), ffaTarEntry{name: "PaxHeaders/file", kind: tar.TypeXHeader, body: ffaPAX("path", ffaRoot+"/rules/boundaries.md")}, ffaTarEntry{name: "alias", kind: tar.TypeReg, body: "overwrite"}), "repeats path"},
				{"malformed-pax", []ffaTarEntry{{name: "PaxHeaders/file", kind: tar.TypeXHeader, body: "999 path=missing\n"}, {name: ffaRoot + "/file", kind: tar.TypeReg, body: "bad"}}, "read downloaded archive"},
				{"truncated-file", []ffaTarEntry{{name: ffaRoot + "/file", kind: tar.TypeReg, body: "short", size: 4096}}, "extract package file"},
				{"oversized-file", []ffaTarEntry{{name: ffaRoot + "/huge", kind: tar.TypeReg, size: (256 << 20) + 1}}, "expands beyond"},
				{"missing-manifest", []ffaTarEntry{{name: ffaRoot + "/file", kind: tar.TypeReg, body: "content"}}, "agent-plugin.yaml"},
			}
			for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink} {
				entries := append([]ffaTarEntry{}, good...)
				for i := range entries {
					if entries[i].name == ffaRoot+"/rules/boundaries.md" {
						entries[i].kind = kind
						entries[i].body = ""
						entries[i].link = "../../outside"
					}
				}
				tests = append(tests, struct {
					name    string
					entries []ffaTarEntry
					want    string
				}{fmt.Sprintf("declared-link-%c", kind), entries, "boundaries.md"})
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					entries := append(append([]ffaTarEntry{}, global...), tc.entries...)
					_, err := dependency.NewResolver(ffaRemote(ffaArchive(t, entries))).Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"})
					if err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("guard error=%v, want %q", err, tc.want)
					}
					if tc.name == "multiple-roots" {
						message := strings.ToLower(err.Error())
						if !strings.Contains(message, ffaRoot) || !strings.Contains(message, "second-root") || strings.Contains(message, "publish one package root") {
							t.Errorf("multi-root diagnostic must name real roots without republish advice: %v", err)
						}
						if !strings.Contains(message, "retry") && !strings.Contains(message, "report") && !strings.Contains(message, "verify") && !strings.Contains(message, "check") {
							t.Errorf("multi-root diagnostic lacks recovery action: %v", err)
						}
					}
				})
			}
		})
	}
	t.Run("no-files", func(t *testing.T) {
		for _, entries := range [][]ffaTarEntry{nil, {{name: "pax_global_header", kind: tar.TypeXGlobalHeader, body: ffaPAX("comment", ffaCommit)}}, {{name: ffaRoot + "/", kind: tar.TypeDir}}} {
			_, err := dependency.NewResolver(ffaRemote(ffaArchive(t, entries))).Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"})
			if err == nil {
				t.Fatalf("resolver accepted no-files archive: %#v", entries)
			}
		}
	})
	t.Run("empty-input", func(t *testing.T) {
		_, err := dependency.NewResolver(ffaRemote(nil)).Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"})
		if err == nil {
			t.Fatal("resolver accepted empty input")
		}
	})
	t.Run("entry-limit", func(t *testing.T) {
		// The existing bound counts caller-visible headers. Count the global header
		// too; metadata must not become a way to evade the resource limit.
		entries := []ffaTarEntry{{name: "pax_global_header", kind: tar.TypeXGlobalHeader, body: ffaPAX("comment", ffaCommit)}}
		entries = append(entries, ffaPackageEntries(false, false)...)
		for len(entries) < 10000 {
			entries = append(entries, ffaTarEntry{name: fmt.Sprintf("%s/extra-%d", ffaRoot, len(entries)), kind: tar.TypeReg})
		}
		resolver := dependency.NewResolver(ffaRemote(ffaArchive(t, entries)))
		if _, err := resolver.Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"}); err != nil {
			t.Fatalf("exact entry boundary: %v", err)
		}
		entries = append(entries, ffaTarEntry{name: ffaRoot + "/one-too-many", kind: tar.TypeReg})
		_, err := dependency.NewResolver(ffaRemote(ffaArchive(t, entries))).Resolve(context.Background(), dependency.Declaration{Source: ffaSource, Requested: "latest"})
		if err == nil || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("entry limit error=%v", err)
		}
	})
}

type ffaDeniedRemote struct{ *dependencytest.Remote }

func (r ffaDeniedRemote) DownloadArchive(context.Context, dependency.Repository, string) ([]byte, error) {
	return nil, fmt.Errorf("download source archive: %w", os.ErrPermission)
}

func TestFFAInstallPermissionFailurePreservesState(t *testing.T) {
	root := t.TempDir()
	remote := ffaDeniedRemote{ffaRemote(nil)}
	ffaRun(t, remote, root, 0, "init", "--agent", "codex", "--freshness", "none", "--non-interactive")
	before := reverify2HashTree(t, root)
	output := ffaRun(t, remote, root, 1, "install", ffaSource, "--non-interactive", "--json")
	if !strings.Contains(output, "permission denied") || !strings.Contains(output, "dependency_operation_failed") {
		t.Fatalf("permission failure=%s", output)
	}
	if !reflect.DeepEqual(before, reverify2HashTree(t, root)) {
		t.Fatal("failed install changed project state")
	}
}

func TestFFAOutdatedStates(t *testing.T) {
	for _, name := range []string{"empty", "pinned-tag", "pinned-commit", "current", "outdated"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			state := dependency.State{Project: dependency.Project{SchemaVersion: dependency.CurrentSchemaVersion}, Lock: dependency.Lockfile{SchemaVersion: dependency.CurrentSchemaVersion}}
			remote := dependencytest.NewRemote()
			if name != "empty" {
				requested := "latest"
				if name == "pinned-tag" {
					requested = "v0.9.38"
				}
				if name == "pinned-commit" {
					requested = ffaCommit
				}
				locked := dependency.LockedDependency{Source: ffaSource, Requested: requested, Kind: dependency.ResolutionRelease, ReleaseID: 42, Tag: "v0.9.38", Commit: ffaCommit, PackageVersion: "0.9.38", ContentHash: "sha256:" + strings.Repeat("a", 64)}
				if name == "pinned-commit" {
					locked.Kind = dependency.ResolutionCommit
					locked.ReleaseID = 0
					locked.Tag = ""
				}
				state.Project.Dependencies = []dependency.Declaration{{Source: ffaSource, Requested: requested}}
				state.Lock.Dependencies = []dependency.LockedDependency{locked}
			}
			if name == "current" {
				remote.Latest[ffaSource] = dependency.Release{ID: 42, Tag: "v0.9.38"}
				remote.Commits[ffaSource+"@v0.9.38"] = ffaCommit
			}
			if name == "outdated" {
				remote.Latest[ffaSource] = dependency.Release{ID: 43, Tag: "v0.9.39"}
				remote.Commits[ffaSource+"@v0.9.39"] = strings.Repeat("b", 40)
			}
			if err := dependency.WriteState(root, state); err != nil {
				t.Fatal(err)
			}
			before := reverify2HashTree(t, root)
			text := strings.ToLower(ffaRun(t, remote, root, 0, "outdated"))
			switch name {
			case "empty":
				if !strings.Contains(text, "no dependencies") || strings.Contains(text, "are current") {
					t.Errorf("empty state misleading: %q", text)
				}
			case "pinned-tag", "pinned-commit":
				if !strings.Contains(text, "no latest") || strings.Contains(text, "are current") {
					t.Errorf("pinned-only state misleading: %q", text)
				}
			case "current":
				if !strings.Contains(text, "current") || strings.Contains(text, "no latest") {
					t.Errorf("current state=%q", text)
				}
			case "outdated":
				if !strings.Contains(text, "outdated") || strings.Contains(text, "are current") {
					t.Errorf("outdated state=%q", text)
				}
				output := ffaRun(t, remote, root, 0, "outdated", "--json")
				if !strings.Contains(output, ffaSource) || !strings.Contains(output, "v0.9.39") {
					t.Errorf("outdated JSON lost candidate: %q", output)
				}
			}
			if !reflect.DeepEqual(before, reverify2HashTree(t, root)) {
				t.Fatal("outdated changed project state")
			}
		})
	}
	for _, name := range []string{"absent", "malformed"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if name == "malformed" {
				reverify2Put(t, root, "agents.yaml", "schemaVersion: [broken", 0o644)
			}
			want := 1
			if name == "absent" {
				want = 0
			}
			output := ffaRun(t, dependencytest.NewRemote(), root, want, "outdated")
			if name == "absent" && !strings.Contains(strings.ToLower(output), "no dependencies") {
				t.Errorf("absent state did not report empty: %s", output)
			}
			if strings.Contains(output, "All latest dependencies are current") {
				t.Fatalf("invalid project certified current: %s", output)
			}
		})
	}
}
