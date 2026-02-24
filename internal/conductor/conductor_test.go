package conductor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/event"
)

// mockChatModel implements model.ChatModel for testing.
type mockChatModel struct {
	generateFn func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

func (m *mockChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.generateFn(ctx, input, opts...)
}

func (m *mockChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
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

func (m *mockChatModel) BindTools(tools []*schema.ToolInfo) error {
	return nil
}

func TestConductorStartAndShutdown(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "mock response",
				}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Verify agents are registered.
	states := c.AgentStates(ctx)
	if len(states) != 4 {
		t.Errorf("agents: got %d, want 4", len(states))
	}

	// Shutdown.
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestConductorShutdownCancelsBackgroundWork(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock response"}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil)
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}

	released := make(chan struct{})
	c.knowledgeWg.Add(1)
	go func() {
		defer c.knowledgeWg.Done()
		<-c.backgroundCtx.Done()
		close(released)
	}()

	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(shutCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case <-released:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("background work was not canceled on shutdown")
	}
}

func TestConductorHandleRequest(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				// First call is disambiguation.
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"clear": true}`,
					}, nil
				}
				// Second call is decomposition by conductor.
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design the system", "depends_on": []}]}`,
					}, nil
				}
				// Subsequent calls are agent task execution.
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Task completed successfully.",
				}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	result, err := c.HandleRequest(ctx, "Design a REST API")
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}

	if result == "" {
		t.Fatal("expected non-empty result")
	}

	// Verify conductor-written summary artifacts.
	// Per-request traces live under shared/artifacts/_requests/{requestID}/.
	// Material artifacts live under shared/artifacts/{agentName}/ (global).
	requestDirs, _ := filepath.Glob(filepath.Join(dir, "shared", "artifacts", "_requests", "*"))
	if len(requestDirs) == 0 {
		t.Fatal("expected _requests artifact directory to be created")
	}
	requestDir := requestDirs[0]
	resultsFile := filepath.Join(requestDir, "results.md")
	if _, err := os.Stat(resultsFile); os.IsNotExist(err) {
		t.Error("expected results.md artifact file")
	}
	taskFile := filepath.Join(requestDir, "t1.md")
	if _, err := os.Stat(taskFile); os.IsNotExist(err) {
		t.Error("expected t1.md artifact file")
	}

	// Verify metering recorded calls (Bug 4).
	states := c.AgentStates(ctx)
	totalCalls := int64(0)
	for _, s := range states {
		totalCalls += s.TotalCalls
	}
	if totalCalls == 0 {
		t.Error("expected metering to record at least one LLM call")
	}

	t.Logf("Result: %s", result)
}

func TestConductorAgentChatInfo(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	entries := c.AgentChatInfo()
	if len(entries) == 0 {
		t.Fatal("expected at least one entry")
	}

	// Conductor should be first.
	if entries[0].Name != "conductor" {
		t.Errorf("first entry: got %q, want conductor", entries[0].Name)
	}
	if entries[0].Status != "conductor" {
		t.Errorf("conductor status: got %q, want conductor", entries[0].Status)
	}

	// All other agents should be idle (no active execution).
	for _, e := range entries[1:] {
		if e.Status != "idle" {
			t.Errorf("agent %q: got status %q, want idle", e.Name, e.Status)
		}
	}
}

func TestConductorChat(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "chat response"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "test question",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Response != "chat response" {
		t.Errorf("response: got %q", resp.Response)
	}
}

func TestConductorRestructureGraphNoExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	// RestructureGraph should fail when no execution is active.
	err := c.RestructureGraph(ctx, "test request", "test amendment")
	if err == nil {
		t.Fatal("expected error when no active execution")
	}
	if err.Error() != "no active execution" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConductorHandleRequestScreened(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				// First call is disambiguation — return clear.
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"clear": true}`,
					}, nil
				}
				// Second call is decompose — return non-actionable.
				return &schema.Message{
					Role:    schema.Assistant,
					Content: `{"actionable": false, "message": "Hey! What can I help you build?"}`,
				}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	result, err := c.HandleRequest(ctx, "Hey/")
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}

	if result != "" {
		t.Errorf("expected empty result for screened request, got: %q", result)
	}

	// Disambiguation (1 call) + decompose (1 call) = 2 calls total.
	if callCount != 2 {
		t.Errorf("expected exactly 2 LLM calls (disambiguate + decompose), got %d", callCount)
	}
}

func TestConductorPauseResumeExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					return &schema.Message{Role: schema.Assistant, Content: `{"clear": true}`}, nil
				}
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design", "depends_on": []}]}`,
					}, nil
				}
				// Task execution — add a small delay to allow pause to happen.
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(50 * time.Millisecond):
				}
				return &schema.Message{Role: schema.Assistant, Content: "Task done."}, nil
			},
		}, nil
	}

	events := event.NewBus()
	c, _ := New(systemDef, dir, mockFactory, events)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	// No active execution: PauseExecution should return false.
	if c.PauseExecution() {
		t.Error("PauseExecution should return false when idle")
	}
	if c.ResumeExecution() {
		t.Error("ResumeExecution should return false when idle")
	}

	// Start a request in the background.
	resultCh := make(chan error, 1)
	go func() {
		_, err := c.HandleRequest(ctx, "Build something")
		resultCh <- err
	}()

	// Drain events until we see TaskStart (execution phase).
	for e := range events {
		if e.Type == event.TaskStart {
			break
		}
	}

	// Now pause.
	if !c.PauseExecution() {
		t.Fatal("PauseExecution should return true during execution")
	}

	// Resume.
	if !c.ResumeExecution() {
		t.Fatal("ResumeExecution should return true during execution")
	}

	// Wait for completion.
	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("HandleRequest: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for HandleRequest to complete")
	}

	events.Close()
}

func TestConductorCancelExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					return &schema.Message{Role: schema.Assistant, Content: `{"clear": true}`}, nil
				}
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design", "depends_on": []}]}`,
					}, nil
				}
				// Block until context cancelled to simulate long task.
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}, nil
	}

	events := event.NewBus()
	c, _ := New(systemDef, dir, mockFactory, events)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resultCh := make(chan error, 1)
	go func() {
		_, err := c.HandleRequest(ctx, "Build something")
		resultCh <- err
	}()

	// Drain events until we see TaskStart.
	for e := range events {
		if e.Type == event.TaskStart {
			break
		}
	}

	// Cancel execution.
	if !c.CancelExecution() {
		t.Fatal("CancelExecution should return true during execution")
	}

	select {
	case err := <-resultCh:
		if err != ErrRequestCancelled {
			t.Fatalf("expected ErrRequestCancelled, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for HandleRequest to return after cancel")
	}

	events.Close()
}

func TestConductorCancelNoExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	if c.CancelExecution() {
		t.Error("CancelExecution should return false when idle")
	}
}
