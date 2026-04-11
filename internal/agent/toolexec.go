package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/sandbox"
)

const maxToolOutputBytes = 50_000

// shellForegroundWait: commands finishing within this many seconds return output
// normally; longer-running processes are backgrounded.
const shellForegroundWait = 10
const toolOutputDirName = ".tool-results"

// shellWrapper wraps shell commands for non-blocking execution.
// Original command via $_SANDBOX_CMD; args: %q = log file, %d = foreground wait secs.
const shellWrapper = `_LOG=%q
eval "$_SANDBOX_CMD" > "$_LOG" 2>&1 &
_PID=$!
# Quick check for instant commands
sleep 0.1
if ! kill -0 $_PID 2>/dev/null; then
  wait $_PID; _RC=$?; cat "$_LOG"; exit $_RC
fi
# Poll every second up to foreground wait
_i=0
while [ $_i -lt %d ]; do
  sleep 1
  if ! kill -0 $_PID 2>/dev/null; then
    wait $_PID; _RC=$?
    cat "$_LOG"
    exit $_RC
  fi
  _i=$((_i + 1))
done
# Still running — it's a server or long process. Return immediately.
printf "[Process %%d running in background]\n" "$_PID"
printf "[Output log: %%s]\n" "$_LOG"
printf "[Check status: kill -0 %%d 2>/dev/null && echo running || echo stopped]\n" "$_PID"
_TAIL=$(tail -20 "$_LOG" 2>/dev/null)
[ -n "$_TAIL" ] && printf "--- recent output ---\n%%s\n" "$_TAIL"
exit 0
`

// ToolExecutor handles execution of live tool calls from the LLM.
type ToolExecutor struct {
	Tools           map[string]runtime.ToolConfig // name → config (live tools only)
	TierPaths       []string                      // scratch, agent, shared
	AgentName       string
	MaxOutputTokens int // model's max output tokens
	ContextLimit    int // model's context window in tokens
	SyncWorkspace   func(context.Context) error

	mu       sync.Mutex
	bgGroups []int // process group IDs kept alive for background servers
}

// SandboxEnv returns the standard environment variables for sandbox commands:
// SCRATCH_DIR, AGENT_DIR, SHARED_DIR, and HOME.
func (e *ToolExecutor) SandboxEnv() []string {
	if len(e.TierPaths) < 3 {
		return nil
	}
	return []string{
		"SCRATCH_DIR=" + e.TierPaths[0],
		"AGENT_DIR=" + e.TierPaths[1],
		"SHARED_DIR=" + e.TierPaths[2],
		"HOME=" + e.TierPaths[1],
	}
}

// SandboxConfig returns a sandbox.Config suitable for running commands with the
// same filesystem policy as live tool calls.
func (e *ToolExecutor) SandboxConfig(timeout time.Duration) sandbox.Config {
	cfg := sandbox.Config{
		WorkDir:     e.TierPaths[0],
		Timeout:     timeout,
		AllowedDirs: e.TierPaths,
	}
	if len(e.TierPaths) >= 3 {
		cfg.Restrict = &sandbox.Policy{
			ReadWrite: []string{e.TierPaths[0], e.TierPaths[1]},
			ReadOnly:  []string{e.TierPaths[2]},
		}
	}
	return cfg
}

