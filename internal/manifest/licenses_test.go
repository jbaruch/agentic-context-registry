package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRootLicensesAgreeAcrossInventories(t *testing.T) {
	t.Parallel()
	for _, names := range [][]string{nil, {"LICENSE"}, {"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md", "COPYING.txt", "UNLICENSE", "UNLICENSE.md", "UNLICENSE.txt"}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			root := writeTestPackage(t, validManifest)
			for _, name := range append(slices.Clone(names), "nested/LICENSE", "LICENSE-dir/hidden", "README.md", "LICENſE", "UNLICENſE") {
				writeTestFile(t, root, name, "notice bytes\x00\r\n")
			}
			value, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{Filename, "rules/project-guidance.md"}, names...)
			slices.Sort(want)
			check := func(label string, got []string, err error) {
				t.Helper()
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %v, %v; want %v", label, got, err, want)
				}
			}
			_, got, err := LoadListed(root, nil)
			check("filesystem", got, err)
			got, err = PlannedPackageFilesFS(os.DirFS(root), value)
			check("planned", got, err)
			list := func(relative string, directory bool) ([]string, error) {
				if relative != "." {
					return []string{relative}, nil
				}
				// Providers can include descendants, duplicates and arbitrary order.
				listed := append(slices.Clone(names), "nested/LICENSE", "README.md")
				listed = append(listed, names...)
				slices.Reverse(listed)
				return listed, nil
			}
			got, err = SelectListed(value, list, func(string) (fs.FileMode, error) { return 0o644, nil })
			check("selected", got, err)
			_, got, err = LoadListed(root, list)
			check("listed", got, err)
			opened, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := opened.Close(); err != nil {
					t.Error(err)
				}
			})
			content, err := ContentFiles(root, value)
			if err != nil || !reflect.DeepEqual(content, []string{Filename, "rules/project-guidance.md"}) {
				t.Fatalf("content closure = %v, %v", content, err)
			}
			_, got, err = LoadPackageBounded(opened, 100, 1<<20)
			if err != nil || !reflect.DeepEqual(got, content) {
				t.Fatalf("bounded content closure = %v, %v; want %v", got, err, content)
			}
		})
	}
}

func TestDeclaredRootLicenseIsDeduplicatedAndRemainsContent(t *testing.T) {
	root := writeTestPackage(t, strings.ReplaceAll(validManifest, "rules/project-guidance.md", "LICENSE"))
	writeTestFile(t, root, "LICENSE", "declared notice\n")
	value, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"LICENSE", Filename}
	_, got, err := LoadListed(root, nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("distribution = %v, %v", got, err)
	}
	got, err = ContentFiles(root, value)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("content = %v, %v", got, err)
	}
	got, err = SelectListed(value, func(name string, _ bool) ([]string, error) {
		if name == "." {
			return []string{"LICENSE", "LICENSE"}, nil
		}
		return []string{name}, nil
	}, func(string) (fs.FileMode, error) { return 0o644, nil })
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("listed = %v, %v", got, err)
	}
}

