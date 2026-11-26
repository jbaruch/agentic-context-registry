package realize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	gitExcludePath               = ".git/info/exclude"
	excludeBegin                 = "# BEGIN ACR GENERATED OUTPUTS"
	excludeEnd                   = "# END ACR GENERATED OUTPUTS"
	nestedExcludeBegin           = "# BEGIN ACR PROJECT OUTPUTS "
	nestedExcludeEnd             = "# END ACR PROJECT OUTPUTS "
	worktreeExcludeBegin         = "# BEGIN ACR WORKTREE OUTPUTS v1 "
	worktreeExcludeEnd           = "# END ACR WORKTREE OUTPUTS v1 "
	gitInspectionTimeout         = 5 * time.Second
	gitInspectionDiagnosticLimit = 32 << 10
)

type gitContext struct {
	enabled     bool
	tracked     map[string]bool
	prefix      string
	worktreeID  string
	excludeRoot string
	excludePath string
}

type gitInspector interface {
	Inspect(ctx context.Context, root string, targets []string) (gitContext, error)
}

type commandGitInspector struct{}

func (commandGitInspector) Inspect(ctx context.Context, root string, targets []string) (gitContext, error) {
	local, err := checkGitMetadata(root)
	if err != nil {
		return gitContext{}, err
	}
	output, err := runInspectionGit(ctx, root, "discover the worktree with git rev-parse --show-toplevel",
		"rev-parse", "--show-toplevel", "--show-prefix", "--absolute-git-dir")
	if err != nil {
		if !local && outsideGitWorktree(err) {
			return gitContext{}, nil
		}
		// Standalone realization has never needed Git on PATH. If Git is
		// missing, check for enclosing metadata before proceeding without it.
		if !local && errors.Is(err, exec.ErrNotFound) {
			return inspectionWithoutGit(root, err)
		}
		return gitContext{}, err
	}
	worktree, rest, _ := strings.Cut(strings.TrimSuffix(string(output), "\n"), "\n")
	prefix, gitDir, ok := strings.Cut(rest, "\n")
	if !ok || !filepath.IsAbs(worktree) || strings.ContainsAny(prefix, "\x00\r\n") ||
		!filepath.IsAbs(gitDir) || strings.ContainsAny(gitDir, "\x00\r\n") ||
		(prefix != "" && !strings.HasSuffix(prefix, "/")) {
		return gitContext{}, errors.New("git rev-parse returned invalid worktree paths; verify the worktree metadata and retry")
	}
	if _, err := checkGitMetadata(worktree); err != nil {
		return gitContext{}, err
	}
	worktreeID, err := gitWorktreeID(gitDir)
	if err != nil {
		return gitContext{}, err
	}
	excludeRoot, excludePath, err := resolveGitExclude(ctx, worktree)
	if err != nil {
		return gitContext{}, err
	}
	result := gitContext{
		enabled: true, tracked: make(map[string]bool),
		prefix: prefix, worktreeID: worktreeID, excludeRoot: excludeRoot, excludePath: excludePath,
	}
	if len(targets) == 0 {
		return result, nil
	}
	args := []string{"--literal-pathspecs", "ls-files", "-z", "--"}
	args = append(args, targets...)
	output, err = runInspectionGit(ctx, root, "inspect tracked realization targets with git ls-files", args...)
	if err != nil {
		return gitContext{}, err
	}
	for _, target := range bytes.Split(output, []byte{0}) {
		if len(target) != 0 {
			result.tracked[string(target)] = true
		}
	}
	return result, nil
}

func gitWorktreeID(gitDir string) (string, error) {
	// A linked worktree's commondir file is created with its private Git
	// directory. Git rewrites gitdir on move but does not rewrite commondir.
	// Include the file's identity and creation-time write, not just the
	// reusable administrative directory name. No ACR state needs enrollment.
	path := filepath.Join(gitDir, "commondir")
	info, err := os.Lstat(path)
	linked := !errors.Is(err, os.ErrNotExist)
	if !linked {
		path = gitDir
		info, err = os.Stat(path)
	}
	if err != nil {
		return "", fmt.Errorf("inspect Git worktree identity at %q: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || linked && !info.Mode().IsRegular() || !linked && !info.IsDir() {
		return "", fmt.Errorf("invalid Git worktree identity at %q; inspect the worktree metadata and retry", path)
	}
	if linked {
		return fmt.Sprintf("linked:%s:%d:%d:%d", filepath.Base(gitDir), stat.Dev, stat.Ino, info.ModTime().UnixNano()), nil
	}
	return fmt.Sprintf("primary:%d:%d", stat.Dev, stat.Ino), nil
}

func checkGitMetadata(root string) (bool, error) {
	metadata := root + string(os.PathSeparator) + ".git"
	info, err := os.Lstat(metadata)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Git metadata at %q: %w", metadata, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return false, fmt.Errorf("Git metadata at %q must be a directory or linked-worktree gitfile before ACR can manage local exclusions", metadata)
	}
	return true, nil
}

