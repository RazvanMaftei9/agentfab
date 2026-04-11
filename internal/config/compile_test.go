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
			name:    "structured template",
			content: "# My Agent\n\n## Purpose\nHandles backend API development.\n\n## Capabilities\n- code_gen",
			want:    "Handles backend API development.",
		},
		{
			name:    "legacy first prose line",
			content: "# My Agent\n\nYou are the agent that does things.\n\n## Rules\n- stuff",
			want:    "You are the agent that does things.",
		},
		{
			name:    "legacy fallback to h1",
			content: "# Backend Developer\n\n- builds things\n- deploys things",
			want:    "Backend Developer",
		},
		{
			name:    "legacy skips sub-headings",
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
			sections := parseSections(tt.content)
			got := extractSectionText(sections, "purpose")
			if got == "" {
				got = extractPurposeLegacy(tt.content)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractCapabilities(t *testing.T) {
	content := "# Agent\n\n## Capabilities\n- code_gen\n- debugging\n- testing\n"
	sections := parseSections(content)
	caps := extractSectionList(sections, "capabilities")

	if len(caps) != 3 {
		t.Fatalf("got %d capabilities, want 3", len(caps))
	}

	seen := map[string]bool{}
	for _, c := range caps {
		seen[c] = true
	}
	for _, want := range []string{"code_gen", "debugging", "testing"} {
		if !seen[want] {
			t.Errorf("missing capability %q", want)
		}
	}
}

func TestExtractCapabilitiesLegacy(t *testing.T) {
	caps := extractCapabilitiesLegacy("game-engineer", "This agent implements code, handles debugging, and does testing.")
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

func TestExtractSectionTools(t *testing.T) {
	content := `# Agent

## Tools
### shell
Execute shell commands for builds and tests.
Timeout: 60

### web-search
Search the web for documentation.
Command: curl -s "https://api.example.com/search?q=$TOOL_ARG_QUERY"
Mode: live
Passthrough_env: API_KEY, OTHER_KEY
`
	sections := parseSections(content)
	tools := extractSectionTools(sections)

	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}

	shell := tools[0]
	if shell.Name != "shell" {
		t.Errorf("tool 0 name: got %q, want %q", shell.Name, "shell")
	}
	if shell.Config["timeout"] != "60" {
		t.Errorf("shell timeout: got %q, want %q", shell.Config["timeout"], "60")
	}
	if shell.Command != "$TOOL_ARG_COMMAND" {
		t.Errorf("shell should get default command, got %q", shell.Command)
	}

	ws := tools[1]
	if ws.Name != "web-search" {
		t.Errorf("tool 1 name: got %q, want %q", ws.Name, "web-search")
	}
	if ws.Mode != "live" {
		t.Errorf("web-search mode: got %q, want %q", ws.Mode, "live")
	}
	if len(ws.PassthroughEnv) != 2 {
		t.Fatalf("web-search passthrough_env: got %d, want 2", len(ws.PassthroughEnv))
	}
	if ws.PassthroughEnv[0] != "API_KEY" || ws.PassthroughEnv[1] != "OTHER_KEY" {
		t.Errorf("web-search passthrough_env: got %v", ws.PassthroughEnv)
	}
}

func TestExtractVerifyConfig(t *testing.T) {
	content := `# Agent

## Verify
Command: cd $SCRATCH_DIR && go test ./...
Max Retries: 3
`
	sections := parseSections(content)
	vc := extractVerifyConfig(sections)
	if vc == nil {
		t.Fatal("expected verify config")
	}
	if vc.Command != "cd $SCRATCH_DIR && go test ./..." {
		t.Errorf("command: got %q", vc.Command)
	}
	if vc.MaxRetries != 3 {
		t.Errorf("max retries: got %d, want 3", vc.MaxRetries)
	}
}

func TestExtractVerifyConfigMissing(t *testing.T) {
	sections := parseSections("# Agent\n\n## Purpose\nDoes stuff.")
	vc := extractVerifyConfig(sections)
	if vc != nil {
		t.Error("expected nil verify config when section is missing")
	}
}

func TestExtractModel(t *testing.T) {
	content := "# Agent\n\n## Model\nopenai/gpt-5.2\n"
	sections := parseSections(content)
	model := extractSectionText(sections, "model")
	if model != "openai/gpt-5.2" {
		t.Errorf("got %q, want %q", model, "openai/gpt-5.2")
	}
}

func TestExtractEscalationTarget(t *testing.T) {
	content := "# Agent\n\n## Escalation Target\narchitect\n"
	sections := parseSections(content)
	target := extractSectionText(sections, "escalation target")
	if target != "architect" {
		t.Errorf("got %q, want %q", target, "architect")
	}
}

func TestCompileStructuredTemplate(t *testing.T) {
	content := `# Backend Dev

## Purpose
Implement backend services and APIs.

## Capabilities
- code_gen
- api_design
- testing

## Model
openai/gpt-5.2

## Tools
### shell
Execute shell commands for builds and tests.

## Review Guidelines
- Verify all endpoints return correct status codes
- Check error handling

## Verify
Command: cd $SCRATCH_DIR && go test ./...
Max Retries: 2

## Special Knowledge
You are the backend developer. Follow REST conventions.
`
	descs := []AgentDescription{{Name: "backend-dev", Content: content}}
	results, err := CompileAgents(descs, "anthropic/claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatalf("CompileAgents: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	def := results[0].Definition
	if def.Purpose != "Implement backend services and APIs." {
		t.Errorf("purpose: got %q", def.Purpose)
	}
	if len(def.Capabilities) != 3 {
		t.Errorf("capabilities: got %d, want 3", len(def.Capabilities))
	}
	if def.Model != "openai/gpt-5.2" {
		t.Errorf("model: got %q", def.Model)
	}
	if len(def.Tools) != 1 || def.Tools[0].Name != "shell" {
		t.Errorf("tools: got %v", def.Tools)
	}
	if def.ReviewGuidelines == "" {
		t.Error("expected review guidelines")
	}
	if def.Verify == nil {
		t.Error("expected verify config")
	}
	if results[0].SpecialKnowledge == "" {
		t.Error("expected special knowledge")
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

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (2 custom + conductor)", len(results))
	}

	hasConductor := false
	for _, r := range results {
		if r.Definition.Name == "conductor" {
			hasConductor = true
		}
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

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

func TestReadAgentDescriptions(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "backend-dev.md"), []byte("# Backend Developer\n\n## Purpose\nWrites backend code"), 0644)
	os.WriteFile(filepath.Join(dir, "frontend-dev.md"), []byte("# Frontend Developer\n\n## Purpose\nWrites UI code"), 0644)
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

	if _, err := os.Stat(filepath.Join(dir, "test-agent.yaml")); err != nil {
		t.Errorf("YAML file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "test-agent.md")); err != nil {
		t.Errorf("MD file not created: %v", err)
	}
}

func TestWriteTemplates(t *testing.T) {
	dir := t.TempDir()

	if err := WriteTemplates(dir, []string{"backend-dev", "frontend-dev"}); err != nil {
		t.Fatalf("WriteTemplates: %v", err)
	}

	for _, name := range []string{"backend-dev.md", "frontend-dev.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("template %q not created: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("template %q is empty", name)
		}
	}

	marker := []byte("custom content")
	os.WriteFile(filepath.Join(dir, "backend-dev.md"), marker, 0644)
	if err := WriteTemplates(dir, []string{"backend-dev"}); err != nil {
		t.Fatalf("second WriteTemplates: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "backend-dev.md"))
	if string(data) != "custom content" {
		t.Error("existing template was overwritten")
	}
}

func TestWriteTemplates_InvalidName(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTemplates(dir, []string{"BadName"}); err == nil {
		t.Fatal("expected error for invalid agent name")
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
