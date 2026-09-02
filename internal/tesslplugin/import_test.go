package tesslplugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPackageHasOneWriteCallSite(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate tesslplugin package directory")
	}
	dir := filepath.Dir(filename)
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, dir, func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := packages["tesslplugin"]
	if !ok {
		t.Fatal("package tesslplugin not found")
	}

	var osWrites []string
	openFileCount := 0
	for name, file := range pkg.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, isIdent := selector.X.(*ast.Ident)
			switch selector.Sel.Name {
			case "Create", "CreateTemp", "WriteFile", "Remove", "RemoveAll", "Rename", "Chmod", "Truncate", "Mkdir", "MkdirAll", "CopyFS", "Link", "Symlink":
				osWrites = append(osWrites, filepath.Base(name)+":"+selector.Sel.Name)
			case "OpenFile":
				openFileCount++
				if isIdent && ident.Name == "os" {
					osWrites = append(osWrites, filepath.Base(name)+":os.OpenFile")
				}
			}
			return true
		})
	}
	if len(osWrites) != 0 {
		t.Fatalf("tesslplugin production code references os write symbols %v", osWrites)
	}
	if openFileCount != 1 {
		t.Fatalf("OpenFile call sites = %d, want 1 (Root.OpenFile for agent-plugin.yaml)", openFileCount)
	}
}
