package conductor

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/config"
)

func mockDecomposeGenerate(response string) func(context.Context, []*schema.Message) (*schema.Message, error) {
	return func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: response}, nil
	}
}

func TestDecompose(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	cannedResponse := `{
		"name": "oauth-login-page",
		"tasks": [
			{"id": "t1", "agent": "architect", "description": "Design auth flow", "depends_on": []},
			{"id": "t2", "agent": "developer", "description": "Implement login page", "depends_on": ["t1"]},
			{"id": "t3", "agent": "architect", "description": "Review implementation", "depends_on": ["t2"]}
		]
	}`

	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(cannedResponse),
		systemDef,
		"Build a login page with OAuth",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}

	graph := result.Graph
	if len(graph.Tasks) != 3 {
		t.Fatalf("tasks: got %d, want 3", len(graph.Tasks))
	}

	if graph.Tasks[0].Agent != "architect" {
		t.Errorf("task 0 agent: got %q", graph.Tasks[0].Agent)
	}
	if graph.Tasks[1].Agent != "developer" {
		t.Errorf("task 1 agent: got %q", graph.Tasks[1].Agent)
	}
	if result.Name != "oauth-login-page" {
		t.Errorf("name: got %q, want %q", result.Name, "oauth-login-page")
	}
	if graph.Name != "oauth-login-page" {
		t.Errorf("graph name: got %q, want %q", graph.Name, "oauth-login-page")
	}
}

func TestDecomposeWithMarkdownFence(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	response := "```json\n{\"tasks\": [{\"id\": \"t1\", \"agent\": \"architect\", \"description\": \"Design\", \"depends_on\": []}]}\n```"

	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(response),
		systemDef,
		"Design something",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}

	if len(result.Graph.Tasks) != 1 {
		t.Fatalf("tasks: got %d, want 1", len(result.Graph.Tasks))
	}
}

func TestDecomposeInvalidJSON(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	_, err := Decompose(
		context.Background(),
		mockDecomposeGenerate("not json at all"),
		systemDef,
		"Do something",
		"",
	)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecomposeWithLoop(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	cannedResponse := `{
		"tasks": [
			{"id": "t1", "agent": "architect", "description": "Design system", "depends_on": []},
			{"id": "t2", "agent": "developer", "description": "Implement with review", "depends_on": ["t1"], "loop_id": "review-1"}
		],
		"loops": [
			{
				"id": "review-1",
				"participants": ["developer", "architect"],
				"states": [
					{"name": "WORKING", "agent": "developer"},
					{"name": "REVIEWING", "agent": "architect"},
					{"name": "REVISING", "agent": "developer"},
					{"name": "APPROVED"},
					{"name": "ESCALATED"}
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
	}`

	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(cannedResponse),
		systemDef,
		"Build a login page",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}

	graph := result.Graph
	if len(graph.Tasks) != 2 {
		t.Fatalf("tasks: got %d, want 2", len(graph.Tasks))
	}
	if graph.Tasks[1].LoopID != "review-1" {
		t.Errorf("task 1 loop_id: got %q, want review-1", graph.Tasks[1].LoopID)
	}
	if len(graph.Loops) != 1 {
		t.Fatalf("loops: got %d, want 1", len(graph.Loops))
	}
	if graph.Loops[0].ID != "review-1" {
		t.Errorf("loop ID: got %q", graph.Loops[0].ID)
	}
	if graph.Loops[0].AgentForState("WORKING") != "developer" {
		t.Errorf("WORKING agent: got %q", graph.Loops[0].AgentForState("WORKING"))
	}
}

func TestDecomposeWithoutLoops(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	// No loops in the output — backward compat.
	cannedResponse := `{
		"tasks": [
			{"id": "t1", "agent": "architect", "description": "Design", "depends_on": []}
		]
	}`

	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(cannedResponse),
		systemDef,
		"Simple request",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if len(result.Graph.Loops) != 0 {
		t.Errorf("expected no loops, got %d", len(result.Graph.Loops))
	}
}

func TestDecomposeWithConductorKnowledge(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	var capturedInput string
	captureGenerate := func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
		if len(input) > 0 {
			capturedInput = input[0].Content // system message
		}
		return &schema.Message{
			Role:    schema.Assistant,
			Content: `{"tasks": [{"id": "t1", "agent": "architect", "description": "Design", "depends_on": []}]}`,
		}, nil
	}

	_, err := Decompose(
		context.Background(),
		captureGenerate,
		systemDef,
		"Build something",
		"Always create review loops for implementations.",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}

	if capturedInput == "" {
		t.Fatal("expected captured input")
	}
	if !contains(capturedInput, "Always create review loops") {
		t.Error("conductor knowledge not in system prompt")
	}
}

