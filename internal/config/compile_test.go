package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestExtractPurpose(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "first prose line",
			content: "# My Agent\n\nYou are the agent that does things.\n\n## Rules\n- stuff",
			want:    "You are the agent that does things.",
		},
		{
			name:    "fallback to h1",
			content: "# Backend Developer\n\n- builds things\n- deploys things",
			want:    "Backend Developer",
		},
		{
			name:    "skips sub-headings",
			content: "# Main\n## Sub\nActual purpose here.",
			want:    "Actual purpose here.",
		},
		{
			name:    "empty",
			content: "",
			want:    "Agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPurpose(tt.content)
			if got != tt.want {
				t.Errorf("extractPurpose() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractCapabilities(t *testing.T) {
	caps := extractCapabilities("game-engineer", "This agent implements code, handles debugging, and does testing.")
	seen := map[string]bool{}
	for _, c := range caps {
		seen[c] = true
	}
	if !seen["game_engineer"] {
		t.Error("missing base capability game_engineer")
	}
	if !seen["code_gen"] {
		t.Error("missing code_gen capability")
	}
	if !seen["debugging"] {
		t.Error("missing debugging capability")
	}
}

func TestCompileAgents(t *testing.T) {
	descs := []AgentDescription{
		{Name: "agent-a", Content: "# Agent A\n\nDoes task A with code and testing."},
		{Name: "agent-b", Content: "# Agent B\n\nHandles design and review."},
	}

	results, err := CompileAgents(descs, "anthropic/claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatalf("CompileAgents: %v", err)
	}

	// Should have 3: agent-a, agent-b, auto-added conductor.
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (2 custom + conductor)", len(results))
	}

	hasConductor := false
	for _, r := range results {
		if r.Definition.Name == "conductor" {
			hasConductor = true
		}
		// Every non-conductor agent should have a shell tool.
		if r.Definition.Name != "conductor" && len(r.Definition.Tools) == 0 {
			t.Errorf("agent %q has no tools", r.Definition.Name)
		}
	}
	if !hasConductor {
		t.Error("conductor was not auto-added")
	}
}

func TestCompileAgents_ConductorInInput(t *testing.T) {
	descs := []AgentDescription{
		{Name: "conductor", Content: "# Conductor\n\nOrchestrates everything."},
		{Name: "worker", Content: "# Worker\n\nDoes the work with code."},
	}

	results, err := CompileAgents(descs, "")
	if err != nil {
		t.Fatalf("CompileAgents: %v", err)
	}

	// Should have 2: conductor from input + worker. No duplicate conductor.
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

func TestReadAgentDescriptions(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "backend-dev.md"), []byte("# Backend Developer\nWrites backend code"), 0644)
	os.WriteFile(filepath.Join(dir, "frontend-dev.md"), []byte("# Frontend Developer\nWrites UI code"), 0644)
	// Non-.md file should be ignored.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an agent"), 0644)

	descs, err := ReadAgentDescriptions(dir)
	if err != nil {
		t.Fatalf("ReadAgentDescriptions: %v", err)
	}
	if len(descs) != 2 {
		t.Fatalf("got %d descriptions, want 2", len(descs))
	}

	names := map[string]bool{}
	for _, d := range descs {
		names[d.Name] = true
	}
	if !names["backend-dev"] || !names["frontend-dev"] {
		t.Errorf("names = %v, want backend-dev and frontend-dev", names)
	}
}

func TestReadAgentDescriptions_InvalidName(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "BadName.md"), []byte("invalid"), 0644)

	_, err := ReadAgentDescriptions(dir)
	if err == nil {
		t.Fatal("expected error for invalid agent name")
	}
}

func TestReadAgentDescriptions_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadAgentDescriptions(dir)
	if err == nil {
		t.Fatal("expected error for empty directory")
	}
}

func TestWriteCompiledAgents(t *testing.T) {
	dir := t.TempDir()

	results := []CompileResult{
		{
			Definition:       defWithName("test-agent", "anthropic/claude-sonnet-4-5-20250929"),
			SpecialKnowledge: "# Test Agent\nDoes testing.",
		},
	}

	if err := WriteCompiledAgents(dir, results); err != nil {
		t.Fatalf("WriteCompiledAgents: %v", err)
	}

	// Check YAML file exists.
	if _, err := os.Stat(filepath.Join(dir, "test-agent.yaml")); err != nil {
		t.Errorf("YAML file not created: %v", err)
	}

	// Check .md file exists.
	if _, err := os.Stat(filepath.Join(dir, "test-agent.md")); err != nil {
		t.Errorf("MD file not created: %v", err)
	}
}

func TestMarshalAgentDef(t *testing.T) {
	def := defWithName("my-agent", "anthropic/claude-sonnet-4-5-20250929")
	data, err := MarshalAgentDef(def)
	if err != nil {
		t.Fatalf("MarshalAgentDef: %v", err)
	}
	if len(data) == 0 {
		t.Error("empty marshal output")
	}
}

func defWithName(name, model string) runtime.AgentDefinition {
	return runtime.AgentDefinition{
		Name:         name,
		Purpose:      "Test purpose",
		Capabilities: []string{"testing"},
		Model:        model,
	}
}
