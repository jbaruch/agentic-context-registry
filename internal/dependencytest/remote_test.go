package dependencytest

import (
	"context"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

func TestRemoteCopiesArchiveBytes(t *testing.T) {
	t.Parallel()

	remote := NewRemote()
	repository := dependency.Repository{Owner: "owner", Name: "package"}
	key := repository.String() + "@" + "0123456789abcdef0123456789abcdef01234567"
	remote.Archives[key] = []byte("archive")

	first, err := remote.DownloadArchive(context.Background(), repository, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'A'
	second, err := remote.DownloadArchive(context.Background(), repository, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(second), "archive"; got != want {
		t.Fatalf("DownloadArchive() = %q, want %q", got, want)
	}
}

var _ dependency.Remote = (*Remote)(nil)
