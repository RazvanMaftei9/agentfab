package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func newTestExecutor(t *testing.T, tools map[string]runtime.ToolConfig) *ToolExecutor {
	t.Helper()
	scratchDir := t.TempDir()
	agentDir := t.TempDir()
	sharedDir := t.TempDir()

	return &ToolExecutor{
		Tools:     tools,
		TierPaths: []string{scratchDir, agentDir, sharedDir},
		AgentName: "test-agent",
	}
}

func shellTool() runtime.ToolConfig {
	return runtime.ToolConfig{
		Name:    "shell",
		Command: "$TOOL_ARG_COMMAND",
		Parameters: map[string]runtime.ToolParam{
			"command": {Type: "string", Required: true},
		},
		Config: map[string]string{"timeout": "30"},
	}
}

func TestToolExecutorShell(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{"shell": shellTool()})
	defer exec.Cleanup()

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "echo hello world"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "hello world") {
		t.Errorf("result should contain 'hello world', got %q", result)
	}
	if !strings.Contains(result, "[Saved tool output: $SCRATCH_DIR/.tool-results/") {
		t.Fatalf("expected saved output hint in result, got %q", result)
	}
	outPath := latestToolOutputPath(t, exec.TierPaths[0])
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read persisted output: %v", err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("persisted output missing expected content: %q", string(data))
	}
}

func TestToolExecutorShellMultiCommand(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{"shell": shellTool()})
	defer exec.Cleanup()

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-multi",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "echo line1 && echo line2 && echo line3"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "line1") || !strings.Contains(result, "line3") {
		t.Errorf("result missing expected lines: %s", result)
	}
}

func TestToolExecutorNamedTool(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{
		"greet": {
			Name:    "greet",
			Command: "echo Hello $TOOL_ARG_NAME",
			Parameters: map[string]runtime.ToolParam{
				"name": {Type: "string", Description: "Name to greet", Required: true},
			},
		},
	})

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-2",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "greet",
			Arguments: `{"name": "Alice"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Hello Alice") {
		t.Errorf("result: got %q, want it to contain %q", strings.TrimSpace(result), "Hello Alice")
	}
	outPath := latestToolOutputPath(t, exec.TierPaths[0])
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read persisted output: %v", err)
	}
	if !strings.Contains(string(data), "Hello Alice") {
		t.Fatalf("persisted output missing expected content: %q", string(data))
	}
}

func TestToolExecutorOutputTruncation(t *testing.T) {
	// Create a file with content larger than maxToolOutputBytes.
	scratchDir := t.TempDir()
	bigFile := filepath.Join(scratchDir, "big.txt")
	data := strings.Repeat("x", maxToolOutputBytes+1000)
	if err := os.WriteFile(bigFile, []byte(data), 0644); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	exec := &ToolExecutor{
		Tools:     map[string]runtime.ToolConfig{"shell": shellTool()},
		TierPaths: []string{scratchDir, t.TempDir(), t.TempDir()},
		AgentName: "test-agent",
	}
	defer exec.Cleanup()

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-3",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "cat ` + bigFile + `"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation marker in output")
	}
	if len(result) > maxToolOutputBytes+200 { // some overhead for truncation message
		t.Errorf("output too large after truncation: %d bytes", len(result))
	}
	outPath := latestToolOutputPath(t, exec.TierPaths[0])
	dataOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read persisted output: %v", err)
	}
	if len(dataOut) != len(data) {
		t.Fatalf("persisted output length mismatch: got %d want %d", len(dataOut), len(data))
	}
}

func TestToolExecutorTimeout(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{
		"slow": {
			Name:    "slow",
			Command: "sleep 10",
			Parameters: map[string]runtime.ToolParam{
				"dummy": {Type: "string"},
			},
			Config: map[string]string{"timeout": "1"}, // 1 second
		},
	})

	_, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-4",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "slow",
			Arguments: `{"dummy": "x"}`,
		},
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("expected failure error, got: %v", err)
	}
}

func TestToolExecutorUnknownTool(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{})

	_, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-5",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "nonexistent",
			Arguments: `{}`,
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got: %v", err)
	}
}

func TestToolExecutorShellEmptyCommand(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{"shell": shellTool()})

	_, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-6",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": ""}`,
		},
	})
	if err == nil {
		t.Fatal("expected error for empty shell command")
	}
}

func TestToolExecutorServerNonBlocking(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{"shell": shellTool()})
	defer exec.Cleanup()

	// Start a foreground server (no &). The wrapper should background it
	// automatically and return within ~shellForegroundWait seconds, NOT
	// block until the sandbox timeout.
	start := time.Now()
	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-server",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "sleep 60"}`,
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return within ~shellForegroundWait + small buffer, NOT 30s (the timeout).
	if elapsed > time.Duration(shellForegroundWait+5)*time.Second {
		t.Errorf("took too long: %s (should return in ~%ds)", elapsed, shellForegroundWait)
	}

	// Should report the process is running in background.
	if !strings.Contains(result, "running in background") {
		t.Errorf("expected 'running in background' message, got: %s", result)
	}
	pid, ok := parseBackgroundPID(result)
	if !ok {
		t.Fatalf("expected background PID in output, got: %s", result)
	}

	// The background process should still be alive from a subsequent shell invocation.
	result2, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-check-running",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "kill -0 ` + pid + ` 2>/dev/null && echo running || echo stopped"}`,
		},
	})
	if err != nil {
		t.Fatalf("process liveness check failed: %v", err)
	}
	if !strings.Contains(result2, "running") {
		t.Errorf("expected process to be running, got: %s", result2)
	}
}

