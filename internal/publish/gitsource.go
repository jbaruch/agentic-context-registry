package publish

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type gitSource interface {
	Clean(context.Context, string) (bool, error)
	Head(context.Context, string) (string, error)
	TagsAtHead(context.Context, string) ([]string, error)
	FileAt(context.Context, string, string, string) (File, error)
}

type commandGitSource struct{}

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

func (commandGitSource) FileAt(ctx context.Context, root, tag, name string) (File, error) {
	output, err := runGit(ctx, root, "ls-tree", "-z", tag, "--", name)
	if err != nil {
		return File{}, err
	}
	records := bytes.Split(bytes.TrimSuffix(output, []byte{0}), []byte{0})
	if len(records) != 1 || len(records[0]) == 0 {
		return File{}, fmt.Errorf("Git tree %q does not contain exactly one file at %q", tag, name)
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

func readTreeFiles(ctx context.Context, root, tag string, names []string, source gitSource) ([]File, error) {
	files := make([]File, 0, len(names))
	for _, name := range names {
		file, err := source.FileAt(ctx, root, tag, name)
		if err != nil {
			return nil, publishError(CodeUnpublishable, "read package file %q from Git tag %q: %v; commit every declared package file and retry", name, tag, err)
		}
		files = append(files, file)
	}
	return files, nil
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