func outsideGitWorktree(err error) bool {
	var exitErr *exec.ExitError
	if errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		!errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		return false
	}
	return bytes.HasPrefix(exitErr.Stderr, []byte("fatal: not a git repository (or any ")) ||
		bytes.HasPrefix(exitErr.Stderr, []byte("fatal: this operation must be run in a work tree"))
}

func inspectionWithoutGit(root string, commandErr error) (gitContext, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return gitContext{}, fmt.Errorf("resolve project path while checking for Git metadata: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return gitContext{}, fmt.Errorf("resolve project path while checking for Git metadata: %w", err)
	}
	for parent := filepath.Dir(root); parent != root; parent = filepath.Dir(root) {
		found, err := checkGitMetadata(parent)
		if err != nil {
			return gitContext{}, err
		}
		if found {
			return gitContext{}, commandErr
		}
		root = parent
	}
	return gitContext{}, nil
}

// TrackedPaths reports which of the project-relative paths Git tracks, using
// the same inspection the planner runs. A project outside a Git worktree
// tracks nothing. The planner keeps a tracked generated target on disk when it
// drops ownership; the application uses this to refuse upgrade omissions
// while allowing explicit uninstall and surface retirement.
func TrackedPaths(ctx context.Context, projectDirectory string, paths []string) (map[string]bool, error) {
	state, err := (commandGitInspector{}).Inspect(ctx, projectDirectory, paths)
	if err == nil && state.tracked == nil {
		return map[string]bool{}, nil
	}
	return state.tracked, err
}

// TrackedPathAliases queries only case-insensitive spellings of a reserved path.
// Unrelated hardlinks are not discoverable without scanning the entire index.
func TrackedPathAliases(ctx context.Context, projectDirectory, filename string) (map[string]bool, error) {
	state, err := (commandGitInspector{}).Inspect(ctx, projectDirectory, nil)
	if err != nil {
		return nil, err
	}
	if !state.enabled {
		return state.tracked, nil
	}
	output, err := runInspectionGit(ctx, projectDirectory, "inspect tracked receipt spellings with git ls-files",
		"--no-literal-pathspecs", "--noglob-pathspecs", "ls-files", "-z", "--", ":(icase,literal)"+filename)
	if err != nil {
		return nil, err
	}
	for _, indexed := range bytes.Split(output, []byte{0}) {
		if len(indexed) != 0 {
			state.tracked[string(indexed)] = true
		}
	}
	return state.tracked, nil
}

