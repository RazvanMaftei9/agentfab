package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSandboxRun(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
	}

	output, _, err := Run(context.Background(), cfg, nil, "echo", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(output), "hello") {
		t.Errorf("output: got %q", output)
	}
}

func TestSandboxTimeout(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 100 * time.Millisecond,
	}

	_, _, err := Run(context.Background(), cfg, nil, "sleep", "10")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSandboxStrippedEnv(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
	}

	// Verify PATH is restricted.
	output, _, err := Run(context.Background(), cfg, nil, "env")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	env := string(output)
	if !strings.Contains(env, "PATH=") {
		t.Error("PATH should be set")
	}
	// Should not contain typical secrets.
	if strings.Contains(env, "ANTHROPIC_API_KEY") {
		t.Error("env should not contain API keys")
	}
}

func TestSandboxAllowedDirs(t *testing.T) {
	cfg := Config{
		WorkDir:     t.TempDir(),
		Timeout:     5 * time.Second,
		AllowedDirs: []string{"/tmp/scratch", "/tmp/agent", "/tmp/shared"},
	}

	output, _, err := Run(context.Background(), cfg, nil, "env")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	env := string(output)
	expected := []string{
		"SANDBOX_ALLOWED_DIR_0=/tmp/scratch",
		"SANDBOX_ALLOWED_DIR_1=/tmp/agent",
		"SANDBOX_ALLOWED_DIR_2=/tmp/shared",
	}
	for _, e := range expected {
		if !strings.Contains(env, e) {
			t.Errorf("missing %s in env", e)
		}
	}
}

func TestSandboxExtraEnv(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
	}

	extra := []string{"TOOL_INPUT_DIR=/tmp/in", "TOOL_OUTPUT_DIR=/tmp/out"}
	output, _, err := Run(context.Background(), cfg, extra, "env")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	env := string(output)
	for _, kv := range extra {
		if !strings.Contains(env, kv) {
			t.Errorf("missing %s in env", kv)
		}
	}
}

func TestSandboxKeepBackground(t *testing.T) {
	cfg := Config{
		WorkDir:        t.TempDir(),
		Timeout:        5 * time.Second,
		KeepBackground: true,
	}

	_, pgid, err := Run(context.Background(), cfg, nil, "echo", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if pgid == 0 {
		t.Error("expected non-zero PGID with KeepBackground=true")
	}
	// Clean up.
	KillProcessGroup(pgid)
}

func TestSandboxBackgroundNoRedirect(t *testing.T) {
	// This is the exact scenario that caused hangs with CombinedOutput:
	// a backgrounded process that does NOT redirect stdout. With pipe-based
	// capture, CombinedOutput blocks until the child closes the pipe (never).
	// With file-based capture, Wait() returns when the shell exits.
	cfg := Config{
		WorkDir:        t.TempDir(),
		Timeout:        5 * time.Second,
		KeepBackground: true,
	}

	// Background sleep WITHOUT redirecting stdout — previously would hang.
	output, pgid, err := Run(context.Background(), cfg, nil,
		"sh", "-c", "sleep 60 & echo shell_done")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(output), "shell_done") {
		t.Errorf("expected 'shell_done' in output, got: %s", output)
	}
	KillProcessGroup(pgid)
}

func TestSandboxDefaultKillsBackground(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
		// KeepBackground defaults to false.
	}

	_, pgid, err := Run(context.Background(), cfg, nil, "echo", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if pgid != 0 {
		t.Errorf("expected pgid=0 with default config, got %d", pgid)
	}
}
