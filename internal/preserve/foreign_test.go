package preserve

import (
	"bytes"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/adapter"
)

func TestAcrOwnershipSurvivesTesslSiblingRemoval(t *testing.T) {
	t.Parallel()
	content := []byte("{\n  \"hooks\": [\n    {\"command\":\"tessl hook run\"},\n    {\"command\":\"acr hook run\"}\n  ],\n  \"theme\": \"dark\"\n}\n")
	document, err := parseConfigDocument(adapter.ConfigJSON, ".claude/settings.json", content, false)
	if err != nil {
		t.Fatal(err)
	}
	var acrHash string
	for _, location := range document.locations() {
		if bytes.Contains(location.raw, []byte("acr hook run")) {
			acrHash = structuredEntryHash(adapter.ConfigJSON, location.container, location.kind, location.key, location.raw)
		}
	}
	if acrHash == "" {
		t.Fatal("did not locate ACR sibling")
	}
	result, removed, err := RemoveForeignConfigEntries(adapter.ConfigJSON, ".claude/settings.json", content, []ForeignSelector{{Container: []string{"hooks"}, Kind: adapter.ConfigElement, Raw: []byte(`{"command":"tessl hook run"}`)}}, []string{acrHash})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || bytes.Contains(result, []byte("tessl hook run")) || !bytes.Contains(result, []byte("acr hook run")) || !bytes.Contains(result, []byte(`"theme": "dark"`)) {
		t.Fatalf("foreign splice changed a sibling:\n%s\nremoved=%#v", result, removed)
	}
}

func TestForeignRemovalRefusesManagedEntry(t *testing.T) {
	t.Parallel()
	content := []byte(`{"hooks":[{"command":"acr hook run"}]}`)
	document, err := parseConfigDocument(adapter.ConfigJSON, ".cursor/hooks.json", content, false)
	if err != nil {
		t.Fatal(err)
	}
	location := document.locations()[0]
	digest := structuredEntryHash(adapter.ConfigJSON, location.container, location.kind, location.key, location.raw)
	_, _, err = RemoveForeignConfigEntries(adapter.ConfigJSON, ".cursor/hooks.json", content, []ForeignSelector{{Container: []string{"hooks"}, Kind: adapter.ConfigElement, Raw: location.raw}}, []string{digest})
	if err == nil {
		t.Fatal("managed entry was removable through foreign API")
	}
}
