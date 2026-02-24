package agent

import (
	"path/filepath"
	"strings"
)

// FileBlock represents a single file extracted from LLM output.
type FileBlock struct {
	Path    string // relative path within artifact dir (e.g., "README.md", "src/login.go")
	Content string
}

// ParseFileBlocks extracts ```file:path blocks from LLM output.
// Returns extracted file blocks and a summary (all text outside file blocks).
// If no file blocks are found, returns nil blocks and the full content as summary.
func ParseFileBlocks(content string) (blocks []FileBlock, summary string) {
	lines := strings.Split(content, "\n")
	var summaryParts []string
	var currentBlock *FileBlock
	var blockContent []string
	openFence := ""   // the fence string that opened the current block
	fenceDepth := 0 // count of nested fences inside a file block

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if currentBlock != nil {
			if isClosingFence(trimmed, openFence) {
				if fenceDepth > 0 {
					fenceDepth--
					blockContent = append(blockContent, line)
				} else {
					currentBlock.Content = strings.Join(blockContent, "\n")
					blocks = append(blocks, *currentBlock)
					currentBlock = nil
					blockContent = nil
					openFence = ""
				}
			} else if isOpenFence(trimmed, len(openFence)) {
				fenceDepth++
				blockContent = append(blockContent, line)
			} else {
				blockContent = append(blockContent, line)
			}
			continue
		}

		fence, path := parseFileFence(trimmed)
		if fence != "" && path != "" {
			if !isValidPath(path) {
				summaryParts = append(summaryParts, line)
				continue
			}
			currentBlock = &FileBlock{Path: filepath.Clean(path)}
			blockContent = nil
			openFence = fence
			fenceDepth = 0
		} else {
			summaryParts = append(summaryParts, line)
		}
	}

	// If a block was never closed, treat it as successfully closed.
	// This frequently happens when an LLM reaches its max tokens during output.
	if currentBlock != nil {
		currentBlock.Content = strings.Join(blockContent, "\n")
		blocks = append(blocks, *currentBlock)
	}

	if len(blocks) == 0 {
		return nil, content
	}

	summary = strings.TrimSpace(strings.Join(summaryParts, "\n"))
	return blocks, summary
}

func parseFileFence(line string) (fence, path string) {
	i := 0
	for i < len(line) && line[i] == '`' {
		i++
	}
	if i < 3 {
		return "", ""
	}
	fence = line[:i]
	rest := line[i:]

	if !strings.HasPrefix(rest, "file:") {
		return "", ""
	}
	path = strings.TrimSpace(rest[len("file:"):])
	if path == "" {
		return "", ""
	}
	return fence, path
}

func isClosingFence(line, openFence string) bool {
	i := 0
	for i < len(line) && line[i] == '`' {
		i++
	}
	if i < len(openFence) {
		return false
	}
	rest := strings.TrimSpace(line[i:])
	return rest == ""
}

func isOpenFence(line string, minBackticks int) bool {
	i := 0
	for i < len(line) && line[i] == '`' {
		i++
	}
	if i < minBackticks {
		return false
	}
	rest := strings.TrimSpace(line[i:])
	return rest != ""
}

func isValidPath(path string) bool {
	if path == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) {
		return false
	}
	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		if part == ".." {
			return false
		}
	}
	return true
}