func resolveGitExclude(ctx context.Context, root string) (string, string, error) {
	output, err := runInspectionGit(ctx, root, "resolve the exclusion path with git rev-parse --git-path", "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return "", "", err
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
		// Git's relative path is based on its physical working directory.
		physicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
		if err != nil {
			return "", "", fmt.Errorf("resolve project symlinks at %q while locating Git exclusions: %w", root, err)
		}
		resolved = filepath.Join(physicalRoot, resolved)
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
// not a Git repository. Notes describe retained legacy exclusions and
// unsupported separator receipts even when no edit is needed.
func PlanGitExclusionEdit(ctx context.Context, projectDirectory string, ledger Ledger) (Ledger, *FileTransactionEdit, []TransactionNote, error) {
	return planGitExclusionEdit(ctx, projectDirectory, ledger, nil)
}

func planGitExclusionEdit(ctx context.Context, projectDirectory string, ledger Ledger, claim *gitExclusionClaim) (Ledger, *FileTransactionEdit, []TransactionNote, error) {
	ledger = canonicalLedger(ledger)
	paths := make([]string, 0, len(ledger.Targets))
	for _, target := range ledger.Targets {
		paths = append(paths, target.Path)
	}
	state, err := (commandGitInspector{}).Inspect(ctx, projectDirectory, paths)
	if err != nil {
		return Ledger{}, nil, nil, err
	}
	if !state.enabled {
		for index := range ledger.Targets {
			ledger.Targets[index].Excluded = false
		}
		return ledger, nil, nil, nil
	}
	var excluded []string
	for index := range ledger.Targets {
		target := &ledger.Targets[index]
		target.Excluded = target.Ownership == OwnershipGenerated && !state.tracked[target.Path]
		if target.Excluded {
			excluded = append(excluded, target.Path)
		}
	}
	var excludeRoot *os.Root
	if claim != nil {
		if err := claim.acquire(ctx, state.excludeRoot, projectDirectory, ""); err != nil {
			return Ledger{}, nil, nil, err
		}
		excludeRoot = claim.root
	} else {
		excludeRoot, err = os.OpenRoot(state.excludeRoot)
		if err != nil {
			return Ledger{}, nil, nil, fmt.Errorf("open Git exclusion directory %q: %w", state.excludeRoot, err)
		}
		defer excludeRoot.Close()
	}
	snapshot, updated, change, note, err := state.planExcludeRewrite(excludeRoot, state.excludePath, excluded)
	if err != nil {
		return Ledger{}, nil, nil, err
	}
	if bytes.Equal(snapshot.content, updated) {
		return ledger, nil, note, nil
	}
	mode := uint32(0o644)
	if snapshot.exists {
		mode = uint32(snapshot.mode.Perm())
	}
	edit := &FileTransactionEdit{
		Path: gitExcludePath, Operation: "splice", After: updated, AfterMode: mode,
		GitExclusion: true, PhysicalRoot: state.excludeRoot, PhysicalPath: state.excludePath,
		exclusionChange: change,
	}
	if snapshot.exists {
		edit.Before = snapshot.content
		edit.BeforeMode = mode
	} else {
		edit.BeforeAbsent = true
	}
	return ledger, edit, note, nil
}

func newInspectionGitCommand(ctx context.Context, root string, args ...string) (*exec.Cmd, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, gitInspectionTimeout)
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	return command, ctx, cancel
}

func runInspectionGit(ctx context.Context, root, action string, args ...string) ([]byte, error) {
	command, ctx, cancel := newInspectionGitCommand(ctx, root, args...)
	defer cancel()
	return runInspectionGitCommand(ctx, command, action, func(stdout, stderr *os.File, _ <-chan struct{}) error {
		deadline := time.Now().Add(time.Second)
		return errors.Join(stdout.SetReadDeadline(deadline), stderr.SetReadDeadline(deadline))
	})
}

func runInspectionGitCommand(ctx context.Context, command *exec.Cmd, action string, drain func(stdout, stderr *os.File, diagnosticReady <-chan struct{}) error) ([]byte, error) {
	command.Env = append(command.Environ(), "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
	// Git can fork an fsmonitor hook that inherits the output pipes.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	output, err := inspectionGitOutput(ctx, command, drain)
	if err == nil {
		return output, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) != 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay) {
		return nil, fmt.Errorf("%s: %w; check or disable Git core.fsmonitor in this worktree and retry", action, err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w; install Git or make it available on PATH and retry", action, err)
	}
	return nil, fmt.Errorf("%s: %w; verify the Git worktree and retry", action, err)
}

// Own the pipes so Wait reports the Git leader's status independently of
// either pipe reaching EOF. Cmd.Output can hide ErrWaitDelay on a failed exit.
func inspectionGitOutput(ctx context.Context, command *exec.Cmd, drain func(stdout, stderr *os.File, diagnosticReady <-chan struct{}) error) ([]byte, error) {
	stdout, outWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	stderr, errWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, outWriter.Close())
	}
	defer stderr.Close()
	command.Stdout, command.Stderr = outWriter, errWriter
	startErr := command.Start()
	pipeErr := errors.Join(outWriter.Close(), errWriter.Close())
	if startErr != nil {
		return nil, errors.Join(startErr, pipeErr, ctx.Err())
	}

	var output, diagnostic bytes.Buffer
	outDone, errDone := make(chan error, 1), make(chan error, 1)
	diagnosticReady := make(chan struct{})
	go func() {
		_, err := io.Copy(&output, stdout)
		outDone <- err
	}()
	go func() {
		// Keep the diagnostic prefix without letting stderr grow unbounded.
		_, err := io.Copy(&diagnostic, io.LimitReader(stderr, gitInspectionDiagnosticLimit))
		close(diagnosticReady)
		if err == nil {
			_, err = io.Copy(io.Discard, stderr)
		}
		errDone <- err
	}()

	waitErr := command.Wait()
	// CommandContext stops watching once Wait returns, even if a helper
	// still holds a pipe. Keep watching until both readers finish.
	kill := func() error {
		err := command.Cancel()
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	killed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { killed <- kill() })
	if err := drain(stdout, stderr, diagnosticReady); err != nil {
		pipeErr = errors.Join(pipeErr, err, stdout.Close(), stderr.Close())
	}
	readErr := errors.Join(<-outDone, <-errDone)
	stopped := stop()
	ctxErr := ctx.Err()
	timedOut := errors.Is(readErr, os.ErrDeadlineExceeded)
	if timedOut {
		readErr = exec.ErrWaitDelay
	}
	var killErr error
	if !stopped {
		killErr = <-killed
	} else if ctxErr != nil || timedOut {
		killErr = kill()
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitErr.Stderr = diagnostic.Bytes()
	}
	return output.Bytes(), errors.Join(waitErr, pipeErr, readErr, ctxErr, killErr)
}

