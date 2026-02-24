package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
	"gopkg.in/yaml.v3"
)

// AgentDescription represents a raw .md description file for an agent.
type AgentDescription struct {
	Name    string // derived from filename (e.g., "backend-dev" from "backend-dev.md")
	Content string // raw markdown content
}

// CompileResult holds the output of compiling one agent description.
type CompileResult struct {
	Definition       runtime.AgentDefinition
	SpecialKnowledge string // the original .md content, used as special knowledge
}

// ReadAgentDescriptions reads all .md files from a directory and returns AgentDescription structs.
func ReadAgentDescriptions(dir string) ([]AgentDescription, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read descriptions dir %q: %w", dir, err)
	}

	var descs []AgentDescription
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if !validName.MatchString(name) {
			return nil, fmt.Errorf("file %q: derived agent name %q must be lowercase alphanumeric with hyphens, starting with a letter", e.Name(), name)
		}
		descs = append(descs, AgentDescription{Name: name, Content: string(data)})
	}
	if len(descs) == 0 {
		return nil, fmt.Errorf("no .md files found in %q", dir)
	}
	return descs, nil
}

// CompileAgents converts .md agent descriptions into structured agent definitions.
// It extracts purpose from the markdown, assigns a default model, and gives every
// agent a shell tool. A conductor is auto-added if none is present.
func CompileAgents(descriptions []AgentDescription, defaultModel string) ([]CompileResult, error) {
	if defaultModel == "" {
		defaultModel = "anthropic/claude-sonnet-4-5-20250929"
	}

	var results []CompileResult
	for _, desc := range descriptions {
		result := compileOne(desc, defaultModel)
		if err := ValidateAgentDefinition(result.Definition); err != nil {
			return nil, fmt.Errorf("agent %q: %w", desc.Name, err)
		}
		results = append(results, result)
	}

	// Add conductor if not present.
	hasConductor := false
	for _, r := range results {
		if r.Definition.Name == "conductor" {
			hasConductor = true
			break
		}
	}
	if !hasConductor {
		results = append(results, defaultConductorResult())
	}

	// Cross-validate.
	var defs []runtime.AgentDefinition
	for _, r := range results {
		defs = append(defs, r.Definition)
	}
	if err := ValidateAgentSet(defs); err != nil {
		return nil, fmt.Errorf("cross-validate compiled agents: %w", err)
	}

	return results, nil
}

// compileOne converts a single .md description into an agent definition.
func compileOne(desc AgentDescription, defaultModel string) CompileResult {
	purpose := extractPurpose(desc.Content)
	caps := extractCapabilities(desc.Name, desc.Content)

	def := runtime.AgentDefinition{
		Name:                 desc.Name,
		Purpose:              purpose,
		Capabilities:         caps,
		Model:                defaultModel,
		SpecialKnowledgeFile: desc.Name + ".md",
		Tools:                defaultTools(desc.Name),
		TaskTimeout:          20 * time.Minute,
		ColdRetentionDays:    1095,
		CurationThreshold:    50,
	}

	// Conductor gets no tools and a different model pattern.
	if desc.Name == "conductor" {
		def.Tools = nil
		def.TaskTimeout = 0
	}

	return CompileResult{
		Definition:       def,
		SpecialKnowledge: desc.Content,
	}
}

// extractPurpose pulls a one-line purpose from the markdown.
// Looks for the first non-heading, non-empty line. Falls back to the H1 title.
func extractPurpose(content string) string {
	var h1 string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			h1 = strings.TrimPrefix(line, "# ")
			continue
		}
		// Skip sub-headings.
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Skip list markers at the start — look for prose.
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			continue
		}
		// First prose line is the purpose.
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	if h1 != "" {
		return h1
	}
	return "Agent"
}

// extractCapabilities derives capability tags from the agent name and content.
func extractCapabilities(name, content string) []string {
	lower := strings.ToLower(content)

	// Start with a generic capability based on the agent name.
	baseCap := strings.ReplaceAll(name, "-", "_")
	caps := []string{baseCap}

	// Scan for common capability keywords.
	keywords := map[string]string{
		"code":          "code_gen",
		"implement":     "implementation",
		"debug":         "debugging",
		"review":        "code_review",
		"design":        "design",
		"architect":     "arch_analysis",
		"test":          "testing",
		"deploy":        "deployment",
		"document":      "documentation",
		"ui":            "ui_design",
		"ux":            "ux_design",
		"api":           "api_design",
		"database":      "database",
		"security":      "security",
		"performance":   "performance",
		"music":         "music_composition",
		"audio":         "procedural_audio",
		"sound":         "sound_design",
		"sprite":        "sprite_design",
		"animation":     "animation",
		"level":         "level_design",
		"game mechanic": "game_design",
		"narrative":     "narrative_design",
	}

	seen := map[string]bool{baseCap: true}
	for keyword, cap := range keywords {
		if strings.Contains(lower, keyword) && !seen[cap] {
			caps = append(caps, cap)
			seen[cap] = true
		}
	}

	// Cap at 6 capabilities.
	if len(caps) > 6 {
		caps = caps[:6]
	}
	return caps
}

// defaultTools returns a shell tool for non-conductor agents.
func defaultTools(name string) []runtime.ToolConfig {
	return []runtime.ToolConfig{
		{
			Name:         "shell",
			Instructions: "Execute shell commands to inspect artifacts, run builds, and verify work.",
			Command:      "$TOOL_ARG_COMMAND",
			Parameters: map[string]runtime.ToolParam{
				"command": {
					Type:        "string",
					Description: "The shell command to execute",
					Required:    true,
				},
			},
			Config: map[string]string{
				"timeout": "120",
			},
		},
	}
}

// WriteCompiledAgents writes CompileResults to an agents directory (YAML + .md files).
func WriteCompiledAgents(agentsDir string, results []CompileResult) error {
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("create agents dir: %w", err)
	}

	for _, r := range results {
		// Write YAML definition.
		data, err := yaml.Marshal(r.Definition)
		if err != nil {
			return fmt.Errorf("marshal agent %q: %w", r.Definition.Name, err)
		}
		yamlPath := filepath.Join(agentsDir, r.Definition.Name+".yaml")
		if err := os.WriteFile(yamlPath, data, 0644); err != nil {
			return fmt.Errorf("write %q: %w", yamlPath, err)
		}

		// Write special knowledge .md file.
		if r.SpecialKnowledge != "" {
			mdPath := filepath.Join(agentsDir, r.Definition.Name+".md")
			if err := os.WriteFile(mdPath, []byte(r.SpecialKnowledge), 0644); err != nil {
				return fmt.Errorf("write %q: %w", mdPath, err)
			}
		}
	}
	return nil
}

// MarshalAgentDef marshals an agent definition to YAML bytes.
func MarshalAgentDef(def runtime.AgentDefinition) ([]byte, error) {
	return yaml.Marshal(def)
}

func defaultConductorResult() CompileResult {
	return CompileResult{
		Definition: runtime.AgentDefinition{
			Name:         "conductor",
			Purpose:      "Request decomposition, orchestration, and user I/O",
			Capabilities: []string{"user_chat", "task_graph", "agent_coordination"},
			Model:        "anthropic/claude-sonnet-4-5-20250929",
		},
	}
}
