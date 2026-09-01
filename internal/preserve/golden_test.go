package preserve

import (
	"context"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/adaptertest"
)

func TestSharedCompilerGoldens(t *testing.T) {
	fixture := adaptertest.NewReferenceAdapter("1.0.0")
	adaptertest.RunGolden(t, canonicalGoldenAdapter{Adapter: fixture}, NewCompiler())
}

type canonicalGoldenAdapter struct {
	adapter.Adapter
}

func (fixture canonicalGoldenAdapter) Render(ctx context.Context, request adapter.RenderRequest) ([]adapter.Output, error) {
	outputs, err := fixture.Adapter.Render(ctx, request)
	if err != nil {
		return nil, err
	}
	descriptor := fixture.Descriptor()
	for outputIndex := range outputs {
		for insertionIndex := range outputs[outputIndex].Markdown {
			insertion := &outputs[outputIndex].Markdown[insertionIndex]
			insertion.BlockID = adapter.CanonicalMarkdownBlockID(insertion.Owner, descriptor.ID)
		}
	}
	return outputs, nil
}
