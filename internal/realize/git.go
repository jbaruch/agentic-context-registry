package realize

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	gitExcludePath = ".git/info/exclude"
	excludeBegin   = "# BEGIN ACR GENERATED OUTPUTS"
	excludeEnd     = "# END ACR GENERATED OUTPUTS"
	excludeJoin    = "# ACR ADDED SEPARATOR\n"
)

type gitContext struct {
	enabled     bool
	tracked     map[string]bool
	excludeRoot string
	excludePath string
}

type gitInspector interface {
	Inspect(root string, targets []string) (gitContext, error)
}

type commandGitInspector struct{}

func (commandGitInspector) Inspect(root string, targets []string) (gitContext, error) {
	info, err := os.Lstat(root + string(os.PathSeparator) + ".git")
	if errors.Is(err, os.ErrNotExist) {
		return gitContext{}, nil
	}
	if err != nil {
		return gitContext{}, fmt.Errorf("inspect repository Git metadata: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
		return gitContext{}, errors.New(".git must be a directory or linked-worktree gitfile, not a symlink or special file, before ACR can manage local exclusions")
	}
	excludeRoot, excludePath, err := resolveGitExclude(root)
	if err != nil {
		return gitContext{}, err
	}
	result := gitContext{
		enabled: true, tracked: make(map[string]bool),
		excludeRoot: excludeRoot, excludePath: excludePath,
	}
	if len(targets) == 0 {
		return result, nil
	}
	args := []string{"--literal-pathspecs", "-C", root, "ls-files", "-z", "--"}
	args = append(args, targets...)
	output, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return gitContext{}, gitCommandError("inspect tracked realization targets", err, output)
	}
	for _, target := range bytes.Split(output, []byte{0}) {
		if len(target) != 0 {
			result.tracked[string(target)] = true
		}
	}
	return result, nil
}

func resolveGitExclude(root string) (string, string, error) {
	output, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").CombinedOutput()
	if err != nil {
		return "", "", gitCommandError("resolve repository Git exclusion path", err, output)
	}
	resolved := strings.TrimSuffix(string(output), "\n")
	resolved = strings.TrimSuffix(resolved, "\r")
	if resolved == "" || strings.ContainsAny(resolved, "\x00\r\n") || filepath.Clean(resolved) != resolved {
		return "", "", errors.New("Git returned an invalid exclusion path; verify the worktree metadata and retry")
	}
	if !filepath.IsAbs(resolved) {
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			return "", "", fmt.Errorf("resolve project directory %q while locating Git exclusions: %w", root, err)
		}
		resolved = filepath.Join(absoluteRoot, resolved)
	}
	if filepath.Base(resolved) != "exclude" || filepath.Base(filepath.Dir(resolved)) != "info" {
		return "", "", fmt.Errorf("Git returned unexpected exclusion path %q; expected an info/exclude file", resolved)
	}
	rootPath, err := filepath.EvalSymlinks(filepath.Dir(resolved))
	if err != nil {
		return "", "", fmt.Errorf("resolve Git exclusion directory %q: %w", filepath.Dir(resolved), err)
	}
	if !filepath.IsAbs(rootPath) {
		return "", "", fmt.Errorf("resolved Git exclusion directory %q is not absolute", rootPath)
	}
	return rootPath, filepath.Base(resolved), nil
}

// PlanGitExclusionEdit brings the ledger's local Git-exclusion state in line
// with its ownership, and returns the edit that writes it.
//
// Finalization can change a target's ownership — a Markdown host left holding
// nothing but ACR's own block becomes generated-only — and a generated-only
// target that Git does not track belongs in the exclusion block. Leaving that
// to the next ordinary realization made a successful finalization hand back a
// project with pending work, so the exclusion travels in the same transaction.
//
// The returned ledger always carries correct Excluded flags. The edit is nil
// when the exclusion file already says what it should, or when the project is
// not a Git repository.
func PlanGitExclusionEdit(projectDirectory string, ledger Ledger) (Ledger, *FileTransactionEdit, error) {
	return planGitExclusionEdit(projectDirectory, ledger, commandGitInspector{})
}

