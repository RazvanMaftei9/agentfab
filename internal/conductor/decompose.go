package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/loop"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/structuredoutput"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

// errNonActionable is a sentinel used internally when the LLM classifies the
// input as non-actionable (greeting, filler, gibberish).
var errNonActionable = errors.New("non-actionable input")

const decomposePrompt = `You are the Conductor of an AgentFab fabric. Decompose the user's request into a task graph.

Before decomposing, evaluate whether the input is actionable.
Actionable = a concrete task, question, or instruction that requires work.
Non-actionable = greetings, filler, gibberish, or empty content.
If non-actionable, respond with: {"actionable": false, "message": "friendly one-sentence reply"}
If actionable, include "actionable": true in the full task graph JSON.

Available agents:
%s
%sOutput a JSON object with this schema:
{
  "name": "short-descriptive-name",
  "tasks": [
    {
      "id": "t1",
      "agent": "agent-name",
      "description": "What the agent should do",
      "depends_on": [],
      "scope": "standard",
      "loop_id": ""
    }
  ],
  "loops": [
    {
      "id": "loop-1",
      "participants": ["agent-a", "agent-b"],
      "states": [
        {"name": "WORKING", "agent": "agent-a"},
        {"name": "REVIEWING", "agent": "agent-b"},
        {"name": "REVISING", "agent": "agent-a"},
        {"name": "APPROVED", "agent": ""},
        {"name": "ESCALATED", "agent": ""}
      ],
      "transitions": [
        {"from": "WORKING", "to": "REVIEWING"},
        {"from": "REVIEWING", "to": "APPROVED", "condition": "approved"},
        {"from": "REVIEWING", "to": "REVISING", "condition": "revise"},
        {"from": "REVISING", "to": "REVIEWING"}
      ],
      "initial_state": "WORKING",
      "terminal_states": ["APPROVED", "ESCALATED"],
      "max_transitions": 5
    }
  ]
}

Rules:
- CRITICAL: You have an ongoing project context. Read "Existing artifacts on disk", "Recent user requests", and "Active project decisions" BEFORE interpreting the user's request. When the user says "the app", "it", "the project", or makes any follow-up request without naming a specific app, they are referring to the most recent project in context. NEVER ask the user to clarify which app when there is only one project in the artifact listing
- If artifacts exist, task descriptions MUST name the concrete app/files (e.g., "Update the todo-app in artifacts/developer/todo-app/ to use Material Design 3 components"). Never treat existing work as greenfield and never use vague references like "the existing application" or "the current project"
- Before creating tasks, review "Prior relevant knowledge" carefully. If prior work already addresses the user's request, produce ONLY a minimal task graph (e.g., a single task to verify existing artifacts)
- If the request is an exact repeat, respond with a single task: "Review and confirm existing work on [topic]"
- Only create full multi-task graphs when the request asks for genuinely new work not covered by existing knowledge
- EXPLICIT FAN-OUT EXCEPTION: When the user request explicitly enumerates a list of independent items (files, packages, services, papers, components, modules, directories, etc.) and asks for one task per item, create exactly one task per item even if the count is large. Independent fan-out is a legitimate workload shape — do not aggregate, summarize, or fold the items into a smaller number of tasks. The "keep the graph small" rule does not apply when the user has already chosen the granularity. The user-provided enumeration is the task list.
- Each task must be assigned to one of the available agents
- Use depends_on to express ordering constraints
- Keep the graph as small as possible — but never skip review loops to save tasks
- For multi-agent workflows, prefer sequential flow: upstream specification agents first, then implementation agents, then review. An implementing agent should depend on upstream spec outputs — do not parallelize implementation with spec generation
- "name" should be 2-5 words, lowercase with hyphens, describing the request (e.g., "todo-app-mvp")
- The conductor does NOT appear as a task agent
- Assign a scope to every task: "small" (trivial change, boilerplate, single-file), "standard" (moderate feature, multi-file), "large" (complex feature, cross-cutting concerns)
- Small tasks: do NOT assign a loop_id — they bypass code review entirely
- Standard tasks: use a review loop with max_transitions: 3 (one review pass)
- Large tasks: use a review loop with max_transitions: 5 (full review cycle)
- Assign the appropriate available agent as reviewer based on the task type — the reviewing agent validates against its own specifications
- Match the reviewer to the agent whose specs the implementing agent must follow most closely
- A task with a loop_id is driven by the loop FSM: the scheduler dispatches to different agents per state
- Terminal states (APPROVED, ESCALATED) have empty agent fields
- Non-terminal states must have an agent assigned
- A loop task already covers the full work cycle for its agent (working + revision). NEVER create a separate follow-up task for the same agent after a loop task — the APPROVED state means the work is done
- Do NOT include verification or testing instructions in implementation task descriptions (e.g., "test locally", "ensure all features work", "validate functionality"). The agent's profile handles build verification. Focus on WHAT to build
- The "Active project decisions" section lists established technology choices, design systems, and architectural patterns. Every task description for affected agents MUST explicitly state the relevant constraints (e.g., if the project uses Material Design 3, a task for the relevant agent must say "using Material Design 3 design system")
- Never produce a task description that contradicts or ignores an active project decision
- Each agent's purpose defines its scope of authority. Do not ask an agent to prescribe details outside its stated purpose
- Upstream specification task descriptions must stay within the agent's stated purpose. NEVER include instructions that cross into another agent's scope of authority — those decisions belong to the agent whose purpose covers them
- When multiple agents produce specs for the same project, each agent owns only the details within its stated purpose. Overlapping prescriptions create conflicts — avoid this by respecting agent boundaries
- When an agent's output specifies multiple deliverable units (separate packages, repos, or modules), create separate tasks for each unit. The library/core task must be listed as a dependency of the application task. Each task description must name the specific unit (e.g., "Implement the audio-engine npm package" and "Implement the browser-daw React app, importing audio-engine as a dependency")
- Output ONLY the JSON, no explanation`

