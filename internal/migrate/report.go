package migrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
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

// MigrationReport is the deterministic coexistence apply/dry-run contract.
type MigrationReport struct {
	SchemaVersion     int                 `json:"schemaVersion"`
	DryRun            bool                `json:"dryRun"`
	Wrote             bool                `json:"wrote"`
	Mode              string              `json:"mode"`
	FinalizationReady bool                `json:"finalizationReady"`
	Mappings          []Mapping           `json:"mappings"`
	Project           dependency.Project  `json:"project"`
	Lock              dependency.Lockfile `json:"lock"`
	Plan              MigrationPlan       `json:"plan"`
	ToolOwned         []OwnershipRecord   `json:"toolOwned"`
	TesslOwned        []OwnershipRecord   `json:"tesslOwned"`
	Unmanaged         []OwnershipRecord   `json:"unmanaged"`
	EffectiveDiffs    []EffectiveDiff     `json:"effectiveDiffs"`
	Notes             []CoexistenceNote   `json:"notes"`
	Vendored          []VendoredPackage   `json:"vendored"`
	Removed           []RemovalRecord     `json:"removed"`
	Retained          []RetentionRecord   `json:"retained"`
	Reanchored        []ReanchoredTarget  `json:"reanchored"`
	StaleReferences   []StaleReference    `json:"staleReferences"`
}

// VendoredPackage reports one reproducible local dependency copy.
type VendoredPackage struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Version     string `json:"version"`
	ContentHash string `json:"contentHash"`
}

// RemovalRecord is one Tessl-owned item removed by finalization.
type RemovalRecord struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	Hash        string `json:"hash"`
	Operation   string `json:"operation"`
	Replacement string `json:"replacement,omitempty"`
}

// RetentionRecord is evidence deliberately left in place.
type RetentionRecord struct {
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason"`
}

// ReanchoredTarget records a shared ledger hash updated after a splice.
type ReanchoredTarget struct {
	Path       string `json:"path"`
	BeforeHash string `json:"beforeHash"`
	AfterHash  string `json:"afterHash"`
}

// StaleReference points at surviving text that names removed Tessl state.
type StaleReference struct {
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Text        string `json:"text"`
	Replacement string `json:"replacement,omitempty"`
}

// MigrationPlan is the report-safe projection of the realization plan.
type MigrationPlan struct {
	Operations    []MigrationOperation `json:"operations"`
	LedgerChanged bool                 `json:"ledgerChanged"`
}