func TestDecomposeInvalidLoopDef(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	// Loop with empty ID — should fail validation.
	cannedResponse := `{
		"tasks": [{"id": "t1", "agent": "architect", "description": "Design", "depends_on": [], "loop_id": "bad"}],
		"loops": [{"id": "", "participants": [], "states": [], "transitions": [], "initial_state": "", "terminal_states": [], "max_transitions": 0}]
	}`

	_, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(cannedResponse),
		systemDef,
		"Build something",
		"",
	)
	if err == nil {
		t.Fatal("expected error for invalid loop definition")
	}
}

func TestParseTaskGraphWithEmbeddedBraces(t *testing.T) {
	// Verify string-aware extraction handles JSON with braces inside strings.
	systemDef := config.DefaultFabricDef("test")

	response := `{"tasks": [{"id": "t1", "agent": "architect", "description": "Design {components}", "depends_on": []}]}`

	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(response),
		systemDef,
		"Build something",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}

	if result.Graph.Tasks[0].Description != "Design {components}" {
		t.Errorf("description: got %q", result.Graph.Tasks[0].Description)
	}
}

func TestDecomposeRetryOnValidationError(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	// First call returns invalid (unknown agent), second call returns valid.
	callCount := 0
	retryGenerate := func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
		callCount++
		if callCount == 1 {
			return &schema.Message{
				Role:    schema.Assistant,
				Content: `{"tasks": [{"id": "t1", "agent": "nonexistent-agent", "description": "Bad", "depends_on": []}]}`,
			}, nil
		}
		// Second call: check that error context was appended.
		lastMsg := input[len(input)-1].Content
		if !contains(lastMsg, "unknown agent") {
			t.Error("expected error context in retry message")
		}
		return &schema.Message{
			Role:    schema.Assistant,
			Content: `{"tasks": [{"id": "t1", "agent": "architect", "description": "Fixed", "depends_on": []}]}`,
		}, nil
	}

	result, err := Decompose(context.Background(), retryGenerate, systemDef, "Build something", "")
	if err != nil {
		t.Fatalf("decompose with retry: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 LLM calls, got %d", callCount)
	}
	if result.Graph.Tasks[0].Agent != "architect" {
		t.Errorf("agent: got %q", result.Graph.Tasks[0].Agent)
	}
}

func TestDecomposeUnknownAgentFailsAfterRetry(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	// Always returns unknown agent — should fail after retry exhaustion.
	_, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(`{"tasks": [{"id": "t1", "agent": "ghost", "description": "Bad", "depends_on": []}]}`),
		systemDef,
		"Build something",
		"",
	)
	if err == nil {
		t.Fatal("expected error for unknown agent after retries")
	}
	if !contains(err.Error(), "unknown agent") {
		t.Errorf("error should mention unknown agent: %v", err)
	}
}

func TestDecomposeAccumulatesTokenUsage(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	callCount := 0
	gen := func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		callCount++
		resp := &schema.Message{
			Role: schema.Assistant,
			ResponseMeta: &schema.ResponseMeta{
				Usage: &schema.TokenUsage{PromptTokens: 100, CompletionTokens: 50},
			},
		}
		if callCount == 1 {
			resp.Content = `{"tasks": [{"id": "t1", "agent": "ghost", "description": "Bad", "depends_on": []}]}`
		} else {
			resp.Content = `{"tasks": [{"id": "t1", "agent": "architect", "description": "Good", "depends_on": []}]}`
		}
		return resp, nil
	}

	result, err := Decompose(context.Background(), gen, systemDef, "Test", "")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	// Two calls × 100+50 tokens each = 200 input, 100 output.
	if result.TokenUsage.InputTokens != 200 {
		t.Errorf("input tokens: got %d, want 200", result.TokenUsage.InputTokens)
	}
	if result.TokenUsage.OutputTokens != 100 {
		t.Errorf("output tokens: got %d, want 100", result.TokenUsage.OutputTokens)
	}
}

func TestDecomposeNonActionable(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(`{"actionable": false, "message": "Hello!"}`),
		systemDef,
		"Hey there",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if result.Actionable {
		t.Error("expected Actionable=false")
	}
	if result.Message != "Hello!" {
		t.Errorf("message: got %q, want %q", result.Message, "Hello!")
	}
	if result.Graph != nil {
		t.Error("expected nil graph for non-actionable input")
	}
}

func TestDecomposeActionableFieldIncluded(t *testing.T) {
	systemDef := config.DefaultFabricDef("test")

	// Actionable response with the new actionable field included.
	result, err := Decompose(
		context.Background(),
		mockDecomposeGenerate(`{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design", "depends_on": []}]}`),
		systemDef,
		"Build something",
		"",
	)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if !result.Actionable {
		t.Error("expected Actionable=true")
	}
	if len(result.Graph.Tasks) != 1 {
		t.Fatalf("tasks: got %d, want 1", len(result.Graph.Tasks))
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
