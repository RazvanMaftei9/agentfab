package conductor

import (
	"fmt"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/razvanmaftei/agentfab/internal/loop"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

// DecomposeTemplate is a pre-defined task graph pattern. By default the LLM
// decompose pass is shown all templates as starting points it can adapt.
// Templates with Strict=true bypass the LLM entirely when their keywords
// strongly match the user request — see selectStrictTemplate. This prevents
// the conductor from re-introducing tasks the operator explicitly forbids
// (recurring failure pattern 3 in agentfab known bugs).
type DecomposeTemplate struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	MatchKeywords []string       `yaml:"match_keywords"`
	Strict        bool           `yaml:"strict,omitempty"`
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

// MatchScore returns how many of the template's keywords appear as
// substrings in the user request (case-insensitive). Used by strict-mode
// template selection to decide whether to bypass the LLM decompose.
func (t DecomposeTemplate) MatchScore(userRequest string) int {
	lower := strings.ToLower(userRequest)
	score := 0
	for _, kw := range t.MatchKeywords {
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			score++
		}
	}
	return score
}

// strictMatchThreshold is the minimum number of keyword hits a strict
// template must score against a user request before it preempts the LLM
// decompose pass. Three hits avoid trigger-happy bypass on a passing
// mention while reliably catching prompts authored against the template.
const strictMatchThreshold = 3

// selectStrictTemplate returns the strict template with the highest match
// score above strictMatchThreshold, or nil if no strict template strongly
// matches. Ties broken by file order (deterministic from LoadTemplates).
func selectStrictTemplate(templates []DecomposeTemplate, userRequest string) *DecomposeTemplate {
	var best *DecomposeTemplate
	bestScore := strictMatchThreshold - 1
	for i := range templates {
		if !templates[i].Strict {
			continue
		}
		s := templates[i].MatchScore(userRequest)
		if s > bestScore {
			bestScore = s
			best = &templates[i]
		}
	}
	return best
}

// BuildTaskGraph constructs a TaskGraph directly from the template definition,
// without an LLM call. Used by strict-mode decompose when the template's
// keywords strongly match the user request.
func (t DecomposeTemplate) BuildTaskGraph(requestID string) (*taskgraph.TaskGraph, string, error) {
	if len(t.Tasks) == 0 {
		return nil, "", fmt.Errorf("strict template %q has zero tasks", t.Name)
	}

	graph := &taskgraph.TaskGraph{
		RequestID: requestID,
		Name:      t.Name,
		Tasks:     make([]*taskgraph.TaskNode, len(t.Tasks)),
	}
	for i, tk := range t.Tasks {
		graph.Tasks[i] = &taskgraph.TaskNode{
			ID:          tk.ID,
			Agent:       tk.Agent,
			Description: tk.Description,
			DependsOn:   tk.DependsOn,
			Scope:       taskgraph.TaskScope(tk.Scope).EffectiveScope(),
			LoopID:      tk.LoopID,
			Status:      taskgraph.StatusPending,
		}
	}
	for _, l := range t.Loops {
		states := make([]loop.StateConfig, len(l.States))
		for i, s := range l.States {
			states[i] = loop.StateConfig{Name: s.Name, Agent: s.Agent}
		}
		transitions := make([]loop.Transition, len(l.Transitions))
		for i, tr := range l.Transitions {
			transitions[i] = loop.Transition{From: tr.From, To: tr.To, Condition: tr.Condition}
		}
		graph.Loops = append(graph.Loops, loop.LoopDefinition{
			ID:             l.ID,
			Participants:   l.Participants,
			States:         states,
			Transitions:    transitions,
			InitialState:   l.InitialState,
			TerminalStates: l.TerminalStates,
			MaxTransitions: l.MaxTransitions,
		})
	}

	if err := graph.Validate(); err != nil {
		return nil, "", fmt.Errorf("strict template %q produced invalid task graph: %w", t.Name, err)
	}
	return graph, t.Name, nil
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
