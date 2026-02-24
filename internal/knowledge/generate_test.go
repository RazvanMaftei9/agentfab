package knowledge

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

func mockGenerate(response string) func(context.Context, []*schema.Message) (*schema.Message, error) {
	return func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: response}, nil
	}
}

func TestGenerateManifest(t *testing.T) {
	graph := &taskgraph.TaskGraph{
		RequestID: "req-1",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "architect", Description: "Design API", Status: taskgraph.StatusCompleted, Result: "REST API with /users and /items endpoints"},
			{ID: "t2", Agent: "developer", Description: "Implement API", Status: taskgraph.StatusCompleted, Result: "Implemented endpoints with JWT auth"},
		},
	}

	llmResponse := `{
		"nodes": [
			{
				"id": "architect/api-design",
				"agent": "architect",
				"title": "API Design",
				"summary": "REST API with users and items endpoints.",
				"content": "# API Design\n\nREST API with /users and /items endpoints.",
				"tags": ["api", "rest"]
			},
			{
				"id": "developer/jwt-auth",
				"agent": "developer",
				"title": "JWT Authentication",
				"summary": "JWT-based authentication for API endpoints.",
				"content": "# JWT Authentication\n\nImplemented JWT auth.",
				"tags": ["auth", "jwt"]
			}
		],
		"edges": [
			{"from": "architect/api-design", "to": "developer/jwt-auth", "relation": "implements"}
		]
	}`

	manifest, _, err := Generate(
		context.Background(),
		mockGenerate(llmResponse),
		graph,
		NewGraph(),
		"todo-app-mvp",
	)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if len(manifest.Nodes) != 2 {
		t.Fatalf("nodes: got %d, want 2", len(manifest.Nodes))
	}
	if manifest.Nodes[0].ID != "architect/api-design" {
		t.Errorf("node 0 ID: got %q", manifest.Nodes[0].ID)
	}
	if manifest.Nodes[0].Content == "" {
		t.Error("node 0 content should not be empty")
	}
	if len(manifest.Edges) != 1 {
		t.Fatalf("edges: got %d, want 1", len(manifest.Edges))
	}
	if manifest.Edges[0].Relation != "implements" {
		t.Errorf("edge relation: got %q", manifest.Edges[0].Relation)
	}
}

func TestGenerateEmptyResults(t *testing.T) {
	graph := &taskgraph.TaskGraph{
		RequestID: "req-1",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "architect", Description: "Design", Status: taskgraph.StatusFailed, Result: "failed"},
		},
	}

	manifest, _, err := Generate(
		context.Background(),
		mockGenerate("should not be called"),
		graph,
		NewGraph(),
		"test",
	)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(manifest.Nodes) != 0 {
		t.Errorf("expected empty manifest for failed tasks, got %d nodes", len(manifest.Nodes))
	}
}

func TestGenerateWithMarkdownFence(t *testing.T) {
	graph := &taskgraph.TaskGraph{
		RequestID: "req-1",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "architect", Description: "Design", Status: taskgraph.StatusCompleted, Result: "Done"},
		},
	}

	response := "```json\n{\"nodes\": [{\"id\": \"architect/design\", \"agent\": \"architect\", \"title\": \"Design\", \"summary\": \"Overview\", \"content\": \"# Design\", \"tags\": []}], \"edges\": []}\n```"

	manifest, _, err := Generate(
		context.Background(),
		mockGenerate(response),
		graph,
		NewGraph(),
		"test",
	)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(manifest.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(manifest.Nodes))
	}
}

func TestExtractJSONWithBracesInContent(t *testing.T) {
	// Simulates LLM output where the content field contains code with braces.
	input := "```json\n" + `{"nodes": [{"id": "dev/auth", "agent": "developer", "title": "Auth", "summary": "S", "content": "# Auth\n\n` + "```" + `go\nfunc main() {\n  fmt.Println(\"hello\")\n}\n` + "```" + `", "tags": []}], "edges": []}` + "\n```"

	m, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parse with braces in content: %v", err)
	}
	if len(m.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(m.Nodes))
	}
	if m.Nodes[0].ID != "dev/auth" {
		t.Errorf("id: got %q", m.Nodes[0].ID)
	}
}

func TestExtractJSONUnbalancedBracesInContent(t *testing.T) {
	// Content has more } than { inside a string — naive counter would break.
	input := `{"nodes": [{"id": "a/b", "agent": "a", "title": "T", "summary": "S", "content": "obj.method()}", "tags": []}], "edges": []}`

	m, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parse with unbalanced braces: %v", err)
	}
	if len(m.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(m.Nodes))
	}
}

func TestParseManifest(t *testing.T) {
	input := `{
		"nodes": [
			{"id": "a/b", "agent": "a", "title": "B", "summary": "S", "content": "C", "tags": ["x"]}
		],
		"edges": [
			{"from": "a/b", "to": "c/d", "relation": "related_to"}
		]
	}`

	m, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(m.Nodes))
	}
	if m.Nodes[0].Agent != "a" {
		t.Errorf("agent: got %q", m.Nodes[0].Agent)
	}
	if len(m.Edges) != 1 {
		t.Fatalf("edges: got %d, want 1", len(m.Edges))
	}
}
