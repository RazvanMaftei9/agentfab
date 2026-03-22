package conductor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type mockToolCallingChatModel struct {
	generateFn func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

func (m *mockToolCallingChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.generateFn(ctx, input, opts...)
}

func (m *mockToolCallingChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.generateFn(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()
	return sr, nil
}

func (m *mockToolCallingChatModel) BindTools(_ []*schema.ToolInfo) error { return nil }

func (m *mockToolCallingChatModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestChatQuestion(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "The system uses JWT for authentication.",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "How does auth work?",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Response != "The system uses JWT for authentication." {
		t.Errorf("response: got %q", resp.Response)
	}
	if resp.Amendment != nil {
		t.Error("expected no amendment for plain question")
	}
}

func TestChatAmendment(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Got it, I'll switch to OAuth.\nAMEND_TASK: Implement OAuth2 login flow instead of basic auth",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName:   "developer",
		Message:     "Actually, use OAuth instead",
		TaskContext: "Implement basic auth login",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Amendment == nil {
		t.Fatal("expected amendment")
	}
	if resp.Amendment.NewDescription != "Implement OAuth2 login flow instead of basic auth" {
		t.Errorf("amendment desc: got %q", resp.Amendment.NewDescription)
	}
	if resp.Amendment.Structural {
		t.Error("expected non-structural amendment")
	}
	// Response should have marker stripped.
	if resp.Response != "Got it, I'll switch to OAuth." {
		t.Errorf("response: got %q", resp.Response)
	}
}

func TestChatStructuralAmendment(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "This needs a designer.\nAMEND_TASK: Build a branded landing page\nRESTRUCTURE: Needs designer agent for visual work",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName:   "developer",
		Message:     "Add a branded landing page",
		TaskContext: "Implement API endpoints",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Amendment == nil {
		t.Fatal("expected amendment")
	}
	if !resp.Amendment.Structural {
		t.Error("expected structural amendment")
	}
	if resp.Response != "This needs a designer." {
		t.Errorf("response: got %q", resp.Response)
	}
}

func TestChatNoTaskContext(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	var capturedInput []*schema.Message
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				capturedInput = input
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Just a plain answer.\nAMEND_TASK: this should be ignored",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "What is the architecture?",
		// No TaskContext.
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	// Without task context, amendment markers should not be parsed.
	if resp.Amendment != nil {
		t.Error("expected no amendment when no task context")
	}
	// System prompt should NOT contain the task context amendment detection section.
	if len(capturedInput) > 0 {
		systemContent := capturedInput[0].Content
		if contains(systemContent, "Current Task Context") {
			t.Error("system prompt should not contain task context section when no task context")
		}
	}
}

func TestParseSuggestedReplies(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantText  string
		wantCount int
		wantFirst string
	}{
		{
			name:      "no suggestions",
			input:     "Just a plain answer.",
			wantText:  "Just a plain answer.",
			wantCount: 0,
		},
		{
			name:      "one suggestion",
			input:     "Here is the design.\nSUGGEST_REPLY: Tell me more about the auth flow",
			wantText:  "Here is the design.",
			wantCount: 1,
			wantFirst: "Tell me more about the auth flow",
		},
		{
			name:      "multiple suggestions",
			input:     "Done.\nSUGGEST_REPLY: Yes, proceed\nSUGGEST_REPLY: No, change approach\nSUGGEST_REPLY: Tell me more",
			wantText:  "Done.",
			wantCount: 3,
			wantFirst: "Yes, proceed",
		},
		{
			name:      "case insensitive",
			input:     "Answer.\nsuggest_reply: Option A",
			wantText:  "Answer.",
			wantCount: 1,
			wantFirst: "Option A",
		},
		{
			name:      "empty suggestion ignored",
			input:     "Answer.\nSUGGEST_REPLY: ",
			wantText:  "Answer.",
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, suggestions := parseSuggestedReplies(tt.input)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if len(suggestions) != tt.wantCount {
				t.Errorf("got %d suggestions, want %d", len(suggestions), tt.wantCount)
			}
			if tt.wantCount > 0 && len(suggestions) > 0 && suggestions[0] != tt.wantFirst {
				t.Errorf("first suggestion = %q, want %q", suggestions[0], tt.wantFirst)
			}
		})
	}
}

func TestParseAmendmentMarkersNoContext(t *testing.T) {
	content := "Hello\nAMEND_TASK: something\nRESTRUCTURE: reason"
	clean, amendment := parseAmendmentMarkers(content, false)
	if amendment != nil {
		t.Error("expected no amendment when hasTaskContext=false")
	}
	if clean != content {
		t.Errorf("content should be unchanged, got %q", clean)
	}
}

func TestParseAmendmentMarkersTaskTargeted(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantText   string
		wantTaskID string
		wantDesc   string
		wantStruct bool
	}{
		{
			name:       "plain format (no task ID)",
			input:      "OK.\nAMEND_TASK: Use OAuth2",
			wantText:   "OK.",
			wantTaskID: "",
			wantDesc:   "Use OAuth2",
		},
		{
			name:       "task-targeted format",
			input:      "I'll tell the designer.\nAMEND_TASK t2: Use Material Design 3 for all components",
			wantText:   "I'll tell the designer.",
			wantTaskID: "t2",
			wantDesc:   "Use Material Design 3 for all components",
		},
		{
			name:       "task-targeted with structural",
			input:      "Needs a rethink.\nAMEND_TASK t3: Rebuild with microservices\nRESTRUCTURE: Architecture change",
			wantText:   "Needs a rethink.",
			wantTaskID: "t3",
			wantDesc:   "Rebuild with microservices",
			wantStruct: true,
		},
		{
			name:       "case insensitive marker",
			input:      "Done.\namend_task t1: New description",
			wantText:   "Done.",
			wantTaskID: "t1",
			wantDesc:   "New description",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, amendment := parseAmendmentMarkers(tt.input, true)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if amendment == nil {
				t.Fatal("expected amendment")
			}
			if amendment.TaskID != tt.wantTaskID {
				t.Errorf("TaskID = %q, want %q", amendment.TaskID, tt.wantTaskID)
			}
			if amendment.NewDescription != tt.wantDesc {
				t.Errorf("NewDescription = %q, want %q", amendment.NewDescription, tt.wantDesc)
			}
			if amendment.Structural != tt.wantStruct {
				t.Errorf("Structural = %v, want %v", amendment.Structural, tt.wantStruct)
			}
		})
	}
}

