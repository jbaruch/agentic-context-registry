package realize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/diagnostic"
)

const maxTargetBytes = 32 << 20

// FileTooLargeError identifies a regular file that exceeds the bounded reader.
// Callers may discard oversized evidence without discarding other I/O errors.
type FileTooLargeError struct {
	Path string
}

func (err *FileTooLargeError) Error() string {
	return fmt.Sprintf("target %q exceeds %d MiB; reduce the file size and retry", err.Path, maxTargetBytes>>20)
}

type fileSnapshot struct {
	exists     bool
	content    []byte
	mode       os.FileMode
	hash       string
	generation fileGeneration
}

// WrapProjectAccessError adds project context and recovery guidance to a non-nil
// access error while preserving its cause.
func WrapProjectAccessError(projectDirectory, operation string, cause error) error {
	return fmt.Errorf("%s in project %q; restore directory access or verify --project, then retry: %w", operation, projectDirectory, cause)
}

// ReadStateFile reads a bounded regular file using the transaction's path and
// symlink checks. Applications use its bytes and existence as preparation
// preconditions before handing state back to the journal.
func ReadStateFile(projectDirectory, filename string) (StateFile, bool, error) {
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return StateFile{}, false, WrapProjectAccessError(projectDirectory, "open project directory for transactional state", err)
	}
	defer root.Close()
	if err := ValidateParentDirectories(root, filename); err != nil {
		return StateFile{}, false, WrapProjectAccessError(projectDirectory, fmt.Sprintf("inspect transactional state path %q", filename), err)
	}
	if info, err := root.Lstat(filename); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o444 == 0 {
		return StateFile{}, false, WrapProjectAccessError(projectDirectory, fmt.Sprintf("inspect transactional state path %q", filename), fmt.Errorf("transactional state path %q has mode %03o; deliberately restore readable permissions and retry realization", filename, info.Mode().Perm()))
	}
	snapshot, err := snapshotFile(root, filename)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return StateFile{}, false, WrapProjectAccessError(projectDirectory, fmt.Sprintf("read transactional state path %q", filename), fmt.Errorf("transactional state path %q is unreadable; deliberately restore readable permissions and retry realization: %w", filename, err))
		}
		return StateFile{}, false, WrapProjectAccessError(projectDirectory, fmt.Sprintf("read transactional state path %q", filename), err)
	}
	return StateFile{
		Path: filename, Content: snapshot.content, Mode: uint32(snapshot.mode),
		BeforeExists: snapshot.exists, BeforeContent: append([]byte(nil), snapshot.content...), BeforeMode: uint32(snapshot.mode.Perm()),
	}, snapshot.exists, nil
}

// ValidateTargetLocation protects reserved state even when the filesystem
// accepts a different spelling or hardlink to it. It inspects metadata only;
// missing entries are allowed so planning can create ordinary targets.
func ValidateTargetLocation(root *os.Root, filename string) error {
	if err := ValidateTargetPath(filename); err != nil {
		return err
	}
	if err := ValidateParentDirectories(root, filename); err != nil {
		return err
	}
	for _, reserved := range []string{"agents.yaml", ".git", ".agents"} {
		state, err := root.Lstat(reserved)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect reserved project state %q: %w", reserved, err)
		}
		for current := filename; current != "."; current = path.Dir(current) {
			info, err := root.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect target location %q: %w", current, err)
			}
			if os.SameFile(state, info) {
				return fmt.Errorf("target %q aliases reserved project state %q; choose a target outside reserved project state", filename, reserved)
			}
		}
	}
	return nil
}

func snapshotFile(root *os.Root, filename string) (fileSnapshot, error) {
	return snapshotFileLimit(root, filename, maxTargetBytes)
}

func snapshotFileLimit(root *os.Root, filename string, limit int) (fileSnapshot, error) {
	if err := ValidateParentDirectories(root, filename); err != nil {
		return fileSnapshot{}, err
	}
	info, err := root.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %s: %w", diagnostic.Token(filename), diagnostic.PathCause(err))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("target %q must be a regular file, not a symlink or special file", filename)
	}
	file, err := root.Open(filename)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("open %q: %w", filename, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect opened %q: %w", filename, err)
	}
	beforeRead, err := root.Lstat(filename)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %q after opening: %w", filename, err)
	}
	if beforeRead.Mode()&os.ModeSymlink != 0 || !beforeRead.Mode().IsRegular() || !os.SameFile(opened, beforeRead) {
		return fileSnapshot{}, fmt.Errorf("target %q changed while being opened; keep it stable and retry", filename)
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("read %q: %w", filename, err)
	}
	if len(content) > limit {
		if limit != maxTargetBytes {
			return fileSnapshot{}, fmt.Errorf("record %q exceeds %d bytes", filename, limit)
		}
		return fileSnapshot{}, &FileTooLargeError{Path: filename}
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect opened %q after reading: %w", filename, err)
	}
	current, err := root.Lstat(filename)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %q after reading: %w", filename, err)
	}
	generation := fileGenerationOf(current)
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(openedAfter, current) ||
		fileGenerationOf(opened) != generation || fileGenerationOf(openedAfter) != generation ||
		opened.Size() != openedAfter.Size() || !opened.ModTime().Equal(openedAfter.ModTime()) || opened.Mode() != openedAfter.Mode() || openedAfter.Mode() != current.Mode() {
		return fileSnapshot{}, fmt.Errorf("target %q changed while being read; keep it stable and retry", filename)
	}
	return fileSnapshot{exists: true, content: content, mode: current.Mode().Perm(), hash: contentHash(content), generation: generation}, nil
}