// separator is a validated byte offset plus one. Zero means no proof.
func (state gitContext) rewriteExclude(content []byte, targets []string, separator int) ([]byte, int, *TransactionNote, error) {
	// Never attribute a prefix-only block to this checkout, even if only one
	// worktree remains or its patterns happen to match this project's ledger.
	legacyBegin, legacyEnd := excludeBegin, excludeEnd
	if state.prefix != "" {
		scope := "[" + url.PathEscape(state.prefix) + "]"
		legacyBegin, legacyEnd = nestedExcludeBegin+scope, nestedExcludeEnd+scope
	}
	legacyStart, _, err := gitExcludeBlock(content, []byte(legacyBegin), []byte(legacyEnd))
	if err != nil {
		return nil, 0, nil, err
	}
	var note *TransactionNote
	if legacyStart >= 0 {
		note = &TransactionNote{Code: "ambiguous_legacy_git_exclusion", Path: gitExcludePath,
			Message: "ACR left legacy Git exclusions intact because their worktree owner is unknown. Re-realize affected worktrees before manually removing obsolete legacy patterns."}
	}
	scope := "[" + contentHash([]byte(state.worktreeID+"\x00"+state.prefix)) + "]"
	begin, end := []byte(worktreeExcludeBegin+scope), []byte(worktreeExcludeEnd+scope)
	blockStart, blockEnd, err := gitExcludeBlock(content, begin, end)
	if err != nil {
		return nil, 0, nil, err
	}
	if blockStart < 0 && len(targets) == 0 {
		return content, separator, note, nil
	}

	sort.Strings(targets)
	var block []byte
	if len(targets) != 0 {
		var builder strings.Builder
		builder.Write(begin)
		builder.WriteByte('\n')
		for _, target := range targets {
			builder.WriteString(gitExcludePattern(state.prefix + target))
			builder.WriteByte('\n')
		}
		builder.Write(end)
		builder.WriteByte('\n')
		block = []byte(builder.String())
	}

	if blockStart < 0 {
		result := append([]byte(nil), content...)
		if len(content) != 0 && content[len(content)-1] != '\n' {
			result = append(result, '\n')
			separator = len(result)
		}
		return append(result, block...), separator, note, nil
	}
	result := append([]byte(nil), content[:blockStart]...)
	result = append(result, block...)
	result = append(result, content[blockEnd:]...)
	switch {
	case separator > blockEnd:
		separator += len(block) - (blockEnd - blockStart)
	case separator > blockStart:
		separator = 0 // The splice removed that byte, not one that looks like it.
	}
	if separator != 0 && separator == len(result) {
		return result[:separator-1], 0, note, nil
	}
	return result, separator, note, nil
}

func gitExcludeBlock(content, begin, end []byte) (int, int, error) {
	beginIndex := bytes.Index(content, begin)
	endIndex := bytes.Index(content, end)
	if beginIndex >= 0 && bytes.Contains(content[beginIndex+len(begin):], begin) || endIndex >= 0 && bytes.Contains(content[endIndex+len(end):], end) {
		return 0, 0, &gitExcludeMarkerError{"duplicate ACR marker blocks; keep one block and retry"}
	}
	if beginIndex < 0 && endIndex < 0 {
		return -1, -1, nil
	}
	if beginIndex < 0 || endIndex < beginIndex+len(begin) {
		return 0, 0, &gitExcludeMarkerError{"an incomplete ACR marker block; repair or remove the markers and retry"}
	}
	if !completeMarkerLine(content, beginIndex, len(begin)) || !completeMarkerLine(content, endIndex, len(end)) {
		return 0, 0, &gitExcludeMarkerError{"ACR marker text outside complete lines; remove the ambiguous text and retry"}
	}
	body := content[beginIndex+len(begin) : endIndex]
	if bytes.Contains(body, []byte("\n# BEGIN ACR ")) || bytes.Contains(body, []byte("\n# END ACR ")) {
		return 0, 0, &gitExcludeMarkerError{"overlapping ACR marker blocks; separate the blocks and retry"}
	}
	blockEnd := endIndex + len(end)
	if blockEnd < len(content) && content[blockEnd] == '\r' {
		blockEnd++
	}
	if blockEnd < len(content) && content[blockEnd] == '\n' {
		blockEnd++
	}
	return beginIndex, blockEnd, nil
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
		if strings.ContainsRune("\\?*[]!# ", character) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(character)
	}
	return builder.String()
}
