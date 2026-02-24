package structuredoutput

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

var testSchema = Schema{
	Name:        "task_result",
	Description: "Structured task output",
	Raw: json.RawMessage(`{
		"type": "object",
		"properties": {
			"status": {"type": "string", "enum": ["success", "failure"]},
			"summary": {"type": "string"}
		},
		"required": ["status", "summary"]
	}`),
}

// mockGenerateFn returns a function that produces the given content.
func mockGenerateFn(content string) GenerateFn {
	return func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.Assistant,
			Content: content,
		}, nil
	}
}

// mockToolCallFn returns a function that produces a tool call response.
func mockToolCallFn(toolName, args string) GenerateFn {
	return func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
		return &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{
				{
					ID: "call_1",
					Function: schema.FunctionCall{
						Name:      toolName,
						Arguments: args,
					},
				},
			},
		}, nil
	}
}

func TestGenerateOpenAI(t *testing.T) {
	jsonContent := `{"status":"success","summary":"All done"}`
	fn := mockGenerateFn(jsonContent)

	result, err := Generate(context.Background(), fn, []*schema.Message{
		schema.UserMessage("Do the thing"),
	}, "openai/gpt-4o", testSchema)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result.JSON, &parsed); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}
	if parsed["status"] != "success" {
		t.Errorf("expected status=success, got %q", parsed["status"])
	}
}

func TestGenerateOpenAIWithMarkdownFence(t *testing.T) {
	content := "Here's the result:\n```json\n{\"status\":\"failure\",\"summary\":\"Error occurred\"}\n```"
	fn := mockGenerateFn(content)

	result, err := Generate(context.Background(), fn, []*schema.Message{
		schema.UserMessage("Do the thing"),
	}, "openai/gpt-4o", testSchema)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result.JSON, &parsed); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if parsed["status"] != "failure" {
		t.Errorf("expected status=failure, got %q", parsed["status"])
	}
}

func TestGenerateClaude(t *testing.T) {
	args := `{"status":"success","summary":"Task completed"}`
	fn := mockToolCallFn("task_result", args)

	result, err := Generate(context.Background(), fn, []*schema.Message{
		schema.UserMessage("Do the thing"),
	}, "anthropic/claude-opus-4", testSchema)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result.JSON, &parsed); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if parsed["status"] != "success" {
		t.Errorf("expected status=success, got %q", parsed["status"])
	}
}

func TestGenerateClaudeFallbackToContent(t *testing.T) {
	// If Claude doesn't return a tool call, fall back to content parsing.
	jsonContent := `{"status":"success","summary":"Fallback"}`
	fn := mockGenerateFn(jsonContent)

	result, err := Generate(context.Background(), fn, []*schema.Message{
		schema.UserMessage("Do the thing"),
	}, "anthropic/claude-opus-4", testSchema)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	json.Unmarshal(result.JSON, &parsed)
	if parsed["summary"] != "Fallback" {
		t.Errorf("expected summary=Fallback, got %q", parsed["summary"])
	}
}

func TestGenerateGemini(t *testing.T) {
	jsonContent := `{"status":"success","summary":"Gemini result"}`
	fn := mockGenerateFn(jsonContent)

	result, err := Generate(context.Background(), fn, []*schema.Message{
		schema.UserMessage("Do the thing"),
	}, "google/gemini-2.0-flash", testSchema)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	json.Unmarshal(result.JSON, &parsed)
	if parsed["summary"] != "Gemini result" {
		t.Errorf("expected summary=Gemini result, got %q", parsed["summary"])
	}
}

func TestGenerateUnknownProvider(t *testing.T) {
	jsonContent := `{"status":"success","summary":"Unknown provider"}`
	fn := mockGenerateFn(jsonContent)

	result, err := Generate(context.Background(), fn, []*schema.Message{
		schema.UserMessage("Do the thing"),
	}, "local/llama3", testSchema)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]string
	json.Unmarshal(result.JSON, &parsed)
	if parsed["summary"] != "Unknown provider" {
		t.Errorf("expected summary=Unknown provider, got %q", parsed["summary"])
	}
}

func TestGenerateInvalidModelID(t *testing.T) {
	fn := mockGenerateFn("{}")
	_, err := Generate(context.Background(), fn, nil, "no-slash", testSchema)
	if err == nil {
		t.Fatal("expected error for invalid model ID")
	}
}

func TestGenerateEmptyContent(t *testing.T) {
	fn := mockGenerateFn("")
	_, err := Generate(context.Background(), fn, nil, "openai/gpt-4o", testSchema)
	if err == nil {
		t.Fatal("expected error for empty content")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(json.RawMessage(`{"status":"success"}`)); err != nil {
		t.Fatalf("valid JSON should pass: %v", err)
	}
	if err := Validate(json.RawMessage("")); err == nil {
		t.Fatal("empty should fail")
	}
	if err := Validate(json.RawMessage("not json")); err == nil {
		t.Fatal("invalid JSON should fail")
	}
}

func TestExtractJSONFromContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantKey string
		wantErr bool
	}{
		{"pure_json", `{"a":"b"}`, "a", false},
		{"with_fence", "```json\n{\"a\":\"b\"}\n```", "a", false},
		{"with_prose", "Here is the result:\n{\"a\":\"b\"}\nDone.", "a", false},
		{"empty", "", "", true},
		{"no_json", "just text", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := ExtractJSONFromContent(tc.content)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var m map[string]string
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if m[tc.wantKey] == "" {
				t.Errorf("missing key %q in result", tc.wantKey)
			}
		})
	}
}

func TestToolInfo(t *testing.T) {
	info := ToolInfo(testSchema)
	if info.Name != "task_result" {
		t.Errorf("expected tool name 'task_result', got %q", info.Name)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("expected non-nil ParamsOneOf")
	}
}