// ValidateParentDirectories rejects a symlink or special file at any parent
// path component of filename within root, even one that stays inside root
// (os.Root follows in-root symlinks by design). It is exported so callers
// that need the same parent-component hardening outside the write boundary
// — such as internal/adapter's RootSnapshot — do not duplicate the logic.
func ValidateParentDirectories(root *os.Root, filename string) error {
	directory := path.Dir(filename)
	if directory == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(directory, "/") {
		if current == "" {
			current = component
		} else {
			current = path.Join(current, component)
		}
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect parent %s: %w", diagnostic.Token(current), diagnostic.PathCause(err))
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("parent %q must be a directory, not a symlink or special file", current)
		}
	}
	return nil
}

func contentHash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// ComparisonLedger builds compiler comparison metadata without changing the
// committed target inventory, ownership, exclusion policy or attribution. The
// second result is eligible evidence, including evidence for omitted agents
// whose current bytes need not match. Neither result grants new ownership.
// A committed byte/mode match always wins over a receipt match.
func ComparisonLedger(projectDirectory string, committed, applied Ledger) (Ledger, Ledger, error) {
	if len(applied.Targets) == 0 {
		return committed, canonicalLedger(applied), nil
	}
	root, err := os.OpenRoot(projectDirectory)
	if err != nil {
		return Ledger{}, Ledger{}, WrapProjectAccessError(projectDirectory, "open project directory for ledger comparison", err)
	}
	defer root.Close()
	info := func(filename string) (os.FileInfo, error) {
		if err := ValidateParentDirectories(root, filename); err != nil {
			return nil, WrapProjectAccessError(projectDirectory, fmt.Sprintf("inspect comparison target %q", filename), err)
		}
		value, err := root.Lstat(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, WrapProjectAccessError(projectDirectory, fmt.Sprintf("inspect comparison target %q", filename), err)
		}
		return value, nil
	}
	committedInfo := make([]os.FileInfo, len(committed.Targets))
	for i, target := range committed.Targets {
		committedInfo[i], err = info(target.Path)
		if err != nil {
			return Ledger{}, Ledger{}, err
		}
	}
	// Filesystem identity handles case aliases and hardlinks without assuming
	// anything about GOOS or core.ignorecase. Missing leaves cannot supply a
	// matching observed tuple; their normalized ledger paths still compare.
	same := func(a, b Target, ai, bi os.FileInfo) bool {
		return a.Path == b.Path || (ai != nil && bi != nil && os.SameFile(ai, bi))
	}
	appliedInfo := make([]os.FileInfo, len(applied.Targets))
	for i, target := range applied.Targets {
		appliedInfo[i], err = info(target.Path)
		if err != nil {
			return Ledger{}, Ledger{}, err
		}
		for j := 0; j < i; j++ {
			if same(target, applied.Targets[j], appliedInfo[i], appliedInfo[j]) {
				return Ledger{}, Ledger{}, fmt.Errorf("receipt targets %q and %q alias the same destination", target.Path, applied.Targets[j].Path)
			}
		}
	}
	view := Ledger{SchemaVersion: committed.SchemaVersion, Targets: append([]Target(nil), committed.Targets...)}
	eligible := Ledger{SchemaVersion: applied.SchemaVersion}
	for i, target := range committed.Targets {
		if target.Ownership != OwnershipGenerated {
			continue
		}
		for j, evidence := range applied.Targets {
			if evidence.Ownership != OwnershipGenerated || !same(target, evidence, committedInfo[i], appliedInfo[j]) || !sameArtifactIdentity(target, evidence) {
				continue
			}
			// Shared ownership wins even if its spelling differs from the
			// generated target or evidence. Ambiguous ownership grants nothing.
			ambiguous := false
			for k, other := range committed.Targets {
				if k != i && same(other, evidence, committedInfo[k], appliedInfo[j]) {
					ambiguous = true
					break
				}
			}
			if ambiguous {
				continue
			}
			// Retain only comparison data on top of committed metadata.
			comparison := target
			comparison.OutputHash, comparison.Mode = evidence.OutputHash, evidence.Mode
			comparison.Entries = append([]Entry(nil), target.Entries...)
			for k, entry := range comparison.Entries {
				for _, prior := range evidence.Entries {
					if entryIdentity(entry) == entryIdentity(prior) {
						comparison.Entries[k].ManagedHash = prior.ManagedHash
					}
				}
			}
			evidence.Path = target.Path
			eligible.Targets = append(eligible.Targets, evidence)
			observed, err := snapshotFile(root, target.Path)
			if err != nil {
				return Ledger{}, Ledger{}, WrapProjectAccessError(projectDirectory, fmt.Sprintf("read comparison target %q", target.Path), err)
			}
			matches := func(candidate Target) bool {
				return observed.exists && observed.hash == candidate.OutputHash && uint32(observed.mode.Perm()) == candidate.Mode
			}
			if !matches(target) && matches(comparison) {
				view.Targets[i] = comparison
			}
			break
		}
	}
	return view, canonicalLedger(eligible), nil
}

// Hashes and adapterVersion describe a realization, not logical identity.
func entryIdentity(entry Entry) [5]string {
	return [5]string{entry.Source, entry.ArtifactID, string(entry.ArtifactKind), entry.SourcePath, entry.Adapter}
}

func sameArtifactIdentity(committed, evidence Target) bool {
	if committed.Owner != evidence.Owner || len(committed.Entries) != len(evidence.Entries) {
		return false
	}
	identities := make(map[[5]string]bool, len(committed.Entries))
	for _, entry := range committed.Entries {
		identities[entryIdentity(entry)] = true
	}
	for _, entry := range evidence.Entries {
		if !identities[entryIdentity(entry)] {
			return false
		}
	}
	return true
}