// DecomposeResult holds the task graph and token usage from decomposition.
type DecomposeResult struct {
	Graph      *taskgraph.TaskGraph
	Name       string // short descriptive name for the request
	Actionable bool   // false when the input is non-actionable (greeting, filler)
	Message    string // friendly reply when Actionable is false
	TokenUsage *message.TokenUsage
}

const decomposeMaxRetries = 1

// 8192 accommodates SWE-bench issues that produce 4000-5000 token task graphs.
const decomposeMaxOutputTokens = 8192

// Decompose breaks a user request into a task graph via LLM, retrying once on parse failure.
func Decompose(ctx context.Context, generate func(context.Context, []*schema.Message) (*schema.Message, error), fabricDef *config.FabricDef, userRequest string, conductorKnowledge string, templates ...DecomposeTemplate) (*DecomposeResult, error) {
	var roster []string
	agentDesc := ""
	for _, a := range fabricDef.Agents {
		if a.Name == "conductor" {
			continue
		}
		roster = append(roster, a.Name)
		agentDesc += fmt.Sprintf("- %s: %s (capabilities: %v)\n", a.Name, a.Purpose, a.Capabilities)
	}

	knowledgeSection := ""
	if conductorKnowledge != "" {
		knowledgeSection = fmt.Sprintf("Conductor knowledge:\n%s\n\n", conductorKnowledge)
	}

	templateSection := ""
	if len(templates) > 0 {
		templateSection = formatTemplatesForPrompt(templates)
	}

	systemMsg := fmt.Sprintf(decomposePrompt, agentDesc, knowledgeSection+templateSection)

	// Single-agent mode: skip multi-agent rules to save tokens.
	var input []*schema.Message
	var prefill string // assistant prefill to prepend to response content
	if len(roster) == 1 {
		singleAgentPrompt := fmt.Sprintf(
			`You are a TASK DECOMPOSER — not a developer, not a coder. You do NOT solve problems.
Your ONLY job: read the user's request and output a JSON task graph assigning it to the %q agent.

CRITICAL RULES:
- Output ONLY a JSON object. Nothing else.
- Do NOT write code, bash commands, XML, or <function_calls> tags.
- Do NOT analyze code or try to fix bugs yourself.
- Do NOT include code snippets in your output.
- The task description should summarize WHAT needs to be done, not HOW.

Format:
{"actionable": true, "name": "short-name", "tasks": [{"id": "t1", "agent": %q, "description": "what to do", "depends_on": [], "scope": "standard"}]}

If the request is not actionable (greeting/filler), respond: {"actionable": false, "message": "friendly reply"}`,
			roster[0], roster[0])

		prefill = `{"actionable": true, "name": "`
		input = []*schema.Message{
			schema.SystemMessage(singleAgentPrompt),
			schema.UserMessage(userRequest),
			&schema.Message{Role: schema.Assistant, Content: prefill},
		}
	} else {
		input = []*schema.Message{
			schema.SystemMessage(systemMsg),
			schema.UserMessage(userRequest),
		}
	}

	var totalUsage message.TokenUsage

	for attempt := 0; attempt <= decomposeMaxRetries; attempt++ {
		resp, err := generate(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("model decompose call: %w", err)
		}

		accumulateUsage(&totalUsage, resp)

		content := resp.Content
		if prefill != "" {
			content = prefill + content
		}

		graph, name, parseErr := parseTaskGraphWithRoster(content, "req-0", roster)
		if parseErr == nil {
			return &DecomposeResult{
				Graph:      graph,
				Name:       name,
				Actionable: true,
				TokenUsage: &totalUsage,
			}, nil
		}

		if errors.Is(parseErr, errNonActionable) {
			return &DecomposeResult{
				Actionable: false,
				Message:    name, // parseTaskGraph returns the message in the name slot
				TokenUsage: &totalUsage,
			}, nil
		}

		if attempt >= decomposeMaxRetries {
			return nil, parseErr
		}

		input = append(input,
			&schema.Message{Role: schema.Assistant, Content: resp.Content},
			schema.UserMessage(fmt.Sprintf(
				"Your output failed validation: %s\n\nPlease fix the issues and output the corrected JSON.", parseErr.Error(),
			)),
		)
	}

	return nil, fmt.Errorf("decompose: exhausted retries")
}

