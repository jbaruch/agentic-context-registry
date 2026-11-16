package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

type gitSource interface {
	Clean(context.Context, string) (bool, error)
	Head(context.Context, string) (string, error)
	TagsAtHead(context.Context, string) ([]string, error)
	Worktree(context.Context, string) (bool, error)
	ListWorktree(context.Context, string, string) ([]string, error)
	Tree(context.Context, string, string, string, bool) ([]treeEntry, error)
	Blob(context.Context, string, string) ([]byte, error)
	// Ignored reports whether Git's ignore rules exclude the untracked path.
	Ignored(context.Context, string, string) (bool, error)
}

type commandGitSource struct{}

// treeEntry is an immutable record from the resolved commit's tree.
type treeEntry struct{ Path, Mode, OID string }

func (commandGitSource) Clean(ctx context.Context, root string) (bool, error) {
	output, err := runGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return len(output) == 0, nil
}

func (commandGitSource) Head(ctx context.Context, root string) (string, error) {
	output, err := runGit(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(output))
	if len(commit) != 40 {
		return "", fmt.Errorf("Git returned invalid HEAD commit %q", commit)
	}
	return commit, nil
}

func (commandGitSource) TagsAtHead(ctx context.Context, root string) ([]string, error) {
	output, err := runGit(ctx, root, "tag", "--points-at", "HEAD")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	sort.Strings(lines)
	return lines, nil
}

func (commandGitSource) Worktree(ctx context.Context, root string) (bool, error) {
	// Leave missing-directory diagnostics to the manifest loader. Other stat
	// failures must not silently disable Git's view.
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	// The not-a-repository diagnostic is matched as text, so pin its language.
	output, err := runGitEnv(ctx, root, []string{"LC_ALL=C"}, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if notAGitWorkTree(err) {
			return false, nil
		}
		return false, err
	}
	// A bare repository, or a directory inside .git, answers false with a
	// successful exit: Git has no work tree there to list.
	switch strings.TrimSpace(string(output)) {
	case "false":
		return false, nil
	case "true":
		// Continue with Git's file listing.
	default:
		return false, fmt.Errorf("git rev-parse --is-inside-work-tree returned unexpected output %q", output)
	}
	return true, nil
}

// notAGitWorkTree reports the two expected reasons Git has no view of a
// directory: no git executable, or no repository around it. Every other
// failure, such as a safe.directory refusal or an unreadable repository, is a
// real error the caller must see.
func notAGitWorkTree(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var failure *gitError
	return errors.As(err, &failure) && failure.exitCode == 128 && strings.HasPrefix(failure.diagnostic, "fatal: not a git repository")
}

func (commandGitSource) ListWorktree(ctx context.Context, root, relative string) ([]string, error) {
	output, err := runGit(ctx, root, "--literal-pathspecs", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", relative)
	if err != nil {
		return nil, err
	}
	return nulPaths(output)
}

func nulPaths(output []byte) ([]string, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("Git returned an unterminated NUL record")
	}
	records := bytes.Split(output[:len(output)-1], []byte{0})
	paths := make([]string, 0, len(records))
	for _, record := range records {
		if len(record) == 0 {
			return nil, fmt.Errorf("Git returned an empty path record")
		}
		paths = append(paths, string(record))
	}
	return paths, nil
}

func (commandGitSource) Ignored(ctx context.Context, root, name string) (bool, error) {
	_, err := runGit(ctx, root, "check-ignore", "-q", "--", name)
	if err == nil {
		return true, nil
	}
	var failure *gitError
	if errors.As(err, &failure) && failure.exitCode == 1 {
		return false, nil
	}
	return false, err
}

func (commandGitSource) Tree(ctx context.Context, root, commit, relative string, recursive bool) ([]treeEntry, error) {
	args := []string{"--literal-pathspecs", "ls-tree", "-z"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, commit)
	if relative != "." {
		args = append(args, "--", relative)
	}
	output, err := runGit(ctx, root, args...)
	if err != nil {
		return nil, err
	}
	records, err := nulPaths(output)
	if err != nil {
		return nil, err
	}
	entries := make([]treeEntry, 0, len(records))
	for _, record := range records {
		metadata, name, found := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !found || name == "" || len(fields) != 3 || !validTreeRecord(fields) {
			return nil, fmt.Errorf("Git returned malformed tree record %q", record)
		}
		entries = append(entries, treeEntry{Path: name, Mode: fields[0], OID: fields[2]})
	}
	return entries, nil
}

