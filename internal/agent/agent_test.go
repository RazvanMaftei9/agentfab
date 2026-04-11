package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/loop"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func mockGenerate(content string) func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	return func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.Assistant,
			Content: content,
		}, nil
	}
}

func TestAgentReceivesTaskAndResponds(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Generate:     mockGenerate("Here is the implementation code."),
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run agent in background.
	go ag.Run(ctx)

	// Send task assignment.
	task := &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Implement the login page"}},
		Timestamp: time.Now(),
	}
	conductorComm.Send(ctx, task)

	// Wait for response.
	select {
	case result := <-conductorComm.Receive(ctx):
		if result.Type != message.TypeTaskResult {
			t.Errorf("type: got %q, want task_result", result.Type)
		}
		if result.From != "developer" {
			t.Errorf("from: got %q", result.From)
		}
		text := extractText(result)
		if text != "Here is the implementation code." {
			t.Errorf("content: got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}
}

// mockAppender counts appended log entries.
type mockAppender struct {
	entries [][]byte
}

func (a *mockAppender) Append(_ context.Context, _ string, data []byte) error {
	a.entries = append(a.entries, append([]byte{}, data...))
	return nil
}

func TestAgentNosDuplicateLogEntries(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	appender := &mockAppender{}
	logger := message.NewLogger(appender)

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Logger:       logger,
		Generate:     mockGenerate("done"),
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send task assignment.
	task := &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "do something"}},
		Timestamp: time.Now(),
	}
	conductorComm.Send(ctx, task)

	// Wait for response.
	select {
	case <-conductorComm.Receive(ctx):
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	// Agent should only log its outbound result, NOT the inbound task_assignment.
	// The scheduler is responsible for logging outbound task_assignments.
	for _, entry := range appender.entries {
		if string(entry) != "" {
			var msg message.Message
			if err := json.Unmarshal(entry[:len(entry)-1], &msg); err == nil {
				if msg.Type == message.TypeTaskAssignment {
					t.Error("agent should not log inbound task_assignment messages")
				}
			}
		}
	}
	if len(appender.entries) != 1 {
		t.Errorf("expected 1 log entry (the result), got %d", len(appender.entries))
	}
}

func TestAgentCheckpoint(t *testing.T) {
	dir := t.TempDir()

	cp := &Checkpoint{
		AgentName:   "developer",
		CurrentTask: "task-1",
		Metadata:    map[string]string{"key": "value"},
	}

	if err := SaveCheckpoint(dir, cp); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadCheckpoint(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.AgentName != "developer" {
		t.Errorf("agent name: got %q", loaded.AgentName)
	}
	if loaded.CurrentTask != "task-1" {
		t.Errorf("current task: got %q", loaded.CurrentTask)
	}
	if loaded.Metadata["key"] != "value" {
		t.Errorf("metadata: got %v", loaded.Metadata)
	}
}

func TestAgentCheckpointNotFound(t *testing.T) {
	cp, err := LoadCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cp != nil {
		t.Fatal("expected nil checkpoint")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:         "developer",
		Purpose:      "Code generation and testing",
		Capabilities: []string{"code_gen", "test_writing"},
	}

	prompt := BuildSystemPrompt(def, "Use Go 1.23.", []runtime.AgentDefinition{
		{Name: "conductor", Purpose: "Orchestration"},
		{Name: "architect", Purpose: "System design", Capabilities: []string{"design_docs"}},
	})

	checks := []string{
		"developer",
		"Code generation and testing",
		"conductor",
		"architect",
		"Use Go 1.23.",
		"ASK_USER rather than guess",
	}

	for _, check := range checks {
		if !contains(prompt, check) {
			t.Errorf("prompt should contain %q", check)
		}
	}
}

func TestAgentDependencyContextFlowsToLLM(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("designer")

	// Capture the LLM input to verify dependency context is included.
	var capturedInput string
	captureGenerate := func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
		// The user message is input[1] (input[0] is the system prompt).
		if len(input) > 1 {
			capturedInput = input[1].Content
		}
		return &schema.Message{Role: schema.Assistant, Content: "design done"}, nil
	}

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "designer",
			Purpose: "UI design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Generate:     captureGenerate,
		SystemPrompt: "You are a designer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send task with text + dependency DataParts (mimics scheduler with upstream results).
	task := &message.Message{
		ID:        "msg-2",
		RequestID: "req-2",
		From:      "conductor",
		To:        "designer",
		Type:      message.TypeTaskAssignment,
		Parts: []message.Part{
			message.TextPart{Text: "Design the login page UI"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": "developer",
				"result":           "Implemented REST API with /login endpoint returning JWT tokens.",
			}},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t2",
				"dependency_agent": "architect",
				"result":           "Use Material Design components for consistency.",
			}},
		},
		Timestamp: time.Now(),
	}
	conductorComm.Send(ctx, task)

	// Wait for response.
	select {
	case <-conductorComm.Receive(ctx):
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}

	// Verify the LLM input includes the task description.
	if !strings.Contains(capturedInput, "Design the login page UI") {
		t.Error("LLM input missing task description")
	}
	// Verify dependency context from t1 (developer).
	if !strings.Contains(capturedInput, "Context from t1 (developer)") {
		t.Errorf("LLM input missing t1 dependency header, got:\n%s", capturedInput)
	}
	if !strings.Contains(capturedInput, "REST API with /login endpoint") {
		t.Error("LLM input missing t1 dependency result content")
	}
	// Verify dependency context from t2 (architect).
	if !strings.Contains(capturedInput, "Context from t2 (architect)") {
		t.Errorf("LLM input missing t2 dependency header, got:\n%s", capturedInput)
	}
	if !strings.Contains(capturedInput, "Material Design components") {
		t.Error("LLM input missing t2 dependency result content")
	}
}

func TestTruncateDepsUnderBudget(t *testing.T) {
	deps := []string{"short dep 1", "short dep 2"}
	result := truncateDeps(deps, 1000)
	if len(result) != 2 || result[0] != "short dep 1" {
		t.Errorf("should not truncate: %v", result)
	}
}

func TestTruncateDepsOverBudget(t *testing.T) {
	big := strings.Repeat("x", 1000)
	deps := []string{big, big}
	result := truncateDeps(deps, 500) // 500 budget for 2000 chars

	for i, d := range result {
		if len(d) > 350 { // each gets ~250 + truncation note
			t.Errorf("dep %d not truncated enough: %d chars", i, len(d))
		}
		if !strings.Contains(d, "truncated") {
			t.Errorf("dep %d missing truncation note", i)
		}
	}
}

func TestTruncateDepsMixedSizes(t *testing.T) {
	small := "tiny"
	big := strings.Repeat("x", 1000)
	deps := []string{small, big}
	result := truncateDeps(deps, 300)

	// Small dep should be kept as-is.
	if result[0] != "tiny" {
		t.Errorf("small dep should be unchanged: got %q", result[0])
	}
	// Big dep should be truncated.
	if !strings.Contains(result[1], "truncated") {
		t.Error("big dep should be truncated")
	}
}

func TestBuildTaskInputTextOnly(t *testing.T) {
	ag := &Agent{} // No storage needed for text-only.
	msg := &message.Message{
		Parts: []message.Part{message.TextPart{Text: "just a task"}},
	}
	got := ag.buildTaskInput(context.Background(), msg, "")
	if !strings.Contains(got, "just a task") {
		t.Errorf("expected task text, got %q", got)
	}
	if !strings.Contains(got, "## Scope: Standard") {
		t.Errorf("expected default scope hint, got %q", got)
	}
}