func TestRootLicenseSafetyDoesNotChangeManifestValidationOrHashClosure(t *testing.T) {
	root := writeTestPackage(t, validManifest)
	if err := os.Symlink("rules/project-guidance.md", filepath.Join(root, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	value, err := Load(root)
	if err != nil {
		t.Fatalf("implicit license affected Load: %v", err)
	}
	if _, err := ContentFiles(root, value); err != nil {
		t.Fatalf("implicit license affected content: %v", err)
	}
	want := `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is a symbolic link; ACR automatically includes root license files; replace it with a regular-file copy or rename it to a name outside the supported root license allowlist`
	if _, _, err := LoadListed(root, nil); err == nil || err.Error() != want {
		t.Fatalf("distribution accepted symlink: %v", err)
	}
	if _, err := PlannedPackageFilesFS(os.DirFS(root), value); err == nil || err.Error() != want {
		t.Fatalf("planned symlink error = %v; want %s", err, want)
	}
	if _, _, err := LoadListed(root, func(name string, _ bool) ([]string, error) {
		if name == "." {
			return []string{"LICENSE"}, nil
		}
		return []string{name}, nil
	}); err == nil || err.Error() != want {
		t.Fatalf("listed accepted symlink: %v", err)
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, got, err := LoadPackageBounded(opened, 100, 1<<20); err != nil || !reflect.DeepEqual(got, []string{Filename, "rules/project-guidance.md"}) {
		t.Fatalf("implicit symlink affected bounded content closure: %v, %v", got, err)
	}
	for _, mode := range []fs.FileMode{fs.ModeNamedPipe, fs.ModeSocket, fs.ModeDevice} {
		tree := fstest.MapFS{"LICENSE": {Mode: mode}}
		want := `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is not a regular file; ACR automatically includes root license files; replace it with a regular file or rename it to a name outside the supported root license allowlist`
		if _, err := collectPackageFilesFS(tree, Manifest{}); err == nil || err.Error() != want {
			t.Fatalf("accepted special mode %v: %v", mode, err)
		}
	}
}

func TestListedRootLicenseMissingOrDirectory(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "tracked-deletion", true: "gitlink-directory"}[directory], func(t *testing.T) {
			root := writeTestPackage(t, validManifest)
			want := `manifest validation failed: path_not_found at LICENSE: root license "LICENSE" is missing; ACR automatically includes root license files; restore the file or commit its deletion before retrying`
			if directory {
				if err := os.Mkdir(filepath.Join(root, "LICENSE"), 0o755); err != nil {
					t.Fatal(err)
				}
				want = `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is not a regular file; ACR automatically includes root license files; replace it with a regular file or rename it to a name outside the supported root license allowlist`
			}
			_, _, err := LoadListed(root, func(name string, _ bool) ([]string, error) {
				if name == "." {
					return []string{"LICENSE"}, nil
				}
				return []string{name}, nil
			})
			if err == nil || err.Error() != want {
				t.Fatalf("listed license error = %v; want %s", err, want)
			}
		})
	}
}

func TestRootLicenseListingFailuresPropagate(t *testing.T) {
	sentinel := errors.New("root listing failed")
	_, err := SelectListed(Manifest{}, func(name string, _ bool) ([]string, error) {
		if name == "." {
			return nil, sentinel
		}
		return []string{name}, nil
	}, func(string) (fs.FileMode, error) { return 0o644, nil })
	if !errors.Is(err, sentinel) {
		t.Fatalf("listing failure = %v", err)
	}
	if _, err := collectPackageFilesFS(os.DirFS(filepath.Join(t.TempDir(), "missing")), Manifest{}); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "list package root") {
		t.Fatalf("filesystem root listing failure = %v", err)
	}
	// Local snapshots never enumerate implicit root notices, even under a tight budget.
	root := writeTestPackage(t, validManifest)
	if _, _, err := LoadListed(root, func(name string, _ bool) ([]string, error) {
		if name == "." {
			return nil, sentinel
		}
		return []string{name}, nil
	}); !errors.Is(err, sentinel) {
		t.Fatalf("LoadListed root failure = %v", err)
	}
	for _, name := range []string{"LICENSE-a", "LICENSE-b", "LICENSE-c"} {
		writeTestFile(t, root, name, "notice")
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, got, err := LoadPackageBounded(opened, 5, 1<<20)
	if err != nil || !reflect.DeepEqual(got, []string{Filename, "rules/project-guidance.md"}) {
		t.Fatalf("implicit notices consumed local budget or entered snapshot: %v, %v", got, err)
	}
}

