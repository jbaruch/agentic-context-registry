package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jbaruch/agentic-context-registry/internal/manifest"
)

type gitSource interface {
	Clean(context.Context, string) (bool, error)
	Head(context.Context, string) (string, error)
	TagsAtHead(context.Context, string) ([]string, error)
	FileAt(context.Context, string, string, string) (File, error)
	// VisiblePaths lists the files Git sees under root: the index plus untracked
	// files that are not ignored. The boolean is false when root is not inside a
	// Git work tree, in which case there is no Git view to apply.
	VisiblePaths(context.Context, string) ([]string, bool, error)
	// Ignored reports whether Git's ignore rules exclude the untracked path.
	Ignored(context.Context, string, string) (bool, error)
}

type commandGitSource struct{}

// errNotInTree marks a declared path the tagged Git tree does not contain.
var errNotInTree = errors.New("path is not in the tagged tree")

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

func (commandGitSource) VisiblePaths(ctx context.Context, root string) ([]string, bool, error) {
	if _, err := runGit(ctx, root, "rev-parse", "--is-inside-work-tree"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, nil
	}
	output, err := runGit(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, false, err
	}
	var paths []string
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) != 0 {
			paths = append(paths, string(record))
		}
	}
	return paths, true, nil
}

func (commandGitSource) Ignored(ctx context.Context, root, name string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-q", "--", name)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore %q: %w", name, err)
}

func (commandGitSource) FileAt(ctx context.Context, root, tag, name string) (File, error) {
	output, err := runGit(ctx, root, "ls-tree", "-z", tag, "--", name)
	if err != nil {
		return File{}, err
	}
	records := bytes.Split(bytes.TrimSuffix(output, []byte{0}), []byte{0})
	if len(records) != 1 || len(records[0]) == 0 {
		return File{}, fmt.Errorf("%w: Git tree %q does not contain exactly one file at %q", errNotInTree, tag, name)
	}
	metadata, treePath, found := bytes.Cut(records[0], []byte{'\t'})
	if !found || string(treePath) != name {
		return File{}, fmt.Errorf("Git tree returned unexpected path for %q", name)
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 || fields[1] != "blob" || fields[0] != "100644" && fields[0] != "100755" {
		return File{}, fmt.Errorf("Git tree path %q has unsupported entry %q; publish only regular files", name, metadata)
	}
	modeValue, err := strconv.ParseUint(fields[0], 8, 32)
	if err != nil {
		return File{}, fmt.Errorf("parse Git mode for %q: %w", name, err)
	}
	content, err := runGit(ctx, root, "cat-file", "blob", fields[2])
	if err != nil {
		return File{}, fmt.Errorf("read Git blob for %q: %w", name, err)
	}
	return File{Path: name, Mode: fs.FileMode(modeValue), Content: content}, nil
}

// PackageFilter returns the filter that limits skill directory enumeration to
// what Git sees: tracked files plus untracked files no ignore rule excludes. It
// is nil, meaning no restriction, when root is not inside a Git work tree.
func PackageFilter(ctx context.Context, root string) (manifest.PathFilter, error) {
	return packageFilter(ctx, root, commandGitSource{})
}

func packageFilter(ctx context.Context, root string, source gitSource) (manifest.PathFilter, error) {
	paths, inside, err := source.VisiblePaths(ctx, root)
	if err != nil {
		return nil, publishError(CodeGitAccess, "list files Git sees under %q: %v", root, err)
	}
	if !inside {
		return nil, nil
	}
	files := make(map[string]struct{}, len(paths))
	directories := make(map[string]struct{})
	for _, name := range paths {
		files[name] = struct{}{}
		for parent := path.Dir(name); parent != "." && parent != "/"; parent = path.Dir(parent) {
			if _, seen := directories[parent]; seen {
				break
			}
			directories[parent] = struct{}{}
		}
	}
	return func(name string, isDir bool) bool {
		if isDir {
			_, found := directories[name]
			return found
		}
		_, found := files[name]
		return found
	}, nil
}

func readTreeFiles(ctx context.Context, root, tag string, names []string, source gitSource) ([]File, error) {
	files := make([]File, 0, len(names))
	for _, name := range names {
		file, err := source.FileAt(ctx, root, tag, name)
		if err != nil {
			return nil, publishError(CodeUnpublishable, "read package file %q from Git tag %q: %v; %s", name, tag, err, unpublishableAdvice(ctx, root, tag, name, err, source))
		}
		files = append(files, file)
	}
	return files, nil
}

// unpublishableAdvice names the remedy that fits why the tag lacks the file.
// Git's ignore rules are consulted so an ignored file is never advised to be
// committed.
func unpublishableAdvice(ctx context.Context, root, tag, name string, cause error, source gitSource) string {
	if errors.Is(cause, errNotInTree) {
		if ignored, err := source.Ignored(ctx, root, name); err == nil && ignored {
			return fmt.Sprintf("Git ignores %q, so tag %q cannot contain it; either stop declaring it in agent-plugin.yaml, or change the ignore rule so Git tracks it, then commit and move the tag", name, tag)
		}
		return fmt.Sprintf("tag %q does not contain %q; commit the declared file and move the tag to that commit, or stop declaring it in agent-plugin.yaml", tag, name)
	}
	return fmt.Sprintf("replace %q with a regular file committed to the tagged tree, or stop declaring it in agent-plugin.yaml", name)
}

func runGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", root}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			diagnostic = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), diagnostic)
	}
	return output, nil
}