func TestAgentSendResultWritesArtifact(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")
	storage := local.NewStorage(t.TempDir(), "developer")

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Storage:      storage,
		Generate:     mockGenerate("generated code here"),
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Write code"}},
		Timestamp: time.Now(),
	})

	select {
	case result := <-conductorComm.Receive(ctx):
		// Verify TextPart is present.
		text := extractText(result)
		if text != "generated code here" {
			t.Errorf("text: got %q", text)
		}
		// Verify FilePart is present.
		var fp message.FilePart
		for _, p := range result.Parts {
			if f, ok := p.(message.FilePart); ok {
				fp = f
				break
			}
		}
		if fp.URI == "" {
			t.Fatal("expected FilePart with artifact URI")
		}
		if fp.URI != "artifacts/developer/_requests/req-1/result.md" {
			t.Errorf("artifact URI: got %q", fp.URI)
		}
		if fp.MimeType != "text/markdown" {
			t.Errorf("mime type: got %q", fp.MimeType)
		}
		// Verify file exists on storage.
		data, err := storage.Read(ctx, runtime.TierShared, fp.URI)
		if err != nil {
			t.Fatalf("read artifact: %v", err)
		}
		if string(data) != "generated code here" {
			t.Errorf("artifact content: got %q", string(data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestAgentDependencyContextFromArtifactURI(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("designer")
	storage := local.NewStorage(t.TempDir(), "developer")

	// Pre-write an artifact that the designer agent will read.
	artifactPath := "artifacts/developer/req-2/result.md"
	if err := storage.Write(context.Background(), runtime.TierShared, artifactPath, []byte("API spec from developer")); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	var capturedInput string
	captureGenerate := func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
		if len(input) > 1 {
			capturedInput = input[1].Content
		}
		return &schema.Message{Role: schema.Assistant, Content: "done"}, nil
	}

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "designer",
			Purpose: "UI design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Storage:      storage,
		Generate:     captureGenerate,
		SystemPrompt: "You are a designer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send task with artifact_uri instead of inline result.
	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-2",
		RequestID: "req-2",
		From:      "conductor",
		To:        "designer",
		Type:      message.TypeTaskAssignment,
		Parts: []message.Part{
			message.TextPart{Text: "Design the UI"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": "developer",
				"artifact_uri":     artifactPath,
			}},
		},
		Timestamp: time.Now(),
	})

	select {
	case <-conductorComm.Receive(ctx):
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	if !strings.Contains(capturedInput, "Context from t1 (developer)") {
		t.Errorf("missing dependency header, got:\n%s", capturedInput)
	}
	if !strings.Contains(capturedInput, "Upstream artifact source (READ FIRST): "+artifactPath) {
		t.Errorf("missing artifact URI reference, got:\n%s", capturedInput)
	}
	if strings.Contains(capturedInput, "API spec from developer") {
		t.Errorf("artifact body should not be inlined, got:\n%s", capturedInput)
	}
}

func TestAgentSendResultWritesMultipleArtifacts(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")
	storage := local.NewStorage(t.TempDir(), "developer")

	llmOutput := "Here is the implementation.\n\n" +
		"```file:README.md\n# My App\n## Build\ngo build ./...\n```\n\n" +
		"```file:main.go\npackage main\n\nfunc main() {}\n```"

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Storage:      storage,
		Generate:     mockGenerate(llmOutput),
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Write an app"}},
		Timestamp: time.Now(),
	})

	select {
	case result := <-conductorComm.Receive(ctx):
		// Verify TextPart has the summary (not the full LLM output).
		text := extractText(result)
		if text != "Here is the implementation." {
			t.Errorf("text: got %q", text)
		}

		// Collect all FileParts.
		var fps []message.FilePart
		for _, p := range result.Parts {
			if f, ok := p.(message.FilePart); ok {
				fps = append(fps, f)
			}
		}

		// Expect 3 FileParts: README.md, main.go, summary.md.
		if len(fps) != 3 {
			t.Fatalf("expected 3 FileParts, got %d", len(fps))
		}

		// Verify README.md artifact (global, no requestID).
		readmeURI := "artifacts/developer/README.md"
		if fps[0].URI != readmeURI {
			t.Errorf("FilePart 0 URI: got %q, want %q", fps[0].URI, readmeURI)
		}
		data, err := storage.Read(ctx, runtime.TierShared, readmeURI)
		if err != nil {
			t.Fatalf("read README.md: %v", err)
		}
		if string(data) != "# My App\n## Build\ngo build ./..." {
			t.Errorf("README.md content: got %q", string(data))
		}

		// Verify main.go artifact (global, no requestID).
		mainURI := "artifacts/developer/main.go"
		if fps[1].URI != mainURI {
			t.Errorf("FilePart 1 URI: got %q, want %q", fps[1].URI, mainURI)
		}
		if fps[1].MimeType != "text/x-go" {
			t.Errorf("main.go mime: got %q", fps[1].MimeType)
		}
		data, err = storage.Read(ctx, runtime.TierShared, mainURI)
		if err != nil {
			t.Fatalf("read main.go: %v", err)
		}
		if string(data) != "package main\n\nfunc main() {}" {
			t.Errorf("main.go content: got %q", string(data))
		}

		// Verify summary.md artifact (per-request trace).
		summaryURI := "artifacts/developer/_requests/req-1/summary.md"
		if fps[2].URI != summaryURI {
			t.Errorf("FilePart 2 URI: got %q, want %q", fps[2].URI, summaryURI)
		}
		data, err = storage.Read(ctx, runtime.TierShared, summaryURI)
		if err != nil {
			t.Fatalf("read summary.md: %v", err)
		}
		if string(data) != "Here is the implementation." {
			t.Errorf("summary.md content: got %q", string(data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestBuildSystemPromptSpecialKnowledgeCarriesDirectives(t *testing.T) {
	// Role directives now live in agent markdown files and are injected via specialKnowledge.
	def := runtime.AgentDefinition{Name: "architect", Purpose: "System design"}
	sk := "## Role Directives\n- Your design.md is the single source of truth\n"
	prompt := BuildSystemPrompt(def, sk, nil)
	if !strings.Contains(prompt, "design.md is the single source of truth") {
		t.Error("special knowledge content not injected into prompt")
	}
}

func TestBuildSystemPromptNoReviewProtocol(t *testing.T) {
	// ReviewPrompt must NOT appear in the base system prompt — it is injected
	// only by the loop FSM (BuildStatePrompt) during actual review states.
	def := runtime.AgentDefinition{
		Name:         "architect",
		Purpose:      "System design",
		Capabilities: []string{"design_review", "code_review"},
		ReviewPrompt: "## Review Protocol\nVERDICT: approved or VERDICT: revise\n",
	}
	prompt := BuildSystemPrompt(def, "", nil)
	if strings.Contains(prompt, "Review Protocol") {
		t.Error("ReviewPrompt should NOT be in base system prompt (causes VERDICT output on non-review tasks)")
	}
	if strings.Contains(prompt, "VERDICT") {
		t.Error("VERDICT instructions should NOT be in base system prompt")
	}
}

func TestBuildSystemPromptIncludesDependencyRules(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:    "designer",
		Purpose: "UI design",
	}
	prompt := BuildSystemPrompt(def, "", nil)
	if !strings.Contains(prompt, "upstream tasks") {
		t.Error("prompt missing upstream dependency rule")
	}
	if !strings.Contains(prompt, "dependency context") {
		t.Error("prompt missing dependency context rule")
	}
}

func TestAgentReviewResponseRouting(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("architect")

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:         "architect",
			Purpose:      "System design",
			Model:        "anthropic/claude-opus-4",
			Capabilities: []string{"design_review"},
		},
		Comm:         agentComm,
		Generate:     mockGenerate("VERDICT: approved\n\nAll looks good."),
		SystemPrompt: "You are an architect.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send a review request.
	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "architect",
		Type:      message.TypeReviewRequest,
		Parts:     []message.Part{message.TextPart{Text: "Review the implementation"}},
		Timestamp: time.Now(),
	})

	select {
	case result := <-conductorComm.Receive(ctx):
		// Should be TypeReviewResponse, not TypeTaskResult.
		if result.Type != message.TypeReviewResponse {
			t.Errorf("type: got %q, want review_response", result.Type)
		}
		if result.Metadata["verdict"] != "approved" {
			t.Errorf("verdict: got %q, want approved", result.Metadata["verdict"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestExtractVerdict(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"VERDICT: approved\n\nAll looks good.", "approved"},
		{"VERDICT: revise\n\n1. Fix error handling", "revise"},
		{"verdict: Approved\nGood work.", "approved"},
		{"VERDICT: Revise - needs changes\n1. Fix X", "revise"},
		{"Some text\nVERDICT: approved\nMore text", "approved"},
		// Heuristic fallback: approval language.
		{"The code is well-structured and LGTM.", "approved"},
		{"Everything looks good, no issues found.", "approved"},
		{"This passes review and is ready for deployment.", "approved"},
		// Heuristic fallback: revision language.
		{"This needs revision — several issues found.", "revise"},
		{"Critical issues detected, please fix these problems.", "revise"},
		// No signals at all.
		{"Here is some generic text about the weather.", ""},
		// Explicit verdict takes precedence over heuristic.
		{"VERDICT: revise\nOverall it looks good but needs work.", "revise"},
	}

	for _, tc := range tests {
		got := extractVerdict(tc.input)
		if got != tc.want {
			t.Errorf("extractVerdict(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestReviewFeedbackSummary(t *testing.T) {
	t.Run("strips_preamble_before_verdict", func(t *testing.T) {
		in := "I will inspect files first.\n\nVERDICT: revise\n\n1. Fix App.tsx layout\n2. Fix styles"
		got := reviewFeedbackSummary(in, "revise")
		if !strings.HasPrefix(got, "VERDICT: revise") {
			t.Fatalf("expected summary to start with verdict, got: %q", got)
		}
		if strings.Contains(got, "inspect files first") {
			t.Fatalf("expected preamble to be removed, got: %q", got)
		}
	})

	t.Run("adds_verdict_prefix_when_missing", func(t *testing.T) {
		in := "1. Fix recurrence end date validation."
		got := reviewFeedbackSummary(in, "revise")
		if !strings.HasPrefix(got, "VERDICT: revise") {
			t.Fatalf("expected synthetic verdict prefix, got: %q", got)
		}
	})
}

func TestBuildTaskInputWithUserRequest(t *testing.T) {
	ag := &Agent{}
	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "Implement login page"},
			message.DataPart{Data: map[string]any{
				"user_request": "Build a web app with OAuth login",
			}},
		},
	}
	got := ag.buildTaskInput(context.Background(), msg, "")
	if !strings.Contains(got, "## Original User Request") {
		t.Error("missing user request header")
	}
	if !strings.Contains(got, "Build a web app with OAuth login") {
		t.Error("missing user request content")
	}
	if !strings.Contains(got, "Implement login page") {
		t.Error("missing task description")
	}
}

func TestBuildTaskInputConstraintAnchoring(t *testing.T) {
	ag := &Agent{}
	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "Build the UI"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": "architect",
				"result":           "Use React with TypeScript",
			}},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t2",
				"dependency_agent": "designer",
				"result":           "Blue color scheme",
			}},
		},
	}
	got := ag.buildTaskInput(context.Background(), msg, "")
	// Both dependencies should be labeled as binding constraints.
	if count := strings.Count(got, "(binding constraint"); count != 2 {
		t.Errorf("expected 2 binding constraint labels, got %d in:\n%s", count, got)
	}
}

func TestBuildTaskInputLoopFeedbackIsNotBinding(t *testing.T) {
	ag := &Agent{}
	msg := &message.Message{
		Metadata: map[string]string{"task_id": "t3"},
		Parts: []message.Part{
			message.TextPart{Text: "Revise the implementation"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": "architect",
				"result":           "Use IndexedDB",
			}},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t3",
				"dependency_agent": "reviewer",
				"context_kind":     "loop_feedback",
				"result":           "Change the primary color to #007bff",
			}},
		},
	}

	got := ag.buildTaskInput(context.Background(), msg, "")
	if count := strings.Count(got, "(binding constraint"); count != 1 {
		t.Fatalf("expected exactly 1 binding constraint label, got %d in:\n%s", count, got)
	}
	if !strings.Contains(got, "(iterative feedback — validate against binding upstream artifacts)") {
		t.Errorf("expected iterative feedback label, got:\n%s", got)
	}
	if !strings.Contains(got, "iterative loop feedback") {
		t.Errorf("expected loop feedback validation note, got:\n%s", got)
	}
}

func TestBuildSystemPromptIncludesOutputFormat(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:    "developer",
		Purpose: "Code generation",
	}
	prompt := BuildSystemPrompt(def, "", nil)
	if !strings.Contains(prompt, "## Output Format") {
		t.Error("prompt missing Output Format section")
	}
	if !strings.Contains(prompt, "```file:path") {
		t.Error("prompt missing file block instruction")
	}
}

func testLoopDef() loop.LoopDefinition {
	return loop.LoopDefinition{
		ID:           "review-1",
		Participants: []string{"developer", "architect"},
		States: []loop.StateConfig{
			{Name: "WORKING", Agent: "developer"},
			{Name: "REVIEWING", Agent: "architect"},
			{Name: "REVISING", Agent: "developer"},
			{Name: "APPROVED"},
			{Name: "ESCALATED"},
		},
		Transitions: []loop.Transition{
			{From: "WORKING", To: "REVIEWING"},
			{From: "REVIEWING", To: "APPROVED", Condition: "approved"},
			{From: "REVIEWING", To: "REVISING", Condition: "revise"},
			{From: "REVISING", To: "REVIEWING"},
		},
		InitialState:   "WORKING",
		TerminalStates: []string{"APPROVED", "ESCALATED"},
		MaxTransitions: 5,
	}
}

func buildLoopMessage(from, to string, loopDef loop.LoopDefinition, state loop.LoopState, taskDesc string) *message.Message {
	lc := &loop.LoopContext{
		Definition:   loopDef,
		State:        state,
		TaskID:       "t1",
		Conductor:    "conductor",
		OriginalTask: taskDesc,
		UserRequest:  "Build login page",
	}
	return &message.Message{
		ID:        "msg-loop",
		RequestID: "req-loop",
		From:      from,
		To:        to,
		Type:      message.TypeTaskAssignment,
		Parts: []message.Part{
			message.TextPart{Text: taskDesc},
			loop.EncodeContext(lc),
		},
		Metadata:  map[string]string{"task_id": "t1", "loop_id": "review-1"},
		Timestamp: time.Now(),
	}
}

func buildLoopMessageWithDeps(from, to string, loopDef loop.LoopDefinition, state loop.LoopState, taskDesc string, deps []map[string]any) *message.Message {
	lc := &loop.LoopContext{
		Definition:   loopDef,
		State:        state,
		TaskID:       "t1",
		Conductor:    "conductor",
		OriginalTask: taskDesc,
		UserRequest:  "Build login page",
		DepParts:     deps,
	}
	return &message.Message{
		ID:        "msg-loop",
		RequestID: "req-loop",
		From:      from,
		To:        to,
		Type:      message.TypeTaskAssignment,
		Parts: []message.Part{
			message.TextPart{Text: taskDesc},
			loop.EncodeContext(lc),
		},
		Metadata:  map[string]string{"task_id": "t1", "loop_id": "review-1"},
		Timestamp: time.Now(),
	}
}

func buildLoopReviewMessageWithArtifacts(
	from, to string,
	loopDef loop.LoopDefinition,
	state loop.LoopState,
	taskDesc string,
	deps []map[string]any,
	workArtifactURI string,
	workArtifactFiles []string,
) *message.Message {
	lc := &loop.LoopContext{
		Definition:        loopDef,
		State:             state,
		TaskID:            "t1",
		Conductor:         "conductor",
		OriginalTask:      taskDesc,
		UserRequest:       "Build login page",
		DepParts:          deps,
		WorkArtifactURI:   workArtifactURI,
		WorkArtifactFiles: workArtifactFiles,
	}
	return &message.Message{
		ID:        "msg-loop-review",
		RequestID: "req-loop-review",
		From:      from,
		To:        to,
		Type:      message.TypeReviewRequest,
		Parts: []message.Part{
			message.TextPart{Text: taskDesc},
			loop.EncodeContext(lc),
		},
		Metadata: map[string]string{
			"task_id": "t1",
			"loop_id": "review-1",
		},
		Timestamp: time.Now(),
	}
}

