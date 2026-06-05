package conductor

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestMatchScoreCounts(t *testing.T) {
	tmpl := DecomposeTemplate{
		MatchKeywords: []string{"grid", "energy", "ERCOT", "PJM", "data center"},
	}
	if got := tmpl.MatchScore("Build a US energy grid analysis for ERCOT and PJM with data centers"); got != 5 {
		t.Errorf("MatchScore got %d, want 5", got)
	}
	if got := tmpl.MatchScore("hello world"); got != 0 {
		t.Errorf("MatchScore empty got %d, want 0", got)
	}
}

func TestSelectStrictTemplateThresholdAndBest(t *testing.T) {
	templates := []DecomposeTemplate{
		{Name: "loose", MatchKeywords: []string{"a", "b"}, Strict: false},
		{Name: "strict-low", MatchKeywords: []string{"a", "b"}, Strict: true},
		{Name: "strict-high", MatchKeywords: []string{"a", "b", "c", "d"}, Strict: true},
	}
	if got := selectStrictTemplate(templates, "a b only"); got != nil {
		t.Errorf("below threshold must return nil, got %q", got.Name)
	}
	if got := selectStrictTemplate(templates, "a b c"); got == nil || got.Name != "strict-high" {
		t.Errorf("expected strict-high at threshold, got %v", got)
	}
	templates[0].MatchKeywords = []string{"x", "y", "z", "w", "v"}
	if got := selectStrictTemplate(templates, "x y z w v"); got != nil {
		t.Errorf("non-strict template must not win, got %q", got.Name)
	}
}

func TestBuildTaskGraphFromTemplate(t *testing.T) {
	tmpl := DecomposeTemplate{
		Name: "us-grid-tour-streamlined",
		Tasks: []templateTask{
			{ID: "t01", Agent: "data-engineer", Description: "ingest"},
			{ID: "t02", Agent: "grid-engineer", Description: "model", DependsOn: []string{"t01"}, LoopID: "rev"},
		},
		Loops: []loopJSON{{
			ID:           "rev",
			Participants: []string{"grid-engineer", "siting-screen-analyst"},
			States: []stateJSON{
				{Name: "WORKING", Agent: "grid-engineer"},
				{Name: "REVIEWING", Agent: "siting-screen-analyst"},
				{Name: "APPROVED"},
				{Name: "ESCALATED"},
			},
			Transitions: []transitionJSON{
				{From: "WORKING", To: "REVIEWING"},
				{From: "REVIEWING", To: "APPROVED", Condition: "approved"},
			},
			InitialState:   "WORKING",
			TerminalStates: []string{"APPROVED", "ESCALATED"},
			MaxTransitions: 4,
		}},
	}
	graph, name, err := tmpl.BuildTaskGraph("req-test")
	if err != nil {
		t.Fatalf("BuildTaskGraph: %v", err)
	}
	if name != "us-grid-tour-streamlined" {
		t.Errorf("name got %q", name)
	}
	if len(graph.Tasks) != 2 {
		t.Errorf("tasks got %d, want 2", len(graph.Tasks))
	}
	if len(graph.Loops) != 1 {
		t.Errorf("loops got %d, want 1", len(graph.Loops))
	}
	if graph.Loops[0].InitialState != "WORKING" {
		t.Errorf("loop initial state got %q (was the silent yaml-tag bug pre-fix)", graph.Loops[0].InitialState)
	}
	if graph.Loops[0].MaxTransitions != 4 {
		t.Errorf("loop max_transitions got %d", graph.Loops[0].MaxTransitions)
	}
}

func TestDecomposeStrictBypassesLLM(t *testing.T) {
	templates := []DecomposeTemplate{{
		Name:          "us-grid-tour-streamlined",
		Strict:        true,
		MatchKeywords: []string{"grid", "ERCOT", "PJM"},
		Tasks: []templateTask{
			{ID: "t01", Agent: "data-engineer", Description: "ingest"},
		},
	}}
	fabricDef := &config.FabricDef{
		Agents: []runtime.AgentDefinition{
			{Name: "conductor"},
			{Name: "data-engineer"},
		},
	}
	llmCallCount := 0
	generate := func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
		llmCallCount++
		return &schema.Message{Content: "{}"}, nil
	}
	res, err := Decompose(context.Background(), generate, fabricDef,
		"Build a grid analysis for ERCOT and PJM", "", templates...)
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if llmCallCount != 0 {
		t.Errorf("strict mode should bypass LLM; got %d calls", llmCallCount)
	}
	if res.Graph == nil || len(res.Graph.Tasks) != 1 {
		t.Fatalf("expected 1 task from strict template")
	}
	if res.Graph.Tasks[0].Agent != "data-engineer" {
		t.Errorf("agent got %q", res.Graph.Tasks[0].Agent)
	}
}

func TestDecomposeStrictBelowThresholdFallsThroughToLLM(t *testing.T) {
	templates := []DecomposeTemplate{{
		Name:          "us-grid-tour-streamlined",
		Strict:        true,
		MatchKeywords: []string{"grid", "ERCOT", "PJM"},
		Tasks:         []templateTask{{ID: "t01", Agent: "data-engineer", Description: "ingest"}},
	}}
	// Two non-conductor agents → multi-agent decompose path (no prefill).
	fabricDef := &config.FabricDef{
		Agents: []runtime.AgentDefinition{
			{Name: "conductor"},
			{Name: "data-engineer"},
			{Name: "grid-engineer"},
		},
	}
	llmCallCount := 0
	generate := func(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
		llmCallCount++
		return &schema.Message{Content: `{"actionable":true,"name":"x","tasks":[{"id":"t1","agent":"data-engineer","description":"x","depends_on":[]}]}`}, nil
	}
	_, err := Decompose(context.Background(), generate, fabricDef,
		"Build a grid analysis", "", templates...)
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if llmCallCount == 0 {
		t.Errorf("below-threshold strict template must fall through to LLM; got 0 calls")
	}
}

func TestLoopJSONYAMLTagsRegression(t *testing.T) {
	// Regression: yaml.v3 doesn't honor json tags. Loop fields with
	// underscores (initial_state, terminal_states, max_transitions) silently
	// parse as zero values before the yaml tags were added.
	fsys := fstest.MapFS{"templates/t.yaml": &fstest.MapFile{Data: []byte(`name: t
description: t
match_keywords: [t]
tasks:
  - id: t1
    agent: a
    description: stub
loops:
  - id: rev
    participants: [a, b]
    initial_state: WORKING
    terminal_states: [DONE]
    max_transitions: 6
    states:
      - { name: WORKING, agent: a }
      - { name: DONE,    agent: "" }
    transitions:
      - { from: WORKING, to: DONE, condition: approved }
`)}}
	templates, err := LoadTemplates(fsys)
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates len: %d", len(templates))
	}
	l := templates[0].Loops[0]
	if l.InitialState != "WORKING" {
		t.Errorf("InitialState got %q — yaml-tag regression", l.InitialState)
	}
	if len(l.TerminalStates) != 1 || l.TerminalStates[0] != "DONE" {
		t.Errorf("TerminalStates got %v", l.TerminalStates)
	}
	if l.MaxTransitions != 6 {
		t.Errorf("MaxTransitions got %d", l.MaxTransitions)
	}
}