func TestToolExecutorCleanupKillsServer(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{"shell": shellTool()})

	// Start a long-running process.
	startResult, startErr := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-start",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "sleep 60"}`,
		},
	})
	if startErr != nil {
		t.Fatalf("unexpected error starting background process: %v", startErr)
	}
	pid, ok := parseBackgroundPID(startResult)
	if !ok {
		t.Fatalf("expected background PID in output, got: %s", startResult)
	}

	// Cleanup should kill it.
	exec.Cleanup()

	// Verify it's dead.
	result, _ := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-check",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "kill -0 ` + pid + ` 2>/dev/null && echo running || echo stopped"}`,
		},
	})
	if !strings.Contains(result, "stopped") {
		t.Fatalf("expected process to be stopped after cleanup, got: %s", result)
	}
	exec.Cleanup() // final cleanup
}

func TestToolExecutorExitCodePreserved(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{"shell": shellTool()})
	defer exec.Cleanup()

	// A command that exits with non-zero should still return the error.
	_, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-fail",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "shell",
			Arguments: `{"command": "exit 1"}`,
		},
	})
	if err == nil {
		t.Error("expected error for exit 1 command")
	}
	outPath := latestToolOutputPath(t, exec.TierPaths[0])
	data, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("read persisted error output: %v", readErr)
	}
	if !strings.Contains(string(data), "Error:") {
		t.Fatalf("expected persisted output to include error details, got %q", string(data))
	}
}

func TestToolExecutorConfigEnvVars(t *testing.T) {
	exec := newTestExecutor(t, map[string]runtime.ToolConfig{
		"mycli": {
			Name:    "mycli",
			Command: `echo "api_url=$TOOL_API_URL user=$TOOL_USER"`,
			Parameters: map[string]runtime.ToolParam{
				"dummy": {Type: "string", Required: false},
			},
			Config: map[string]string{
				"timeout": "10",
				"match":   "*.go",
				"api_url": "https://example.com",
				"user":    "alice",
			},
		},
	})
	defer exec.Cleanup()

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-config",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "mycli",
			Arguments: `{"dummy": "x"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "api_url=https://example.com") {
		t.Errorf("expected TOOL_API_URL in output, got %q", result)
	}
	if !strings.Contains(result, "user=alice") {
		t.Errorf("expected TOOL_USER in output, got %q", result)
	}
}

func TestToolExecutorPassthroughEnv(t *testing.T) {
	const envKey = "AGENTFAB_TEST_PASSTHROUGH_SECRET"
	const envVal = "s3cret-test-value"
	os.Setenv(envKey, envVal)
	defer os.Unsetenv(envKey)

	exec := newTestExecutor(t, map[string]runtime.ToolConfig{
		"search": {
			Name:    "search",
			Command: `echo "key=$AGENTFAB_TEST_PASSTHROUGH_SECRET"`,
			Parameters: map[string]runtime.ToolParam{
				"query": {Type: "string", Required: true},
			},
			PassthroughEnv: []string{envKey},
		},
	})
	defer exec.Cleanup()

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-passthrough",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "search",
			Arguments: `{"query": "test"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "key="+envVal) {
		t.Errorf("expected passthrough env var in output, got %q", result)
	}
}

func TestToolExecutorPassthroughEnvMissing(t *testing.T) {
	const envKey = "AGENTFAB_TEST_MISSING_VAR"
	os.Unsetenv(envKey) // ensure it's not set

	exec := newTestExecutor(t, map[string]runtime.ToolConfig{
		"search": {
			Name:    "search",
			Command: `echo "key=${AGENTFAB_TEST_MISSING_VAR:-unset}"`,
			Parameters: map[string]runtime.ToolParam{
				"query": {Type: "string", Required: true},
			},
			PassthroughEnv: []string{envKey},
		},
	})
	defer exec.Cleanup()

	result, err := exec.Execute(context.Background(), schema.ToolCall{
		ID:   "call-passthrough-missing",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "search",
			Arguments: `{"query": "test"}`,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "key=unset") {
		t.Errorf("expected missing env var to be unset, got %q", result)
	}
}

func latestToolOutputPath(t *testing.T, scratchDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(scratchDir, toolOutputDirName, "*.out"))
	if err != nil {
		t.Fatalf("glob persisted outputs: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected persisted tool output under %s", filepath.Join(scratchDir, toolOutputDirName))
	}
	latest := matches[0]
	latestInfo, err := os.Stat(latest)
	if err != nil {
		t.Fatalf("stat persisted output %s: %v", latest, err)
	}
	for _, m := range matches[1:] {
		info, err := os.Stat(m)
		if err != nil {
			t.Fatalf("stat persisted output %s: %v", m, err)
		}
		if info.ModTime().After(latestInfo.ModTime()) {
			latest = m
			latestInfo = info
		}
	}
	return latest
}

func parseBackgroundPID(result string) (string, bool) {
	const prefix = "[Process "
	const suffix = " running in background]"
	start := strings.Index(result, prefix)
	if start < 0 {
		return "", false
	}
	start += len(prefix)
	end := strings.Index(result[start:], suffix)
	if end < 0 {
		return "", false
	}
	pid := strings.TrimSpace(result[start : start+end])
	if pid == "" {
		return "", false
	}
	return pid, true
}