func TestParseChatDone(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantText string
		wantDone bool
	}{
		{
			name:     "no marker",
			input:    "Just a regular response.",
			wantText: "Just a regular response.",
			wantDone: false,
		},
		{
			name:     "marker at end",
			input:    "Got it, proceeding.\nCHAT_DONE",
			wantText: "Got it, proceeding.",
			wantDone: true,
		},
		{
			name:     "marker with surrounding text",
			input:    "Sure thing.\nCHAT_DONE\nExtra text.",
			wantText: "Sure thing.\nExtra text.",
			wantDone: true,
		},
		{
			name:     "case insensitive",
			input:    "OK.\nchat_done",
			wantText: "OK.",
			wantDone: true,
		},
		{
			name:     "marker with whitespace",
			input:    "Yes.\n  CHAT_DONE  ",
			wantText: "Yes.",
			wantDone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, done := parseChatDone(tt.input)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if done != tt.wantDone {
				t.Errorf("done = %v, want %v", done, tt.wantDone)
			}
		})
	}
}

func TestChatDoneIntegration(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Understood, I'll proceed with that.\nCHAT_DONE",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "Yes, proceed",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !resp.Done {
		t.Error("expected Done=true when CHAT_DONE marker present")
	}
	if resp.Response != "Understood, I'll proceed with that." {
		t.Errorf("response: got %q", resp.Response)
	}
}

func TestStripFileBlocks(t *testing.T) {
	input := "Starting.\n```file:src/main.ts\nconsole.log('x')\n```\nDone."
	clean, removed := stripFileBlocks(input)
	if !removed {
		t.Fatal("expected removed=true")
	}
	if clean != "Starting.\nDone." {
		t.Fatalf("clean = %q, want %q", clean, "Starting.\nDone.")
	}
}

