package freshness

import (
	"embed"
	"io/fs"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const (
	// Source is the synthetic package's stable owner identity.
	Source = "github:jbaruch/agentic-context-registry"
	// ArtifactID is the generated hook's stable ownership identity.
	ArtifactID = "freshness-session-start"
	// SourcePath is the hook's stable package path recorded in the ledger.
	SourcePath = "internal/freshness/session-start.sh"
)

//go:embed session-start.sh
var embedded embed.FS

// HookPackage returns the synthetic package rendered through every selected
// native adapter. PolicyNone intentionally contributes no package.
func HookPackage(policy Policy) (adapter.Package, bool) {
	if policy == PolicyNone {
		return adapter.Package{}, false
	}
	return adapter.Package{
		Source: Source,
		Root:   prefixedFS{FS: embedded},
		Manifest: manifest.Manifest{Artifacts: manifest.Artifacts{Hooks: []manifest.HookArtifact{{
			ID: ArtifactID, Event: manifest.HookSessionStart, Path: SourcePath,
			Args: []string{"--policy", string(policy)},
		}}}},
	}, true
}

// prefixedFS exposes the embedded basename at its stable repository-relative
// source path without requiring runtime access to the source checkout.
type prefixedFS struct {
	FS fs.FS
}

func (filesystem prefixedFS) Open(name string) (fs.File, error) {
	if name != SourcePath {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return filesystem.FS.Open("session-start.sh")
}