func validTreeRecord(fields []string) bool {
	if len(fields) != 3 {
		return false
	}
	mode, kind, oid := fields[0], fields[1], fields[2]
	if !(mode == "040000" && kind == "tree" || mode == "160000" && kind == "commit" || (mode == "100644" || mode == "100755" || mode == "120000") && kind == "blob") {
		return false
	}
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (commandGitSource) Blob(ctx context.Context, root, oid string) ([]byte, error) {
	return runGit(ctx, root, "cat-file", "blob", oid)
}

// PackageListing selects Git's worktree inventory, or the legacy filesystem
// behavior when there is no Git worktree. No filesystem reconciliation occurs.
func PackageListing(ctx context.Context, root string) (manifest.ListFiles, error) {
	return packageListing(ctx, root, commandGitSource{})
}

func packageListing(ctx context.Context, root string, source gitSource) (manifest.ListFiles, error) {
	inside, err := source.Worktree(ctx, root)
	if err != nil {
		return nil, gitAccessError(ctx, "inspect Git worktree", err)
	}
	if !inside {
		return nil, nil
	}
	return func(relative string, directory bool) ([]string, error) {
		names, err := source.ListWorktree(ctx, root, relative)
		if err != nil {
			return nil, gitAccessError(ctx, "list package paths", err)
		}
		if !directory {
			// ls-files also accepts directory selectors. A required file
			// request resolves leaves at its own depth, never descendants of
			// a directory occupying that pathname in the index.
			leaves := make([]string, 0, len(names))
			for _, name := range names {
				if strings.Count(name, "/") == strings.Count(relative, "/") {
					leaves = append(leaves, name)
				}
			}
			names = leaves
		}
		return names, nil
	}, nil
}

func gitAccessError(ctx context.Context, action string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return publishError(CodeGitAccess, "%s: %v; verify repository access and integrity (git fsck), then retry", action, err)
}

func readTreeFiles(ctx context.Context, root string, identity Identity, value manifest.Manifest, source gitSource) ([]File, error) {
	selected := map[string]treeEntry{}
	list := func(relative string, directory bool) ([]string, error) {
		// Resolve the root without a trailing slash: a symlink or gitlink at a
		// skill root must be rejected rather than hidden by recursive selection.
		entries, err := source.Tree(ctx, root, identity.Commit, relative, false)
		if err != nil {
			return nil, gitAccessError(ctx, "select committed package path", err)
		}
		if relative == "." && directory {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				if entry.Mode != "040000" {
					selected[entry.Path] = entry
					names = append(names, entry.Path)
				}
			}
			return names, nil
		}
		if len(entries) == 0 {
			if directory {
				// SelectListed also requires this skill's SKILL.md. Diagnose
				// that leaf's absence (and its ignore rule), even when Git
				// has no tree at the root because every descendant is ignored.
				return nil, nil
			}
			ignored, err := source.Ignored(ctx, root, relative)
			if err != nil {
				return nil, gitAccessError(ctx, "inspect ignore rules for absent declaration", err)
			}
			if ignored {
				return nil, publishError(CodeUnpublishable, "tag %q does not contain %q; Git ignores %q; stop declaring it in agent-plugin.yaml, or intentionally change the ignore rule and track the file before moving the tag", identity.Tag, relative, relative)
			}
			return nil, publishError(CodeUnpublishable, "tag %q does not contain %q; commit the declared file and move the tag to that commit, or stop declaring it in agent-plugin.yaml", identity.Tag, relative)
		}
		if len(entries) != 1 {
			return nil, gitAccessError(ctx, "select committed package path", fmt.Errorf("Git returned multiple roots for %q", relative))
		}
		entry := entries[0]
		if entry.Path != relative {
			return nil, publishError(CodeUnpublishable, "agent-plugin.yaml declares %+q but Git stores %+q; correct the declaration in agent-plugin.yaml to Git's exact spelling, commit and retag before publication", relative, entry.Path)
		}
		if directory {
			if entry.Mode != "040000" {
				return nil, unsupportedEntry(entry, true)
			}
			entries, err = source.Tree(ctx, root, identity.Commit, relative, true)
			if err != nil {
				return nil, gitAccessError(ctx, "list committed skill files", err)
			}
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Mode != "100644" && entry.Mode != "100755" {
				return nil, unsupportedEntry(entry, false)
			}
			selected[entry.Path] = entry
			names = append(names, entry.Path)
		}
		return names, nil
	}
	names, err := manifest.SelectListed(value, list)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(names))
	for _, name := range names {
		entry := selected[name]
		if entry.Mode != "100644" && entry.Mode != "100755" {
			return nil, unsupportedEntry(entry, false)
		}
		content, err := source.Blob(ctx, root, entry.OID)
		if err != nil {
			return nil, gitAccessError(ctx, fmt.Sprintf("read Git blob for %q", name), err)
		}
		mode := fs.FileMode(0o644)
		if entry.Mode == "100755" {
			mode = 0o755
		}
		files = append(files, File{Path: name, Mode: mode, Content: content})
	}
	return files, nil
}

func unsupportedEntry(entry treeEntry, directory bool) error {
	kind := "entry"
	switch entry.Mode {
	case "120000":
		kind = "symbolic link"
	case "160000":
		kind = "gitlink"
	case "040000":
		kind = "directory"
	}
	replacement := "regular file"
	if directory {
		replacement = "directory containing regular files"
	}
	return publishError(CodeUnpublishable, "Git tree path %q has unsupported %s mode %s; replace it with a %s committed to the tagged tree, or stop declaring it in agent-plugin.yaml", entry.Path, kind, entry.Mode, replacement)
}

// gitError is a failed git invocation. It keeps the exit status and Git's own
// diagnostic so callers can tell an expected non-result from a tool failure.
type gitError struct {
	args       []string
	exitCode   int
	diagnostic string
	cause      error
}

func (failure *gitError) Error() string {
	return fmt.Sprintf("git %s: %s", strings.Join(failure.args, " "), failure.diagnostic)
}

func (failure *gitError) Unwrap() error { return failure.cause }

func runGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	return runGitEnv(ctx, root, nil, args...)
}

func runGitEnv(ctx context.Context, root string, environment []string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"--no-replace-objects", "-C", root}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"), environment...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			diagnostic = err.Error()
		}
		failure := &gitError{args: args, exitCode: -1, diagnostic: diagnostic, cause: err}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			failure.exitCode = exit.ExitCode()
		}
		return nil, failure
	}
	return output, nil
}
