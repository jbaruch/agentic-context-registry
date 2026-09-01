package adaptertest

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// NEW-3: injected NotExist is absence; any other error still propagates.
// Complements the skip-removal: helpers must not treat every failure as
// missing, and must not skip when the seam injects a deterministic error.
func TestThirdRoundWantsErrorDistinguishesNotExistFromOtherErrors(t *testing.T) {
	t.Parallel()

	absent, err := wantsErrorWith("missing/error.json", func(string) (os.FileInfo, error) {
		return nil, fs.ErrNotExist
	})
	if err != nil || absent {
		t.Fatalf("wantsErrorWith(NotExist) = %t, %v, want false, nil (absence, not a skip)", absent, err)
	}

	injected := errors.New("injected EIO")
	_, err = wantsErrorWith("broken/error.json", func(string) (os.FileInfo, error) {
		return nil, injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("wantsErrorWith(EIO) error = %v, want the injected error", err)
	}
}
