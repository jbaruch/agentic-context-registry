package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuilderUsesTaggedTreeContent(t *testing.T) {
	root := t.TempDir()
	manifestContent := []byte("schemaVersion: 1\nname: owner/plugin\nversion: 1.2.3\nsource:\n  repository: https://github.com/owner/plugin\nartifacts:\n  rules:\n    - id: guidance\n      path: guidance.md\n      activation:\n        mode: always\n")
	if err := os.WriteFile(filepath.Join(root, "agent-plugin.yaml"), manifestContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "guidance.md"), []byte("changed worktree content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	source := &fakeGitSource{
		clean: true, head: commit, tags: []string{"v1.2.3"},
		files: map[string]File{
			"agent-plugin.yaml": {Path: "agent-plugin.yaml", Mode: 0o644, Content: manifestContent},
			"guidance.md":       {Path: "guidance.md", Mode: 0o644, Content: []byte("committed content\n")},
		},
	}
	prepared, err := newBuilder(source, NewGate(), "acr test").Prepare(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Identity.Tag != "v1.2.3" || prepared.Identity.Commit != commit {
		t.Fatalf("identity = %#v", prepared.Identity)
	}
	reader, err := gzip.NewReader(bytes.NewReader(prepared.Assets.Archive.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, "/guidance.md") {
			content, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "committed content\n" {
				t.Fatalf("archived guidance = %q", content)
			}
			return
		}
	}
	t.Fatal("archive omitted guidance.md")
}