func TestStripPseudoToolTranscript(t *testing.T) {
	input := "Checking now.\n<attempt_tool_use>{\"name\":\"shell\"}</attempt_tool_use>\nLooks broken.\n<tool_use_result>ENOENT</tool_use_result>\nDone."
	clean, removed := stripPseudoToolTranscript(input)
	if !removed {
		t.Fatal("expected removed=true")
	}
	if clean != "Checking now.\nLooks broken.\nDone." {
		t.Fatalf("clean = %q, want %q", clean, "Checking now.\nLooks broken.\nDone.")
	}
}

func TestStripPseudoToolTranscriptToolNameCode(t *testing.T) {
	input := "Start.\n<tool_name>shell</tool_name> <tool_code>ls -la</tool_code>\nAfter."
	clean, removed := stripPseudoToolTranscript(input)
	if !removed {
		t.Fatal("expected removed=true")
	}
	if clean != "Start.\nAfter." {
		t.Fatalf("clean = %q, want %q", clean, "Start.\nAfter.")
	}
}

func TestChatStripsFileBlocksFromResponse(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Implemented.\n```file:todo-app/index.html\n<html></html>\n```\nCHAT_DONE",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "developer",
		Message:   "Proceed",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !resp.Done {
		t.Fatal("expected Done=true")
	}
	if contains(resp.Response, "```file:") {
		t.Fatalf("response should not contain file blocks: %q", resp.Response)
	}
	if !contains(resp.Response, "Chat note: file blocks were omitted") {
		t.Fatalf("expected omission note, got: %q", resp.Response)
	}
}

func TestChatStripsPseudoToolTranscript(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "I checked.\n<attempt_tool_use>{\"name\":\"shell\",\"arguments\":{\"command\":\"ls\"}}</attempt_tool_use>\n<tool_use_result>total 0</tool_use_result>\nPlease run coordinated work.\nCHAT_DONE",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "developer",
		Message:   "What is wrong?",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !resp.Done {
		t.Fatal("expected Done=true")
	}
	if contains(resp.Response, "attempt_tool_use") || contains(resp.Response, "tool_use_result") {
		t.Fatalf("response should not contain pseudo tool traces: %q", resp.Response)
	}
	if !contains(resp.Response, "Chat note: raw tool transcript markup was removed for readability.") {
		t.Fatalf("expected pseudo tool note, got: %q", resp.Response)
	}
}

func TestChatExecutesRealTools(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")
	for i := range td.Agents {
		if td.Agents[i].Name != "architect" {
			continue
		}
		td.Agents[i].Tools = []runtime.ToolConfig{
			{
				Name:         "inspect",
				Instructions: "Inspect local project state.",
				Command:      "echo tool-ok",
				Parameters: map[string]runtime.ToolParam{
					"path": {Type: "string", Required: true},
				},
			},
		}
	}

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockToolCallingChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					idx := 0
					return &schema.Message{
						Role: schema.Assistant,
						ToolCalls: []schema.ToolCall{
							{
								Index: &idx,
								ID:    "tc-1",
								Type:  "function",
								Function: schema.FunctionCall{
									Name:      "inspect",
									Arguments: `{"path":"."}`,
								},
							},
						},
					}, nil
				}

				seenToolResult := false
				for _, m := range input {
					if m.Role == schema.Tool && contains(m.Content, "tool-ok") {
						seenToolResult = true
						break
					}
				}
				if !seenToolResult {
					t.Fatalf("expected tool result in chat loop input")
				}

				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Checked with tools. The files are present.",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "Please inspect whether files exist.",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !contains(resp.Response, "Checked with tools") {
		t.Fatalf("expected tool-backed response, got: %q", resp.Response)
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", callCount)
	}
}