func accumulateUsage(total *message.TokenUsage, resp *schema.Message) {
	if resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return
	}
	u := resp.ResponseMeta.Usage
	total.InputTokens += int64(u.PromptTokens)
	total.OutputTokens += int64(u.CompletionTokens)
	total.TotalTokens += int64(u.PromptTokens + u.CompletionTokens)
}

type taskGraphJSON struct {
	Actionable *bool          `json:"actionable,omitempty"` // nil = legacy (treated as true)
	Message    string         `json:"message,omitempty"`    // friendly reply when non-actionable
	Name       string         `json:"name,omitempty"`
	Tasks      []taskNodeJSON `json:"tasks"`
	Loops      []loopJSON     `json:"loops,omitempty"`
}

type taskNodeJSON struct {
	ID          string   `json:"id"`
	Agent       string   `json:"agent"`
	Description string   `json:"description"`
	DependsOn   []string `json:"depends_on"`
	Scope       string   `json:"scope,omitempty"`
	LoopID      string   `json:"loop_id,omitempty"`
}

type loopJSON struct {
	ID             string           `json:"id"`
	Participants   []string         `json:"participants"`
	States         []stateJSON      `json:"states"`
	Transitions    []transitionJSON `json:"transitions"`
	InitialState   string           `json:"initial_state"`
	TerminalStates []string         `json:"terminal_states"`
	MaxTransitions int              `json:"max_transitions"`
}

type stateJSON struct {
	Name  string `json:"name"`
	Agent string `json:"agent,omitempty"`
}

type transitionJSON struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

func parseTaskGraphWithRoster(content string, requestID string, roster []string) (*taskgraph.TaskGraph, string, error) {
	graph, name, err := parseTaskGraph(content, requestID)
	if err != nil {
		if errors.Is(err, errNonActionable) {
			return nil, name, err
		}
		return nil, "", err
	}
	if len(roster) > 0 {
		if err := graph.ValidateWithRoster(roster); err != nil {
			return nil, "", fmt.Errorf("validate task graph: %w", err)
		}
	}
	return graph, name, nil
}

func parseTaskGraph(content string, requestID string) (*taskgraph.TaskGraph, string, error) {
	rawJSON, err := structuredoutput.ExtractJSONFromContent(content)
	if err != nil {
		return nil, "", fmt.Errorf("parse task graph: %w (content: %q)", err, truncate(content, 200))
	}

	var raw taskGraphJSON
	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return nil, "", fmt.Errorf("parse task graph JSON: %w", err)
	}

	if raw.Actionable != nil && !*raw.Actionable {
		return nil, raw.Message, errNonActionable
	}

	if len(raw.Tasks) == 0 {
		return nil, "", fmt.Errorf("task graph has 0 tasks — the model must produce at least one task (content: %q)", truncate(content, 200))
	}

	graph := &taskgraph.TaskGraph{
		RequestID: requestID,
		Name:      raw.Name,
		Tasks:     make([]*taskgraph.TaskNode, len(raw.Tasks)),
	}

	for i, t := range raw.Tasks {
		graph.Tasks[i] = &taskgraph.TaskNode{
			ID:          t.ID,
			Agent:       t.Agent,
			Description: t.Description,
			DependsOn:   t.DependsOn,
			Scope:       taskgraph.TaskScope(t.Scope).EffectiveScope(),
			LoopID:      t.LoopID,
			Status:      taskgraph.StatusPending,
		}
	}

	for _, l := range raw.Loops {
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
		return nil, "", fmt.Errorf("validate task graph: %w", err)
	}

	return graph, raw.Name, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
