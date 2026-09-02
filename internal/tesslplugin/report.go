package tesslplugin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

const reportVersion = 1

// ArtifactRecord is one converted artifact in the conversion report.
type ArtifactRecord struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Event string `json:"event,omitempty"`
}

// IgnoredItem is an ignore-file line echoed without interpretation.
type IgnoredItem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// NoteItem is informational conversion context.
type NoteItem struct {
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

// UnmappedItem is a blocking Tessl concept that prevented a write.
type UnmappedItem struct {
	Field  string `json:"field"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Report is the producer-conversion result envelope payload.
type Report struct {
	ReportVersion  int              `json:"reportVersion"`
	DryRun         bool             `json:"dryRun"`
	Wrote          bool             `json:"wrote"`
	Manifest       string           `json:"manifest"`
	SourceManifest string           `json:"sourceManifest"`
	Package        string           `json:"package"`
	Version        string           `json:"version"`
	Artifacts      []ArtifactRecord `json:"artifacts"`
	Lossy          []LossyItem      `json:"lossy"`
	Ignored        []IgnoredItem    `json:"ignored"`
	Unmapped       []UnmappedItem   `json:"unmapped"`
	Notes          []NoteItem       `json:"notes,omitempty"`
	PublishedFiles []string         `json:"publishedFiles"`
}

func newReport() Report {
	return Report{
		ReportVersion:  reportVersion,
		Manifest:       manifest.Filename,
		Artifacts:      []ArtifactRecord{},
		Lossy:          []LossyItem{},
		Ignored:        []IgnoredItem{},
		Unmapped:       []UnmappedItem{},
		Notes:          []NoteItem{},
		PublishedFiles: []string{},
	}
}

func reportArtifacts(value manifest.Manifest) []ArtifactRecord {
	records := make([]ArtifactRecord, 0, len(value.Artifacts.Rules)+len(value.Artifacts.Skills)+len(value.Artifacts.Hooks)+len(value.Artifacts.Scripts))
	for _, rule := range value.Artifacts.Rules {
		records = append(records, ArtifactRecord{ID: rule.ID, Kind: "rule", Path: rule.Path})
	}
	for _, skill := range value.Artifacts.Skills {
		records = append(records, ArtifactRecord{ID: skill.ID, Kind: "skill", Path: skill.Path})
	}
	for _, script := range value.Artifacts.Scripts {
		records = append(records, ArtifactRecord{ID: script.ID, Kind: "script", Path: script.Path})
	}
	for _, hook := range value.Artifacts.Hooks {
		records = append(records, ArtifactRecord{ID: hook.ID, Kind: "hook", Path: hook.Path, Event: string(hook.Event)})
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].Kind != records[right].Kind {
			return records[left].Kind < records[right].Kind
		}
		return records[left].ID < records[right].ID
	})
	return records
}

func sortReport(report *Report) {
	sort.Slice(report.Lossy, func(left, right int) bool {
		if report.Lossy[left].Field != report.Lossy[right].Field {
			return report.Lossy[left].Field < report.Lossy[right].Field
		}
		return report.Lossy[left].Reason < report.Lossy[right].Reason
	})
	sort.Slice(report.Ignored, func(left, right int) bool {
		if report.Ignored[left].Path != report.Ignored[right].Path {
			return report.Ignored[left].Path < report.Ignored[right].Path
		}
		return report.Ignored[left].Reason < report.Ignored[right].Reason
	})
	sort.Slice(report.Notes, func(left, right int) bool {
		if report.Notes[left].Path != report.Notes[right].Path {
			return report.Notes[left].Path < report.Notes[right].Path
		}
		return report.Notes[left].Reason < report.Notes[right].Reason
	})
	sort.Slice(report.Unmapped, func(left, right int) bool {
		return report.Unmapped[left].Field < report.Unmapped[right].Field
	})
}

// FormatText renders a human-readable conversion report.
func FormatText(report Report) string {
	var builder strings.Builder
	action := "Converted"
	if report.DryRun {
		action = "Would convert"
	}
	if !report.Wrote && !report.DryRun {
		action = "Already current"
	}
	fmt.Fprintf(&builder, "%s %s → %s\n", action, report.SourceManifest, report.Manifest)
	fmt.Fprintf(&builder, "package: %s %s\n", report.Package, report.Version)
	fmt.Fprintf(&builder, "artifacts: %d\n", len(report.Artifacts))
	writeLossySection(&builder, "lossy", report.Lossy)
	writeIgnoredSection(&builder, "ignored", report.Ignored)
	writeUnmappedSection(&builder, "unmapped", report.Unmapped)
	if len(report.Notes) != 0 {
		builder.WriteString("notes:\n")
		for _, note := range report.Notes {
			fmt.Fprintf(&builder, "  - %s %s\n", note.Path, note.Reason)
		}
	}
	return builder.String()
}

func writeLossySection(builder *strings.Builder, title string, items []LossyItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(builder, "%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(builder, "  - %s: %s\n", item.Field, item.Value)
	}
}

func writeIgnoredSection(builder *strings.Builder, title string, items []IgnoredItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(builder, "%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(builder, "  - %s (%s)\n", item.Path, item.Reason)
	}
}

func writeUnmappedSection(builder *strings.Builder, title string, items []UnmappedItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(builder, "%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(builder, "  - %s: %s\n", item.Field, item.Reason)
	}
}
