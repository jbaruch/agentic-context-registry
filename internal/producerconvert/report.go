// Package producerconvert plans deterministic ACR-only producer migrations.
package producerconvert

import (
	"fmt"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/tesslplugin"
)

const ReceiptPath = ".acr-producer-migration.json"
const transactionPath = ".acr-producer-transaction"

// Options binds a clean migration to its selected source and explicit target.
type Options struct {
	PackageRoot         string `json:"packageRoot"`
	Repository          string `json:"repository"`
	PackageVersion      string `json:"packageVersion,omitempty"`
	AcceptAgentWidening bool   `json:"acceptAgentWidening"`
	DryRun              bool   `json:"-"`
}

// Change is an exact before/after diff, including deletions and permission bits.
// Content is text, not a patch recipe; the transaction uses private before-images.
type Change struct {
	Path       string `json:"path"`
	Operation  string `json:"operation"`
	Before     string `json:"before"`
	After      string `json:"after"`
	BeforeMode uint32 `json:"beforeMode"`
	AfterMode  uint32 `json:"afterMode"`
	Diff       string `json:"diff"`
}

type Blocker struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Report struct {
	ReportVersion  int                          `json:"reportVersion"`
	DryRun         bool                         `json:"dryRun"`
	Wrote          bool                         `json:"wrote"`
	Current        bool                         `json:"current"`
	RepositoryRoot string                       `json:"repositoryRoot"`
	SourcePackage  string                       `json:"sourcePackage"`
	SourceVersion  string                       `json:"sourceVersion"`
	Package        string                       `json:"package"`
	Version        string                       `json:"version"`
	Manifest       string                       `json:"manifest"`
	Receipt        string                       `json:"receipt"`
	Artifacts      []tesslplugin.ArtifactRecord `json:"artifacts"`
	PublishedFiles []string                     `json:"publishedFiles"`
	Changes        []Change                     `json:"changes"`
	Blockers       []Blocker                    `json:"blockers"`
	Notes          []string                     `json:"notes"`
}

type Error struct {
	Code   string
	Path   string
	Reason string
}

func (e *Error) Error() string               { return fmt.Sprintf("%s: %s", e.Path, e.Reason) }
func refuse(code, path, reason string) error { return &Error{Code: code, Path: path, Reason: reason} }

func FormatText(r Report) string {
	var b strings.Builder
	if len(r.Blockers) != 0 {
		b.WriteString("Clean conversion refused before writes:\n")
		for _, v := range r.Blockers {
			fmt.Fprintf(&b, "  %s: %s\n", v.Path, v.Reason)
		}
		return b.String()
	}
	action := "Converted"
	if r.DryRun {
		action = "Would convert"
	} else if r.Current {
		action = "Already current"
	}
	fmt.Fprintf(&b, "%s %s %s → %s %s\nroot: %s\n", action, r.SourcePackage, r.SourceVersion, r.Package, r.Version, r.RepositoryRoot)
	for _, n := range r.Notes {
		fmt.Fprintln(&b, n)
	}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "%s %s (mode %04o → %04o)\n%s", c.Operation, c.Path, c.BeforeMode, c.AfterMode, c.Diff)
	}
	b.WriteString("Published files:\n")
	for _, f := range r.PublishedFiles {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return b.String()
}

func exactDiff(name, before, after string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", name, name)
	lines := func(body string) (int, int) {
		if body == "" {
			return 0, 0
		}
		n := strings.Count(body, "\n")
		if !strings.HasSuffix(body, "\n") {
			n++
		}
		return 1, n
	}
	oldStart, oldCount := lines(before)
	newStart, newCount := lines(after)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for _, part := range []struct{ prefix, body string }{{"-", before}, {"+", after}} {
		if part.body == "" {
			continue
		}
		lines := strings.SplitAfter(part.body, "\n")
		for _, line := range lines {
			if line != "" {
				b.WriteString(part.prefix + line)
				if !strings.HasSuffix(line, "\n") {
					b.WriteString("\n\\ No newline at end of file\n")
				}
			}
		}
	}
	return b.String()
}
