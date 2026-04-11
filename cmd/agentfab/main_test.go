package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/config"
)

func TestParseInteractionMode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "work"},
		{"1", "work"},
		{"work", "work"},
		{"2", "chat"},
		{"chat", "chat"},
		{"3", "status"},
		{"status", "status"},
		{"q", "quit"},
		{"quit", "quit"},
		{"unknown", ""},
	}

	for _, tc := range tests {
		if got := parseInteractionMode(tc.in); got != tc.want {
			t.Fatalf("parseInteractionMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultRootArgs(t *testing.T) {
	if got := defaultRootArgs(nil); len(got) != 1 || got[0] != "run" {
		t.Fatalf("defaultRootArgs(nil) = %v, want [run]", got)
	}

	if got := defaultRootArgs([]string{"run"}); got != nil {
		t.Fatalf("defaultRootArgs(non-empty) = %v, want nil", got)
	}
}

func TestParseDirectAgentCommandAsk(t *testing.T) {
	cmd, matched, err := parseDirectAgentCommand("/ask developer what changed?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Fatal("expected command to match")
	}
	if cmd.mode != "ask" || cmd.agent != "developer" || cmd.message != "what changed?" {
		t.Fatalf("unexpected parse result: %+v", cmd)
	}
}

func TestParseDirectAgentCommandDo(t *testing.T) {
	cmd, matched, err := parseDirectAgentCommand("/do architect update the taskgraph")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Fatal("expected command to match")
	}
	if cmd.mode != "do" || cmd.agent != "architect" || cmd.message != "update the taskgraph" {
		t.Fatalf("unexpected parse result: %+v", cmd)
	}
}

func TestParseDirectAgentCommandUsageError(t *testing.T) {
	_, matched, err := parseDirectAgentCommand("/ask developer")
	if !matched {
		t.Fatal("expected command to match")
	}
	if err == nil {
		t.Fatal("expected usage error")
	}
}

func TestParseDirectAgentCommandNonCommand(t *testing.T) {
	_, matched, err := parseDirectAgentCommand("build a todo app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Fatal("did not expect command match")
	}
}

func TestFindProjectEntry(t *testing.T) {
	entries := []config.ProjectEntry{
		{Name: "alpha", Dir: "/tmp/alpha"},
		{Name: "beta", Dir: "/tmp/beta"},
	}

	entry, err := findProjectEntry(entries, "beta")
	if err != nil {
		t.Fatalf("findProjectEntry: %v", err)
	}
	if entry.Name != "beta" {
		t.Fatalf("got %q, want beta", entry.Name)
	}
}

func TestProjectConfigPath(t *testing.T) {
	t.Run("valid project", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "agents.yaml")
		if err := os.WriteFile(cfg, []byte("fabric:\n  name: demo\n  version: 1\n"), 0644); err != nil {
			t.Fatalf("write agents.yaml: %v", err)
		}

		got, err := projectConfigPath(config.ProjectEntry{Name: "demo", Dir: dir})
		if err != nil {
			t.Fatalf("projectConfigPath: %v", err)
		}
		if got != cfg {
			t.Fatalf("got %q, want %q", got, cfg)
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		_, err := projectConfigPath(config.ProjectEntry{Name: "missing", Dir: filepath.Join(t.TempDir(), "gone")})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "directory does not exist") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing agents file", func(t *testing.T) {
		dir := t.TempDir()
		_, err := projectConfigPath(config.ProjectEntry{Name: "missing-config", Dir: dir})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "agents.yaml") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestProjectWorkspaceEmpty(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		empty, err := projectWorkspaceEmpty(config.ProjectEntry{Name: "demo", Dir: dir})
		if err != nil {
			t.Fatalf("projectWorkspaceEmpty: %v", err)
		}
		if !empty {
			t.Fatal("expected empty workspace")
		}
	})

	t.Run("not empty", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "agents.yaml"), []byte("x"), 0644); err != nil {
			t.Fatalf("write agents.yaml: %v", err)
		}
		empty, err := projectWorkspaceEmpty(config.ProjectEntry{Name: "demo", Dir: dir})
		if err != nil {
			t.Fatalf("projectWorkspaceEmpty: %v", err)
		}
		if empty {
			t.Fatal("expected non-empty workspace")
		}
	})
}

func TestShortProjectError(t *testing.T) {
	tests := []struct {
		err  string
		want string
	}{
		{"workspace is empty", "empty workspace"},
		{"directory does not exist", "missing directory"},
		{"project is missing /tmp/demo/agents.yaml", "missing agents.yaml"},
		{"path is not a directory", "invalid path"},
		{"something else", "invalid project"},
	}

	for _, tt := range tests {
		got := shortProjectError(assertError(tt.err))
		if got != tt.want {
			t.Fatalf("shortProjectError(%q) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestPromptRecreateProject(t *testing.T) {
	entry := config.ProjectEntry{Name: "yappler", Dir: "/tmp/yappler"}

	yes, err := promptRecreateProject(ioDiscard{}, func() (string, bool) { return "", true }, false, entry)
	if err != nil {
		t.Fatalf("promptRecreateProject: %v", err)
	}
	if !yes {
		t.Fatal("empty answer should default to yes")
	}

	no, err := promptRecreateProject(ioDiscard{}, func() (string, bool) { return "n", true }, false, entry)
	if err != nil {
		t.Fatalf("promptRecreateProject: %v", err)
	}
	if no {
		t.Fatal("expected no for 'n'")
	}
}

func TestShutdownFabricCancelsBeforeShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	err := shutdownFabric(cancel, nil, func(shutCtx context.Context) error {
		select {
		case <-ctx.Done():
		default:
			t.Fatal("expected caller context to be canceled before shutdown")
		}

		select {
		case <-shutCtx.Done():
			t.Fatal("shutdown context should still be active")
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatalf("shutdownFabric: %v", err)
	}
}

func TestShutdownFabricTimesOutShutdown(t *testing.T) {
	err := shutdownFabric(func() {}, nil, func(shutCtx context.Context) error {
		<-shutCtx.Done()
		return shutCtx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownFabric error = %v, want deadline exceeded", err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

type assertError string

func (e assertError) Error() string { return string(e) }
