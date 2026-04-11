package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/sandbox"
)

// runTools executes post-processing tools on matched artifact files.
func (a *Agent) runTools(ctx context.Context, rp *resultParts, requestID string, taskMeta map[string]string) {
	tierPaths := a.toolTierPaths()
	sharedDir := a.sharedToolDir()
	if len(tierPaths) < 3 || sharedDir == "" {
		return
	}
	if a.Workspace != nil && a.Workspace.Shared != nil {
		if err := a.Workspace.Shared.Refresh(ctx); err != nil {
			slog.Warn("refresh shared workspace failed", "agent", a.Def.Name, "error", err)
		}
	}

	for _, tc := range a.Def.Tools {
		if tc.Command == "" || !tc.IsPostProcess() {
			continue
		}

		pattern := tc.Config["match"]
		if pattern == "" {
			pattern = "*"
		}

		var matched []string
		for _, f := range rp.files {
			ok, err := filepath.Match(pattern, filepath.Base(f))
			if err != nil {
				slog.Warn("bad tool match pattern", "tool", tc.Name, "pattern", pattern, "error", err)
				continue
			}
			if ok {
				matched = append(matched, f)
			}
		}
		if len(matched) == 0 {
			continue
		}

		outputs, err := execTool(ctx, tc, execToolRequest{
			dir:         rp.dir,
			resolvedDir: filepath.Join(sharedDir, rp.dir),
			agentName:   a.Def.Name,
			requestID:   requestID,
			taskMeta:    taskMeta,
			files:       matched,
			tierPaths:   tierPaths,
			readFile: func(relPath string) ([]byte, error) {
				return a.Storage.Read(ctx, runtime.TierShared, fmt.Sprintf("%s/%s", rp.dir, relPath))
			},
		})
		if err != nil {
			slog.Warn("tool execution failed", "tool", tc.Name, "agent", a.Def.Name, "error", err)
			continue
		}

		for _, out := range outputs {
			outPath := fmt.Sprintf("%s/%s", rp.dir, out.path)
			if err := a.Storage.Write(ctx, runtime.TierShared, outPath, out.data); err != nil {
				slog.Warn("failed to write tool output", "agent", a.Def.Name, "tool_path", out.path, "error", err)
				continue
			}
			rp.parts = append(rp.parts, message.FilePart{
				URI:      outPath,
				MimeType: out.mimeType,
				Name:     out.path,
			})
		}
	}
}

func (a *Agent) toolTierPaths() []string {
	if a.ToolExecutor != nil && len(a.ToolExecutor.TierPaths) >= 3 {
		return a.ToolExecutor.TierPaths
	}
	if a.Workspace != nil {
		return a.Workspace.TierPaths()
	}
	if a.Storage == nil {
		return nil
	}
	return []string{
		a.Storage.TierDir(runtime.TierScratch),
		a.Storage.TierDir(runtime.TierAgent),
		a.Storage.TierDir(runtime.TierShared),
	}
}

func (a *Agent) sharedToolDir() string {
	tierPaths := a.toolTierPaths()
	if len(tierPaths) < 3 {
		return ""
	}
	return tierPaths[2]
}

type execToolRequest struct {
	dir         string
	resolvedDir string
	agentName   string
	requestID   string
	taskMeta    map[string]string // task_id, task_description, etc.
	files       []string
	tierPaths   []string // scratch, agent, shared — passed as AllowedDirs
	readFile    func(string) ([]byte, error)
}

type toolOutput struct {
	path     string
	data     []byte
	mimeType string
}

func execTool(ctx context.Context, tc runtime.ToolConfig, req execToolRequest) ([]toolOutput, error) {
	workDir, err := os.MkdirTemp("", "agentfab-tool-*")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	inputDir := filepath.Join(workDir, "input")
	outputDir := filepath.Join(workDir, "output")
	if err := os.MkdirAll(inputDir, 0755); err != nil {
		return nil, fmt.Errorf("create input dir: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	for _, file := range req.files {
		data, err := req.readFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		dest := filepath.Join(inputDir, filepath.Base(file))
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return nil, fmt.Errorf("write %s to input: %w", file, err)
		}
	}

	env := []string{
		"TOOL_INPUT_DIR=" + inputDir,
		"TOOL_OUTPUT_DIR=" + outputDir,
		"TOOL_ARTIFACT_DIR=" + req.dir,
		"TOOL_RESOLVED_DIR=" + req.resolvedDir,
		"TOOL_MATCHED_FILES=" + strings.Join(req.files, "\n"),
		"TOOL_REQUEST_ID=" + req.requestID,
		"TOOL_AGENT_NAME=" + req.agentName,
	}
	for k, v := range req.taskMeta {
		env = append(env, "TOOL_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))+"="+v)
	}
	for k, v := range tc.Config {
		if k == "match" || k == "timeout" {
			continue
		}
		env = append(env, "TOOL_"+strings.ToUpper(k)+"="+v)
	}

	timeout := 30 * time.Second
	if v, ok := tc.Config["timeout"]; ok {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	cfg := sandbox.Config{
		WorkDir:     workDir,
		Timeout:     timeout,
		AllowedDirs: req.tierPaths,
	}

	output, _, err := sandbox.Run(ctx, cfg, env, "sh", "-c", tc.Command)
	if len(output) > 0 {
		slog.Debug("tool output", "tool", tc.Name, "output", string(output))
	}
	if err != nil {
		return nil, fmt.Errorf("command failed: %w\n%s", err, string(output))
	}

	return collectOutputs(outputDir)
}

func collectOutputs(outputDir string) ([]toolOutput, error) {
	var outputs []toolOutput
	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(outputDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read output %s: %w", rel, err)
		}
		outputs = append(outputs, toolOutput{
			path:     rel,
			data:     data,
			mimeType: mimeForPath(rel),
		})
		return nil
	})
	return outputs, err
}
