package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/razvanmaftei/agentfab/defaults"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestValidateAgentDefinition(t *testing.T) {
	valid := runtime.AgentDefinition{
		Name:         "developer",
		Purpose:      "Code generation",
		Capabilities: []string{"code_gen"},
		Model:        "anthropic/claude-opus-4",
	}
	if err := ValidateAgentDefinition(valid); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}

	tests := []struct {
		name string
		mod  func(d *runtime.AgentDefinition)
	}{
		{"empty name", func(d *runtime.AgentDefinition) { d.Name = "" }},
		{"invalid name chars", func(d *runtime.AgentDefinition) { d.Name = "Dev_Agent" }},
		{"name starts with number", func(d *runtime.AgentDefinition) { d.Name = "1agent" }},
		{"empty purpose", func(d *runtime.AgentDefinition) { d.Purpose = "" }},
		{"no capabilities", func(d *runtime.AgentDefinition) { d.Capabilities = nil }},
		{"empty model", func(d *runtime.AgentDefinition) { d.Model = "" }},
		{"model no provider", func(d *runtime.AgentDefinition) { d.Model = "claude-opus-4" }},
		{"invalid escalation", func(d *runtime.AgentDefinition) { d.EscalationTarget = "Bad_Name" }},
		{"empty required node label key", func(d *runtime.AgentDefinition) {
			d.RequiredNodeLabels = map[string]string{"": "gpu"}
		}},
		{"empty required node label value", func(d *runtime.AgentDefinition) {
			d.RequiredNodeLabels = map[string]string{"accelerator": ""}
		}},
		{"empty tool name", func(d *runtime.AgentDefinition) {
			d.Tools = []runtime.ToolConfig{{Name: ""}}
		}},
		{"empty instructions", func(d *runtime.AgentDefinition) {
			d.Tools = []runtime.ToolConfig{{Name: "my-tool", Instructions: ""}}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := valid
			tc.mod(&d)
			if err := ValidateAgentDefinition(d); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateAgentDefinitionWithTools(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:         "designer",
		Purpose:      "UI design",
		Capabilities: []string{"mockups"},
		Model:        "anthropic/claude-opus-4",
		Tools: []runtime.ToolConfig{
			{Name: "chrome", Instructions: "Headless browser for screenshots."},
			{Name: "lighthouse", Instructions: "Run Lighthouse audits on HTML pages."},
		},
	}
	if err := ValidateAgentDefinition(def); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestLoadAgentsDirFS(t *testing.T) {
	defs, err := LoadAgentsDirFS(defaults.AgentFS, "agents")
	if err != nil {
		t.Fatalf("load embedded agents: %v", err)
	}
	if len(defs) != 4 {
		t.Fatalf("expected 4 agents, got %d", len(defs))
	}

	// All should have auto-set SpecialKnowledgeFile.
	for _, d := range defs {
		if d.SpecialKnowledgeFile == "" {
			t.Errorf("agent %q: expected auto-set SpecialKnowledgeFile", d.Name)
		}
	}
}

func TestLoadAgentsDir(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal agent YAML + MD.
	yaml := []byte("name: test-agent\npurpose: testing\ncapabilities: [test]\nmodel: openai/gpt-4o\n")
	if err := os.WriteFile(filepath.Join(dir, "test-agent.yaml"), yaml, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-agent.md"), []byte("# Test"), 0644); err != nil {
		t.Fatal(err)
	}

	defs, err := LoadAgentsDir(dir)
	if err != nil {
		t.Fatalf("load agents dir: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(defs))
	}
	if defs[0].Name != "test-agent" {
		t.Errorf("name: got %q, want %q", defs[0].Name, "test-agent")
	}
	if defs[0].SpecialKnowledgeFile != "test-agent.md" {
		t.Errorf("special knowledge: got %q, want %q", defs[0].SpecialKnowledgeFile, "test-agent.md")
	}
}

func TestExtractDefaultAgents(t *testing.T) {
	dir := t.TempDir()

	if err := ExtractDefaultAgents(defaults.AgentFS, dir); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Check that files were extracted.
	for _, name := range []string{"conductor.yaml", "architect.yaml", "designer.yaml", "developer.yaml",
		"conductor.md", "architect.md", "designer.md", "developer.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %q to exist: %v", name, err)
		}
	}

	// Second call should not overwrite.
	marker := []byte("custom content")
	if err := os.WriteFile(filepath.Join(dir, "conductor.yaml"), marker, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ExtractDefaultAgents(defaults.AgentFS, dir); err != nil {
		t.Fatalf("second extract: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "conductor.yaml"))
	if string(data) != "custom content" {
		t.Error("existing file was overwritten")
	}
}

func TestValidateAgentSet(t *testing.T) {
	defs := DefaultProfiles()
	if err := ValidateAgentSet(defs); err != nil {
		t.Fatalf("default profiles should be valid: %v", err)
	}

	// Duplicate name.
	dup := append(defs, defs[0])
	if err := ValidateAgentSet(dup); err == nil {
		t.Fatal("expected duplicate name error")
	}

	// Bad escalation target.
	bad := []runtime.AgentDefinition{{
		Name:             "solo",
		Purpose:          "alone",
		Capabilities:     []string{"x"},
		Model:            "openai/gpt-4o",
		EscalationTarget: "nonexistent",
	}}
	if err := ValidateAgentSet(bad); err == nil {
		t.Fatal("expected unknown escalation target error")
	}
}
