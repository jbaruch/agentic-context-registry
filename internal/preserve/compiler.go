package preserve

import (
	"context"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// ConflictError reports a preservation boundary that requires user action.
type ConflictError struct {
	Code    string
	Path    string
	Message string
}

func (err *ConflictError) Error() string {
	return fmt.Sprintf("%s: %s: %s", err.Code, err.Path, err.Message)
}

// Compiler is the production preservation-aware SharedCompiler.
type Compiler struct{}

// NewCompiler constructs the production preservation-aware compiler.
func NewCompiler() *Compiler {
	return &Compiler{}
}

// DiscoverIncludeGraph implements adapter.IncludeGraphProvider.
func (*Compiler) DiscoverIncludeGraph(project adapter.Snapshot, selectedRoots []string) (adapter.InstructionIncludeGraph, error) {
	return DiscoverSelectedIncludeGraph(project, selectedRoots)
}

// CompileMarkdown implements adapter.SharedCompiler.
func (*Compiler) CompileMarkdown(ctx context.Context, request adapter.MarkdownCompileRequest) (adapter.SharedCompilation, error) {
	if err := ctx.Err(); err != nil {
		return adapter.SharedCompilation{}, err
	}
	return compileMarkdown(request)
}

// CompileConfig implements adapter.SharedCompiler. Structured compilation is
// supplied by jsondoc.go and tomldoc.go.
func (*Compiler) CompileConfig(ctx context.Context, request adapter.ConfigCompileRequest) (adapter.SharedCompilation, error) {
	if err := ctx.Err(); err != nil {
		return adapter.SharedCompilation{}, err
	}
	return compileConfig(request)
}

func conflict(code, path, message string) error {
	return &ConflictError{Code: code, Path: path, Message: message}
}
