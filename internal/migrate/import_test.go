package migrate

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMigratePackageImportsNoWriter(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate migrate package directory")
	}
	dir := filepath.Dir(filename)
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, dir, func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := packages["migrate"]
	if !ok {
		t.Fatal("package migrate not found")
	}
	for name, file := range pkg.Files {
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			if imported == "os" || imported == "github.com/jbaruch/agentic-context-registry/internal/realize" {
				t.Errorf("%s imports %s; inventory must not write or realize", filepath.Base(name), imported)
			}
		}
	}
}
