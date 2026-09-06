package migrateapp

import (
	"fmt"

	"github.com/jbaruch/agentic-context-registry/internal/migrate"
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

// Retirement contract for the Tessl MCP server entry in each supported
// agent's config, spliced inside the same finalization transaction as every
// other edit.
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
// mcpRetirementDecisions classifies every MCP record the inventory produced
// into the hosts whose canonical entry may be retired, the evidence retained,
// and the refusals. It plans no splice: the splice is composed against the
// exact bytes planning reads for that host, so ownership is bound to what the
// transaction will actually accept as its before-image.
func mcpRetirementDecisions(inventory migrate.Report) (map[string]migrate.MCPEntry, []migrate.RetentionRecord, []migrate.Blocker) {
	canonical := make(map[string]migrate.MCPEntry)
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
			canonical[entry.Path] = entry
		}
	}
	return canonical, retained, blockers
}

// mcpOwnershipBlocker refuses a host whose Tessl entry no longer matches the
// evidence the inventory classified.
func mcpOwnershipBlocker(entry migrate.MCPEntry, reason string) migrate.Blocker {
	return migrate.Blocker{
		Code: blockerMCPOwnership, Path: entry.Path, Kind: "structured-entry", ID: entry.Container + "." + entry.Key,
		Detail: "the " + entry.Key + " entry changed after it was inventoried: " + reason,
		Remedy: fmt.Sprintf("re-run 'acr migrate tessl --vendor-unmapped' to re-inventory %s, then finalize", entry.Path),
	}
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
