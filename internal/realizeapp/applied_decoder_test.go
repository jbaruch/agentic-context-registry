package realizeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func replaceAppliedField(t *testing.T, data []byte, key, value string) []byte {
	t.Helper()
	prefix := `"` + key + `":`
	start := bytes.Index(data, []byte(prefix))
	if start < 0 {
		t.Fatalf("fixture has no %s", key)
	}
	start += len(prefix)
	decoder := json.NewDecoder(bytes.NewReader(data[start:]))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return []byte(string(data[:start]) + value + string(data[start+int(decoder.InputOffset()):]))
}

func TestAppliedReceiptDecoderMatrixAfterPull(t *testing.T) {
	tests := map[string]func(*testing.T, []byte) []byte{
		"root-null":          func(_ *testing.T, _ []byte) []byte { return []byte("null") },
		"root-array":         func(_ *testing.T, _ []byte) []byte { return []byte("[]") },
		"trailing-object":    func(_ *testing.T, data []byte) []byte { return append(data, []byte(" {}")...) },
		"trailing-null":      func(_ *testing.T, data []byte) []byte { return append(data, []byte(" null")...) },
		"trailing-malformed": func(_ *testing.T, data []byte) []byte { return append(data, '!') },
		"target-null": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "targets", "[null]")
		},
		"entry-null": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "entries", "[null]")
		},
		"target-wrong-container": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "targets", "[[]]")
		},
		"entry-wrong-container": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "entries", "[[]]")
		},
		"number-as-string": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "schema_version", `"1"`)
		},
		"fractional-mode": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "mode", "420.5")
		},
		"string-as-number": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "source", "1")
		},
		"boolean-as-string": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "excluded", `"true"`)
		},
		"duplicate-version-99-then-1": func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"schema_version":2`), []byte(`"schema_version":99,"schema_version":2`), 1)
		},
		"duplicate-ledger-object-then-null": func(_ *testing.T, data []byte) []byte {
			return append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"ledger":null}`)...)
		},
		"duplicate-ledger-object-then-object": func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"ledger":`), []byte(`"ledger":{"schemaVersion":1},"ledger":`), 1)
		},
		"escaped-equivalent-entry-keys": func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"source":`), []byte(`"so\u0075rce":"github:example/other","source":`), 1)
		},
		"escaped-equivalent-root-keys": func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"schema_version":`), []byte(`"schema_vers\u0069on":99,"schema_version":`), 1)
		},
		"shared-claim": func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"generated-only"`), []byte(`"shared"`), 1)
		},
		"whole-file-hash-mismatch": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "outputHash", `"sha256:`+strings.Repeat("e", 64)+`"`)
		},
		"invalid-hash": func(t *testing.T, data []byte) []byte {
			return replaceAppliedField(t, data, "managedHash", `"invalid"`)
		},
		"duplicate-canonical-path": func(t *testing.T, data []byte) []byte {
			ledger, err := decodeApplied(data)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Targets = append(ledger.Targets, ledger.Targets[0])
			encoded, err := marshalAppliedLedger(data, ledger)
			if err != nil {
				t.Fatal(err)
			}
			return encoded
		},
	}
	// Every level uses exact decoded key names; omission is allowed only for
	// omitempty fields, whereas null is rejected everywhere, including excluded.
	levels := map[string][]string{
		"receipt":  {"schema_version", "ledger", "checkout", "files"},
		"ledger":   {"schemaVersion", "targets"},
		"checkout": {"device", "inode"},
		"file":     {"size", "mtime_seconds", "mtime_nanos", "ctime_seconds", "ctime_nanos"},
		"target":   {"path", "owner", "mode", "ownership", "outputHash", "excluded", "entries"},
		"entry":    {"source", "artifactId", "artifactKind", "sourcePath", "adapter", "adapterVersion", "managedHash"},
	}
	for level, fields := range levels {
		for _, field := range fields {
			tests[level+"/case-"+field] = func(_ *testing.T, data []byte) []byte {
				return bytes.Replace(data, []byte(`"`+field+`":`), []byte(`"`+strings.ToUpper(field)+`":`), 1)
			}
			tests[level+"/null-"+field] = func(t *testing.T, data []byte) []byte {
				return replaceAppliedField(t, data, field, "null")
			}
			tests[level+"/wrong-type-"+field] = func(t *testing.T, data []byte) []byte {
				value := "[]"
				if field == "targets" || field == "entries" || field == "files" {
					value = "{}"
				}
				return replaceAppliedField(t, data, field, value)
			}
			tests[level+"/duplicate-"+field] = func(_ *testing.T, data []byte) []byte {
				return bytes.Replace(data, []byte(`"`+field+`":`), []byte(`"`+field+`":null,"`+field+`":`), 1)
			}
			if field != "targets" && field != "owner" && field != "excluded" {
				tests[level+"/missing-"+field] = func(t *testing.T, data []byte) []byte {
					replaced := replaceAppliedField(t, data, field, "null")
					member := []byte(`"` + field + `":null,`)
					if bytes.Contains(replaced, member) {
						return bytes.Replace(replaced, member, nil, 1)
					}
					return bytes.Replace(replaced, []byte(`,"`+field+`":null`), nil, 1)
				}
			}
		}
		first := fields[0]
		tests[level+"/unknown-field"] = func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"`+first+`":`), []byte(`"unknown":true,"`+first+`":`), 1)
		}
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			a, _, _, serviceB, pull := appliedPullFixture(t, true)
			// git receipts include excluded, shared-surface receipts include owner.
			var compact bytes.Buffer
			if err := json.Compact(&compact, readFile(t, filepath.Join(a, appliedPath))); err != nil {
				t.Fatal(err)
			}
			bad := corrupt(t, compact.Bytes())
			if _, err := decodeApplied(bad); err == nil {
				t.Fatalf("decoder accepted %s", name)
			}
			writeFixture(t, filepath.Join(a, appliedPath), bad, 0o644)
			pull()
			assertAppliedConflictUnchanged(t, a, serviceB, true)
		})
	}
}

func TestAppliedReceiptOptionalOmissionAndEscapedNames(t *testing.T) {
	for _, data := range []string{
		`{"schema_version":2,"ledger":{"schemaVersion":1},"checkout":{"device":0,"inode":0},"files":[]}`,
		`{"schema_vers\u0069on":2,"ledger":{"schemaVersion":1,"targets":[]},"checkout":{"device":0,"inode":0},"files":[]}`,
	} {
		if _, err := decodeApplied([]byte(data)); err != nil {
			t.Fatalf("valid optional omission/escaped exact key rejected: %v", err)
		}
	}
	a, _, _, _, _ := appliedPullFixture(t, false)
	ledger := readApplied(t, a)
	for _, target := range ledger.Targets {
		if target.Owner == "" && !target.Excluded {
			return // The exact generated fixture successfully decoded both omissions.
		}
	}
	t.Fatal("fixture did not exercise omitted owner and excluded")
}

func TestAppliedReceiptCanonicalAliasDuplicatesAreUnusable(t *testing.T) {
	a, _, _, serviceB, pull := appliedPullFixture(t, true)
	ledger := readApplied(t, a)
	for _, target := range ledger.Targets {
		if strings.HasSuffix(target.Path, "/notes.md") {
			alias := target
			alias.Path = strings.TrimSuffix(target.Path, "notes.md") + "NOTES.MD"
			original, err := os.Stat(filepath.Join(a, target.Path))
			if err != nil {
				t.Fatal(err)
			}
			other, err := os.Stat(filepath.Join(a, alias.Path))
			if errors.Is(err, os.ErrNotExist) {
				// Exercise the same filesystem identity check on sensitive storage.
				if err := os.Link(filepath.Join(a, target.Path), filepath.Join(a, alias.Path)); err != nil {
					t.Fatal(err)
				}
				other, err = os.Stat(filepath.Join(a, alias.Path))
			}
			if err != nil || !os.SameFile(original, other) {
				t.Fatalf("fixture alias: %v", err)
			}
			ledger.Targets = append(ledger.Targets, alias)
			break
		}
	}
	// On sensitive storage native validation also flags the hardlink spelling;
	// on insensitive storage it is the same leaf. Neither can authorize apply.
	writeApplied(t, a, ledger)
	_, _, err := realize.ComparisonLedger(a, loadLedger(t, a), ledger)
	if err == nil || !strings.Contains(err.Error(), "alias the same destination") {
		t.Fatalf("canonical duplicate receipt accepted: %v", err)
	}
	pull()
	before := hashProjectTree(t, a)
	if _, err := serviceB.Run(context.Background(), a, nil, realize.ModeApply); err == nil {
		t.Fatal("ambiguous receipt authorized apply")
	}
	if !reflect.DeepEqual(before, hashProjectTree(t, a)) {
		t.Fatal("canonical duplicate refusal wrote files")
	}
}