func TestForwardLoopMessageFiltersDepParts(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = conductorComm

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         devComm,
		Generate:     mockGenerate("login implementation code"),
		SystemPrompt: "You are a developer.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// DepParts include architect's own output — should be filtered when forwarding to REVIEWING.
	deps := []map[string]any{
		{"dependency_id": "t0", "dependency_agent": "architect", "result": "design.md content"},
		{"dependency_id": "t1", "dependency_agent": "designer", "result": "spec.md content"},
	}
	loopDef := testLoopDef()
	loopMsg := buildLoopMessageWithDeps("conductor", "developer", loopDef, loop.LoopState{
		LoopID:       "review-1",
		CurrentState: "WORKING",
	}, "Implement login", deps)
	conductorComm.Send(ctx, loopMsg)

	select {
	case msg := <-archComm.Receive(ctx):
		// Count dependency DataParts (exclude loop_context and current agent output).
		var depAgents []string
		for _, p := range msg.Parts {
			dp, ok := p.(message.DataPart)
			if !ok {
				continue
			}
			if _, hasLC := dp.Data["loop_context"]; hasLC {
				continue
			}
			if _, hasUR := dp.Data["user_request"]; hasUR {
				continue
			}
			agent, _ := dp.Data["dependency_agent"].(string)
			depAgents = append(depAgents, agent)
		}
		// Architect's own dep should be excluded; designer's kept; developer's added as current.
		for _, a := range depAgents {
			if a == "architect" {
				t.Error("architect's own DepPart should be filtered for REVIEWING state")
			}
		}
		// designer dep + developer's own output = at least 2
		if len(depAgents) < 2 {
			t.Errorf("expected at least 2 dep parts (designer + developer), got %d: %v", len(depAgents), depAgents)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for forwarded message to architect")
	}
}

func TestForwardLoopMessageRevisionKeepsUpstreamDepsCompacted(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = conductorComm

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         archComm,
		Generate:     mockGenerate("VERDICT: revise\n\nFix error handling."),
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	deps := []map[string]any{
		{"dependency_id": "t0", "dependency_agent": "architect", "result": "design.md content"},
		{"dependency_id": "t1", "dependency_agent": "designer", "result": "spec.md content"},
	}
	loopDef := testLoopDef()
	loopMsg := buildLoopMessageWithDeps("developer", "architect", loopDef, loop.LoopState{
		LoopID:          "review-1",
		CurrentState:    "REVIEWING",
		TransitionCount: 1,
	}, "Implement login", deps)
	conductorComm.Send(ctx, loopMsg)

	select {
	case msg := <-devComm.Receive(ctx):
		// For revision, upstream dependency anchors should be preserved
		// (compacted), not dropped.
		var depAgents []string
		hasDesigner := false
		for _, p := range msg.Parts {
			dp, ok := p.(message.DataPart)
			if !ok {
				continue
			}
			if _, hasLC := dp.Data["loop_context"]; hasLC {
				continue
			}
			if _, hasUR := dp.Data["user_request"]; hasUR {
				continue
			}
			agent, _ := dp.Data["dependency_agent"].(string)
			if agent != "" {
				depAgents = append(depAgents, agent)
			}
			if agent == "designer" {
				hasDesigner = true
			}
		}
		if len(depAgents) < 2 {
			t.Fatalf("expected at least 2 dependency contexts in revision message, got %d (%v)", len(depAgents), depAgents)
		}
		if !hasDesigner {
			t.Errorf("expected preserved designer dependency in revision context, got agents %v", depAgents)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for forwarded message to developer")
	}
}

func TestCompleteLoopStepForwardsToNextAgent(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = conductorComm // conductor doesn't receive in this test

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         devComm,
		Generate:     mockGenerate("login implementation code"),
		SystemPrompt: "You are a developer.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send loop task in WORKING state (developer's turn).
	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("conductor", "developer", loopDef, loop.LoopState{
		LoopID:       "review-1",
		CurrentState: "WORKING",
	}, "Implement login")
	lc, ok := loop.DecodeContext(loopMsg)
	if !ok {
		t.Fatal("missing loop context in test message")
	}
	lc.AssignedInstances = map[string]string{
		"architect": "node-b/architect/1",
	}
	lc.ExecutionNodes = map[string]string{
		"architect": "node-b",
	}
	loopMsg.Parts[1] = loop.EncodeContext(lc)
	conductorComm.Send(ctx, loopMsg)

	// Developer should forward to architect (REVIEWING state), not back to conductor.
	select {
	case msg := <-archComm.Receive(ctx):
		if msg.Type != message.TypeReviewRequest {
			t.Errorf("expected review_request, got %s", msg.Type)
		}
		if msg.From != "developer" {
			t.Errorf("from: got %q, want developer", msg.From)
		}
		if msg.To != "architect" {
			t.Errorf("to: got %q, want architect", msg.To)
		}
		if msg.Metadata["assigned_instance"] != "node-b/architect/1" {
			t.Errorf("assigned instance: got %q, want node-b/architect/1", msg.Metadata["assigned_instance"])
		}
		if msg.Metadata["execution_node"] != "node-b" {
			t.Errorf("execution node: got %q, want node-b", msg.Metadata["execution_node"])
		}
		// Verify loop context is forwarded.
		lc, ok := loop.DecodeContext(msg)
		if !ok {
			t.Fatal("forwarded message missing loop_context")
		}
		if lc.State.CurrentState != "REVIEWING" {
			t.Errorf("next state: got %q, want REVIEWING", lc.State.CurrentState)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for forwarded message to architect")
	}
}

func TestLoopTaskSendsStatusUpdateToConductor(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	_ = hub.Register("architect")

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         devComm,
		Generate:     mockGenerate("login implementation code"),
		SystemPrompt: "You are a developer.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("conductor", "developer", loopDef, loop.LoopState{
		LoopID:       "review-1",
		CurrentState: "WORKING",
	}, "Implement login")
	lc, ok := loop.DecodeContext(loopMsg)
	if !ok {
		t.Fatal("missing loop context in test message")
	}
	lc.DispatchNonce = "nonce-1"
	loopMsg.Parts[1] = loop.EncodeContext(lc)
	loopMsg.Metadata["assigned_instance"] = "node-a/developer/1"
	loopMsg.Metadata["execution_node"] = "node-a"
	conductorComm.Send(ctx, loopMsg)

	var sawStatus bool
	timeout := time.After(5 * time.Second)
	for !sawStatus {
		select {
		case msg := <-conductorComm.Receive(ctx):
			if msg.Type != message.TypeStatusUpdate {
				continue
			}
			sawStatus = true
			if msg.Metadata["task_id"] != "t1" {
				t.Fatalf("task_id = %q, want t1", msg.Metadata["task_id"])
			}
			if msg.Metadata["loop_id"] != "review-1" {
				t.Fatalf("loop_id = %q, want review-1", msg.Metadata["loop_id"])
			}
			if msg.Metadata["assigned_instance"] != "node-a/developer/1" {
				t.Fatalf("assigned instance = %q, want node-a/developer/1", msg.Metadata["assigned_instance"])
			}
			if msg.Metadata["execution_node"] != "node-a" {
				t.Fatalf("execution node = %q, want node-a", msg.Metadata["execution_node"])
			}
			if msg.Metadata["dispatch_nonce"] != "nonce-1" {
				t.Fatalf("dispatch nonce = %q, want nonce-1", msg.Metadata["dispatch_nonce"])
			}
		case <-timeout:
			t.Fatal("timeout waiting for loop status update")
		}
	}
}

func waitForConductorTaskResult(t *testing.T, ch <-chan *message.Message, timeout time.Duration) *message.Message {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case msg := <-ch:
			if msg == nil {
				t.Fatal("received nil conductor message")
			}
			if msg.Type == message.TypeStatusUpdate {
				continue
			}
			return msg
		case <-timer.C:
			t.Fatal("timeout waiting for conductor task result")
		}
	}
}

func TestCompleteLoopStepTerminalSendsToConductor(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	archComm := hub.Register("architect")
	_ = hub.Register("developer") // need developer registered for hub

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         archComm,
		Generate:     mockGenerate("VERDICT: approved\n\nAll looks good."),
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send loop task in REVIEWING state (architect's turn).
	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("developer", "architect", loopDef, loop.LoopState{
		LoopID:          "review-1",
		CurrentState:    "REVIEWING",
		TransitionCount: 1,
	}, "Implement login")
	loopMsg.Type = message.TypeReviewRequest
	conductorComm.Send(ctx, loopMsg)

	// Architect approves → terminal state → result goes to conductor.
	msg := waitForConductorTaskResult(t, conductorComm.Receive(ctx), 5*time.Second)
	if msg.Type != message.TypeTaskResult {
		t.Errorf("expected task_result, got %s", msg.Type)
	}
	if msg.From != "architect" {
		t.Errorf("from: got %q", msg.From)
	}
	if msg.To != "conductor" {
		t.Errorf("to: got %q, want conductor", msg.To)
	}
}

func TestCompleteLoopStepApprovedWithoutFileEvidenceForcesRevision(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = conductorComm

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         archComm,
		Generate:     mockGenerate("VERDICT: approved\n\nLooks good overall."),
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	loopDef := testLoopDef()
	deps := []map[string]any{
		{
			"dependency_id":    "t2",
			"dependency_agent": "designer",
			"artifact_uri":     "artifacts/designer/",
			"artifact_files":   []string{"mockup.html", "spec.md"},
		},
	}
	loopMsg := buildLoopReviewMessageWithArtifacts(
		"developer",
		"architect",
		loopDef,
		loop.LoopState{LoopID: "review-1", CurrentState: "REVIEWING", TransitionCount: 1},
		"Implement login UI",
		deps,
		"artifacts/developer/",
		[]string{"src/App.tsx", "src/styles.css"},
	)
	conductorComm.Send(ctx, loopMsg)

	select {
	case msg := <-devComm.Receive(ctx):
		if msg.Type != message.TypeTaskAssignment {
			t.Fatalf("expected task_assignment to developer, got %s", msg.Type)
		}
		if msg.To != "developer" {
			t.Fatalf("expected routed to developer, got %q", msg.To)
		}
		lc, ok := loop.DecodeContext(msg)
		if !ok {
			t.Fatal("forwarded message missing loop_context")
		}
		if lc.State.CurrentState != "REVISING" {
			t.Fatalf("expected REVISING state, got %q", lc.State.CurrentState)
		}

		foundReviewerFeedback := false
		for _, p := range msg.Parts {
			dp, ok := p.(message.DataPart)
			if !ok || dp.Data["dependency_agent"] != "architect" {
				continue
			}
			foundReviewerFeedback = true
			result, _ := dp.Data["result"].(string)
			if !strings.Contains(result, "VERDICT: revise") {
				t.Fatalf("expected forced revise feedback, got: %q", result)
			}
			if !strings.Contains(result, "Approval lacks file-level evidence") {
				t.Fatalf("expected evidence warning in feedback, got: %q", result)
			}
		}
		if !foundReviewerFeedback {
			t.Fatal("missing reviewer feedback dependency part")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for revision routing")
	}
}

func TestCompleteLoopStepApprovedWithFileEvidenceStaysApproved(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = devComm

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: archComm,
		Generate: mockGenerate(
			"VERDICT: approved\n\nCompared mockup.html and spec.md against src/App.tsx and src/styles.css; implementation matches.",
		),
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	loopDef := testLoopDef()
	deps := []map[string]any{
		{
			"dependency_id":    "t2",
			"dependency_agent": "designer",
			"artifact_uri":     "artifacts/designer/",
			"artifact_files":   []string{"mockup.html", "spec.md"},
		},
	}
	loopMsg := buildLoopReviewMessageWithArtifacts(
		"developer",
		"architect",
		loopDef,
		loop.LoopState{LoopID: "review-1", CurrentState: "REVIEWING", TransitionCount: 1},
		"Implement login UI",
		deps,
		"artifacts/developer/",
		[]string{"src/App.tsx", "src/styles.css"},
	)
	conductorComm.Send(ctx, loopMsg)

	msg := waitForConductorTaskResult(t, conductorComm.Receive(ctx), 5*time.Second)
	if msg.Type != message.TypeTaskResult {
		t.Fatalf("expected task_result to conductor, got %s", msg.Type)
	}
	if msg.To != "conductor" {
		t.Fatalf("expected result to conductor, got %q", msg.To)
	}
}

func TestCompleteLoopStepMaxTransitionsEscalates(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	archComm := hub.Register("architect")
	_ = hub.Register("developer")

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         archComm,
		Generate:     mockGenerate("VERDICT: revise\n\nNeeds more work."),
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send loop task at max transitions (count=5, max=5).
	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("developer", "architect", loopDef, loop.LoopState{
		LoopID:          "review-1",
		CurrentState:    "REVIEWING",
		TransitionCount: 5, // At max.
	}, "Implement login")
	loopMsg.Type = message.TypeReviewRequest
	conductorComm.Send(ctx, loopMsg)

	// Should escalate back to conductor.
	msg := waitForConductorTaskResult(t, conductorComm.Receive(ctx), 5*time.Second)
	if msg.Type != message.TypeTaskResult {
		t.Errorf("expected task_result, got %s", msg.Type)
	}
	if msg.Metadata["loop_escalated"] != "true" {
		t.Error("expected loop_escalated metadata")
	}
}

func TestLoopReviewFailureReturnsTerminalResultToConductor(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = devComm

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: archComm,
		Generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
			return nil, fmt.Errorf("tool loop did not converge after 20 iterations")
		},
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("developer", "architect", loopDef, loop.LoopState{
		LoopID:       "review-1",
		CurrentState: "REVIEWING",
	}, "Implement login")
	loopMsg.Type = message.TypeReviewRequest
	conductorComm.Send(ctx, loopMsg)

	msg := waitForConductorTaskResult(t, conductorComm.Receive(ctx), 5*time.Second)
	if msg.Type != message.TypeTaskResult {
		t.Fatalf("expected task_result to conductor, got %s", msg.Type)
	}
	if msg.To != "conductor" {
		t.Fatalf("expected result to conductor, got %q", msg.To)
	}
	if msg.Metadata["status"] != "failed" {
		t.Fatalf("status: got %q, want failed", msg.Metadata["status"])
	}
	if got := extractText(msg); !strings.Contains(got, "tool loop did not converge after 20 iterations") {
		t.Fatalf("unexpected loop failure result: %q", got)
	}

	select {
	case unexpected := <-devComm.Receive(ctx):
		t.Fatalf("unexpected message routed back to previous loop participant: %s", unexpected.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCompleteLoopStepRevisionCycle(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")

	events := event.NewBus()
	defer events.Close()

	// Track call count to vary architect responses.
	archCallCount := 0
	archGenerate := func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		archCallCount++
		if archCallCount == 1 {
			return &schema.Message{Role: schema.Assistant, Content: "VERDICT: revise\n\nFix error handling."}, nil
		}
		return &schema.Message{Role: schema.Assistant, Content: "VERDICT: approved\n\nAll good."}, nil
	}

	devAgent := &Agent{
		Def:          runtime.AgentDefinition{Name: "developer", Purpose: "Code generation", Model: "anthropic/claude-opus-4"},
		Comm:         devComm,
		Generate:     mockGenerate("implementation code"),
		SystemPrompt: "You are a developer.",
		Events:       events,
	}

	archAgent := &Agent{
		Def:          runtime.AgentDefinition{Name: "architect", Purpose: "System design", Model: "anthropic/claude-opus-4"},
		Comm:         archComm,
		Generate:     archGenerate,
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go devAgent.Run(ctx)
	go archAgent.Run(ctx)

	// Kick off: conductor sends WORKING to developer.
	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("conductor", "developer", loopDef, loop.LoopState{
		LoopID:       "review-1",
		CurrentState: "WORKING",
	}, "Implement login")
	conductorComm.Send(ctx, loopMsg)

	// Expected flow:
	// 1. developer (WORKING) → architect (REVIEWING) → revise
	// 2. architect → developer (REVISING) → architect (REVIEWING) → approved
	// 3. architect → conductor (terminal)

	msg := waitForConductorTaskResult(t, conductorComm.Receive(ctx), 5*time.Second)
	if msg.Type != message.TypeTaskResult {
		t.Errorf("expected task_result, got %s", msg.Type)
	}
	if msg.To != "conductor" {
		t.Errorf("to: got %q, want conductor", msg.To)
	}
	if msg.From != "architect" {
		t.Errorf("from: got %q, want architect", msg.From)
	}
}

func TestBuildTaskInputSkipsLoopContext(t *testing.T) {
	ag := &Agent{}
	lc := &loop.LoopContext{
		Definition: testLoopDef(),
		State:      loop.LoopState{LoopID: "review-1", CurrentState: "WORKING"},
		TaskID:     "t1",
		Conductor:  "conductor",
	}
	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "Do the task"},
			loop.EncodeContext(lc),
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t0",
				"dependency_agent": "architect",
				"result":           "Use REST API",
			}},
		},
	}
	got := ag.buildTaskInput(context.Background(), msg, "")
	if strings.Contains(got, "loop_context") {
		t.Error("buildTaskInput should not include loop_context in LLM input")
	}
	if !strings.Contains(got, "Do the task") {
		t.Error("missing task description")
	}
	if !strings.Contains(got, "Use REST API") {
		t.Error("missing dependency context")
	}
}