// MigrationOperation omits generated content while retaining reviewable intent.
type MigrationOperation struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// OwnershipRecord identifies one path or owned entry on the migration surface.
type OwnershipRecord struct {
	Path    string `json:"path"`
	Kind    string `json:"kind,omitempty"`
	ID      string `json:"id,omitempty"`
	Package string `json:"package,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// CoexistenceNote is one non-fatal condition that can still block deletion.
type CoexistenceNote struct {
	Code      string   `json:"code"`
	Event     string   `json:"event,omitempty"`
	Path      string   `json:"path,omitempty"`
	IgnoredBy string   `json:"ignoredBy,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Artifacts int      `json:"artifacts,omitempty"`
	Tessl     string   `json:"tessl,omitempty"`
	ACR       string   `json:"acr,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Paths     []string `json:"paths,omitempty"`
}

// FormatCoexistenceText renders ownership, diffs, and safety notes.
func FormatCoexistenceText(report MigrationReport) string {
	var builder strings.Builder
	state := "applied"
	if report.DryRun {
		state = "dry-run"
	}
	finalization := "ready"
	blocked := len(report.EffectiveDiffs)
	for _, note := range report.Notes {
		if note.Code == "uncovered-agent" || note.Code == "ambiguous" || note.Code == "unsupported" || note.Code == "lossy" {
			blocked++
		}
	}
	if !report.FinalizationReady {
		finalization = fmt.Sprintf("blocked (%d)", blocked)
	}
	finalizeMode := report.Mode == "finalize" || report.Mode == "finalized"
	if finalizeMode {
		fmt.Fprintf(&builder, "Tessl finalization %s. Removed: %d; retained: %d\n", state, len(report.Removed), len(report.Retained))
	} else {
		fmt.Fprintf(&builder, "Coexistence %s. Finalization: %s\n", state, finalization)
	}
	if len(report.Vendored) != 0 {
		builder.WriteString("\nVendored packages\n")
		for _, item := range report.Vendored {
			fmt.Fprintf(&builder, "  %s  %s  %s\n", item.Source, item.Destination, item.ContentHash)
		}
	}
	if finalizeMode {
		builder.WriteString("\nRemoved (rollback)\n")
		for _, item := range report.Removed {
			fmt.Fprintf(&builder, "  %s  %s  %s\n", item.Operation, item.Path, item.Hash)
			if item.Replacement != "" {
				fmt.Fprintf(&builder, "    replacement: %s\n", item.Replacement)
			}
		}
		builder.WriteString("\nRetained Tessl output\n")
		if len(report.Retained) == 0 {
			builder.WriteString("  (none)\n")
		}
		for _, item := range report.Retained {
			fmt.Fprintf(&builder, "  %s  %s\n", item.Path, item.Reason)
		}
		builder.WriteString("\nStale tracked references\n")
		if len(report.StaleReferences) == 0 {
			builder.WriteString("  (none)\n")
		}
		for _, item := range report.StaleReferences {
			fmt.Fprintf(&builder, "  %s:%d  %s\n", item.Path, item.Line, item.Text)
			if item.Replacement != "" {
				fmt.Fprintf(&builder, "    replacement: %s\n", item.Replacement)
			}
		}
		builder.WriteString("  Scan is limited to Git-tracked files; out-of-repository references cannot be detected.\n")
	}
	writeOwnershipSection(&builder, "Tool-owned", report.ToolOwned)
	writeOwnershipSection(&builder, "Tessl-owned (frozen)", report.TesslOwned)
	writeOwnershipSection(&builder, "Unmanaged (preserved)", report.Unmanaged)
	builder.WriteString("\nEffective differences\n")
	if len(report.EffectiveDiffs) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, diff := range report.EffectiveDiffs {
		fmt.Fprintf(&builder, "  %s %s %s  %s\n", diff.Package, diff.Kind, diff.ID, diff.Reason)
	}
	for _, note := range report.Notes {
		if note.Code == "duplicate-effect" {
			fmt.Fprintf(&builder, "WARNING duplicate-effect %s: %s + %s\n", note.Event, note.Tessl, note.ACR)
			continue
		}
		var evidence []string
		if note.Path != "" {
			evidence = append(evidence, note.Path)
		}
		if note.Agent != "" {
			evidence = append(evidence, fmt.Sprintf("agent=%s", note.Agent), fmt.Sprintf("artifacts=%d", note.Artifacts))
		}
		if note.IgnoredBy != "" {
			evidence = append(evidence, "ignored by "+note.IgnoredBy)
		}
		if note.Detail != "" {
			evidence = append(evidence, note.Detail)
		}
		if len(note.Paths) != 0 {
			evidence = append(evidence, "paths="+strings.Join(note.Paths, ","))
		}
		fmt.Fprintf(&builder, "NOTE %s: %s\n", note.Code, strings.Join(evidence, "; "))
	}
	return builder.String()
}

func writeOwnershipSection(builder *strings.Builder, title string, records []OwnershipRecord) {
	fmt.Fprintf(builder, "\n%s\n", title)
	if len(records) == 0 {
		builder.WriteString("  (none)\n")
		return
	}
	for _, record := range records {
		fmt.Fprintf(builder, "  %s  %s %s\n", record.Path, record.Kind, record.ID)
	}
}

// SortMigrationReport canonicalizes every order-bearing report field.
func SortMigrationReport(report *MigrationReport) {
	sort.Slice(report.Vendored, func(i, j int) bool { return report.Vendored[i].Source < report.Vendored[j].Source })
	sort.Slice(report.Removed, func(i, j int) bool {
		return report.Removed[i].Path+"\x00"+report.Removed[i].Kind+"\x00"+report.Removed[i].ID < report.Removed[j].Path+"\x00"+report.Removed[j].Kind+"\x00"+report.Removed[j].ID
	})
	sort.Slice(report.Retained, func(i, j int) bool {
		return report.Retained[i].Path+"\x00"+report.Retained[i].Kind+"\x00"+report.Retained[i].ID < report.Retained[j].Path+"\x00"+report.Retained[j].Kind+"\x00"+report.Retained[j].ID
	})
	sort.Slice(report.Reanchored, func(i, j int) bool { return report.Reanchored[i].Path < report.Reanchored[j].Path })
	sort.Slice(report.StaleReferences, func(i, j int) bool {
		if report.StaleReferences[i].Path != report.StaleReferences[j].Path {
			return report.StaleReferences[i].Path < report.StaleReferences[j].Path
		}
		return report.StaleReferences[i].Line < report.StaleReferences[j].Line
	})
	sort.Slice(report.Mappings, func(i, j int) bool { return report.Mappings[i].From < report.Mappings[j].From })
	sortOwnership(report.ToolOwned)
	sortOwnership(report.TesslOwned)
	sortOwnership(report.Unmanaged)
	sort.Slice(report.EffectiveDiffs, func(i, j int) bool {
		return effectiveKeyLess(report.EffectiveDiffs[i].EffectiveKey, report.EffectiveDiffs[j].EffectiveKey)
	})
	sort.Slice(report.Notes, func(i, j int) bool {
		left := report.Notes[i].Code + "\x00" + report.Notes[i].Event + "\x00" + report.Notes[i].Path + "\x00" + report.Notes[i].Agent
		right := report.Notes[j].Code + "\x00" + report.Notes[j].Event + "\x00" + report.Notes[j].Path + "\x00" + report.Notes[j].Agent
		return left < right
	})
	for index := range report.Notes {
		sort.Strings(report.Notes[index].Paths)
	}
}

func sortOwnership(records []OwnershipRecord) {
	sort.Slice(records, func(i, j int) bool {
		left := records[i].Path + "\x00" + records[i].Kind + "\x00" + records[i].ID
		right := records[j].Path + "\x00" + records[j].Kind + "\x00" + records[j].ID
		return left < right
	})
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
		for _, class := range []string{classMigratable, classAmbiguous, classUnsupported} {
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
