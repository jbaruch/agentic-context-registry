package migrateapp

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
	"github.com/jbaruch/agentic-context-registry/internal/migrate"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
)

// MCP blocker codes.
const (
	blockerMCPAmbiguous = "mcp-ambiguous"
	blockerMCPMalformed = "mcp-malformed-config"
	blockerMCPOwnership = "mcp-ownership-changed"
)

// malformedConfigBlockers reports the supported agent configs that do not
// parse. A config ACR cannot read is a refusal, and it has to be raised before
// realization: the preservation compiler reaches the same file first and would
// fail the whole command with a decoder message instead of the blocker report
// finalization owes the operator.
func malformedConfigBlockers(inventory migrate.Report) []migrate.Blocker {
	var blockers []migrate.Blocker
	for _, entry := range inventory.MCP {
		if entry.Disposition != migrate.MCPAmbiguous || entry.Reason != "malformed-config" {
			continue
		}
		blockers = append(blockers, migrate.Blocker{
			Code: blockerMCPMalformed, Path: entry.Path, Kind: "structured-entry", Detail: mcpBlockerDetail(entry),
			Remedy: fmt.Sprintf("repair %s so it parses, then re-run 'acr migrate tessl --finalize'", entry.Path),
		})
	}
	return blockers
}

// malformedConfigRetentions names the same files as deliberately untouched.
func malformedConfigRetentions(inventory migrate.Report) []migrate.RetentionRecord {
	var retained []migrate.RetentionRecord
	for _, entry := range inventory.MCP {
		if entry.Disposition != migrate.MCPAmbiguous || entry.Reason != "malformed-config" {
			continue
		}
		retained = append(retained, migrate.RetentionRecord{Path: entry.Path, Kind: "structured-entry", Reason: entry.Reason})
	}
	return retained
}

// mcpRetirementPlan splices the Tessl MCP server entry out of each supported
// agent's config, inside the same finalization transaction as every other
// edit.
//
// Only an entry keyed exactly tessl whose whole object matches the shape real
// Tessl writes is retired. A user's differently named server survives whatever
// command it runs, and an entry keyed tessl that ACR cannot prove is retained
// byte for byte and blocks with a remedy — deleting it on the strength of its
// name would destroy configuration ACR never authored.
//
// Diagnostics name the file, the container, the key, the field names and a
// digest. No command string, argument value or environment value is ever
// carried out of this function: an MCP entry can hold a credential, and these
// records reach stdout, the JSON envelope and CI logs.
func mcpRetirementPlan(snapshot adapter.Snapshot, inventory migrate.Report, managed map[string][]string) ([]migrate.FinalizeEdit, []migrate.RetentionRecord, []migrate.Blocker, error) {
	var edits []migrate.FinalizeEdit
	var retained []migrate.RetentionRecord
	var blockers []migrate.Blocker
	for _, entry := range inventory.MCP {
		switch entry.Disposition {
		case migrate.MCPForeign:
			// An unsupported agent's config is evidence, never a mutation
			// target: ACR does not own that surface and cannot replace it.
			retained = append(retained, migrate.RetentionRecord{Path: entry.Path, Kind: "mcp-config", Reason: entry.Reason})
		case migrate.MCPAmbiguous:
			code := blockerMCPAmbiguous
			remedy := fmt.Sprintf("remove the %q server from %s yourself, then re-run 'acr migrate tessl --finalize'", entry.Key, entry.Path)
			if entry.Reason == "malformed-config" {
				code = blockerMCPMalformed
				remedy = fmt.Sprintf("repair %s so it parses, then re-run 'acr migrate tessl --finalize'", entry.Path)
			}
			retained = append(retained, migrate.RetentionRecord{
				Path: entry.Path, Kind: "structured-entry", ID: entry.Container + "." + entry.Key, Reason: entry.Reason,
			})
			blockers = append(blockers, migrate.Blocker{
				Code: code, Path: entry.Path, Kind: "structured-entry", ID: entry.Container + "." + entry.Key,
				Detail: mcpBlockerDetail(entry), Remedy: remedy,
			})
		case migrate.MCPCanonical:
			edit, err := mcpRetirementEdit(snapshot, entry, managed[entry.Path])
			if err != nil {
				return nil, nil, nil, err
			}
			if edit != nil {
				edits = append(edits, *edit)
			}
		}
	}
	return edits, retained, blockers, nil
}

// mcpBlockerDetail names the field that failed the predicate and the field
// names present, plus the digest that identifies the entry without rendering
// it.
func mcpBlockerDetail(entry migrate.MCPEntry) string {
	detail := entry.Reason
	if entry.Detail != "" {
		detail += "; " + entry.Detail
	}
	if len(entry.Fields) != 0 {
		detail += "; fields present: " + joinFields(entry.Fields)
	}
	if entry.Digest != "" {
		detail += "; entry " + entry.Digest
	}
	return detail
}

func joinFields(fields []string) string {
	result := ""
	for index, field := range fields {
		if index != 0 {
			result += ", "
		}
		result += field
	}
	return result
}

func mcpRetirementEdit(snapshot adapter.Snapshot, entry migrate.MCPEntry, managedHashes []string) (*migrate.FinalizeEdit, error) {
	config, known := migrate.MCPRetirementConfig(entry.Path)
	if !known {
		return nil, fmt.Errorf("no MCP retirement contract for %q", entry.Path)
	}
	observed, err := snapshot.ReadFile(entry.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	selector := preserve.ForeignSelector{
		Container: config.Container, Kind: adapter.ConfigField, Key: entry.Key,
	}
	if config.Format == adapter.ConfigTOML {
		// The TOML document records the three fields but no location for the
		// [mcp_servers.tessl] header, so removing the fields alone would leave
		// an orphan header behind.
		selector = preserve.ForeignSelector{Container: append(append([]string(nil), config.Container...), entry.Key), Table: true}
	}
	after, removed, err := preserve.RemoveForeignConfigEntries(config.Format, entry.Path, observed.Content, []preserve.ForeignSelector{selector}, managedHashes)
	if err != nil {
		return nil, &Error{Code: "finalization_blocked", Message: fmt.Sprintf("retire the Tessl MCP entry in %s: %v", entry.Path, err), Cause: err,
			Remedy: fmt.Sprintf("remove the %q server from %s yourself, then re-run 'acr migrate tessl --finalize'", entry.Key, entry.Path)}
	}
	edit := migrate.FinalizeEdit{
		Path: entry.Path, Kind: "structured-entry", ID: entry.Container + "." + entry.Key, Operation: "splice",
		Before: append([]byte(nil), observed.Content...), After: append([]byte(nil), after...),
		Mode: observed.Mode.Perm(), Hash: migrate.HashFinalizationContent(observed.Content),
	}
	for _, item := range removed {
		edit.Removed = append(edit.Removed, migrate.RemovalRecord{
			Path: entry.Path, Kind: "structured-entry", ID: entry.Container + "." + entry.Key,
			Operation: "splice", Hash: migrate.HashFinalizationContent(item.Raw),
		})
	}
	return &edit, nil
}