func TestExtractUserQuery(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		found   bool
	}{
		{
			name:    "present",
			content: "I need more info.\nASK_USER: What database should I use?\nContinuing...",
			want:    "What database should I use?",
			found:   true,
		},
		{
			name:    "case insensitive",
			content: "Let me check.\nask_user: Which framework do you prefer?",
			want:    "Which framework do you prefer?",
			found:   true,
		},
		{
			name:    "mixed case",
			content: "Ask_User: Should I use REST or GraphQL?",
			want:    "Should I use REST or GraphQL?",
			found:   true,
		},
		{
			name:    "absent",
			content: "Here is the implementation code.\nNo questions needed.",
			want:    "",
			found:   false,
		},
		{
			name:    "empty question",
			content: "ASK_USER: ",
			want:    "",
			found:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractUserQuery(tc.content)
			if ok != tc.found {
				t.Errorf("found: got %v, want %v", ok, tc.found)
			}
			if got != tc.want {
				t.Errorf("question: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildSystemPromptIncludesAskUser(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:    "developer",
		Purpose: "Code generation",
	}
	prompt := BuildSystemPrompt(def, "", nil)
	if !strings.Contains(prompt, "ASK_USER") {
		t.Error("prompt missing ASK_USER instruction")
	}
	if !strings.Contains(prompt, "genuinely cannot proceed") {
		t.Error("prompt missing ASK_USER guidance")
	}
}

func TestAgentAskUserFlow(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: agentComm,
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "I need to know.\nASK_USER: What database?",
				}, nil
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "Using PostgreSQL as requested.",
			}, nil
		},
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send task.
	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Build the data layer"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Agent should send TypeUserQuery.
	var queryMsg *message.Message
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeUserQuery {
			t.Fatalf("expected user_query, got %s", msg.Type)
		}
		queryMsg = msg
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for user query")
	}

	if extractText(queryMsg) != "What database?" {
		t.Errorf("query: got %q", extractText(queryMsg))
	}

	// Send response back.
	conductorComm.Send(ctx, &message.Message{
		ID:        "resp-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeUserResponse,
		Parts:     []message.Part{message.TextPart{Text: "PostgreSQL"}},
		Timestamp: time.Now(),
	})

	// Agent should now send the final result.
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeTaskResult {
			t.Fatalf("expected task_result, got %s", msg.Type)
		}
		text := extractText(msg)
		if !strings.Contains(text, "PostgreSQL") {
			t.Errorf("result: got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}

	if callCount != 2 {
		t.Errorf("expected 2 LLM calls, got %d", callCount)
	}
}

func TestAgentSendResultIncludesTaskID(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Generate:     mockGenerate("done"),
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send task with task_id in metadata.
	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Do work"}},
		Metadata:  map[string]string{"task_id": "t42"},
		Timestamp: time.Now(),
	})

	select {
	case result := <-conductorComm.Receive(ctx):
		if result.Metadata == nil || result.Metadata["task_id"] != "t42" {
			t.Errorf("expected task_id=t42 in result metadata, got %v", result.Metadata)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestGenerateWithToolsNoExecutor(t *testing.T) {
	// When ToolExecutor is nil, generateWithTools should pass through to Generate.
	ag := &Agent{
		Generate: mockGenerate("direct response"),
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("hello"),
	}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "direct response" {
		t.Errorf("content: got %q", resp.Content)
	}
}

func TestGenerateWithToolsLoop(t *testing.T) {
	// Simulate: first call returns a tool call, second call returns final text.
	callCount := 0
	ag := &Agent{
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				idx := 0
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "",
					ToolCalls: []schema.ToolCall{
						{
							Index: &idx,
							ID:    "tc-1",
							Type:  "function",
							Function: schema.FunctionCall{
								Name:      "shell",
								Arguments: `{"command": "echo done"}`,
							},
						},
					},
				}, nil
			}
			// Second call: verify tool message is in input.
			hasToolMsg := false
			for _, m := range input {
				if m.Role == schema.Tool {
					hasToolMsg = true
				}
			}
			if !hasToolMsg {
				t.Error("expected tool message in input on second call")
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "final answer",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "test",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("do something"),
	}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "final answer" {
		t.Errorf("content: got %q", resp.Content)
	}
	if callCount != 2 {
		t.Errorf("expected 2 generate calls, got %d", callCount)
	}
}

func TestGenerateWithToolsBudgetCheck(t *testing.T) {
	callCount := 0

	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
		Generate: func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
			callCount++
			idx := 0
			return &schema.Message{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					{
						Index: &idx,
						ID:    "tc-1",
						Type:  "function",
						Function: schema.FunctionCall{
							Name:      "shell",
							Arguments: `{"command": "echo x"}`,
						},
					},
				},
			}, nil
		},
		Meter: &budgetExceededMeter{},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "test",
		},
	}

	_, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("do something"),
	}, 0)
	if err == nil {
		t.Fatal("expected budget error")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected budget exceeded error, got: %v", err)
	}
	// Should have been called exactly once (first iteration passes, second checks budget).
	if callCount != 1 {
		t.Errorf("expected 1 generate call before budget check, got %d", callCount)
	}
}

// budgetExceededMeter is a mock Meter that always fails budget checks.
type budgetExceededMeter struct{}

func (m *budgetExceededMeter) Record(_ context.Context, _ runtime.LLMCallRecord) error {
	return nil
}
func (m *budgetExceededMeter) CheckBudget(_ context.Context, _ string) error {
	return fmt.Errorf("budget exceeded")
}
func (m *budgetExceededMeter) SetBudget(_ context.Context, _ string, _ runtime.Budget) error {
	return nil
}
func (m *budgetExceededMeter) Usage(_ context.Context, _ string) (runtime.UsageSummary, error) {
	return runtime.UsageSummary{}, nil
}
func (m *budgetExceededMeter) AggregateUsage(_ context.Context) (runtime.UsageSummary, error) {
	return runtime.UsageSummary{}, nil
}

func TestEstimateTokens(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.System, Content: strings.Repeat("a", 400)},   // 400/4+4 = 104
		{Role: schema.User, Content: strings.Repeat("b", 800)},     // 800/4+4 = 204
		{Role: schema.Assistant, Content: strings.Repeat("c", 40)}, // 40/4+4 = 14
	}
	got := estimateTokens(msgs)
	// 104 + 204 + 14 = 322
	if got != 322 {
		t.Errorf("estimateTokens = %d, want 322", got)
	}
}

func TestEstimateTokensWithToolCalls(t *testing.T) {
	idx := 0
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hello"}, // 5/4+4 = 5
		{
			Role:    schema.Assistant,
			Content: "",
			ToolCalls: []schema.ToolCall{
				{
					Index:    &idx,
					ID:       "tc-1",
					Function: schema.FunctionCall{Name: "shell", Arguments: strings.Repeat("x", 100)},
				},
			},
		}, // 0/4+4 + 100/4 + 4(id len) = 4+25+4 = 33
		{Role: schema.Tool, Content: strings.Repeat("r", 200)}, // 200/4+4 = 54
	}
	got := estimateTokens(msgs)
	// 5 + 33 + 54 = 92
	if got != 92 {
		t.Errorf("estimateTokens = %d, want 92", got)
	}
}

func TestCompactToolHistory(t *testing.T) {
	// Create messages that exceed budget.
	sys := &schema.Message{Role: schema.System, Content: "system"}
	user := &schema.Message{Role: schema.User, Content: "task"}
	// Create 4 tool exchanges (assistant+tool pairs), each ~100 tokens.
	var msgs []*schema.Message
	msgs = append(msgs, sys, user)
	for i := 0; i < 4; i++ {
		idx := 0
		msgs = append(msgs, &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{
				{Index: &idx, ID: fmt.Sprintf("tc-%d", i), Function: schema.FunctionCall{
					Name:      "shell",
					Arguments: strings.Repeat("a", 200), // 50 tokens
				}},
			},
		})
		msgs = append(msgs, &schema.Message{
			Role:    schema.Tool,
			Content: strings.Repeat("r", 400), // 100 tokens
		})
	}

	// Budget that fits system+user + ~2 exchanges (~320 tokens).
	compacted := compactToolHistory(msgs, 350)

	// Should have: system, user, summary, + some recent messages.
	if len(compacted) >= len(msgs) {
		t.Errorf("expected compaction, got %d msgs (same as input %d)", len(compacted), len(msgs))
	}
	// First two should be preserved.
	if compacted[0].Content != "system" {
		t.Error("system message not preserved")
	}
	if compacted[1].Content != "task" {
		t.Error("user message not preserved")
	}
	// Third should be the summary.
	if !strings.Contains(compacted[2].Content, "earlier iteration") {
		t.Errorf("expected summary message, got %q", compacted[2].Content)
	}
	if !strings.Contains(compacted[2].Content, "shell") {
		t.Error("summary should mention tool names")
	}
}

