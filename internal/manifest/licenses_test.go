package manifest

import (
	"errors"
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
	for _, names := range [][]string{nil, {"LICENSE"}, {"LICENSE", "license-MIT.txt", "LiCeNcE.md", "CoPyInG.LESSER", "UnLiCeNsE-notice"}} {
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
			got, err = SelectListed(value, list)
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
			_, got, err = LoadPackageBounded(opened, 100, 1<<20)
			check("bounded", got, err)
			content, err := ContentFiles(root, value)
			if err != nil || !reflect.DeepEqual(content, []string{Filename, "rules/project-guidance.md"}) {
				t.Fatalf("content closure = %v, %v", content, err)
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
	})
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
	want := `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is a symbolic link; ACR automatically includes root license files; replace it with a regular-file copy or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*`
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
	if _, _, err := LoadPackageBounded(opened, 100, 1<<20); err == nil || err.Error() != want {
		t.Fatalf("bounded accepted symlink: %v", err)
	}
	for _, mode := range []fs.FileMode{fs.ModeNamedPipe, fs.ModeSocket, fs.ModeDevice} {
		tree := fstest.MapFS{"LICENSE": {Mode: mode}}
		want := `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is not a regular file; ACR automatically includes root license files; replace it with a regular file or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*`
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
				want = `manifest validation failed: invalid_artifact_type at LICENSE: root license "LICENSE" is not a regular file; ACR automatically includes root license files; replace it with a regular file or rename it so it no longer matches LICENSE*, LICENCE*, COPYING*, or UNLICENSE*`
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
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("listing failure = %v", err)
	}
	if _, err := collectPackageFilesFS(os.DirFS(filepath.Join(t.TempDir(), "missing")), Manifest{}); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "list package root") {
		t.Fatalf("filesystem root listing failure = %v", err)
	}
	// The root listing consumes the same entry budget as artifact traversal.
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
	_, _, err = LoadPackageBounded(opened, 5, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "filesystem entries") {
		t.Fatalf("root listing bypassed budget: %v", err)
	}
}