func TestExtractPseudoToolCalls(t *testing.T) {
	content := `<tool_name>shell</tool_name> <tool_code>echo hi</tool_code>
<attempt_tool_use>{"name":"shell","arguments":{"command":"pwd"}}</attempt_tool_use>`
	calls := extractPseudoToolCalls(content)
	if len(calls) != 2 {
		t.Fatalf("expected 2 pseudo tool calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "shell" || !contains(calls[0].Function.Arguments, "echo hi") {
		t.Fatalf("unexpected first call: %+v", calls[0])
	}
	if calls[1].Function.Name != "shell" || !contains(calls[1].Function.Arguments, "pwd") {
		t.Fatalf("unexpected second call: %+v", calls[1])
	}
}

func TestChatExecutesPseudoToolMarkupFallback(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")
	for i := range td.Agents {
		if td.Agents[i].Name != "architect" {
			continue
		}
		td.Agents[i].Tools = []runtime.ToolConfig{
			{
				Name:         "shell",
				Instructions: "Execute shell commands.",
				Parameters: map[string]runtime.ToolParam{
					"command": {Type: "string", Required: true},
				},
			},
		}
	}

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		// Intentionally return a model that does not support WithTools, forcing fallback parsing.
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: "<tool_name>shell</tool_name> <tool_code>printf fallback-ok</tool_code>",
					}, nil
				}

				seenToolResult := false
				for _, m := range input {
					if m.Role == schema.Tool && contains(m.Content, "fallback-ok") {
						seenToolResult = true
						break
					}
				}
				if !seenToolResult {
					t.Fatalf("expected fallback tool result in chat loop input")
				}

				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Checked via tool fallback.",
				}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "Inspect quickly.",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !contains(resp.Response, "Checked via tool fallback.") {
		t.Fatalf("expected fallback response, got: %q", resp.Response)
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", callCount)
	}
}

func TestPersistChatScratch(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	// Simulate an agent writing a file to its scratch directory.
	storage := c.StorageFactory("developer")
	scratchDir := storage.TierDir(runtime.TierScratch)
	os.MkdirAll(filepath.Join(scratchDir, "src"), 0755)
	if err := os.WriteFile(filepath.Join(scratchDir, "src", "App.tsx"), []byte("export default function App() {}"), 0644); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}

	// Call persistChatScratch.
	c.persistChatScratch(ctx, "developer")

	// Verify the file was persisted to shared storage.
	sharedDir := storage.TierDir(runtime.TierShared)
	persistedPath := filepath.Join(sharedDir, "artifacts", "developer", "src", "App.tsx")
	data, err := os.ReadFile(persistedPath)
	if err != nil {
		t.Fatalf("expected persisted file at %s: %v", persistedPath, err)
	}
	if string(data) != "export default function App() {}" {
		t.Errorf("persisted content = %q", string(data))
	}

	// Clean up scratch to avoid interfering with other tests.
	os.RemoveAll(scratchDir)
}

func TestPersistChatScratchSkipsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	td := config.DefaultFabricDef("test")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
			},
		}, nil
	}

	c, _ := New(td, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	storage := c.StorageFactory("developer")
	scratchDir := storage.TierDir(runtime.TierScratch)
	os.MkdirAll(scratchDir, 0755)

	// Write a hidden file and a normal file.
	os.WriteFile(filepath.Join(scratchDir, ".sandbox-out-12345"), []byte("noise"), 0644)
	os.WriteFile(filepath.Join(scratchDir, "index.ts"), []byte("console.log('hi')"), 0644)

	c.persistChatScratch(ctx, "developer")

	sharedDir := storage.TierDir(runtime.TierShared)

	// Normal file should be persisted.
	if _, err := os.Stat(filepath.Join(sharedDir, "artifacts", "developer", "index.ts")); err != nil {
		t.Error("expected index.ts to be persisted")
	}

	// Hidden file should NOT be persisted.
	if _, err := os.Stat(filepath.Join(sharedDir, "artifacts", "developer", ".sandbox-out-12345")); err == nil {
		t.Error("hidden file should not be persisted")
	}

	os.RemoveAll(scratchDir)
}

func TestParseChatEscalation(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantText   string
		wantReason string
	}{
		{
			name:       "no escalation",
			input:      "Just a regular response.",
			wantText:   "Just a regular response.",
			wantReason: "",
		},
		{
			name:       "escalation at end",
			input:      "This needs multi-agent coordination.\nESCALATE: requires architect and developer",
			wantText:   "This needs multi-agent coordination.",
			wantReason: "requires architect and developer",
		},
		{
			name:       "escalation mid text",
			input:      "Line 1.\nESCALATE: too complex for chat\nLine 2.",
			wantText:   "Line 1.\nLine 2.",
			wantReason: "too complex for chat",
		},
		{
			name:       "case sensitive prefix",
			input:      "ESCALATE: needs orchestration",
			wantText:   "",
			wantReason: "needs orchestration",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, reason := parseChatEscalation(tt.input)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
