package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

// CanonicalConfigOwnerKey returns the stable ownership key for one native
// structured hook entry.
func CanonicalConfigOwnerKey(owner OwnerRef, adapterID, target, nativeEvent string) string {
	payload := "acr-config-owner-v1\x00" + owner.Source + "\x00" + owner.ArtifactID + "\x00" +
		string(owner.Kind) + "\x00" + string(owner.Event) + "\x00" + adapterID + "\x00" + target + "\x00" + nativeEvent
	digest := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// ShellQuote quotes one argument for a POSIX shell command string.
func ShellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>()*?[]{}!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// ShellJoin appends arguments to command using POSIX shell quoting.
func ShellJoin(command string, args []string) string {
	var builder strings.Builder
	builder.WriteString(command)
	for _, argument := range args {
		builder.WriteByte(' ')
		builder.WriteString(ShellQuote(argument))
	}
	return builder.String()
}

// TOMLCommandHookValue encodes the native Codex matcher group as exactly one
// TOML inline-table value.
func TOMLCommandHookValue(command string) []byte {
	return []byte("{ hooks = [{ type = \"command\", command = " + strconv.Quote(command) + " }] }")
}

// SortedEvents returns the complete neutral v1 hook vocabulary in lexical
// order. Adapters still own the mapping to native spellings.
func SortedEvents() []manifest.HookEvent {
	return []manifest.HookEvent{
		manifest.HookPostToolUse,
		manifest.HookPreToolUse,
		manifest.HookSessionEnd,
		manifest.HookSessionStart,
		manifest.HookStop,
		manifest.HookUserPromptSubmit,
	}
}