// Execute runs a single tool call and returns the result string.
func (e *ToolExecutor) Execute(ctx context.Context, call schema.ToolCall) (string, error) {
	name := call.Function.Name

	var args map[string]any
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("parse tool arguments: %w", err)
		}
	}

	var command string
	origCmd := ""
	var env []string
	keepBackground := false

	if name == "shell" {
		cmd, ok := args["command"].(string)
		if !ok || cmd == "" {
			return "", fmt.Errorf("shell tool requires a 'command' argument")
		}
		origCmd = cmd

		logFile := filepath.Join(e.TierPaths[0], fmt.Sprintf(".cmd_%d.log", time.Now().UnixNano()))
		env = append(env, "_SANDBOX_CMD="+cmd)
		command = fmt.Sprintf(shellWrapper, logFile, shellForegroundWait)
		keepBackground = true

		env = append(env, e.SandboxEnv()...)
	} else {
		tc, ok := e.Tools[name]
		if !ok {
			return "", fmt.Errorf("unknown tool %q", name)
		}
		if tc.Command == "" {
			return "", fmt.Errorf("tool %q has no command defined", name)
		}
		command = tc.Command
		origCmd = command

		for k, v := range args {
			env = append(env, fmt.Sprintf("TOOL_ARG_%s=%v", strings.ToUpper(k), v))
		}

		argsJSON, _ := json.Marshal(args)
		env = append(env, "TOOL_ARGS_JSON="+string(argsJSON))

		for k, v := range tc.Config {
			if k == "timeout" || k == "match" {
				continue
			}
			env = append(env, fmt.Sprintf("TOOL_%s=%s", strings.ToUpper(k), v))
		}

		for _, envName := range tc.PassthroughEnv {
			if v, ok := os.LookupEnv(envName); ok {
				env = append(env, envName+"="+v)
			}
		}
	}

	timeout := 30 * time.Second
	if tc, ok := e.Tools[name]; ok {
		if v, ok := tc.Config["timeout"]; ok {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				timeout = time.Duration(secs) * time.Second
			}
		}
	}

	cfg := e.SandboxConfig(timeout)
	cfg.KeepBackground = keepBackground

	toolStart := time.Now()
	slog.Debug("executing live tool", "agent", e.AgentName, "tool", name, "command", truncateLog(command, 100))
	if name == "shell" {
		slog.Info("shell command", "agent", e.AgentName, "cmd", truncateLog(origCmd, 500))
	}

	output, pgid, err := sandbox.Run(ctx, cfg, env, "sh", "-c", command)
	toolDuration := time.Since(toolStart)
	rawOutput := string(output)
	if rawOutput == "" && err != nil {
		rawOutput = "Error: " + err.Error() + "\n"
	}
	savedRef := e.persistToolOutput(call, origCmd, rawOutput, err)

	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	logAttrs := []any{
		"agent", e.AgentName,
		"tool", name,
		"duration_ms", toolDuration.Milliseconds(),
		"exit_code", exitCode,
		"output_bytes", len(rawOutput),
	}
	if exitCode != 0 {
		logAttrs = append(logAttrs, "output_head", truncateLog(rawOutput, 500))
	}
	slog.Info("tool call", logAttrs...)

	if pgid > 0 {
		e.mu.Lock()
		e.bgGroups = append(e.bgGroups, pgid)
		e.mu.Unlock()
	}

	if e.SyncWorkspace != nil {
		if syncErr := e.SyncWorkspace(ctx); syncErr != nil && err == nil {
			err = fmt.Errorf("sync workspace: %w", syncErr)
		}
	}

	if err != nil {
		result := rawOutput
		if result == "" {
			result = err.Error()
		}
		result = truncateOutput(result)
		if savedRef != "" {
			hint := "[Saved tool output: " + savedRef + "]"
			if !strings.Contains(result, hint) {
				if strings.TrimSpace(result) == "" {
					result = hint
				} else {
					result = result + "\n\n" + hint
				}
			}
		}
		return result, fmt.Errorf("tool %q failed: %w", name, err)
	}

	result := truncateOutput(rawOutput)
	if result == "" {
		result = "(no output)"
	}
	if savedRef != "" {
		hint := "[Saved tool output: " + savedRef + "]"
		if !strings.Contains(result, hint) {
			if strings.TrimSpace(result) == "" {
				result = hint
			} else {
				result = result + "\n\n" + hint
			}
		}
	}
	return result, nil
}

type toolOutputMeta struct {
	TimestampUTC string `json:"timestamp_utc"`
	Tool         string `json:"tool"`
	ToolCallID   string `json:"tool_call_id"`
	Command      string `json:"command,omitempty"`
	OutputFile   string `json:"output_file"`
	OutputBytes  int    `json:"output_bytes"`
	Error        string `json:"error,omitempty"`
}

func (e *ToolExecutor) persistToolOutput(call schema.ToolCall, command, output string, execErr error) string {
	if len(e.TierPaths) == 0 {
		return ""
	}
	scratchDir := e.TierPaths[0]
	outputDir := filepath.Join(scratchDir, toolOutputDirName)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		slog.Warn("failed to create tool output dir", "agent", e.AgentName, "dir", outputDir, "error", err)
		return ""
	}

	ts := time.Now().UTC()
	toolPart := sanitizePathPart(call.Function.Name)
	if toolPart == "" {
		toolPart = "tool"
	}
	idPart := sanitizePathPart(call.ID)
	if idPart == "" {
		idPart = "call"
	}
	base := fmt.Sprintf("%s-%s-%s", ts.Format("20060102T150405.000000000Z"), toolPart, idPart)
	outputAbsPath := filepath.Join(outputDir, base+".out")
	if err := os.WriteFile(outputAbsPath, []byte(output), 0644); err != nil {
		slog.Warn("failed to persist tool output", "agent", e.AgentName, "path", outputAbsPath, "error", err)
		return ""
	}

	meta := toolOutputMeta{
		TimestampUTC: ts.Format(time.RFC3339Nano),
		Tool:         call.Function.Name,
		ToolCallID:   call.ID,
		Command:      command,
		OutputFile:   "$SCRATCH_DIR/" + toolOutputDirName + "/" + filepath.Base(outputAbsPath),
		OutputBytes:  len(output),
	}
	if execErr != nil {
		meta.Error = execErr.Error()
	}

	metaBytes, err := json.Marshal(meta)
	if err == nil {
		_ = os.WriteFile(filepath.Join(outputDir, base+".json"), metaBytes, 0644)
		indexPath := filepath.Join(outputDir, "index.jsonl")
		f, ferr := os.OpenFile(indexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if ferr == nil {
			_, _ = f.Write(append(metaBytes, '\n'))
			_ = f.Close()
		}
	}

	return meta.OutputFile
}

func sanitizePathPart(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// Cleanup kills all background process groups accumulated during the tool loop.
func (e *ToolExecutor) Cleanup() {
	e.mu.Lock()
	groups := e.bgGroups
	e.bgGroups = nil
	e.mu.Unlock()

	for _, pgid := range groups {
		slog.Debug("killing background process group", "agent", e.AgentName, "pgid", pgid)
		sandbox.KillProcessGroup(pgid)
	}
}

func truncateOutput(s string) string {
	if len(s) <= maxToolOutputBytes {
		return s
	}
	return s[:maxToolOutputBytes] + fmt.Sprintf("\n\n[... truncated, %d bytes omitted]", len(s)-maxToolOutputBytes)
}

func truncateLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
