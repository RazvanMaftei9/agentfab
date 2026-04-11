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
	Name    string
	Content string
}

// CompileResult holds the output of compiling one agent description.
type CompileResult struct {
	Definition       runtime.AgentDefinition
	SpecialKnowledge string
}

// AgentTemplate is the structured markdown template users fill out to define agents.
const AgentTemplate = `# %s

## Purpose
Describe what this agent does in one sentence.

## Capabilities
- capability_one
- capability_two

## Model
anthropic/claude-sonnet-4-5-20250929

## Escalation Target
architect

## Tools
### shell
Execute shell commands for builds, tests, and file operations.

## Review Guidelines
- Guideline one
- Guideline two

## Verify
Command: cd $SCRATCH_DIR && go test ./...
Max Retries: 2

## Special Knowledge
Everything below this point becomes the agent's persistent context.
Write the agent's role, rules, constraints, and domain expertise here.
`

// GenerateTemplate returns a filled-in template for a given agent name.
func GenerateTemplate(agentName string) string {
	return fmt.Sprintf(AgentTemplate, agentName)
}

// WriteTemplates writes starter .md templates for the given agent names into dir.
func WriteTemplates(dir string, names []string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create templates dir: %w", err)
	}
	for _, name := range names {
		if !validName.MatchString(name) {
			return fmt.Errorf("invalid agent name %q: must be lowercase alphanumeric with hyphens, starting with a letter", name)
		}
		path := filepath.Join(dir, name+".md")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(GenerateTemplate(name)), 0644); err != nil {
			return fmt.Errorf("write template %q: %w", path, err)
		}
	}
	return nil
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
// It parses structured sections from the template format. A conductor is auto-added
// if none is present.
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

	var defs []runtime.AgentDefinition
	for _, r := range results {
		defs = append(defs, r.Definition)
	}
	if err := ValidateAgentSet(defs); err != nil {
		return nil, fmt.Errorf("cross-validate compiled agents: %w", err)
	}

	return results, nil
}

func compileOne(desc AgentDescription, defaultModel string) CompileResult {
	sections := parseSections(desc.Content)

	purpose := extractSectionText(sections, "purpose")
	if purpose == "" {
		purpose = extractPurposeLegacy(desc.Content)
	}

	caps := extractSectionList(sections, "capabilities")
	if len(caps) == 0 {
		caps = extractCapabilitiesLegacy(desc.Name, desc.Content)
	}

	model := extractSectionText(sections, "model")
	if model == "" {
		model = defaultModel
	}

	escalation := extractSectionText(sections, "escalation target")

	tools := extractSectionTools(sections)
	if len(tools) == 0 && desc.Name != "conductor" {
		tools = defaultTools(desc.Name)
	}

	reviewGuidelines := extractSectionRaw(sections, "review guidelines")
	reviewPrompt := extractSectionRaw(sections, "review prompt")

	var verify *runtime.VerifyConfig
	if v := extractVerifyConfig(sections); v != nil {
		verify = v
	}

	specialKnowledge := extractSectionRaw(sections, "special knowledge")
	if specialKnowledge == "" {
		specialKnowledge = desc.Content
	}

	def := runtime.AgentDefinition{
		Name:                 desc.Name,
		Purpose:              purpose,
		Capabilities:         caps,
		Model:                model,
		SpecialKnowledgeFile: desc.Name + ".md",
		EscalationTarget:     escalation,
		Tools:                tools,
		ReviewGuidelines:     reviewGuidelines,
		ReviewPrompt:         reviewPrompt,
		Verify:               verify,
		TaskTimeout:          20 * time.Minute,
		ColdRetentionDays:    1095,
		CurationThreshold:    50,
	}

	if desc.Name == "conductor" {
		def.Tools = nil
		def.TaskTimeout = 0
	}

	return CompileResult{
		Definition:       def,
		SpecialKnowledge: specialKnowledge,
	}
}

type section struct {
	heading string
	body    string
}

func parseSections(content string) []section {
	var sections []section
	var current *section
	var bodyLines []string

	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "## ") {
			if current != nil {
				current.body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
				sections = append(sections, *current)
			}
			current = &section{heading: strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "##")))}
			bodyLines = nil
			continue
		}
		if current != nil {
			bodyLines = append(bodyLines, line)
		}
	}
	if current != nil {
		current.body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
		sections = append(sections, *current)
	}
	return sections
}

func findSection(sections []section, heading string) (section, bool) {
	heading = strings.ToLower(heading)
	for _, s := range sections {
		if s.heading == heading {
			return s, true
		}
	}
	return section{}, false
}

func extractSectionText(sections []section, heading string) string {
	s, ok := findSection(sections, heading)
	if !ok {
		return ""
	}
	text := strings.TrimSpace(s.body)
	if idx := strings.Index(text, "\n"); idx > 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}