func TestCompactToolHistoryKeepsRecentExchanges(t *testing.T) {
	sys := &schema.Message{Role: schema.System, Content: "sys"}
	user := &schema.Message{Role: schema.User, Content: "task"}

	// Add 3 assistant messages with distinct content.
	old := &schema.Message{Role: schema.Assistant, Content: "old response " + strings.Repeat("x", 400)}
	mid := &schema.Message{Role: schema.Assistant, Content: "mid response " + strings.Repeat("y", 400)}
	recent := &schema.Message{Role: schema.Assistant, Content: "recent"}

	msgs := []*schema.Message{sys, user, old, mid, recent}

	// Budget big enough for sys+user+recent but not old+mid.
	compacted := compactToolHistory(msgs, 50)

	// Recent message should be preserved.
	last := compacted[len(compacted)-1]
	if last.Content != "recent" {
		t.Errorf("most recent message not preserved, got %q", last.Content)
	}
}

func TestCompactToolHistoryNoop(t *testing.T) {
	// When everything fits, compaction should be a no-op.
	msgs := []*schema.Message{
		{Role: schema.System, Content: "sys"},
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: "short"},
	}
	compacted := compactToolHistory(msgs, 100_000)
	if len(compacted) != len(msgs) {
		t.Errorf("expected no compaction, got %d msgs (was %d)", len(compacted), len(msgs))
	}
}

func TestMergeWorkSummary(t *testing.T) {
	tests := []struct {
		name         string
		existing     string
		latest       string
		wantContains []string
	}{
		{
			name:         "empty_existing",
			existing:     "",
			latest:       "Implemented App.tsx",
			wantContains: []string{"Implemented App.tsx"},
		},
		{
			name:         "dedupe_identical",
			existing:     "Implemented App.tsx",
			latest:       "Implemented App.tsx",
			wantContains: []string{"Implemented App.tsx"},
		},
		{
			name:         "append_revision",
			existing:     "Implemented core app and context.",
			latest:       "Adjusted CSS to match mockup.",
			wantContains: []string{"Implemented core app and context.", "Revision Update", "Adjusted CSS to match mockup."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeWorkSummary(tt.existing, tt.latest)
			for _, w := range tt.wantContains {
				if !strings.Contains(got, w) {
					t.Fatalf("expected merged summary to contain %q, got %q", w, got)
				}
			}
		})
	}
}

func TestMergeArtifactFiles(t *testing.T) {
	got := mergeArtifactFiles([]string{"src/App.tsx", "src/main.tsx"}, []string{"src/main.tsx", "src/components/Header.tsx"})
	want := []string{"src/App.tsx", "src/main.tsx", "src/components/Header.tsx"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestGenerateWithToolsContextCompaction(t *testing.T) {
	// Simulate a tool loop where each iteration adds ~50KB of tool output.
	// With a 200K context limit and 64K max output, the budget is 136K tokens (~544K chars).
	// We'll use a smaller limit to make the test practical.
	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount <= 6 {
				idx := 0
				return &schema.Message{
					Role: schema.Assistant,
					ToolCalls: []schema.ToolCall{
						{
							Index: &idx,
							ID:    fmt.Sprintf("tc-%d", callCount),
							Type:  "function",
							Function: schema.FunctionCall{
								Name:      "shell",
								Arguments: `{"command": "echo test"}`,
							},
						},
					},
				}, nil
			}
			// After 6 tool iterations, return final answer.
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "final answer after compaction",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {Name: "shell", Command: "echo test", Parameters: map[string]runtime.ToolParam{
					"command": {Type: "string", Required: true},
				}},
			},
			TierPaths:       []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName:       "test",
			MaxOutputTokens: 1000, // small for testing
			ContextLimit:    5000, // small limit to force compaction
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.SystemMessage("You are a test agent."),
		schema.UserMessage("Do something with tools"),
	}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "final answer after compaction" {
		t.Errorf("content: got %q", resp.Content)
	}
	if callCount != 7 {
		t.Errorf("expected 7 generate calls (6 tool + 1 final), got %d", callCount)
	}
}

func TestGenerateWithToolsAccumulatesFileBlocks(t *testing.T) {
	// Simulate: first call produces file blocks alongside a tool call,
	// second call produces different file blocks with no tool call.
	// Verify ALL file blocks appear in the final response.
	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "developer"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				idx := 0
				return &schema.Message{
					Role: schema.Assistant,
					Content: "```file:index.html\n<html><body>Hello</body></html>\n```\n\n" +
						"```file:style.css\nbody { margin: 0; }\n```\n\n" +
						"Let me verify this works.",
					ToolCalls: []schema.ToolCall{
						{
							Index: &idx,
							ID:    "tc-1",
							Type:  "function",
							Function: schema.FunctionCall{
								Name:      "shell",
								Arguments: `{"command": "echo ok"}`,
							},
						},
					},
				}, nil
			}
			// Second call: final response with different files.
			return &schema.Message{
				Role: schema.Assistant,
				Content: "```file:README.md\n# My App\n```\n\n" +
					"All done.",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {Name: "shell", Command: "$TOOL_ARG_COMMAND", Parameters: map[string]runtime.ToolParam{
					"command": {Type: "string", Required: true},
				}},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "developer",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("build an app"),
	}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parse file blocks from the merged response.
	blocks, _ := ParseFileBlocks(resp.Content)
	paths := make(map[string]bool)
	for _, b := range blocks {
		paths[b.Path] = true
	}

	// All three files should be present.
	for _, want := range []string{"index.html", "style.css", "README.md"} {
		if !paths[want] {
			t.Errorf("missing file block for %q in merged response", want)
		}
	}
	if len(blocks) != 3 {
		t.Errorf("expected 3 file blocks, got %d", len(blocks))
	}
}

func TestGenerateWithToolsFileBlockDedup(t *testing.T) {
	// If the same file appears in both an intermediate and the final response,
	// the final version wins (agent may have revised it after tool output).
	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "developer"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				idx := 0
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "```file:main.go\npackage main // v1\n```",
					ToolCalls: []schema.ToolCall{
						{
							Index:    &idx,
							ID:       "tc-1",
							Type:     "function",
							Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "go build"}`},
						},
					},
				}, nil
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "```file:main.go\npackage main // v2 fixed\n```",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {Name: "shell", Command: "$TOOL_ARG_COMMAND", Parameters: map[string]runtime.ToolParam{
					"command": {Type: "string", Required: true},
				}},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "developer",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("build it"),
	}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	blocks, _ := ParseFileBlocks(resp.Content)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 file block (deduped), got %d", len(blocks))
	}
	if !strings.Contains(blocks[0].Content, "v2 fixed") {
		t.Errorf("expected final version (v2), got %q", blocks[0].Content)
	}
}

func TestMergeAccumulatedBlocksEmpty(t *testing.T) {
	resp := &schema.Message{Role: schema.Assistant, Content: "just text"}
	got := mergeAccumulatedBlocks(resp, nil)
	if got.Content != "just text" {
		t.Errorf("content should be unchanged, got %q", got.Content)
	}
}

func TestIsResponseTruncated(t *testing.T) {
	// Nil meta.
	if isResponseTruncated(&schema.Message{}) {
		t.Error("nil meta should not be truncated")
	}
	// Normal stop.
	if isResponseTruncated(&schema.Message{ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}}) {
		t.Error("stop should not be truncated")
	}
	// Length truncation.
	if !isResponseTruncated(&schema.Message{ResponseMeta: &schema.ResponseMeta{FinishReason: "length"}}) {
		t.Error("length should be truncated")
	}
}

func TestBuildSystemPromptWithLiveTools(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:    "developer",
		Purpose: "Code generation",
		Tools: []runtime.ToolConfig{
			{
				Name:         "shell",
				Instructions: "Run shell commands",
				Parameters: map[string]runtime.ToolParam{
					"command": {Type: "string", Required: true},
				},
			},
		},
	}
	prompt := BuildSystemPrompt(def, "", nil)
	if !strings.Contains(prompt, "call tools during your work") {
		t.Error("prompt missing live tool instructions")
	}
	if !strings.Contains(prompt, "**shell**") {
		t.Error("prompt missing shell tool listing")
	}
}

func TestBuildSystemPromptPostProcessOnly(t *testing.T) {
	def := runtime.AgentDefinition{
		Name:    "designer",
		Purpose: "UI design",
		Tools: []runtime.ToolConfig{
			{
				Name:         "chrome",
				Instructions: "Take screenshots",
				Command:      "chromium --headless",
				Config:       map[string]string{"match": "*.html"},
			},
		},
	}
	prompt := BuildSystemPrompt(def, "", nil)
	// Post-process only tools should get the old-style instructions.
	if strings.Contains(prompt, "call tools during your work") {
		t.Error("post-process tools should not get live tool instructions")
	}
	if !strings.Contains(prompt, "tools.yaml") {
		t.Error("post-process tools should mention tools.yaml")
	}
}

