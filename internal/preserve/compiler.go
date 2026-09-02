package preserve

import (
	"context"
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

// Compiler is the production preservation-aware SharedCompiler.
type Compiler struct{}

// NewCompiler constructs the production preservation-aware compiler.
func NewCompiler() *Compiler {
	return &Compiler{}
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
	return fmt.Errorf("%s: %s: %s", code, path, message)
}
