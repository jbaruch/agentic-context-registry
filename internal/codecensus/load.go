// Package codecensus checks the finite string vocabulary reaching selected Go
// fields. It is a source-analysis tool for the CLI documentation contract, not
// a runtime validator or a general Go interpreter.
package codecensus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Source is one non-test Go input. Package is its full import path.
type Source struct {
	Package, Filename string
	Content           []byte
}

// Repository reads the build-selected production sources of the complete module.
// Go supplies export data only for external imports; local bodies are checked
// from source. No tests, generated test mains, or dependency source are scanned.
func Repository(root string) ([]Source, types.Importer, error) {
	cmd := exec.Command("go", "list", "-deps", "-export", "-json", "./...")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return nil, nil, fmt.Errorf("list census inputs: %w: %s", err, e.Stderr)
		}
		return nil, nil, err
	}
	exports := map[string]string{}
	var sources []Source
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var p struct {
			ImportPath, Dir, Export string
			GoFiles, CgoFiles       []string
			Module                  *struct{ Main bool }
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, fmt.Errorf("decode census inputs: %w", err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module == nil || !p.Module.Main {
			continue
		}
		for _, name := range append(p.GoFiles, p.CgoFiles...) {
			filename := filepath.Join(p.Dir, name)
			content, err := os.ReadFile(filename)
			if err != nil {
				return nil, nil, err
			}
			relative, err := filepath.Rel(root, filename)
			if err != nil {
				return nil, nil, err
			}
			sources = append(sources, Source{p.ImportPath, filepath.ToSlash(relative), content})
		}
	}
	fallback := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		filename := exports[path]
		if filename == "" {
			return nil, fmt.Errorf("missing export data for %s", path)
		}
		return os.Open(filename)
	})
	return sources, fallback, nil
}

type sourceImporter struct {
	initializers []*types.Initializer
	files        map[string][]*ast.File
	packages     map[string]*types.Package
	info         *types.Info
	fset         *token.FileSet
	fallback     types.Importer
}

func (s *sourceImporter) Import(path string) (*types.Package, error) {
	if p := s.packages[path]; p != nil {
		return p, nil
	}
	files, ok := s.files[path]
	if !ok {
		if s.fallback == nil {
			return nil, fmt.Errorf("no source for import %s", path)
		}
		return s.fallback.Import(path)
	}
	config := types.Config{Importer: s}
	p, err := config.Check(path, s.fset, files, s.info)
	if err != nil {
		return nil, err
	}
	// Check records dependencies first. Copy entries before the next Check
	// reuses Info.InitOrder for another package.
	s.initializers = append(s.initializers, s.info.InitOrder...)
	s.packages[path] = p
	return p, nil
}

func parse(sources []Source, fallback types.Importer) (*sourceImporter, error) {
	s := &sourceImporter{files: map[string][]*ast.File{}, packages: map[string]*types.Package{}, fset: token.NewFileSet(), fallback: fallback,
		info: &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}}
	ordered := append([]Source(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Filename < ordered[j].Filename })
	for _, source := range ordered {
		f, err := parser.ParseFile(s.fset, source.Filename, source.Content, 0)
		if err != nil {
			return nil, err
		}
		s.files[source.Package] = append(s.files[source.Package], f)
	}
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, err := s.Import(path); err != nil {
			return nil, err
		}
	}
	return s, nil
}
