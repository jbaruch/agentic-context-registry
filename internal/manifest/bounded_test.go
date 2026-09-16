package manifest

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestBoundedManifestRefusesBeforeDecode(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, Filename, strings.Repeat("# padding\n", 20)+"broken: [\n")
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, _, err = LoadPackageBounded(opened, 10, 64)
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("decoded oversized input: %v", err)
	}
	_, err = Load(root)
	if err == nil || !strings.Contains(err.Error(), "yaml:") {
		t.Fatalf("unbounded control did not reach syntax error: %v", err)
	}
}

func TestBoundedPackageMatchesReleaseInventory(t *testing.T) {
	for _, example := range []string{"minimal", "complete"} {
		t.Run(example, func(t *testing.T) {
			root := filepath.Join(repositoryRoot(t), "examples", example)
			value, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			want, err := PackageFiles(root, value)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := opened.Close(); err != nil {
					t.Error(err)
				}
			}()
			bounded, got, err := LoadPackageBounded(opened, 100, 1<<20)
			if err != nil || !reflect.DeepEqual(value, bounded) || !reflect.DeepEqual(got, want) {
				t.Fatalf("release mismatch: %v, %v, %v", got, want, err)
			}
			_, _, err = LoadPackageBounded(opened, 1, 1<<20)
			if err == nil || !strings.Contains(err.Error(), "filesystem entries") {
				t.Fatalf("entry budget not applied in validation: %v", err)
			}
		})
	}
}

// Counting ReadDir calls distinguishes streaming refusal from limiting only
// the result of fs.ReadDir, which would already have consumed the full tree.
type countedDirectoryFS struct {
	fs.FS
	reads, opens, closes int
}
type countedDirectory struct {
	fs.ReadDirFile
	owner *countedDirectoryFS
}

func (counted *countedDirectoryFS) Open(name string) (fs.File, error) {
	file, err := counted.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if directory, ok := file.(fs.ReadDirFile); ok {
		counted.opens++
		return &countedDirectory{directory, counted}, nil
	}
	return file, nil
}
func (directory *countedDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	directory.owner.reads++
	return directory.ReadDirFile.ReadDir(n)
}
func (directory *countedDirectory) Close() error {
	directory.owner.closes++
	return directory.ReadDirFile.Close()
}

func TestBoundedTraversalStopsBeforeReadingRemainingEntries(t *testing.T) {
	for _, directories := range []bool{false, true} {
		t.Run(map[bool]string{false: "files", true: "empty-directories"}[directories], func(t *testing.T) {
			tree := fstest.MapFS{}
			for _, name := range []string{"a", "b", "c", "d", "e"} {
				file := &fstest.MapFile{Data: []byte("support\n"), Mode: 0o644}
				if directories {
					file.Mode = fs.ModeDir | 0o755
				}
				tree["skill/"+name] = file
			}
			counted := &countedDirectoryFS{FS: tree}
			bounded := &boundedPackageFS{FS: counted, limit: 3, seen: make(map[string]struct{}), directories: make(map[string][]fs.DirEntry)}
			_, err := collectSkillFilesFS(bounded, "skill")
			if err == nil || !strings.Contains(err.Error(), "filesystem entries") {
				t.Fatalf("traversal not bounded: %v", err)
			}
			if counted.reads != 3 || counted.closes != counted.opens {
				t.Fatalf("read past limit or leaked directory: reads=%d closes=%d", counted.reads, counted.closes)
			}
		})
	}
}
