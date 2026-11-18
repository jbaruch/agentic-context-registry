package realizeapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const appliedPath = ".agents/.acr-applied.json"
const appliedIgnorePath = ".agents/.gitignore"

// appliedReceipt belongs to this service, not the dependency lock. It records
// what this checkout applied before Git can replace the committed ledger.
type appliedReceipt struct {
	SchemaVersion int              `json:"schema_version"`
	Ledger        realize.Ledger   `json:"ledger"`
	Checkout      checkoutIdentity `json:"checkout"`
	Files         []appliedFile    `json:"files"`
}

type checkoutIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type appliedFile struct {
	Path          string `json:"path"`
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
	Size          int64  `json:"size"`
	ModSeconds    int64  `json:"mtime_seconds"`
	ModNanos      int64  `json:"mtime_nanos"`
	ChangeSeconds int64  `json:"ctime_seconds"`
	ChangeNanos   int64  `json:"ctime_nanos"`
}

func decodeApplied(data []byte) (realize.Ledger, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkApplied(decoder, "receipt"); err != nil {
		return realize.Ledger{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return realize.Ledger{}, fmt.Errorf("receipt must contain exactly one JSON object")
	}
	var receipt appliedReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return realize.Ledger{}, err
	}
	if receipt.SchemaVersion != 2 {
		return realize.Ledger{}, fmt.Errorf("unsupported or missing schema_version %d", receipt.SchemaVersion)
	}
	if err := realize.ValidateLedger(receipt.Ledger); err != nil {
		return realize.Ledger{}, err
	}
	seen := make(map[string]bool, len(receipt.Files))
	for _, file := range receipt.Files {
		if seen[file.Path] || file.Size < 0 || file.ModNanos < 0 || file.ModNanos >= 1e9 || file.ChangeNanos < 0 || file.ChangeNanos >= 1e9 {
			return realize.Ledger{}, fmt.Errorf("invalid or duplicate receipt file binding %q", file.Path)
		}
		seen[file.Path] = true
	}
	if len(seen) != len(receipt.Ledger.Targets) {
		return realize.Ledger{}, fmt.Errorf("receipt file bindings must match ledger targets")
	}
	for _, target := range receipt.Ledger.Targets {
		if !seen[target.Path] {
			return realize.Ledger{}, fmt.Errorf("missing receipt file binding %q", target.Path)
		}
		if target.Ownership != realize.OwnershipGenerated {
			return realize.Ledger{}, fmt.Errorf("receipt target %q must be generated-only", target.Path)
		}
		for _, entry := range target.Entries {
			if entry.ArtifactKind == realize.ArtifactFile && target.OutputHash != entry.ManagedHash {
				return realize.Ledger{}, fmt.Errorf("receipt target %q outputHash must match its file managedHash", target.Path)
			}
		}
	}
	return receipt.Ledger, nil
}

// Walk the finite receipt schema before typed decoding: encoding/json alone
// accepts duplicate/case-folded names and null scalars as valid struct input.
func walkApplied(decoder *json.Decoder, shape string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	var fields map[string]string
	switch shape {
	case "receipt":
		fields = map[string]string{"schema_version": "number", "ledger": "ledger", "checkout": "checkout", "files": "files"}
	case "checkout":
		fields = map[string]string{"device": "number", "inode": "number"}
	case "file":
		fields = map[string]string{"path": "string", "device": "number", "inode": "number", "size": "number", "mtime_seconds": "number", "mtime_nanos": "number", "ctime_seconds": "number", "ctime_nanos": "number"}
	case "ledger":
		fields = map[string]string{"schemaVersion": "number", "targets": "targets"}
	case "target":
		fields = map[string]string{"path": "string", "owner": "string", "mode": "number", "ownership": "string", "outputHash": "string", "excluded": "bool", "entries": "entries"}
	case "entry":
		fields = map[string]string{"source": "string", "artifactId": "string", "artifactKind": "string", "sourcePath": "string", "adapter": "string", "adapterVersion": "string", "managedHash": "string"}
	case "targets", "entries", "files":
		if token != json.Delim('[') {
			return fmt.Errorf("%s must be an array", shape)
		}
		item := "target"
		if shape == "entries" {
			item = "entry"
		} else if shape == "files" {
			item = "file"
		}
		for decoder.More() {
			if err := walkApplied(decoder, item); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case "string":
		if _, ok := token.(string); !ok {
			return fmt.Errorf("receipt value must be a string")
		}
		return nil
	case "number":
		if _, ok := token.(json.Number); !ok {
			return fmt.Errorf("receipt value must be a number")
		}
		return nil
	case "bool":
		if _, ok := token.(bool); !ok {
			return fmt.Errorf("receipt value must be a boolean")
		}
		return nil
	}
	if token != json.Delim('{') {
		return fmt.Errorf("%s must be an object", shape)
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || fields[name] == "" || seen[name] {
			return fmt.Errorf("unknown or duplicate %s key %q", shape, key)
		}
		seen[name] = true
		if err := walkApplied(decoder, fields[name]); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	for name := range fields {
		optional := (shape == "ledger" && name == "targets") || (shape == "target" && (name == "owner" || name == "excluded"))
		if !optional && !seen[name] {
			return fmt.Errorf("missing %s key %q", shape, name)
		}
	}
	return nil
}

// Binding is comparison evidence only. An unavailable or changed binding
// leaves the committed ledger's comparison in force for that target.
func boundApplied(project string, data []byte) (realize.Ledger, error) {
	ledger, err := decodeApplied(data)
	if err != nil {
		return realize.Ledger{}, err
	}
	// Validate the entire receipt's canonical destinations before discarding
	// stale bindings; invalid aliases must never be salvaged as partial evidence.
	if _, _, err := realize.ComparisonLedger(project, realize.Ledger{SchemaVersion: ledger.SchemaVersion}, ledger); err != nil {
		return realize.Ledger{}, err
	}
	var receipt appliedReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return realize.Ledger{}, err
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return realize.Ledger{}, realize.WrapProjectAccessError(project, "open project directory to bind applied receipt", err)
	}
	defer root.Close()
	checkout, ok := appliedCheckout(root)
	eligible := realize.Ledger{SchemaVersion: ledger.SchemaVersion}
	if !ok || checkout != receipt.Checkout {
		return eligible, nil
	}
	bindings := make(map[string]appliedFile, len(receipt.Files))
	for _, file := range receipt.Files {
		bindings[file.Path] = file
	}
	for _, target := range ledger.Targets {
		live, ok := statAppliedFile(root, target.Path)
		if ok && live == bindings[target.Path] {
			eligible.Targets = append(eligible.Targets, target)
		}
	}
	return eligible, nil
}

func appliedCheckout(root *os.Root) (checkoutIdentity, bool) {
	info, err := root.Stat(".")
	if err != nil {
		return checkoutIdentity{}, false
	}
	file, ok := appliedMetadata(info)
	return checkoutIdentity{Device: file.Device, Inode: file.Inode}, ok
}

func statAppliedFile(root *os.Root, name string) (appliedFile, bool) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return appliedFile{}, false
	}
	file, ok := appliedMetadata(info)
	file.Path = name
	return file, ok
}