func planGitExclusionEdit(projectDirectory string, ledger Ledger, inspector gitInspector) (Ledger, *FileTransactionEdit, error) {
	ledger = canonicalLedger(ledger)
	paths := make([]string, 0, len(ledger.Targets))
	for _, target := range ledger.Targets {
		paths = append(paths, target.Path)
	}
	state, err := inspector.Inspect(projectDirectory, paths)
	if err != nil {
		return Ledger{}, nil, err
	}
	if !state.enabled {
		for index := range ledger.Targets {
			ledger.Targets[index].Excluded = false
		}
		return ledger, nil, nil
	}
	var excluded []string
	for index := range ledger.Targets {
		target := &ledger.Targets[index]
		target.Excluded = target.Ownership == OwnershipGenerated && !state.tracked[target.Path]
		if target.Excluded {
			excluded = append(excluded, target.Path)
		}
	}
	excludeRoot, err := os.OpenRoot(state.excludeRoot)
	if err != nil {
		return Ledger{}, nil, fmt.Errorf("open Git exclusion directory %q: %w", state.excludeRoot, err)
	}
	defer excludeRoot.Close()
	snapshot, err := snapshotFile(excludeRoot, state.excludePath)
	if err != nil {
		return Ledger{}, nil, err
	}
	updated, err := rewriteGitExclude(snapshot.content, excluded)
	if err != nil {
		return Ledger{}, nil, err
	}
	if snapshot.exists && bytes.Equal(snapshot.content, updated) {
		return ledger, nil, nil
	}
	mode := uint32(0o644)
	if snapshot.exists {
		mode = uint32(snapshot.mode.Perm())
	}
	edit := &FileTransactionEdit{
		Path: gitExcludePath, Operation: "splice", After: updated, AfterMode: mode,
		GitExclusion: true, PhysicalRoot: state.excludeRoot, PhysicalPath: state.excludePath,
	}
	if snapshot.exists {
		edit.Before = snapshot.content
		edit.BeforeMode = mode
	} else {
		edit.BeforeAbsent = true
	}
	return ledger, edit, nil
}

func gitCommandError(action string, commandErr error, output []byte) error {
	diagnostic := strings.TrimSpace(string(output))
	if diagnostic == "" {
		return fmt.Errorf("%s: %w; verify the Git worktree and retry", action, commandErr)
	}
	return fmt.Errorf("%s: %w: %s; verify the Git worktree and retry", action, commandErr, diagnostic)
}

func rewriteGitExclude(content []byte, targets []string) ([]byte, error) {
	begin := []byte(excludeBegin)
	end := []byte(excludeEnd)
	beginIndex := bytes.Index(content, begin)
	endIndex := bytes.Index(content, end)
	if beginIndex >= 0 && bytes.Index(content[beginIndex+len(begin):], begin) >= 0 || endIndex >= 0 && bytes.Index(content[endIndex+len(end):], end) >= 0 {
		return nil, errors.New(".git/info/exclude contains duplicate ACR marker blocks; keep one block and retry")
	}
	if beginIndex < 0 != (endIndex < 0) || beginIndex >= 0 && endIndex < beginIndex {
		return nil, errors.New(".git/info/exclude contains an incomplete ACR marker block; repair or remove the markers and retry")
	}
	if beginIndex >= 0 && (!completeMarkerLine(content, beginIndex, len(begin)) || !completeMarkerLine(content, endIndex, len(end))) {
		return nil, errors.New(".git/info/exclude contains ACR marker text outside complete lines; remove the ambiguous text and retry")
	}

	sort.Strings(targets)
	var block []byte
	if len(targets) != 0 {
		var builder strings.Builder
		builder.WriteString(excludeBegin)
		builder.WriteByte('\n')
		for _, target := range targets {
			builder.WriteString(gitExcludePattern(target))
			builder.WriteByte('\n')
		}
		builder.WriteString(excludeEnd)
		builder.WriteByte('\n')
		block = []byte(builder.String())
	}

	if beginIndex < 0 {
		if len(block) == 0 {
			return append([]byte(nil), content...), nil
		}
		result := append([]byte(nil), content...)
		if len(result) != 0 && result[len(result)-1] != '\n' {
			result = append(result, '\n')
			result = append(result, []byte(excludeJoin)...)
		}
		return append(result, block...), nil
	}
	blockEnd := endIndex + len(end)
	if blockEnd < len(content) && content[blockEnd] == '\r' {
		blockEnd++
	}
	if blockEnd < len(content) && content[blockEnd] == '\n' {
		blockEnd++
	}
	blockStart := beginIndex
	join := []byte(excludeJoin)
	if len(block) == 0 && beginIndex >= len(join) && bytes.Equal(content[beginIndex-len(join):beginIndex], join) {
		blockStart = beginIndex - len(join)
		if blockStart > 0 && content[blockStart-1] == '\n' {
			blockStart--
		}
	}
	result := append([]byte(nil), content[:blockStart]...)
	result = append(result, block...)
	result = append(result, content[blockEnd:]...)
	return result, nil
}

func completeMarkerLine(content []byte, index, length int) bool {
	lineStart := index == 0 || content[index-1] == '\n'
	after := index + length
	lineEnd := after == len(content) || content[after] == '\n' || content[after] == '\r' && after+1 < len(content) && content[after+1] == '\n'
	return lineStart && lineEnd
}

func gitExcludePattern(target string) string {
	var builder strings.Builder
	builder.WriteByte('/')
	for _, character := range target {
		if strings.ContainsRune("?*[]!# ", character) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(character)
	}
	return builder.String()
}
