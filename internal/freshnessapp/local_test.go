package freshnessapp

import (
	"errors"
	"os"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
)

func TestLocalAuthorizationFailureIsNotNetworkFailure(t *testing.T) {
	failure := &dependency.LocalSourceError{Code: "local_source_unauthorized", Err: &os.PathError{Op: "open", Path: "local.json", Err: os.ErrNotExist}}
	got := classifyFailure(failure)
	if got.Code != CodeUpdateFailed || got.Outcome != freshness.OutcomeFailed || !errors.Is(got, failure) {
		t.Fatalf("local failure classified as %+v", got)
	}
}
