package adapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDocumentedInstructionRootsMatchCandidateSlices(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "rulebundle.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string][]string)
	ast.Inspect(source, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "existingInstructionRoots" {
			return true
		}
		fallback, ok := stringLiteral(call.Args[2])
		if !ok {
			t.Fatalf("instruction-root fallback is not a string literal")
		}
		literal, ok := call.Args[1].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("candidate list for %s is not a composite literal", fallback)
		}
		for _, element := range literal.Elts {
			candidate, ok := stringLiteral(element)
			if !ok {
				t.Fatalf("candidate for %s is not a string literal", fallback)
			}
			got[fallback] = append(got[fallback], candidate)
		}
		return true
	})

	want := map[string][]string{
		"CLAUDE.md": {".claude/CLAUDE.md", "CLAUDE.md"},
		"AGENTS.md": {"AGENTS.md", "AGENTS.override.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("instruction-root candidates = %#v, want %#v", got, want)
	}

	document, err := os.ReadFile(filepath.Join("..", "..", "docs", "shared-files.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, candidates := range want {
		for _, candidate := range candidates {
			if !strings.Contains(string(document), "`"+candidate+"`") {
				t.Errorf("shared-files documentation does not name %s", candidate)
			}
		}
	}
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}
