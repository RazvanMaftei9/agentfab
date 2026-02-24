package conductor

import (
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

// DecomposeTemplate is a pre-defined task graph pattern that the LLM
// can select and adapt instead of generating from scratch.
type DecomposeTemplate struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	MatchKeywords []string       `yaml:"match_keywords"`
	Tasks         []templateTask `yaml:"tasks"`
	Loops         []loopJSON     `yaml:"loops,omitempty"`
}

type templateTask struct {
	ID          string   `yaml:"id"`
	Agent       string   `yaml:"agent"`
	Description string   `yaml:"description"`
	DependsOn   []string `yaml:"depends_on,omitempty"`
	Scope       string   `yaml:"scope,omitempty"`
	LoopID      string   `yaml:"loop_id,omitempty"`
}

// LoadTemplates reads all decomposition templates from an embedded FS.
func LoadTemplates(fsys fs.FS) ([]DecomposeTemplate, error) {
	entries, err := fs.ReadDir(fsys, "templates")
	if err != nil {
		return nil, err
	}

	var templates []DecomposeTemplate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := fs.ReadFile(fsys, "templates/"+entry.Name())
		if err != nil {
			continue
		}
		var tmpl DecomposeTemplate
		if err := yaml.Unmarshal(data, &tmpl); err != nil {
			continue
		}
		if tmpl.Name != "" {
			templates = append(templates, tmpl)
		}
	}
	return templates, nil
}

// formatTemplatesForPrompt builds a text block describing available templates
// for the decompose LLM prompt.
func formatTemplatesForPrompt(templates []DecomposeTemplate) string {
	if len(templates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available decomposition templates (use as starting points, adapt as needed):\n")
	for _, t := range templates {
		b.WriteString("- ")
		b.WriteString(t.Name)
		b.WriteString(": ")
		b.WriteString(t.Description)
		b.WriteString(" [keywords: ")
		b.WriteString(strings.Join(t.MatchKeywords, ", "))
		b.WriteString("]\n")

		// Show task structure.
		b.WriteString("  Tasks: ")
		for i, task := range t.Tasks {
			if i > 0 {
				b.WriteString(" → ")
			}
			b.WriteString(task.Agent)
			if task.LoopID != "" {
				b.WriteString("(loop)")
			}
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
