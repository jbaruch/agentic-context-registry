package publishapp

import (
	"context"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
)

type releases interface {
	LookupRelease(context.Context, dependency.Repository, string) (dependency.Release, bool, error)
	DownloadReleaseAsset(context.Context, dependency.Repository, dependency.ReleaseAsset) ([]byte, error)
	TagCommit(context.Context, dependency.Repository, string) (string, bool, error)
	CreateRelease(context.Context, dependency.Repository, string, string) (dependency.Release, error)
	UploadAsset(context.Context, dependency.Repository, int64, string, string, []byte) (dependency.ReleaseAsset, []byte, error)
	PublishRelease(context.Context, dependency.Repository, int64) (dependency.Release, error)
	DeleteRelease(context.Context, dependency.Repository, int64) error
}