func encodeApplied(project string, ledger realize.Ledger) ([]byte, error) {
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, realize.WrapProjectAccessError(project, "open project directory to encode applied receipt", err)
	}
	defer root.Close()
	checkout, available := appliedCheckout(root)
	receipt := appliedReceipt{SchemaVersion: 2, Checkout: checkout, Ledger: realize.Ledger{SchemaVersion: ledger.SchemaVersion}, Files: []appliedFile{}}
	if available {
		for _, target := range ledger.Targets {
			before, ok := statAppliedFile(root, target.Path)
			if !ok {
				continue
			}
			file, exists, err := realize.ReadStateFile(project, target.Path)
			if err != nil {
				return nil, err
			}
			after, ok := statAppliedFile(root, target.Path)
			if !ok || before != after || !exists || fmt.Sprintf("sha256:%x", sha256.Sum256(file.Content)) != target.OutputHash || file.Mode != target.Mode {
				continue
			}
			receipt.Ledger.Targets = append(receipt.Ledger.Targets, target)
			receipt.Files = append(receipt.Files, after)
		}
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	return append(data, '\n'), err
}

func generatedLedger(ledger realize.Ledger) realize.Ledger {
	generated := realize.Ledger{SchemaVersion: ledger.SchemaVersion}
	for _, target := range ledger.Targets {
		if target.Ownership == realize.OwnershipGenerated {
			generated.Targets = append(generated.Targets, target)
		}
	}
	return generated
}

func refuseTrackedApplied(projectDirectory string) error {
	tracked, err := realize.TrackedPathAliases(projectDirectory, appliedPath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return realize.WrapProjectAccessError(projectDirectory, "open project directory to check tracked applied receipt", err)
	}
	defer root.Close()
	receiptInfo, receiptErr := root.Stat(appliedPath)
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return realize.WrapProjectAccessError(projectDirectory, fmt.Sprintf("inspect applied receipt %q", appliedPath), receiptErr)
	}
	for indexed := range tracked {
		normalized := path.Clean(indexed)
		if !strings.EqualFold(normalized, appliedPath) {
			continue
		}
		info, err := root.Stat(normalized)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return realize.WrapProjectAccessError(projectDirectory, fmt.Sprintf("inspect potential tracked receipt alias %q", indexed), err)
		}
		// Hardlink detection is best effort, limited to receipt-name candidates;
		// unrelated tracked paths must not require a full-index metadata scan.
		alias := receiptErr == nil && err == nil && os.SameFile(receiptInfo, info)
		// A missing potential alias cannot establish that creating the reserved
		// spelling is safe on this filesystem. Refuse without changing the index.
		if normalized == appliedPath || alias || receiptErr != nil || err != nil {
			return fmt.Errorf("%s is tracked by Git; run 'git rm --cached -- %s' to keep last-applied evidence local, then retry 'acr realize'", indexed, indexed)
		}
	}
	return nil
}

func appliedIgnore(before realize.StateFile, exists bool) realize.StateFile {
	content := append([]byte(nil), before.Content...)
	// Require a canonical rule after every unescaped negation while preserving
	// existing bytes. Append a separator after an unterminated line if needed.
	found := false
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "!") {
			found = false
		} else if strings.TrimRight(strings.TrimSuffix(line, "\r"), " ") == "/.acr-applied.json" {
			found = true
		}
	}
	if !found {
		if len(content) != 0 && content[len(content)-1] != '\n' {
			content = append(content, '\n')
		}
		content = append(content, []byte("/.acr-applied.json\n")...)
	}
	mode := before.Mode
	if !exists {
		mode = 0o644
	}
	before.Content, before.Mode = content, mode
	return before
}
