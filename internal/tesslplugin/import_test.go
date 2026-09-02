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
	renameCount := 0
	removeCount := 0
	for name, file := range pkg.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, isIdent := selector.X.(*ast.Ident)
			osReceiver := isIdent && ident.Name == "os"
			switch selector.Sel.Name {
			case "Create", "CreateTemp", "WriteFile", "RemoveAll", "Chmod", "Truncate", "Mkdir", "MkdirAll", "CopyFS", "Link", "Symlink":
				if osReceiver {
					osWrites = append(osWrites, filepath.Base(name)+":os."+selector.Sel.Name)
				}
			case "Remove":
				if osReceiver {
					osWrites = append(osWrites, filepath.Base(name)+":os.Remove")
				} else {
					removeCount++
				}
			case "Rename":
				if osReceiver {
					osWrites = append(osWrites, filepath.Base(name)+":os.Rename")
				} else {
					renameCount++
				}
			case "OpenFile":
				openFileCount++
				if osReceiver {
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
		t.Fatalf("OpenFile call sites = %d, want 1 (Root.OpenFile for the package-local temp manifest)", openFileCount)
	}
	if renameCount != 1 {
		t.Fatalf("Rename call sites = %d, want 1 (Root.Rename onto agent-plugin.yaml)", renameCount)
	}
	if removeCount != 1 {
		t.Fatalf("Remove call sites = %d, want 1 (Root.Remove of the package-local temp manifest)", removeCount)
	}
}