func extractSectionRaw(sections []section, heading string) string {
	s, ok := findSection(sections, heading)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s.body)
}

func extractSectionList(sections []section, heading string) []string {
	s, ok := findSection(sections, heading)
	if !ok {
		return nil
	}
	var items []string
	for _, line := range strings.Split(s.body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimSpace(line)
		if line != "" {
			items = append(items, line)
		}
	}
	return items
}

func extractSectionTools(sections []section) []runtime.ToolConfig {
	s, ok := findSection(sections, "tools")
	if !ok {
		return nil
	}

	var tools []runtime.ToolConfig
	var current *runtime.ToolConfig
	var instrLines []string

	for _, line := range strings.Split(s.body, "\n") {
		if strings.HasPrefix(line, "### ") {
			if current != nil {
				current.Instructions = strings.TrimSpace(strings.Join(instrLines, "\n"))
				if current.Command == "" {
					current.Command = "$TOOL_ARG_COMMAND"
					current.Parameters = map[string]runtime.ToolParam{
						"command": {Type: "string", Description: "The shell command to execute", Required: true},
					}
					if current.Config == nil {
						current.Config = map[string]string{"timeout": "120"}
					} else if current.Config["timeout"] == "" {
						current.Config["timeout"] = "120"
					}
				}
				tools = append(tools, *current)
			}
			name := strings.TrimSpace(strings.TrimPrefix(line, "###"))
			current = &runtime.ToolConfig{Name: strings.ToLower(name)}
			instrLines = nil
			continue
		}

		if current != nil {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToLower(trimmed), "command:") {
				current.Command = strings.TrimSpace(strings.TrimPrefix(trimmed, "Command:"))
				current.Command = strings.TrimSpace(strings.TrimPrefix(current.Command, "command:"))
			} else if strings.HasPrefix(strings.ToLower(trimmed), "mode:") {
				current.Mode = strings.TrimSpace(trimmed[5:])
			} else if strings.HasPrefix(strings.ToLower(trimmed), "timeout:") {
				if current.Config == nil {
					current.Config = map[string]string{}
				}
				current.Config["timeout"] = strings.TrimSpace(trimmed[8:])
			} else if strings.HasPrefix(strings.ToLower(trimmed), "passthrough_env:") {
				envs := strings.TrimSpace(trimmed[16:])
				for _, e := range strings.Split(envs, ",") {
					e = strings.TrimSpace(e)
					if e != "" {
						current.PassthroughEnv = append(current.PassthroughEnv, e)
					}
				}
			} else {
				instrLines = append(instrLines, line)
			}
		}
	}

	if current != nil {
		current.Instructions = strings.TrimSpace(strings.Join(instrLines, "\n"))
		if current.Command == "" {
			current.Command = "$TOOL_ARG_COMMAND"
			current.Parameters = map[string]runtime.ToolParam{
				"command": {Type: "string", Description: "The shell command to execute", Required: true},
			}
			if current.Config == nil {
				current.Config = map[string]string{"timeout": "120"}
			} else if current.Config["timeout"] == "" {
				current.Config["timeout"] = "120"
			}
		}
		tools = append(tools, *current)
	}

	return tools
}

func extractVerifyConfig(sections []section) *runtime.VerifyConfig {
	s, ok := findSection(sections, "verify")
	if !ok {
		return nil
	}

	vc := &runtime.VerifyConfig{MaxRetries: 2}
	for _, line := range strings.Split(s.body, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "command:") {
			vc.Command = strings.TrimSpace(trimmed[8:])
		} else if strings.HasPrefix(lower, "max retries:") || strings.HasPrefix(lower, "max_retries:") {
			fmt.Sscanf(strings.TrimSpace(trimmed[strings.Index(trimmed, ":")+1:]), "%d", &vc.MaxRetries)
		}
	}

	if vc.Command == "" {
		return nil
	}
	return vc
}

// Legacy fallbacks for unstructured markdown files.

func extractPurposeLegacy(content string) string {
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
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			continue
		}
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

func extractCapabilitiesLegacy(name, content string) []string {
	lower := strings.ToLower(content)
	baseCap := strings.ReplaceAll(name, "-", "_")
	caps := []string{baseCap}

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

	if len(caps) > 6 {
		caps = caps[:6]
	}
	return caps
}

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
		data, err := yaml.Marshal(r.Definition)
		if err != nil {
			return fmt.Errorf("marshal agent %q: %w", r.Definition.Name, err)
		}
		yamlPath := filepath.Join(agentsDir, r.Definition.Name+".yaml")
		if err := os.WriteFile(yamlPath, data, 0644); err != nil {
			return fmt.Errorf("write %q: %w", yamlPath, err)
		}

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
