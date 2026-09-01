package realize

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	enabled bool
	tracked map[string]bool
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
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return gitContext{}, errors.New(".git must be a directory, not a symlink or special file, before ACR can manage local exclusions")
	}
	result := gitContext{enabled: true, tracked: make(map[string]bool)}
	if len(targets) == 0 {
		return result, nil
	}
	args := []string{"--literal-pathspecs", "-C", root, "ls-files", "-z", "--"}
	args = append(args, targets...)
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		return gitContext{}, fmt.Errorf("inspect tracked realization targets: %w; verify the Git worktree and retry", err)
	}
	for _, target := range bytes.Split(output, []byte{0}) {
		if len(target) != 0 {
			result.tracked[string(target)] = true
		}
	}
	return result, nil
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
