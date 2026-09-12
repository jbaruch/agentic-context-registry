package tesslplugin

import (
	"errors"
	"fmt"
	"os"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// Map reuses the closed Tessl parser and artifact mapping without writing.
// A nonempty targetRepository selects clean-mode identity; empty retains the
// compatibility contract. The caller owns cleanup and repository-level paths.
func Map(opts Options, targetRepository, targetVersion string) (value manifest.Manifest, report Report, err error) {
	report = newReport(opts.DryRun)
	root, err := os.OpenRoot(opts.PackageRoot)
	if err != nil {
		return value, report, err
	}
	defer func() {
		if e := root.Close(); e != nil {
			err = errors.Join(err, fmt.Errorf("close package: %w", e))
		}
	}()
	return MapRoot(root, opts, targetRepository, targetVersion)
}

// MapRoot keeps parsing, artifact discovery and complete validation on one
// opened package. It does not reopen opts.PackageRoot or close the caller's root.
func MapRoot(root *os.Root, opts Options, targetRepository, targetVersion string) (value manifest.Manifest, report Report, err error) {
	report = newReport(opts.DryRun)
	sources, err := ReadRoot(root)
	if err != nil {
		recordUnmapped(&report, err)
		return manifest.Manifest{}, report, err
	}
	if err := checkAmbiguous(root, sources); err != nil {
		return manifest.Manifest{}, report, err
	}

	value, report, err = buildManifest(root, sources, opts, targetRepository, targetVersion)
	if err != nil {
		recordUnmapped(&report, err)
		return manifest.Manifest{}, report, err
	}
	sortManifest(&value)
	if err := validateConverted(root, value); err != nil {
		return manifest.Manifest{}, report, err
	}
	files, err := publishedFromManifest(root, value)
	if err != nil {
		return value, report, err
	}
	if err := rejectUnpublishable(files); err != nil {
		return value, report, err
	}
	report.Artifacts = reportArtifacts(value)
	report.PublishedFiles = files
	sortReport(&report)
	return value, report, nil
}