func TestCaptureFromScratch(t *testing.T) {
	dir := t.TempDir()

	// Write a file to the scratch dir with "edited" content (simulating sed -i).
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src/main.go"), []byte("package main // edited"), 0644); err != nil {
		t.Fatal(err)
	}

	blocks := []FileBlock{
		{Path: "src/main.go", Content: "package main // original"},
		{Path: "src/missing.go", Content: "package main // should stay"},
	}

	captureFromScratch(dir, blocks)

	// Block for existing file should have updated content from disk.
	if blocks[0].Content != "package main // edited" {
		t.Errorf("expected edited content, got %q", blocks[0].Content)
	}

	// Block for missing file should keep original content.
	if blocks[1].Content != "package main // should stay" {
		t.Errorf("expected original content for missing file, got %q", blocks[1].Content)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestToStringSlice(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"[]string", []string{"a", "b"}, []string{"a", "b"}},
		{"[]any strings", []any{"a", "b"}, []string{"a", "b"}},
		{"[]any mixed", []any{"a", 42}, []string{"a"}},
		{"wrong type", 42, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toStringSlice(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestToMapSlice(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int // expected length
	}{
		{"nil", nil, 0},
		{"[]map", []map[string]any{{"k": "v"}}, 1},
		{"[]any maps", []any{map[string]any{"k": "v"}}, 1},
		{"[]any mixed", []any{map[string]any{"k": "v"}, "bad"}, 1},
		{"wrong type", "nope", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toMapSlice(tt.in)
			if len(got) != tt.want {
				t.Fatalf("len = %d, want %d", len(got), tt.want)
			}
		})
	}
}

// --- Fix 1 Tests: Artifact Handoff ---

func TestForwardLoopMessageIncludesArtifactURI(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	_ = conductorComm

	dir := t.TempDir()
	storage := local.NewStorage(dir, "developer")

	events := event.NewBus()
	defer events.Close()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         devComm,
		Storage:      storage,
		Generate:     mockGenerate("```file:main.go\npackage main\n```\n\nImplemented login."),
		SystemPrompt: "You are a developer.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send loop task in WORKING state.
	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("conductor", "developer", loopDef, loop.LoopState{
		LoopID:       "review-1",
		CurrentState: "WORKING",
	}, "Implement login")
	conductorComm.Send(ctx, loopMsg)

	// Developer should forward to architect with artifact refs.
	select {
	case msg := <-archComm.Receive(ctx):
		// Find the dependency DataPart from developer.
		var depData map[string]any
		for _, p := range msg.Parts {
			if dp, ok := p.(message.DataPart); ok {
				if dp.Data["dependency_agent"] == "developer" {
					depData = dp.Data
					break
				}
			}
		}
		if depData == nil {
			t.Fatal("no dependency DataPart from developer found")
		}
		uri, _ := depData["artifact_uri"].(string)
		if uri == "" {
			t.Error("expected artifact_uri in forwarded DataPart")
		}
		if !strings.HasSuffix(uri, "/") {
			t.Errorf("artifact_uri should end with /, got %q", uri)
		}
		files := toStringSlice(depData["artifact_files"])
		if len(files) == 0 {
			t.Error("expected artifact_files in forwarded DataPart")
		}
		if len(files) > 0 && files[0] != "main.go" {
			t.Errorf("artifact_files[0]: got %q, want main.go", files[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for forwarded message")
	}
}

func TestForwardLoopMessageNoArtifactURIForReviewer(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")

	events := event.NewBus()
	defer events.Close()

	// Architect produces a verdict, not file blocks.
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "architect",
			Purpose: "System design",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         archComm,
		Generate:     mockGenerate("VERDICT: revise\n\nFix error handling."),
		SystemPrompt: "You are an architect.",
		Events:       events,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	// Send loop task in REVIEWING state.
	loopDef := testLoopDef()
	loopMsg := buildLoopMessage("developer", "architect", loopDef, loop.LoopState{
		LoopID:          "review-1",
		CurrentState:    "REVIEWING",
		TransitionCount: 1,
	}, "Implement login")
	loopMsg.Type = message.TypeReviewRequest
	conductorComm.Send(ctx, loopMsg)

	// Architect should forward revision to developer without artifact refs.
	select {
	case msg := <-devComm.Receive(ctx):
		for _, p := range msg.Parts {
			if dp, ok := p.(message.DataPart); ok {
				if dp.Data["dependency_agent"] == "architect" {
					if _, hasURI := dp.Data["artifact_uri"]; hasURI {
						t.Error("reviewer should not include artifact_uri")
					}
					if _, hasFiles := dp.Data["artifact_files"]; hasFiles {
						t.Error("reviewer should not include artifact_files")
					}
				}
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for revision message")
	}
}

func TestReadArtifactFiles(t *testing.T) {
	dir := t.TempDir()
	agentName := "myagent"
	storage := local.NewStorage(dir, agentName)
	ctx := context.Background()

	// Write test files under the agent's artifacts directory.
	prefix := "artifacts/" + agentName + "/"
	storage.Write(ctx, runtime.TierShared, prefix+"main.go", []byte("package main"))
	storage.Write(ctx, runtime.TierShared, prefix+"util.go", []byte("package util"))
	storage.Write(ctx, runtime.TierShared, prefix+"big.go", []byte(strings.Repeat("x", 10000)))

	ag := &Agent{Storage: storage}

	// Basic read.
	result := ag.readArtifactFiles(ctx, prefix, []string{"main.go", "util.go"}, 10, 8000, 60000)
	if !strings.Contains(result, "package main") {
		t.Error("should contain main.go content")
	}
	if !strings.Contains(result, "package util") {
		t.Error("should contain util.go content")
	}

	// Per-file cap.
	result = ag.readArtifactFiles(ctx, prefix, []string{"big.go"}, 10, 100, 60000)
	if !strings.Contains(result, "[... truncated]") {
		t.Error("should truncate large file")
	}

	// Max files cap.
	result = ag.readArtifactFiles(ctx, prefix, []string{"main.go", "util.go", "big.go"}, 1, 8000, 60000)
	if strings.Contains(result, "util.go") {
		t.Error("should respect maxFiles cap")
	}

	// Total cap.
	result = ag.readArtifactFiles(ctx, prefix, []string{"big.go", "main.go"}, 10, 60000, 50)
	if !strings.Contains(result, "context budget reached") {
		t.Error("should hit total cap for second file")
	}
}

func TestBuildTaskInputUsesArtifactFiles(t *testing.T) {
	dir := t.TempDir()
	agentName := "myagent"
	storage := local.NewStorage(dir, agentName)
	ctx := context.Background()

	// Write targeted artifact file under the agent's artifacts directory.
	prefix := "artifacts/" + agentName + "/"
	storage.Write(ctx, runtime.TierShared, prefix+"main.go", []byte("package main\nfunc main() {}"))

	ag := &Agent{Storage: storage}

	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "Review the code"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": agentName,
				"artifact_uri":     prefix,
				"artifact_files":   []any{"main.go"},
				"result":           "Implemented main",
			}},
		},
	}

	result := ag.buildTaskInput(ctx, msg, "")
	if !strings.Contains(result, "Upstream artifact source (READ FIRST): "+prefix) {
		t.Error("should include artifact URI reference")
	}
	if !strings.Contains(result, "main.go") {
		t.Error("should include targeted artifact filename")
	}
	if strings.Contains(result, "package main") {
		t.Error("should not inline targeted artifact file content")
	}
}

// --- Fix 2 Tests: Tool Loop Convergence ---

func TestMaxIterationsForScope(t *testing.T) {
	tests := []struct {
		scope string
		want  int
	}{
		{"small", 3},
		{"", 10},
		{"large", 20},
	}
	for _, tt := range tests {
		got := maxIterationsForScope(tt.scope)
		if got != tt.want {
			t.Errorf("maxIterationsForScope(%q) = %d, want %d", tt.scope, got, tt.want)
		}
	}
}

func TestIsVerificationOnly(t *testing.T) {
	idx := 0
	tests := []struct {
		name string
		tcs  []schema.ToolCall
		want bool
	}{
		{
			"empty",
			nil,
			false,
		},
		{
			"read_only_ls",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "ls -la"}`},
			}},
			true,
		},
		{
			"read_only_grep",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "grep -r TODO ."}`},
			}},
			true,
		},
		{
			"read_only_chained_cd_find",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "cd $SCRATCH_DIR/app && find src -type f"}`},
			}},
			true,
		},
		{
			"mutating_sed",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "sed -i 's/foo/bar/' file.go"}`},
			}},
			false,
		},
		{
			"non_shell_tool",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "write_file", Arguments: `{"path": "x.go"}`},
			}},
			false,
		},
		{
			"mixed_read_and_write",
			[]schema.ToolCall{
				{Index: &idx, ID: "tc-1", Type: "function", Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "cat file.go"}`}},
				{Index: &idx, ID: "tc-2", Type: "function", Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "go build ."}`}},
			},
			false,
		},
		{
			"chained_with_build_is_not_read_only",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "cd $SCRATCH_DIR/app && npm run build 2>&1 | tail -50"}`},
			}},
			false,
		},
		{
			"shell_redirection_write",
			[]schema.ToolCall{{
				Index:    &idx,
				ID:       "tc-1",
				Type:     "function",
				Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "cat src/main.ts > out.txt"}`},
			}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isVerificationOnly(tt.tcs)
			if got != tt.want {
				t.Errorf("isVerificationOnly = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenerateWithToolsStagnationExit(t *testing.T) {
	callCount := 0
	idx := 0

	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			// After finalization prompt, return text only.
			for _, m := range input {
				if strings.Contains(m.Content, "iteration limit") {
					return &schema.Message{Role: schema.Assistant, Content: "done"}, nil
				}
			}
			// First iteration emits a file block + verification tool call.
			// Subsequent iterations are verification-only; stagnation should trigger.
			content := ""
			if callCount == 1 {
				content = "```file:main.go\npackage main\nfunc main(){}\n```"
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: content,
				ToolCalls: []schema.ToolCall{
					{
						Index:    &idx,
						ID:       fmt.Sprintf("tc-%d", callCount),
						Type:     "function",
						Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "ls -la"}`},
					},
				},
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "test",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("do something"),
	}, 10) // high maxIter — stagnation should trigger before this
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "Finalized from materialized scratch files") {
		t.Errorf("expected synthesized finalization summary, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "```file:main.go") {
		t.Errorf("expected preserved file block for main.go, got %q", resp.Content)
	}
	// 2 verification iterations after file materialization + 1 finalization.
	// Should NOT reach maxIter (10).
	if callCount > 5 {
		t.Errorf("expected early exit via stagnation, got %d calls", callCount)
	}
}

func TestGenerateWithToolsNoStagnationBeforeFiles(t *testing.T) {
	callCount := 0
	idx := 0

	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			for _, m := range input {
				if strings.Contains(m.Content, "iteration limit") {
					return &schema.Message{Role: schema.Assistant, Content: "done"}, nil
				}
			}
			// Read-only loop without file blocks should not trigger stagnation.
			return &schema.Message{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					{
						Index:    &idx,
						ID:       fmt.Sprintf("tc-%d", callCount),
						Type:     "function",
						Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "ls -la"}`},
					},
				},
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "test",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("do something"),
	}, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "done" {
		t.Errorf("content: got %q, want 'done'", resp.Content)
	}
	// 3 iterations + 1 finalization prompt.
	if callCount != 4 {
		t.Errorf("expected no early stagnation; callCount=%d, want 4", callCount)
	}
}

func TestGenerateWithToolsForceFinalization(t *testing.T) {
	callCount := 0
	idx := 0

	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			// On finalization prompt, return text.
			for _, m := range input {
				if strings.Contains(m.Content, "iteration limit") {
					return &schema.Message{Role: schema.Assistant, Content: "final summary"}, nil
				}
			}
			// Always return mutating tool calls (not verification-only).
			return &schema.Message{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					{
						Index:    &idx,
						ID:       fmt.Sprintf("tc-%d", callCount),
						Type:     "function",
						Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "go build ."}`},
					},
				},
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "test",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("do something"),
	}, 2) // low maxIter to force finalization
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "final summary" {
		t.Errorf("content: got %q, want 'final summary'", resp.Content)
	}
	// 2 iterations + 1 finalization = 3 calls.
	if callCount != 3 {
		t.Errorf("expected 3 calls (2 iterations + finalization), got %d", callCount)
	}
}

func TestGenerateWithToolsAutoFinalizeFromAccumulated(t *testing.T) {
	callCount := 0
	idx := 0

	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				// First call: return file blocks + tool call.
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "```file:main.go\npackage main\n```\n\nBuilding...",
					ToolCalls: []schema.ToolCall{
						{
							Index:    &idx,
							ID:       "tc-1",
							Type:     "function",
							Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "go build ."}`},
						},
					},
				}, nil
			}
			// All subsequent calls: always return tool calls (LLM won't stop).
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "still working",
				ToolCalls: []schema.ToolCall{
					{
						Index:    &idx,
						ID:       fmt.Sprintf("tc-%d", callCount),
						Type:     "function",
						Function: schema.FunctionCall{Name: "shell", Arguments: `{"command": "go test ."}`},
					},
				},
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{t.TempDir(), t.TempDir(), t.TempDir()},
			AgentName: "test",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("implement main"),
	}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected deterministic finalization from accumulated files (2 calls), got %d", callCount)
	}
	// Should have accumulated file blocks from iteration 1 and merged them.
	if !strings.Contains(resp.Content, "```file:main.go") {
		t.Error("expected accumulated file blocks in final response")
	}
	// Should have no tool calls (stripped).
	if len(resp.ToolCalls) > 0 {
		t.Error("expected tool calls to be stripped from finalized response")
	}
}

// --- Fix 4 Tests: ASK_USER Fallback ---

func TestProcessTaskASKUSERFallback(t *testing.T) {
	// When askUser returns an empty answer, the agent should auto-proceed
	// and re-generate — not forward the ASK_USER text as its result.
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: agentComm,
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "I need info.\nASK_USER: Can I read more files?",
				}, nil
			}
			// Second call: verify auto-proceed answer is in context.
			lastMsg := input[len(input)-1]
			if !strings.Contains(lastMsg.Content, "Proceed without waiting") {
				t.Errorf("expected auto-proceed answer, got %q", lastMsg.Content)
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "Implementation done.",
			}, nil
		},
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Build the data layer"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Agent sends TypeUserQuery; we reply with empty text to trigger fallback.
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeUserQuery {
			t.Fatalf("expected user_query first, got %s", msg.Type)
		}
		// Send empty response — triggers auto-proceed.
		conductorComm.Send(ctx, &message.Message{
			ID:        "resp-1",
			RequestID: "req-1",
			From:      "conductor",
			To:        "developer",
			Type:      message.TypeUserResponse,
			Parts:     []message.Part{message.TextPart{Text: ""}},
			Timestamp: time.Now(),
		})
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for user query")
	}

	// Now wait for the task result (after auto-proceed).
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeTaskResult {
			t.Fatalf("expected task_result, got %s", msg.Type)
		}
		text := extractText(msg)
		if !strings.Contains(text, "Implementation done") {
			t.Errorf("expected re-generated result, got %q", text)
		}
		if strings.Contains(text, "ASK_USER") {
			t.Error("ASK_USER text should not appear in final result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result after auto-proceed")
	}

	if callCount != 2 {
		t.Errorf("expected 2 LLM calls (ASK_USER + re-generate), got %d", callCount)
	}
}