func TestExactRootLicenseNames(t *testing.T) {
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md", "COPYING.txt", "UNLICENSE", "UNLICENSE.md", "UNLICENSE.txt"} {
		calls := 0
		got, err := CheckRootLicense(name, func(string) (fs.FileMode, error) { calls++; return 0o755, nil })
		if !IsRootLicense(name) || !got || err != nil || calls != 1 {
			t.Fatalf("%q: %v, %v, calls=%d", name, got, err, calls)
		}
	}
	for _, name := range []string{"license", "License.md", "LICENSE.MD", "LICENSEfoo", "UNLICENSEfoo", "LICENSE.pyc", "UnLiCeNsE.pyo", "LICENSE-MIT", "LICENSE\\MIT", "LICENSE-café", "licenſe", "nested/LICENSE", "LICENSE\x00MIT"} {
		got, err := CheckRootLicense(name, func(string) (fs.FileMode, error) { t.Fatalf("unselected %q inspected", name); return 0, nil })
		if IsRootLicense(name) || got || err != nil {
			t.Fatalf("%q: %v, %v", name, got, err)
		}
		image := fstest.MapFS{Filename: {Data: []byte(validManifest)}, "rules/project-guidance.md": {Data: []byte("rule")}, name: {Mode: fs.ModeSymlink}}
		if _, err := collectPackageFilesFS(image, Manifest{}); err != nil {
			t.Fatalf("unselected %q inspected: %v", name, err)
		}
	}
	root := writeTestPackage(t, validManifest)
	writeTestFile(t, root, "LICENSE/hidden", "not a root file")
	if _, files, err := LoadListed(root, nil); err != nil || slices.Contains(files, "LICENSE/hidden") {
		t.Fatalf("directory selected: %v, %v", files, err)
	}
}

func TestDistributionNamespaceAcrossInventories(t *testing.T) {
	for _, tc := range []struct {
		paths []string
		field string
	}{
		{[]string{"LICENSE", "licenſe"}, "licenſe"},
		{[]string{"LICENSE", "license/guidance.md"}, "license/guidance.md"},
		{[]string{"license", "LICENSE/SKILL.md"}, "license"},
		{[]string{"café/a", "cafe\u0301/b"}, "café/a"},
		{[]string{"A/x", "a/y"}, "a/y"},
		{[]string{"x", "x/y"}, "x/y"},
		{[]string{"LICENSE\\MIT"}, "LICENSE\\MIT"},
		{[]string{"LICENSE\x00MIT"}, "LICENSE\x00MIT"},
		{[]string{"a/../x"}, "a/../x"},
	} {
		t.Run(strings.Join(tc.paths, "+"), func(t *testing.T) {
			value, err := decodeManifest([]byte(validManifest), ".")
			if err != nil {
				t.Fatal(err)
			}
			value.Artifacts.Rules = nil
			image := fstest.MapFS{Filename: {Data: []byte("manifest")}}
			for index, name := range tc.paths {
				value.Artifacts.Rules = append(value.Artifacts.Rules, RuleArtifact{ID: fmt.Sprintf("rule-%d", index), Path: name, Activation: RuleActivation{Mode: ActivationAlways}})
				image[name] = &fstest.MapFile{Data: []byte("content")}
			}
			var want string
			for range 2 {
				list := func(name string, _ bool) ([]string, error) {
					if name == "." {
						return tc.paths, nil
					}
					return []string{name}, nil
				}
				_, selectedErr := SelectListed(value, list, func(string) (fs.FileMode, error) { return 0o644, nil })
				_, fsErr := collectPackageFilesFS(image, value)
				failures := []error{ValidateDistributionPaths(tc.paths), selectedErr, fsErr}
				if tc.field != "x/y" && !strings.ContainsAny(tc.field, "\\\x00") && tc.field != "a/../x" {
					_, plannedErr := PlannedPackageFilesFS(image, value)
					failures = append(failures, plannedErr)
				}
				for _, err := range failures {
					var failure *ValidationErrors
					if !errors.As(err, &failure) || len(failure.Issues) != 1 || failure.Issues[0].Code != CodeInvalidPath || failure.Issues[0].Field != tc.field || !strings.Contains(err.Error(), "rename") {
						t.Fatalf("namespace refusal: %v; want invalid_path at %q", err, tc.field)
					}
					if want == "" {
						want = err.Error()
					}
					if err.Error() != want {
						t.Fatalf("unstable refusal: %v; want %s", err, want)
					}
				}
				slices.Reverse(tc.paths)
			}
		})
	}
	for _, paths := range [][]string{{"shared/a", "shared/b", "shared/a"}, {"café/a"}, {"cafe\u0301/a"}, {"ß", "ss"}} {
		if err := ValidateDistributionPaths(paths); err != nil {
			t.Fatalf("valid %v: %v", paths, err)
		}
	}
}
