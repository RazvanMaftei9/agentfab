package agent

import (
	"testing"
)

func TestParseFileBlocksSingle(t *testing.T) {
	input := "Here is the summary.\n```file:README.md\n# Hello\nThis is a readme.\n```\nDone."
	blocks, summary := ParseFileBlocks(input)

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Path != "README.md" {
		t.Errorf("path: got %q", blocks[0].Path)
	}
	if blocks[0].Content != "# Hello\nThis is a readme." {
		t.Errorf("content: got %q", blocks[0].Content)
	}
	if summary != "Here is the summary.\nDone." {
		t.Errorf("summary: got %q", summary)
	}
}

func TestParseFileBlocksMultiple(t *testing.T) {
	input := "Summary text.\n" +
		"```file:src/main.go\n" +
		"package main\n\nfunc main() {}\n" +
		"```\n" +
		"Middle text.\n" +
		"```file:go.mod\n" +
		"module example\n" +
		"```\n" +
		"End."

	blocks, summary := ParseFileBlocks(input)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	if blocks[0].Path != "src/main.go" {
		t.Errorf("block 0 path: got %q", blocks[0].Path)
	}
	if blocks[0].Content != "package main\n\nfunc main() {}" {
		t.Errorf("block 0 content: got %q", blocks[0].Content)
	}

	if blocks[1].Path != "go.mod" {
		t.Errorf("block 1 path: got %q", blocks[1].Path)
	}
	if blocks[1].Content != "module example" {
		t.Errorf("block 1 content: got %q", blocks[1].Content)
	}

	if summary != "Summary text.\nMiddle text.\nEnd." {
		t.Errorf("summary: got %q", summary)
	}
}

func TestParseFileBlocksNone(t *testing.T) {
	input := "Just regular text.\n\n```go\ncode block\n```\n\nMore text."
	blocks, summary := ParseFileBlocks(input)

	if blocks != nil {
		t.Errorf("expected nil blocks, got %d", len(blocks))
	}
	if summary != input {
		t.Errorf("summary should be original content, got %q", summary)
	}
}

func TestParseFileBlocksNestedFences(t *testing.T) {
	input := "Summary.\n" +
		"```file:doc.md\n" +
		"# Document\n" +
		"Here is a code example:\n" +
		"```go\n" +
		"fmt.Println(\"hello\")\n" +
		"```\n" +
		"End of doc.\n" +
		"```\n" +
		"Done."

	blocks, summary := ParseFileBlocks(input)

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Path != "doc.md" {
		t.Errorf("path: got %q", blocks[0].Path)
	}
	expected := "# Document\nHere is a code example:\n```go\nfmt.Println(\"hello\")\n```\nEnd of doc."
	if blocks[0].Content != expected {
		t.Errorf("content: got %q, want %q", blocks[0].Content, expected)
	}
	if summary != "Summary.\nDone." {
		t.Errorf("summary: got %q", summary)
	}
}

func TestParseFileBlocksPathTraversal(t *testing.T) {
	input := "```file:../../../etc/passwd\nmalicious content\n```"
	blocks, summary := ParseFileBlocks(input)

	if blocks != nil {
		t.Error("expected nil blocks for path traversal attempt")
	}
	if summary != input {
		t.Errorf("summary should be original content, got %q", summary)
	}
}

func TestParseFileBlocksAbsolutePath(t *testing.T) {
	input := "```file:/etc/passwd\nmalicious content\n```"
	blocks, summary := ParseFileBlocks(input)

	if blocks != nil {
		t.Error("expected nil blocks for absolute path")
	}
	if summary != input {
		t.Errorf("summary should be original content, got %q", summary)
	}
}

func TestParseFileBlocksUnclosedBlock(t *testing.T) {
	input := "Summary.\n```file:test.go\npackage test"
	blocks, summary := ParseFileBlocks(input)

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block for unclosed block, got %d", len(blocks))
	}
	if blocks[0].Path != "test.go" {
		t.Errorf("path: got %q", blocks[0].Path)
	}
	if blocks[0].Content != "package test" {
		t.Errorf("content: got %q", blocks[0].Content)
	}
	if summary != "Summary." {
		t.Errorf("summary: got %q", summary)
	}
}

func TestParseFileBlocksSubdirectoryPaths(t *testing.T) {
	input := "```file:src/pkg/handler.go\npackage pkg\n```"
	blocks, _ := ParseFileBlocks(input)

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Path != "src/pkg/handler.go" {
		t.Errorf("path: got %q", blocks[0].Path)
	}
}