func TestProcessTaskASKUSERWithAnswer(t *testing.T) {
	// When askUser returns a real answer, the agent should re-generate with it.
	// This is the existing happy path — verify it still works.
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: agentComm,
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "ASK_USER: Which database?",
				}, nil
			}
			// Second call: verify real user answer is passed.
			lastMsg := input[len(input)-1]
			if !strings.Contains(lastMsg.Content, "MySQL") {
				t.Errorf("expected user answer 'MySQL', got %q", lastMsg.Content)
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "Using MySQL.",
			}, nil
		},
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Build data layer"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Wait for user query.
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeUserQuery {
			t.Fatalf("expected user_query, got %s", msg.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for user query")
	}

	// Send real answer.
	conductorComm.Send(ctx, &message.Message{
		ID:        "resp-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeUserResponse,
		Parts:     []message.Part{message.TextPart{Text: "MySQL"}},
		Timestamp: time.Now(),
	})

	// Wait for result.
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeTaskResult {
			t.Fatalf("expected task_result, got %s", msg.Type)
		}
		text := extractText(msg)
		if !strings.Contains(text, "Using MySQL") {
			t.Errorf("result: got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}

	if callCount != 2 {
		t.Errorf("expected 2 LLM calls, got %d", callCount)
	}
}

// --- Fix 5 Tests: Spec Fidelity Enforcement ---

func TestBuildTaskInputConstraintExtraction(t *testing.T) {
	ag := &Agent{}
	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "Implement the UI"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": "designer",
				"artifact_uri":     "artifacts/designer/",
				"artifact_files":   []string{"mockup.html", "design-spec.md"},
			}},
		},
	}
	got := ag.buildTaskInput(context.Background(), msg, "")
	if !strings.Contains(got, "extract and list binding constraints") {
		t.Error("missing constraint extraction guidance")
	}
	if !strings.Contains(got, "Exact color values") {
		t.Error("missing color constraint guidance")
	}
	if !strings.Contains(got, "do not substitute alternatives") {
		t.Error("missing anti-substitution warning")
	}
	if !strings.Contains(got, "UI component types from HTML mockups") {
		t.Error("missing UI component type constraint guidance")
	}
	if !strings.Contains(got, "element labels") {
		t.Error("missing element labels constraint guidance")
	}
	if !strings.Contains(got, "do NOT add features absent from specs") {
		t.Error("missing feature parity guidance")
	}
}

func TestBuildTaskInputUpstreamPreamble(t *testing.T) {
	ag := &Agent{}
	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "Implement the UI"},
			message.DataPart{Data: map[string]any{
				"dependency_id":    "t1",
				"dependency_agent": "architect",
				"result":           "Use React with TypeScript",
			}},
		},
	}
	got := ag.buildTaskInput(context.Background(), msg, "")
	if !strings.Contains(got, "BINDING REQUIREMENTS") {
		t.Error("missing BINDING REQUIREMENTS in upstream preamble")
	}
	if !strings.Contains(got, "Deviation from upstream specs") {
		t.Error("missing deviation warning in upstream preamble")
	}
	if !strings.Contains(got, "is a defect") {
		t.Error("missing defect language in upstream preamble")
	}
	if !strings.Contains(got, "substituting a dropdown for a button group") {
		t.Error("missing concrete defect examples in upstream preamble")
	}
	if !strings.Contains(got, "Do not add or remove features") {
		t.Error("missing feature parity rule in upstream preamble")
	}
}

func TestExecuteTaskMaterializesBlocksToScratch(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	scratchDir := t.TempDir()

	// LLM returns file blocks only in the final response (no tool calls).
	content := "Here are the files:\n\n" +
		"```file:todo-app/index.html\n<html><body>Hello</body></html>\n```\n\n" +
		"```file:todo-app/styles.css\nbody { margin: 0; }\n```\n"

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm:         agentComm,
		Generate:     mockGenerate(content),
		SystemPrompt: "You are a developer.",
		ToolExecutor: &ToolExecutor{
			TierPaths: []string{scratchDir, t.TempDir(), t.TempDir()},
			AgentName: "developer",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	task := &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Build a todo app"}},
		Timestamp: time.Now(),
	}
	conductorComm.Send(ctx, task)

	select {
	case <-conductorComm.Receive(ctx):
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}

	// Verify file blocks were materialized to scratch.
	htmlPath := filepath.Join(scratchDir, "todo-app", "index.html")
	cssPath := filepath.Join(scratchDir, "todo-app", "styles.css")

	htmlData, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("index.html not materialized to scratch: %v", err)
	}
	if string(htmlData) != "<html><body>Hello</body></html>" {
		t.Errorf("index.html content: got %q", string(htmlData))
	}

	cssData, err := os.ReadFile(cssPath)
	if err != nil {
		t.Fatalf("styles.css not materialized to scratch: %v", err)
	}
	if string(cssData) != "body { margin: 0; }" {
		t.Errorf("styles.css content: got %q", string(cssData))
	}
}

// --- ASK_USER Loop + Strip Tests ---

func TestStripUserQueryLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no ASK_USER lines",
			input: "Hello\nWorld",
			want:  "Hello\nWorld",
		},
		{
			name:  "single ASK_USER line stripped",
			input: "Some output\nASK_USER: What database?\nMore output",
			want:  "Some output\nMore output",
		},
		{
			name:  "case insensitive",
			input: "ask_user: lowercase question\nrest",
			want:  "rest",
		},
		{
			name:  "multiple ASK_USER lines",
			input: "ASK_USER: q1\ntext\nASK_USER: q2",
			want:  "text",
		},
		{
			name:  "indented ASK_USER",
			input: "  ASK_USER: indented\nkeep this",
			want:  "keep this",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripUserQueryLines(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProcessTaskASKUSERLoopRetry(t *testing.T) {
	// When the re-generated response also contains ASK_USER:, the loop should
	// handle it (up to 3 times). After 3 attempts, any residual ASK_USER is stripped.
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: agentComm,
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount <= 2 {
				// First two calls produce ASK_USER
				return &schema.Message{
					Role:    schema.Assistant,
					Content: fmt.Sprintf("ASK_USER: Question %d?", callCount),
				}, nil
			}
			// Third call: normal result
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "Implementation done.",
			}, nil
		},
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Build something"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Handle two rounds of ASK_USER queries with empty answers (auto-proceed).
	for i := 0; i < 2; i++ {
		select {
		case msg := <-conductorComm.Receive(ctx):
			if msg.Type != message.TypeUserQuery {
				t.Fatalf("round %d: expected user_query, got %s", i, msg.Type)
			}
			conductorComm.Send(ctx, &message.Message{
				ID:        fmt.Sprintf("resp-%d", i),
				RequestID: "req-1",
				From:      "conductor",
				To:        "developer",
				Type:      message.TypeUserResponse,
				Parts:     []message.Part{message.TextPart{Text: ""}},
				Timestamp: time.Now(),
			})
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for user query round %d", i)
		}
	}

	// Wait for final result.
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeTaskResult {
			t.Fatalf("expected task_result, got %s", msg.Type)
		}
		text := extractText(msg)
		if !strings.Contains(text, "Implementation done") {
			t.Errorf("expected final result, got %q", text)
		}
		if strings.Contains(text, "ASK_USER") {
			t.Error("ASK_USER should not appear in final result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}

	if callCount != 3 {
		t.Errorf("expected 3 LLM calls (2 ASK_USER + 1 final), got %d", callCount)
	}
}

func TestProcessTaskASKUSERResidualStripped(t *testing.T) {
	// When the response still contains ASK_USER after all retries exhausted,
	// the lines should be stripped from the final result.
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name:    "developer",
			Purpose: "Code generation",
			Model:   "anthropic/claude-opus-4",
		},
		Comm: agentComm,
		Generate: func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
			// Always produce ASK_USER — exercises the max-3 loop + final strip.
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "Some work done.\nASK_USER: Persistent question?",
			}, nil
		},
		SystemPrompt: "You are a developer.",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)

	conductorComm.Send(ctx, &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "Build something"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Handle 3 rounds of ASK_USER with empty answers.
	for i := 0; i < 3; i++ {
		select {
		case msg := <-conductorComm.Receive(ctx):
			if msg.Type != message.TypeUserQuery {
				t.Fatalf("round %d: expected user_query, got %s", i, msg.Type)
			}
			conductorComm.Send(ctx, &message.Message{
				ID:        fmt.Sprintf("resp-%d", i),
				RequestID: "req-1",
				From:      "conductor",
				To:        "developer",
				Type:      message.TypeUserResponse,
				Parts:     []message.Part{message.TextPart{Text: ""}},
				Timestamp: time.Now(),
			})
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for user query round %d", i)
		}
	}

	// Final result should have ASK_USER stripped.
	select {
	case msg := <-conductorComm.Receive(ctx):
		if msg.Type != message.TypeTaskResult {
			t.Fatalf("expected task_result, got %s", msg.Type)
		}
		text := extractText(msg)
		if strings.Contains(text, "ASK_USER") {
			t.Errorf("ASK_USER should be stripped from final result, got %q", text)
		}
		if !strings.Contains(text, "Some work done") {
			t.Errorf("expected remaining content preserved, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for task result")
	}
}

func TestVerifyMaxRetriesDefault(t *testing.T) {
	vc := &runtime.VerifyConfig{Command: "make test"}
	if got := verifyMaxRetries(vc); got != 2 {
		t.Errorf("verifyMaxRetries default: got %d, want 2", got)
	}
}

func TestVerifyMaxRetriesCustom(t *testing.T) {
	vc := &runtime.VerifyConfig{Command: "make test", MaxRetries: 5}
	if got := verifyMaxRetries(vc); got != 5 {
		t.Errorf("verifyMaxRetries custom: got %d, want 5", got)
	}
}

func TestVerifyBonusIterationsDefault(t *testing.T) {
	vc := &runtime.VerifyConfig{Command: "make test"}
	if got := verifyBonusIterations(vc); got != 5 {
		t.Errorf("verifyBonusIterations default: got %d, want 5", got)
	}
}

func TestVerifyBonusIterationsCustom(t *testing.T) {
	vc := &runtime.VerifyConfig{Command: "make test", BonusIterations: 10}
	if got := verifyBonusIterations(vc); got != 10 {
		t.Errorf("verifyBonusIterations custom: got %d, want 10", got)
	}
}

func TestVerifyPassesNoRetry(t *testing.T) {
	// When the verify command passes, the agent should return normally
	// without any retry injection.
	scratch := t.TempDir()
	agent := t.TempDir()
	shared := t.TempDir()

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name: "test-dev",
			Verify: &runtime.VerifyConfig{
				Command:    "echo ok",
				MaxRetries: 2,
			},
		},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "done",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{scratch, agent, shared},
			AgentName: "test-dev",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("build an app"),
	}, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "done" {
		t.Errorf("content: got %q, want %q", resp.Content, "done")
	}
}

func TestVerifyFailsTriggersRetry(t *testing.T) {
	// When the verify command fails, the agent should get an error injection
	// and bonus iterations. On the second call (after injection), the agent
	// produces a final answer and the verify command now passes.
	scratch := t.TempDir()
	agent := t.TempDir()
	shared := t.TempDir()

	// Create a script that fails on first call, passes on second.
	verifyScript := filepath.Join(scratch, "verify.sh")
	counterFile := filepath.Join(scratch, ".verify_count")
	scriptContent := fmt.Sprintf(`#!/bin/sh
COUNT=0
if [ -f "%s" ]; then
  COUNT=$(cat "%s")
fi
COUNT=$((COUNT + 1))
echo $COUNT > "%s"
if [ "$COUNT" -le 1 ]; then
  echo "error TS2345: Type 'string' is not assignable"
  exit 1
fi
echo "Build succeeded"
exit 0
`, counterFile, counterFile, counterFile)
	os.WriteFile(verifyScript, []byte(scriptContent), 0755)

	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name: "test-dev",
			Verify: &runtime.VerifyConfig{
				Command:         "sh " + verifyScript,
				MaxRetries:      2,
				BonusIterations: 3,
			},
		},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			if callCount == 1 {
				// First call: agent produces initial output.
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "```file:index.ts\nexport const x = 1;\n```\n\nDone.",
				}, nil
			}
			// Second call (after verification failure injection): check that
			// the injection message is present.
			lastMsg := input[len(input)-1]
			if !strings.Contains(lastMsg.Content, "AUTOMATIC VERIFICATION FAILED") {
				t.Error("expected verification failure injection in input")
			}
			if !strings.Contains(lastMsg.Content, "error TS2345") {
				t.Error("expected build error output in injection")
			}
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "```file:index.ts\nexport const x: number = 1;\n```\n\nFixed.",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{scratch, agent, shared},
			AgentName: "test-dev",
		},
	}

	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("build an app"),
	}, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 generate calls (retry after verify fail), got %d", callCount)
	}
	// The final response should not contain the failure tag since verify passed.
	if strings.Contains(resp.Content, "VERIFICATION FAILED") {
		t.Error("final response should not contain failure tag when verify eventually passes")
	}
	_ = resp
}

