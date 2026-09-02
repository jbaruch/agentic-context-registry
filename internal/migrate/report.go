package migrate

import (
	"fmt"
	"sort"
	"strings"
)

const (
	classMigratable  = "migratable"
	classUnmapped    = "unmapped"
	classAmbiguous   = "ambiguous"
	classUnsupported = "unsupported"

	kindRule  = "rule"
	kindSkill = "skill"
	kindHook  = "hook"
)

// Report is the schemaVersion 1 Tessl inventory consumed by #2 and #8.
type Report struct {
	SchemaVersion int             `json:"schemaVersion"`
	DryRun        bool            `json:"dryRun"`
	Wrote         bool            `json:"wrote"`
	Agents        []AgentCoverage `json:"agents"`
	Packages      []PackageReport `json:"packages"`
	Preserved     []PathRecord    `json:"preserved"`
	Unmapped      []PathRecord    `json:"unmapped"`
	Ambiguous     []PathRecord    `json:"ambiguous"`
	Unsupported   []PathRecord    `json:"unsupported"`
}

// AgentCoverage records whether ACR can realize one Tessl native tree.
type AgentCoverage struct {
	ID       string   `json:"id"`
	Covered  bool     `json:"covered"`
	Evidence []string `json:"evidence"`
}

// PackageReport is one installed Tessl package and its classified artifacts.
type PackageReport struct {
	Name             string           `json:"name"`
	TesslIdentity    string           `json:"tesslIdentity"`
	Version          string           `json:"version"`
	Manifest         string           `json:"manifest"`
	PackageMapping   string           `json:"packageMapping"`
	MappingCandidate string           `json:"mappingCandidate"`
	Artifacts        []ArtifactReport `json:"artifacts"`
}

// ArtifactReport is one logical artifact independent of native filenames.
type ArtifactReport struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Classification string            `json:"classification"`
	Activation     *ActivationReport `json:"activation,omitempty"`
	Event          string            `json:"event,omitempty"`
	Digest         string            `json:"digest,omitempty"`
	Lossy          []string          `json:"lossy,omitempty"`
	Natives        []string          `json:"natives,omitempty"`
}

// ActivationReport is the #4 rule activation after Tessl frontmatter mapping.
type ActivationReport struct {
	Mode  string   `json:"mode"`
	Paths []string `json:"paths,omitempty"`
}

// PathRecord is one project path that is not a package artifact.
type PathRecord struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func emptyReport() Report {
	return Report{
		SchemaVersion: 1,
		DryRun:        true,
		Wrote:         false,
		Agents:        []AgentCoverage{},
		Packages:      []PackageReport{},
		Preserved:     []PathRecord{},
		Unmapped:      []PathRecord{},
		Ambiguous:     []PathRecord{},
		Unsupported:   []PathRecord{},
	}
}

func sortReport(report *Report) {
	sort.Slice(report.Agents, func(left, right int) bool { return report.Agents[left].ID < report.Agents[right].ID })
	for index := range report.Agents {
		sort.Strings(report.Agents[index].Evidence)
	}
	sort.Slice(report.Packages, func(left, right int) bool { return report.Packages[left].Name < report.Packages[right].Name })
	for index := range report.Packages {
		artifacts := report.Packages[index].Artifacts
		sort.Slice(artifacts, func(left, right int) bool {
			if artifacts[left].Kind != artifacts[right].Kind {
				return artifacts[left].Kind < artifacts[right].Kind
			}
			return artifacts[left].ID < artifacts[right].ID
		})
		for artifact := range artifacts {
			sort.Strings(artifacts[artifact].Natives)
			sort.Strings(artifacts[artifact].Lossy)
		}
		report.Packages[index].Artifacts = artifacts
	}
	sortPathRecords(report.Preserved)
	sortPathRecords(report.Unmapped)
	sortPathRecords(report.Ambiguous)
	sortPathRecords(report.Unsupported)
}

func sortPathRecords(records []PathRecord) {
	sort.Slice(records, func(left, right int) bool {
		if records[left].Path != records[right].Path {
			return records[left].Path < records[right].Path
		}
		return records[left].Reason < records[right].Reason
	})
}

// FormatText renders the inventory grouped by package and classification.
func FormatText(report Report) string {
	var builder strings.Builder
	builder.WriteString("Tessl inventory (dry-run; no files written)\n")
	builder.WriteString("\nAgents\n")
	if len(report.Agents) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, agent := range report.Agents {
		coverage := "uncovered"
		if agent.Covered {
			coverage = "covered"
		}
		fmt.Fprintf(&builder, "  %-12s %-10s %s\n", agent.ID, coverage, strings.Join(agent.Evidence, ", "))
	}
	for _, pkg := range report.Packages {
		fmt.Fprintf(&builder, "\nPackage %s (%s, %s", pkg.Name, pkg.Manifest, pkg.PackageMapping)
		if pkg.MappingCandidate != "" {
			fmt.Fprintf(&builder, " → %s", pkg.MappingCandidate)
		}
		builder.WriteString(")\n")
		groups := map[string][]ArtifactReport{}
		for _, artifact := range pkg.Artifacts {
			groups[artifact.Classification] = append(groups[artifact.Classification], artifact)
		}
		for _, class := range []string{classMigratable, classAmbiguous, classUnmapped, classUnsupported} {
			fmt.Fprintf(&builder, "  %s\n", class)
			artifacts := groups[class]
			if len(artifacts) == 0 {
				builder.WriteString("    (none)\n")
				continue
			}
			for _, artifact := range artifacts {
				line := artifact.Kind + " " + artifact.ID
				if artifact.Event != "" {
					line += " " + artifact.Event
				}
				if artifact.Activation != nil {
					line += " " + artifact.Activation.Mode
				}
				if artifact.Digest != "" {
					line += " " + artifact.Digest
				}
				fmt.Fprintf(&builder, "    %s\n", line)
			}
		}
	}
	writePathSection(&builder, "Preserved", report.Preserved)
	writePathSection(&builder, "Unmapped", report.Unmapped)
	writePathSection(&builder, "Ambiguous", report.Ambiguous)
	writePathSection(&builder, "Unsupported", report.Unsupported)
	return builder.String()
}

func writePathSection(builder *strings.Builder, title string, records []PathRecord) {
	fmt.Fprintf(builder, "\n%s\n", title)
	if len(records) == 0 {
		builder.WriteString("  (none)\n")
		return
	}
	for _, record := range records {
		fmt.Fprintf(builder, "  %s  %s\n", record.Path, record.Reason)
	}
}

func appendUnique(records []PathRecord, record PathRecord) []PathRecord {
	for _, existing := range records {
		if existing.Path == record.Path && existing.Reason == record.Reason {
			return records
		}
	}
	return append(records, record)
}