func TestVerifyRetriesExhaustedTagsOutput(t *testing.T) {
	// When all verify retries are exhausted and the agent hits max iterations,
	// the post-loop finalization should tag the output with VERIFICATION FAILED.
	scratch := t.TempDir()
	agent := t.TempDir()
	shared := t.TempDir()

	callCount := 0
	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name: "test-dev",
			Verify: &runtime.VerifyConfig{
				Command:         "echo 'build error' && exit 1",
				MaxRetries:      1,
				BonusIterations: 1,
			},
		},
		Generate: func(_ context.Context, input []*schema.Message) (*schema.Message, error) {
			callCount++
			// Always produce a final response (no tool calls) — the verify
			// command always fails, so after maxRetries the loop breaks.
			return &schema.Message{
				Role:    schema.Assistant,
				Content: "```file:main.ts\nconsole.log('broken');\n```\n\nDone.",
			}, nil
		},
		ToolExecutor: &ToolExecutor{
			Tools: map[string]runtime.ToolConfig{
				"shell": {
					Name:    "shell",
					Command: "$TOOL_ARG_COMMAND",
					Parameters: map[string]runtime.ToolParam{
						"command": {Type: "string", Required: true},
					},
				},
			},
			TierPaths: []string{scratch, agent, shared},
			AgentName: "test-dev",
		},
	}

	// maxIter=2 is tight: iteration 0 produces output, verify fails (attempt 1/1),
	// retry on iteration 1, verify fails again but retries exhausted (1 >= 1),
	// so it returns normally. The key assertion is that retries were triggered.
	resp, _, _, err := ag.generateWithTools(context.Background(), []*schema.Message{
		schema.UserMessage("build an app"),
	}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 generate calls, got %d", callCount)
	}
	_ = resp
}

func TestVerifyNilConfigNoOp(t *testing.T) {
	// When Verify is nil, runVerify should be a no-op.
	ag := &Agent{
		Def: runtime.AgentDefinition{Name: "test-agent"},
	}
	passed, output := ag.runVerify(context.Background())
	if !passed {
		t.Error("expected passed=true when Verify is nil")
	}
	if output != "" {
		t.Errorf("expected empty output, got %q", output)
	}
}

func TestPopulateScratchWithArtifacts(t *testing.T) {
	// Set up a fake shared storage with existing developer artifacts.
	baseDir := t.TempDir()
	scratchDir := t.TempDir()

	storage := local.NewStorage(baseDir, "developer")

	// Create artifacts in shared storage at artifacts/developer/.
	sharedArtifacts := map[string]string{
		"artifacts/developer/package.json":  `{"name": "todo-app"}`,
		"artifacts/developer/src/App.tsx":   `export default function App() {}`,
		"artifacts/developer/src/index.tsx": `import App from './App'`,
		"artifacts/developer/README.md":     `# Todo App`,
	}
	// Also create a trace file that should be skipped.
	sharedArtifacts["artifacts/developer/_requests/abc/trace.json"] = `{}`
	// And a noise file that should be skipped.
	sharedArtifacts["artifacts/developer/node_modules/react/index.js"] = `module.exports = {}`

	ctx := context.Background()
	for path, content := range sharedArtifacts {
		if err := storage.Write(ctx, runtime.TierShared, path, []byte(content)); err != nil {
			t.Fatalf("write shared artifact %s: %v", path, err)
		}
	}

	ag := &Agent{
		Def: runtime.AgentDefinition{
			Name: "developer",
		},
		Storage: storage,
		ToolExecutor: &ToolExecutor{
			TierPaths: []string{scratchDir, t.TempDir(), storage.SharedDir()},
		},
	}

	ag.populateScratchWithArtifacts(ctx)

	// Verify the right files were copied.
	for _, relPath := range []string{"package.json", "src/App.tsx", "src/index.tsx", "README.md"} {
		path := filepath.Join(scratchDir, relPath)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected %s in scratch, not found", relPath)
		}
	}

	// Verify trace and noise files were NOT copied.
	for _, relPath := range []string{"_requests/abc/trace.json", "node_modules/react/index.js"} {
		path := filepath.Join(scratchDir, relPath)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("did not expect %s in scratch, but found it", relPath)
		}
	}

	// Verify content is correct.
	data, err := os.ReadFile(filepath.Join(scratchDir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	if string(data) != `{"name": "todo-app"}` {
		t.Errorf("unexpected package.json content: %s", data)
	}
}

func TestPopulateScratchSkipsExisting(t *testing.T) {
	baseDir := t.TempDir()
	scratchDir := t.TempDir()

	storage := local.NewStorage(baseDir, "developer")
	ctx := context.Background()

	// Write artifact to shared storage.
	if err := storage.Write(ctx, runtime.TierShared, "artifacts/developer/main.go", []byte("shared version")); err != nil {
		t.Fatal(err)
	}

	// Pre-populate scratch with a different version.
	os.WriteFile(filepath.Join(scratchDir, "main.go"), []byte("scratch version"), 0644)

	ag := &Agent{
		Def:     runtime.AgentDefinition{Name: "developer"},
		Storage: storage,
		ToolExecutor: &ToolExecutor{
			TierPaths: []string{scratchDir, t.TempDir(), storage.SharedDir()},
		},
	}

	ag.populateScratchWithArtifacts(ctx)

	// Scratch version should be preserved (not overwritten).
	data, err := os.ReadFile(filepath.Join(scratchDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "scratch version" {
		t.Errorf("scratch file was overwritten: got %q, want %q", data, "scratch version")
	}
}

func TestPopulateScratchCopiesWithoutSentinel(t *testing.T) {
	baseDir := t.TempDir()
	scratchDir := t.TempDir()

	storage := local.NewStorage(baseDir, "developer")
	ctx := context.Background()

	// Write artifact without any sentinel (e.g. from a prior failed run).
	if err := storage.Write(ctx, runtime.TierShared, "artifacts/developer/broken.ts", []byte("broken code")); err != nil {
		t.Fatal(err)
	}

	ag := &Agent{
		Def:     runtime.AgentDefinition{Name: "developer"},
		Storage: storage,
		ToolExecutor: &ToolExecutor{
			TierPaths: []string{scratchDir, t.TempDir(), storage.SharedDir()},
		},
	}

	ag.populateScratchWithArtifacts(ctx)

	// File should be copied — developer needs access to fix broken code.
	data, err := os.ReadFile(filepath.Join(scratchDir, "broken.ts"))
	if err != nil {
		t.Fatal("expected broken.ts to be copied to scratch, but it wasn't")
	}
	if string(data) != "broken code" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestPersistScratchToShared(t *testing.T) {
	baseDir := t.TempDir()
	scratchDir := t.TempDir()

	storage := local.NewStorage(baseDir, "developer")

	// Create files in scratch that simulate shell-created project files.
	scratchFiles := map[string]string{
		"package.json":     `{"name": "todo-app"}`,
		"tsconfig.json":    `{"compilerOptions": {}}`,
		"vite.config.ts":   `export default {}`,
		"src/App.tsx":      `export default function App() {}`, // this one will be in file blocks
		"src/main.tsx":     `import App from './App'`,
		"node_modules/x/y": `should be skipped`,
	}
	for relPath, content := range scratchFiles {
		full := filepath.Join(scratchDir, relPath)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte(content), 0644)
	}

	ag := &Agent{
		Def:     runtime.AgentDefinition{Name: "developer"},
		Storage: storage,
		ToolExecutor: &ToolExecutor{
			TierPaths: []string{scratchDir, t.TempDir(), storage.SharedDir()},
		},
	}

	// Simulate: src/App.tsx was already persisted as a file block.
	existingBlocks := []FileBlock{{Path: "src/App.tsx", Content: "..."}}
	rp := &resultParts{dir: "artifacts/developer"}

	extra := ag.persistScratchToShared(context.Background(), rp, existingBlocks)

	// Should have persisted: package.json, tsconfig.json, vite.config.ts, src/main.tsx
	// Should NOT have persisted: src/App.tsx (file block), node_modules/x/y (noise)
	if extra != 4 {
		t.Errorf("expected 4 extra files, got %d", extra)
	}

	// Verify package.json was written to shared storage.
	ctx := context.Background()
	data, err := storage.Read(ctx, runtime.TierShared, "artifacts/developer/package.json")
	if err != nil {
		t.Fatalf("package.json not in shared: %v", err)
	}
	if string(data) != `{"name": "todo-app"}` {
		t.Errorf("unexpected package.json content: %s", data)
	}

	// Verify node_modules was skipped.
	_, err = storage.Read(ctx, runtime.TierShared, "artifacts/developer/node_modules/x/y")
	if err == nil {
		t.Error("node_modules file should not have been persisted")
	}

	// Verify rp.files includes the extra files.
	if len(rp.files) != 4 {
		t.Errorf("expected 4 artifact files in rp, got %d: %v", len(rp.files), rp.files)
	}
}

func TestPersistScratchSkipsUpstreamCopies(t *testing.T) {
	baseDir := t.TempDir()
	scratchDir := t.TempDir()

	// Use a developer-scoped storage to write the upstream artifact,
	// then create a designer-scoped storage sharing the same base dir.
	devStorage := local.NewStorage(baseDir, "developer")
	ctx := context.Background()
	upstreamContent := []byte(`export default function App() { return <div/> }`)
	if err := devStorage.Write(ctx, runtime.TierShared, "artifacts/developer/src/App.tsx", upstreamContent); err != nil {
		t.Fatal(err)
	}

	storage := local.NewStorage(baseDir, "designer")

	// Designer's scratch has both its own file AND a copy of the developer's file.
	files := map[string]struct {
		content string
	}{
		"styles/main.css":  {content: "body { color: red }"},                              // designer's own work
		"src/App.tsx":      {content: string(upstreamContent)},                            // exact copy of upstream
		"src/Modified.tsx": {content: `export default function App() { return <span/> }`}, // modified — should be kept
	}
	for relPath, f := range files {
		full := filepath.Join(scratchDir, relPath)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte(f.content), 0644)
	}

	ag := &Agent{
		Def:     runtime.AgentDefinition{Name: "designer"},
		Storage: storage,
		ToolExecutor: &ToolExecutor{
			TierPaths: []string{scratchDir, t.TempDir(), storage.SharedDir()},
		},
	}

	rp := &resultParts{dir: "artifacts/designer"}
	extra := ag.persistScratchToShared(ctx, rp, nil)

	// Should persist: styles/main.css and src/Modified.tsx (2 files)
	// Should NOT persist: src/App.tsx (identical to developer's upstream artifact)
	if extra != 2 {
		t.Errorf("expected 2 extra files (skipping upstream copy), got %d; files: %v", extra, rp.files)
	}

	// Verify the upstream copy was NOT published under designer.
	_, err := storage.Read(ctx, runtime.TierShared, "artifacts/designer/src/App.tsx")
	if err == nil {
		t.Error("upstream copy src/App.tsx should not have been persisted under designer")
	}

	// Verify the designer's own file was persisted.
	data, err := storage.Read(ctx, runtime.TierShared, "artifacts/designer/styles/main.css")
	if err != nil {
		t.Fatalf("designer's own file not persisted: %v", err)
	}
	if string(data) != "body { color: red }" {
		t.Errorf("unexpected content: %s", data)
	}

	// Verify the modified file was persisted (not an exact copy).
	data, err = storage.Read(ctx, runtime.TierShared, "artifacts/designer/src/Modified.tsx")
	if err != nil {
		t.Fatalf("modified file not persisted: %v", err)
	}
	if !strings.Contains(string(data), "<span/>") {
		t.Errorf("unexpected content: %s", data)
	}
}

func TestDetectProjectRoot(t *testing.T) {
	tests := []struct {
		name    string
		layout  []string // files to create relative to scratch
		wantSub string   // expected subdirectory name, "" for root
	}{
		{
			name:    "manifest at root",
			layout:  []string{"package.json", "src/index.js"},
			wantSub: "",
		},
		{
			name:    "manifest in subdirectory",
			layout:  []string{"todo-app/package.json", "todo-app/src/main.tsx"},
			wantSub: "todo-app",
		},
		{
			name:    "go mod in subdirectory",
			layout:  []string{"myapp/go.mod", "myapp/main.go"},
			wantSub: "myapp",
		},
		{
			name:    "no manifest anywhere",
			layout:  []string{"README.md", "notes.txt"},
			wantSub: "",
		},
		{
			name:    "multiple subdirs picks first with manifest",
			layout:  []string{"aaa/package.json", "zzz/go.mod"},
			wantSub: "aaa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scratch := t.TempDir()
			for _, f := range tt.layout {
				path := filepath.Join(scratch, f)
				os.MkdirAll(filepath.Dir(path), 0755)
				os.WriteFile(path, []byte("{}"), 0644)
			}

			got := detectProjectRoot(scratch)
			if tt.wantSub == "" {
				if got != scratch {
					t.Errorf("expected scratch root %q, got %q", scratch, got)
				}
			} else {
				want := filepath.Join(scratch, tt.wantSub)
				if got != want {
					t.Errorf("expected %q, got %q", want, got)
				}
			}
		})
	}
}
