package conductor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/loop"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

func TestSchedulerIncludesDependencyContext(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-1",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "architect",
				Description: "Design the API",
				Status:      taskgraph.StatusCompleted,
				Result:      "Use REST with JSON.",
				ArtifactURI: "artifacts/architect/req-1/result.md",
			},
			{
				ID:          "t2",
				Agent:       "developer",
				Description: "Implement the API",
				DependsOn:   []string{"t1"},
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-1",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run dispatchTask in a goroutine (it blocks waiting for result).
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(ctx, graph.Tasks[1], graph)
	}()

	// Receive the dispatched message at the agent.
	select {
	case msg := <-agentComm.Receive(ctx):
		if msg.Type != message.TypeTaskAssignment {
			t.Fatalf("expected task_assignment, got %s", msg.Type)
		}
		// Verify dependency context is included.
		// Find the dependency DataPart (skip pipeline_context DataPart).
		var dp message.DataPart
		foundDep := false
		for _, p := range msg.Parts {
			if d, ok := p.(message.DataPart); ok {
				if _, hasDep := d.Data["dependency_id"]; hasDep {
					dp = d
					foundDep = true
					break
				}
			}
		}
		if !foundDep {
			t.Fatal("no dependency DataPart found in message parts")
		}
		if dp.Data["dependency_id"] != "t1" {
			t.Errorf("dependency_id: got %v, want t1", dp.Data["dependency_id"])
		}
		if dp.Data["result"] != "Use REST with JSON." {
			t.Errorf("result: got %v", dp.Data["result"])
		}
		if dp.Data["artifact_uri"] != "artifacts/architect/req-1/result.md" {
			t.Errorf("artifact_uri: got %v", dp.Data["artifact_uri"])
		}

		// Verify pipeline context is included.
		foundPipeline := false
		for _, p := range msg.Parts {
			if d, ok := p.(message.DataPart); ok {
				if _, has := d.Data["pipeline_context"]; has {
					foundPipeline = true
					break
				}
			}
		}
		if !foundPipeline {
			t.Error("no pipeline_context DataPart found in message parts")
		}

		// Send back a result so dispatchTask can complete.
		agentComm.Send(ctx, &message.Message{
			ID:        "r1",
			RequestID: "req-1",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts:     []message.Part{message.TextPart{Text: "implemented"}},
			Metadata:  map[string]string{"task_id": "t2"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	// Wait for dispatchTask to complete.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for dispatchTask to complete")
	}

	if graph.Tasks[1].Result != "implemented" {
		t.Errorf("task result: got %q", graph.Tasks[1].Result)
	}
}

func TestSchedulerExtractsArtifactURI(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-2",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "developer",
				Description: "Write code",
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-2",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(ctx, graph.Tasks[0], graph)
	}()

	// Receive the task at the agent and send back a result with a FilePart.
	select {
	case <-agentComm.Receive(ctx):
		agentComm.Send(ctx, &message.Message{
			ID:        "r1",
			RequestID: "req-2",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts: []message.Part{
				message.TextPart{Text: "code output"},
				message.FilePart{
					URI:      "artifacts/developer/req-2/result.md",
					MimeType: "text/markdown",
					Name:     "result.md",
				},
			},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for dispatchTask")
	}

	task := graph.Tasks[0]
	if task.Result != "code output" {
		t.Errorf("result: got %q", task.Result)
	}
	if task.ArtifactURI != "artifacts/developer/req-2/result.md" {
		t.Errorf("artifact URI: got %q", task.ArtifactURI)
	}
	if task.Status != taskgraph.StatusCompleted {
		t.Errorf("status: got %q", task.Status)
	}
}

func TestSchedulerExtractsDirectoryArtifactURI(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-3",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "developer",
				Description: "Write code",
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-3",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(ctx, graph.Tasks[0], graph)
	}()

	// Receive the task and send back a result with multiple FileParts.
	select {
	case <-agentComm.Receive(ctx):
		agentComm.Send(ctx, &message.Message{
			ID:        "r1",
			RequestID: "req-3",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts: []message.Part{
				message.TextPart{Text: "multi-file output"},
				message.FilePart{
					URI:      "artifacts/developer/req-3/README.md",
					MimeType: "text/markdown",
					Name:     "README.md",
				},
				message.FilePart{
					URI:      "artifacts/developer/req-3/main.go",
					MimeType: "text/x-go",
					Name:     "main.go",
				},
				message.FilePart{
					URI:      "artifacts/developer/req-3/summary.md",
					MimeType: "text/markdown",
					Name:     "summary.md",
				},
			},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for dispatchTask")
	}

	task := graph.Tasks[0]
	if task.Result != "multi-file output" {
		t.Errorf("result: got %q", task.Result)
	}
	// With multiple FileParts, artifact URI should be the common directory prefix.
	expected := "artifacts/developer/req-3/"
	if task.ArtifactURI != expected {
		t.Errorf("artifact URI: got %q, want %q", task.ArtifactURI, expected)
	}
}

func TestSchedulerIncludesUserRequest(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-ur",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Write code", Status: taskgraph.StatusRunning},
		},
	}

	s := &Scheduler{
		Comm:        conductorComm,
		RequestID:   "req-ur",
		UserRequest: "Build a login page",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(ctx, graph.Tasks[0], graph)
	}()

	select {
	case msg := <-agentComm.Receive(ctx):
		// Verify user_request DataPart.
		foundUR := false
		for _, p := range msg.Parts {
			if d, ok := p.(message.DataPart); ok {
				if ur, ok := d.Data["user_request"].(string); ok {
					foundUR = true
					if ur != "Build a login page" {
						t.Errorf("user_request: got %q", ur)
					}
				}
			}
		}
		if !foundUR {
			t.Error("no user_request DataPart found")
		}

		agentComm.Send(ctx, &message.Message{
			ID: "r1", RequestID: "req-ur", From: "developer", To: "conductor",
			Type: message.TypeTaskResult, Parts: []message.Part{message.TextPart{Text: "done"}},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout")
	}
}

func schedulerTestLoop() loop.LoopDefinition {
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

// TestLoopDispatchInitialMessage verifies the conductor sends the initial loop
// message to the first agent with LoopContext attached.
func TestLoopDispatchInitialMessage(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	_ = hub.Register("architect")

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-loop",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement login", LoopID: "review-1", Status: taskgraph.StatusRunning},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	events := event.NewBus()
	s := &Scheduler{
		Comm:        conductorComm,
		RequestID:   "req-loop",
		Events:      events,
		UserRequest: "Build login page",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchLoopTask(ctx, graph.Tasks[0], graph)
	}()

	// Developer receives initial task_assignment with loop_context.
	select {
	case msg := <-devComm.Receive(ctx):
		if msg.Type != message.TypeTaskAssignment {
			t.Fatalf("expected task_assignment, got %s", msg.Type)
		}
		if !strings.Contains(extractText(msg), "Implement login") {
			t.Error("missing task description")
		}
		// Verify LoopContext is attached.
		lc, ok := loop.DecodeContext(msg)
		if !ok {
			t.Fatal("missing loop_context in initial message")
		}
		if lc.State.CurrentState != "WORKING" {
			t.Errorf("initial state: got %q, want WORKING", lc.State.CurrentState)
		}
		if lc.TaskID != "t1" {
			t.Errorf("task_id: got %q", lc.TaskID)
		}
		if lc.Conductor != "conductor" {
			t.Errorf("conductor: got %q", lc.Conductor)
		}

		// Simulate terminal result back to conductor (agents handled routing).
		devComm.Send(ctx, &message.Message{
			ID: "r1", RequestID: "req-loop", From: "developer", To: "conductor",
			Type: message.TypeTaskResult, Parts: []message.Part{message.TextPart{Text: "final result"}},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for initial dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchLoopTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for completion")
	}

	task := graph.Tasks[0]
	if task.Status != taskgraph.StatusCompleted {
		t.Errorf("status: got %q, want completed", task.Status)
	}
	if task.Result != "final result" {
		t.Errorf("result: got %q", task.Result)
	}

	events.Close()
}

func TestLoopDispatchPinsAssignedInstances(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	_ = hub.Register("architect")
	store := controlplane.NewMemoryStore("test-fabric")

	ctx := context.Background()
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterInstance developer: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-b/architect/1",
		Profile:         "architect",
		NodeID:          "node-b",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterInstance architect: %v", err)
	}

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-loop-placement",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement login", LoopID: "review-1", Status: taskgraph.StatusRunning},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	s := &Scheduler{
		Comm:         conductorComm,
		ControlPlane: store,
		RequestID:    "req-loop-placement",
		LeaseOwnerID: "conductor-test",
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchLoopTask(runCtx, graph.Tasks[0], graph)
	}()

	select {
	case msg := <-devComm.Receive(runCtx):
		if msg.Metadata["assigned_instance"] != "node-a/developer/1" {
			t.Fatalf("assigned instance = %q, want node-a/developer/1", msg.Metadata["assigned_instance"])
		}
		if msg.Metadata["execution_node"] != "node-a" {
			t.Fatalf("execution node = %q, want node-a", msg.Metadata["execution_node"])
		}
		lc, ok := loop.DecodeContext(msg)
		if !ok {
			t.Fatal("missing loop_context in initial message")
		}
		if lc.AssignedInstances["developer"] != "node-a/developer/1" {
			t.Fatalf("developer assignment = %q, want node-a/developer/1", lc.AssignedInstances["developer"])
		}
		if lc.AssignedInstances["architect"] != "node-b/architect/1" {
			t.Fatalf("architect assignment = %q, want node-b/architect/1", lc.AssignedInstances["architect"])
		}
		if lc.ExecutionNodes["architect"] != "node-b" {
			t.Fatalf("architect execution node = %q, want node-b", lc.ExecutionNodes["architect"])
		}

		devComm.Send(runCtx, &message.Message{
			ID:        "r1",
			RequestID: "req-loop-placement",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts:     []message.Part{message.TextPart{Text: "loop done"}},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-runCtx.Done():
		t.Fatal("timeout waiting for initial loop dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchLoopTask: %v", err)
		}
	case <-runCtx.Done():
		t.Fatal("timeout waiting for loop completion")
	}
}

func TestSchedulerLoopUpdatesActiveExecutionFromStatusUpdate(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	archComm := hub.Register("architect")
	store := controlplane.NewMemoryStore("test-fabric")

	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-a",
		State:           controlplane.NodeStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterNode node-a: %v", err)
	}
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-b",
		State:           controlplane.NodeStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterNode node-b: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstance developer: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-b/architect/1",
		Profile:         "architect",
		NodeID:          "node-b",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstance architect: %v", err)
	}

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-loop-transition",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement login", LoopID: "review-1", Status: taskgraph.StatusPending},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	s := &Scheduler{
		Comm:         conductorComm,
		ControlPlane: store,
		RequestID:    "req-loop-transition",
		LeaseOwnerID: "conductor-test",
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Execute(runCtx, graph)
	}()

	select {
	case <-devComm.Receive(runCtx):
	case <-runCtx.Done():
		t.Fatal("timeout waiting for initial loop dispatch")
	}

	conductorComm.Send(runCtx, &message.Message{
		ID:        "status-1",
		RequestID: "req-loop-transition",
		From:      "architect",
		To:        "conductor",
		Type:      message.TypeStatusUpdate,
		Parts:     []message.Part{message.TextPart{Text: "Loop state REVIEWING active on architect"}},
		Metadata: map[string]string{
			"task_id":           "t1",
			"loop_id":           "review-1",
			"loop_state":        "REVIEWING",
			"assigned_instance": "node-b/architect/1",
			"execution_node":    "node-b",
		},
		Timestamp: time.Now(),
	})

	select {
	case <-archComm.Receive(runCtx):
		t.Fatal("unexpected direct message to architect in scheduler-only transition test")
	case <-time.After(200 * time.Millisecond):
	}

	staleAt := time.Now().UTC().Add(-controlplane.InstanceHeartbeatTTL - time.Second)
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-b/architect/1",
		Profile:         "architect",
		NodeID:          "node-b",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: staleAt,
	}); err != nil {
		t.Fatalf("RegisterInstance stale architect: %v", err)
	}

	var redispatch *message.Message
	select {
	case redispatch = <-devComm.Receive(runCtx):
	case <-runCtx.Done():
		t.Fatal("timeout waiting for loop redispatch")
	}
	if redispatch.Metadata["assigned_instance"] != "node-a/developer/1" {
		t.Fatalf("redispatch assigned instance = %q, want node-a/developer/1", redispatch.Metadata["assigned_instance"])
	}

	devComm.Send(runCtx, &message.Message{
		ID:        "r2",
		RequestID: "req-loop-transition",
		From:      "developer",
		To:        "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "loop recovered"}},
		Metadata: map[string]string{
			"task_id":        "t1",
			"dispatch_nonce": redispatch.Metadata["dispatch_nonce"],
		},
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-runCtx.Done():
		t.Fatal("timeout waiting for loop execution")
	}

	task := graph.Tasks[0]
	if task.Status != taskgraph.StatusCompleted {
		t.Fatalf("task status = %q, want completed", task.Status)
	}
	if task.AssignedInstance != "node-a/developer/1" {
		t.Fatalf("task assigned instance = %q, want node-a/developer/1", task.AssignedInstance)
	}
}

// TestLoopDispatchCompletesOnResult verifies the conductor correctly handles
// the terminal TypeTaskResult from any loop participant.
func TestLoopDispatchCompletesOnResult(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	_ = hub.Register("developer")
	archComm := hub.Register("architect")

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-done",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement feature", LoopID: "review-1", Status: taskgraph.StatusRunning},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	events := event.NewBus()
	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-done",
		Events:    events,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchLoopTask(ctx, graph.Tasks[0], graph)
	}()

	// Drain the initial message to developer (don't need to inspect it).
	select {
	case <-hub.Register("developer-drain").Receive(ctx):
		// This won't work since developer is already registered.
		// Instead, just have the architect send the final result.
	case <-time.After(100 * time.Millisecond):
		// Brief delay to let dispatch happen.
	}

	// Architect sends final result to conductor (simulating terminal approval).
	archComm.Send(ctx, &message.Message{
		ID: "r-final", RequestID: "req-done", From: "architect", To: "conductor",
		Type:  message.TypeTaskResult,
		Parts: []message.Part{message.TextPart{Text: "approved implementation"}},
		Metadata: map[string]string{
			"task_id": "t1",
			"loop_id": "review-1",
		},
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchLoopTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for completion")
	}

	task := graph.Tasks[0]
	if task.Status != taskgraph.StatusCompleted {
		t.Errorf("status: got %q, want completed", task.Status)
	}
	if task.Result != "approved implementation" {
		t.Errorf("result: got %q", task.Result)
	}

	events.Close()
}

// TestLoopDispatchEscalation verifies that a loop_escalated result
// sets task status to escalated.
func TestLoopDispatchEscalation(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	_ = hub.Register("architect")

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-esc",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement", LoopID: "review-1", Status: taskgraph.StatusRunning},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	events := event.NewBus()
	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-esc",
		Events:    events,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchLoopTask(ctx, graph.Tasks[0], graph)
	}()

	// Drain initial dispatch.
	select {
	case <-devComm.Receive(ctx):
	case <-ctx.Done():
		t.Fatal("timeout waiting for initial dispatch")
	}

	// Simulate escalation: developer sends TypeTaskResult with loop_escalated.
	devComm.Send(ctx, &message.Message{
		ID: "r-esc", RequestID: "req-esc", From: "developer", To: "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "max transitions reached"}},
		Metadata:  map[string]string{"task_id": "t1", "loop_escalated": "true"},
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchLoopTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for completion")
	}

	if graph.Tasks[0].Status != taskgraph.StatusEscalated {
		t.Errorf("status: got %q, want escalated", graph.Tasks[0].Status)
	}

	events.Close()
}

func TestLoopDispatchFailureMarksTaskFailed(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	_ = hub.Register("architect")

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-loop-fail",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement", LoopID: "review-1", Status: taskgraph.StatusRunning},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	events := event.NewBus()
	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-loop-fail",
		Events:    events,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchLoopTask(ctx, graph.Tasks[0], graph)
	}()

	select {
	case <-devComm.Receive(ctx):
	case <-ctx.Done():
		t.Fatal("timeout waiting for initial dispatch")
	}

	devComm.Send(ctx, &message.Message{
		ID: "r-fail", RequestID: "req-loop-fail", From: "developer", To: "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "Cannot complete task: reviewer failed"}},
		Metadata:  map[string]string{"task_id": "t1", "status": "failed"},
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchLoopTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for completion")
	}

	if graph.Tasks[0].Status != taskgraph.StatusFailed {
		t.Errorf("status: got %q, want failed", graph.Tasks[0].Status)
	}
	if graph.Tasks[0].Result != "Cannot complete task: reviewer failed" {
		t.Errorf("result: got %q", graph.Tasks[0].Result)
	}

	events.Close()
}

func TestLoopDispatchUserQueryForwarding(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	devComm := hub.Register("developer")
	_ = hub.Register("architect")

	loopDef := schedulerTestLoop()
	graph := &taskgraph.TaskGraph{
		RequestID: "req-loop-query",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Implement", LoopID: "review-1", Status: taskgraph.StatusRunning},
		},
		Loops: []loop.LoopDefinition{loopDef},
	}

	userQueryCh := make(chan *UserQuery, 1)
	events := event.NewBus()
	s := &Scheduler{
		Comm:        conductorComm,
		RequestID:   "req-loop-query",
		Events:      events,
		UserQueryCh: userQueryCh,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchLoopTask(ctx, graph.Tasks[0], graph)
	}()

	// Drain initial loop dispatch.
	select {
	case <-devComm.Receive(ctx):
	case <-ctx.Done():
		t.Fatal("timeout waiting for initial dispatch")
	}

	// Developer asks user question from inside loop.
	devComm.Send(ctx, &message.Message{
		ID: "q-loop", RequestID: "req-loop-query", From: "developer", To: "conductor",
		Type:      message.TypeUserQuery,
		Parts:     []message.Part{message.TextPart{Text: "Need confirmation?"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Scheduler should forward query to UI channel.
	select {
	case q := <-userQueryCh:
		if q.AgentName != "developer" {
			t.Errorf("agent: got %q, want developer", q.AgentName)
		}
		if q.TaskID != "t1" {
			t.Errorf("task_id: got %q, want t1", q.TaskID)
		}
		if q.Question != "Need confirmation?" {
			t.Errorf("question: got %q", q.Question)
		}
		q.ResponseCh <- "Yes"
	case <-ctx.Done():
		t.Fatal("timeout waiting for loop user query")
	}

	// Developer should receive user_response.
	select {
	case msg := <-devComm.Receive(ctx):
		if msg.Type != message.TypeUserResponse {
			t.Fatalf("expected user_response, got %s", msg.Type)
		}
		if extractText(msg) != "Yes" {
			t.Fatalf("response text: got %q, want Yes", extractText(msg))
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for loop user response")
	}

	// Finish loop with a terminal result.
	devComm.Send(ctx, &message.Message{
		ID: "r-loop", RequestID: "req-loop-query", From: "developer", To: "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "done"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchLoopTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for loop completion")
	}

	if graph.Tasks[0].Status != taskgraph.StatusCompleted {
		t.Errorf("status: got %q, want completed", graph.Tasks[0].Status)
	}
	events.Close()
}

func TestSchedulerAmendTask(t *testing.T) {
	graph := &taskgraph.TaskGraph{
		RequestID: "req-amend",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "developer",
				Description: "Original task",
				Status:      taskgraph.StatusRunning,
				Result:      "some result",
				ArtifactURI: "artifacts/developer/result.md",
			},
		},
	}

	s := &Scheduler{
		graph:       graph,
		taskCancels: make(map[string]context.CancelFunc),
	}

	// Register a cancel func.
	cancelled := false
	s.taskCancels["t1"] = func() { cancelled = true }

	err := s.AmendTask("t1", "New task description", "User: change it\n\nYou: done")
	if err != nil {
		t.Fatalf("amend: %v", err)
	}

	task := graph.Get("t1")
	if task.Description != "New task description" {
		t.Errorf("description: got %q", task.Description)
	}
	if task.Status != taskgraph.StatusPending {
		t.Errorf("status: got %q, want pending", task.Status)
	}
	if task.Result != "" {
		t.Errorf("result should be cleared, got %q", task.Result)
	}
	if task.ArtifactURI != "" {
		t.Errorf("artifact_uri should be cleared, got %q", task.ArtifactURI)
	}
	if task.ChatContext != "User: change it\n\nYou: done" {
		t.Errorf("chat context: got %q", task.ChatContext)
	}
	if !cancelled {
		t.Error("expected task cancel to be called")
	}
}

func TestSchedulerAmendTaskNotFound(t *testing.T) {
	s := &Scheduler{
		graph:       &taskgraph.TaskGraph{Tasks: []*taskgraph.TaskNode{}},
		taskCancels: make(map[string]context.CancelFunc),
	}
	err := s.AmendTask("nonexistent", "new desc", "")
	if err == nil {
		t.Error("expected error for non-existent task")
	}
}

func TestSchedulerUserQueryForwarding(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-query",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "developer",
				Description: "Build something",
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	userQueryCh := make(chan *UserQuery, 1)
	s := &Scheduler{
		Comm:        conductorComm,
		RequestID:   "req-query",
		Events:      event.NewBus(),
		UserQueryCh: userQueryCh,
		taskCancels: make(map[string]context.CancelFunc),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run waitForResult in a goroutine.
	resultCh := make(chan *message.Message, 1)
	errCh := make(chan error, 1)
	go func() {
		s.ensureDemux(ctx)
		msg, err := s.waitForResult(ctx, graph.Tasks[0], "")
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- msg
	}()

	// Give demux time to start.
	time.Sleep(50 * time.Millisecond)

	// Agent sends a user query.
	agentComm.Send(ctx, &message.Message{
		ID:        "q1",
		RequestID: "req-query",
		From:      "developer",
		To:        "conductor",
		Type:      message.TypeUserQuery,
		Parts:     []message.Part{message.TextPart{Text: "Which DB?"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	// Read from UserQueryCh and answer.
	select {
	case query := <-userQueryCh:
		if query.AgentName != "developer" {
			t.Errorf("agent: got %q", query.AgentName)
		}
		if query.Question != "Which DB?" {
			t.Errorf("question: got %q", query.Question)
		}
		query.ResponseCh <- "PostgreSQL"
	case <-ctx.Done():
		t.Fatal("timeout waiting for user query")
	}

	// Verify the response was sent to agent.
	select {
	case msg := <-agentComm.Receive(ctx):
		if msg.Type != message.TypeUserResponse {
			t.Errorf("expected user_response, got %s", msg.Type)
		}
		text := extractText(msg)
		if text != "PostgreSQL" {
			t.Errorf("response: got %q", text)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for user response")
	}

	// Agent sends the final result.
	agentComm.Send(ctx, &message.Message{
		ID:        "r1",
		RequestID: "req-query",
		From:      "developer",
		To:        "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "done with PostgreSQL"}},
		Metadata:  map[string]string{"task_id": "t1"},
		Timestamp: time.Now(),
	})

	select {
	case msg := <-resultCh:
		if extractText(msg) != "done with PostgreSQL" {
			t.Errorf("result: got %q", extractText(msg))
		}
	case err := <-errCh:
		t.Fatalf("waitForResult error: %v", err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for final result")
	}

	s.stopDemux()
	s.Events.Close()
}

func TestSchedulerCancelAllRunning(t *testing.T) {
	s := &Scheduler{
		taskCancels: make(map[string]context.CancelFunc),
	}

	cancelled := make(map[string]bool)
	s.taskCancels["t1"] = func() { cancelled["t1"] = true }
	s.taskCancels["t2"] = func() { cancelled["t2"] = true }

	s.CancelAllRunning()

	if !cancelled["t1"] || !cancelled["t2"] {
		t.Errorf("expected all tasks cancelled: %v", cancelled)
	}
}

func TestEmitTaskHeartbeatIncludesPlacement(t *testing.T) {
	events := event.NewBus()
	s := &Scheduler{Events: events}
	task := &taskgraph.TaskNode{
		Agent:            "architect",
		AssignedInstance: "node-5/architect",
		ExecutionNode:    "node-5",
	}

	s.emitTaskHeartbeat(task)

	select {
	case evt := <-events:
		if evt.Type != event.TaskProgress {
			t.Fatalf("event type = %v, want TaskProgress", evt.Type)
		}
		if evt.TaskAgent != "architect" {
			t.Fatalf("task agent = %q, want architect", evt.TaskAgent)
		}
		if evt.ProgressText != "Still working on node-5/architect via node-5" {
			t.Fatalf("progress text = %q", evt.ProgressText)
		}
	default:
		t.Fatal("expected task heartbeat event")
	}
}

func TestTaskResultSummary(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hello world", 150, "hello world"},
		{"collapse whitespace", "line1\n  line2\n\tline3", 150, "line1 line2 line3"},
		{"truncate", "abcdefghij", 5, "abcde..."},
		{"exact", "12345", 5, "12345"},
		{"empty", "", 150, ""},
		{"only whitespace", "   \n\t  ", 150, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := taskResultSummary(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("taskResultSummary(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestCommonDirPrefixNoSharedDir(t *testing.T) {
	// This previously caused an infinite loop when prefix ended with "/"
	// and LastIndex found the trailing "/" producing the same string.
	done := make(chan string, 1)
	go func() {
		done <- commonDirPrefix([]string{"a/foo.txt", "b/bar.txt"})
	}()

	select {
	case got := <-done:
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commonDirPrefix hung — infinite loop")
	}
}

func TestSchedulerPauseResume(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-pause",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Build feature", Status: taskgraph.StatusPending},
		},
	}

	events := event.NewBus()
	defer events.Close()

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-pause",
		Events:    events,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	execDone := make(chan error, 1)
	go func() {
		execDone <- s.Execute(ctx, graph)
	}()

	// Wait for the task to be dispatched to the agent.
	select {
	case <-agentComm.Receive(ctx):
	case <-ctx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	// Pause the scheduler.
	s.Pause()

	// After pause, running tasks should have been cancelled via context.
	// The dispatch goroutine will see the context error. The task status will be
	// set to Running (it was dispatched), but because Pause calls CancelAllRunning,
	// the goroutine will get a context error. Since the task isn't marked cancelled,
	// it would normally be marked failed. But Resume resets running tasks to pending.

	// Brief delay to let the dispatch goroutine handle cancellation.
	time.Sleep(50 * time.Millisecond)

	// Resume: this resets running tasks to pending and unblocks the loop.
	s.Resume()

	// The scheduler will re-dispatch t1 because it was reset to pending.
	// Send a result this time.
	select {
	case <-agentComm.Receive(ctx):
		agentComm.Send(ctx, &message.Message{
			ID: "r1", RequestID: "req-pause", From: "developer", To: "conductor",
			Type: message.TypeTaskResult, Parts: []message.Part{message.TextPart{Text: "done"}},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for re-dispatch after resume")
	}

	select {
	case err := <-execDone:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Execute to complete")
	}

	if graph.Tasks[0].Status != taskgraph.StatusCompleted {
		t.Errorf("status: got %q, want completed", graph.Tasks[0].Status)
	}
}

func TestSchedulerCancel(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	_ = hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-cancel",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Build feature", Status: taskgraph.StatusPending},
			{ID: "t2", Agent: "developer", Description: "Test feature", DependsOn: []string{"t1"}, Status: taskgraph.StatusPending},
		},
	}

	events := event.NewBus()
	defer events.Close()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-cancel",
		Events:    events,
		reqCancel: reqCancel,
	}

	ctx, cancel := context.WithTimeout(reqCtx, 10*time.Second)
	defer cancel()

	execDone := make(chan error, 1)
	go func() {
		execDone <- s.Execute(ctx, graph)
	}()

	// Wait briefly for scheduler to start dispatching.
	time.Sleep(100 * time.Millisecond)

	// Cancel.
	s.Cancel()

	select {
	case err := <-execDone:
		// Execute returns context.Canceled since reqCancel() was called.
		if err != nil && err != context.Canceled {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Execute to return after cancel")
	}

	// All tasks should be cancelled.
	for _, task := range graph.Tasks {
		if task.Status != taskgraph.StatusCancelled {
			t.Errorf("task %s: got %q, want cancelled", task.ID, task.Status)
		}
	}
}

func TestSchedulerCancelWhilePaused(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	_ = hub.Register("developer")

	graph := &taskgraph.TaskGraph{
		RequestID: "req-pc",
		Tasks: []*taskgraph.TaskNode{
			{ID: "t1", Agent: "developer", Description: "Build feature", Status: taskgraph.StatusPending},
		},
	}

	events := event.NewBus()
	defer events.Close()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-pc",
		Events:    events,
		reqCancel: reqCancel,
	}

	ctx, cancel := context.WithTimeout(reqCtx, 10*time.Second)
	defer cancel()

	execDone := make(chan error, 1)
	go func() {
		execDone <- s.Execute(ctx, graph)
	}()

	// Wait for dispatch, then pause.
	time.Sleep(100 * time.Millisecond)
	s.Pause()
	time.Sleep(50 * time.Millisecond)

	// Cancel while paused — should unblock the pause gate.
	s.Cancel()

	select {
	case err := <-execDone:
		if err != nil && err != context.Canceled {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: cancel-while-paused did not unblock Execute")
	}

	if graph.Tasks[0].Status != taskgraph.StatusCancelled {
		t.Errorf("status: got %q, want cancelled", graph.Tasks[0].Status)
	}
}

// TestDemuxConcurrentSameAgent verifies two tasks targeting the same agent
// both get their respective results (no deadlock or collision).
func TestDemuxConcurrentSameAgent(t *testing.T) {
	d := newDemux()

	ch1 := d.subscribe("task-1")
	ch2 := d.subscribe("task-2")

	// Route messages with different task_ids.
	d.route(&message.Message{
		From: "developer", Type: message.TypeTaskResult,
		Parts:    []message.Part{message.TextPart{Text: "result-1"}},
		Metadata: map[string]string{"task_id": "task-1"},
	})
	d.route(&message.Message{
		From: "developer", Type: message.TypeTaskResult,
		Parts:    []message.Part{message.TextPart{Text: "result-2"}},
		Metadata: map[string]string{"task_id": "task-2"},
	})

	msg1 := <-ch1
	msg2 := <-ch2

	if extractText(msg1) != "result-1" {
		t.Errorf("task-1 got: %q", extractText(msg1))
	}
	if extractText(msg2) != "result-2" {
		t.Errorf("task-2 got: %q", extractText(msg2))
	}
}

// TestDemuxRouteByTaskID verifies routing uses metadata task_id, not msg.From.
func TestDemuxRouteByTaskID(t *testing.T) {
	d := newDemux()

	ch := d.subscribe("my-task")

	// Route a message from an agent — should match task_id, not agent name.
	d.route(&message.Message{
		From: "architect", Type: message.TypeTaskResult,
		Parts:    []message.Part{message.TextPart{Text: "routed correctly"}},
		Metadata: map[string]string{"task_id": "my-task"},
	})

	select {
	case msg := <-ch:
		if extractText(msg) != "routed correctly" {
			t.Errorf("unexpected text: %q", extractText(msg))
		}
	default:
		t.Fatal("expected message on channel")
	}
}

// TestDemuxOverflowTaskID verifies overflow messages are delivered by task_id
// when a subscriber registers after the message arrived.
func TestDemuxOverflowTaskID(t *testing.T) {
	d := newDemux()

	// Route before subscribing — goes to overflow.
	d.route(&message.Message{
		From: "developer", Type: message.TypeTaskResult,
		Parts:    []message.Part{message.TextPart{Text: "early-result"}},
		Metadata: map[string]string{"task_id": "late-task"},
	})
	// Route another task's message.
	d.route(&message.Message{
		From: "developer", Type: message.TypeTaskResult,
		Parts:    []message.Part{message.TextPart{Text: "other-result"}},
		Metadata: map[string]string{"task_id": "other-task"},
	})

	// Subscribe to late-task — should get the overflow message.
	ch := d.subscribe("late-task")

	select {
	case msg := <-ch:
		if extractText(msg) != "early-result" {
			t.Errorf("unexpected text: %q", extractText(msg))
		}
	default:
		t.Fatal("expected overflow message on channel")
	}

	// The other-task message should remain in overflow.
	d.mu.Lock()
	if len(d.overflow) != 1 {
		t.Errorf("expected 1 remaining overflow message, got %d", len(d.overflow))
	}
	d.mu.Unlock()
}

// TestDemuxNoMetadataGoesToOverflow verifies messages without metadata go to overflow.
func TestDemuxNoMetadataGoesToOverflow(t *testing.T) {
	d := newDemux()
	_ = d.subscribe("some-task")

	d.route(&message.Message{
		From:  "developer",
		Type:  message.TypeTaskResult,
		Parts: []message.Part{message.TextPart{Text: "no-meta"}},
	})

	d.mu.Lock()
	if len(d.overflow) != 1 {
		t.Errorf("expected 1 overflow message, got %d", len(d.overflow))
	}
	d.mu.Unlock()
}

// TestTryAcquireAgent verifies concurrency limit enforcement.
func TestTryAcquireAgent(t *testing.T) {
	s := &Scheduler{
		Agents: []runtime.AgentDefinition{
			{Name: "developer", MaxConcurrentTasks: 2},
		},
		inflight: make(map[string]int),
	}

	if !s.tryAcquireAgent("developer") {
		t.Fatal("first acquire should succeed")
	}
	if !s.tryAcquireAgent("developer") {
		t.Fatal("second acquire should succeed (limit=2)")
	}
	if s.tryAcquireAgent("developer") {
		t.Fatal("third acquire should fail (limit=2)")
	}

	s.releaseAgent("developer")
	if !s.tryAcquireAgent("developer") {
		t.Fatal("acquire after release should succeed")
	}
}

// TestTryAcquireAgentUnlimited verifies 0 means unlimited.
func TestTryAcquireAgentUnlimited(t *testing.T) {
	s := &Scheduler{
		Agents: []runtime.AgentDefinition{
			{Name: "developer", MaxConcurrentTasks: 0},
		},
		inflight: make(map[string]int),
	}

	for i := 0; i < 100; i++ {
		if !s.tryAcquireAgent("developer") {
			t.Fatalf("acquire %d should succeed with unlimited", i)
		}
	}
}

// TestTryAcquireAgentUnknown verifies unknown agents are treated as unlimited.
func TestTryAcquireAgentUnknown(t *testing.T) {
	s := &Scheduler{
		Agents:   []runtime.AgentDefinition{},
		inflight: make(map[string]int),
	}

	if !s.tryAcquireAgent("nonexistent") {
		t.Fatal("unknown agent should be unlimited")
	}
}

func TestSchedulerAssignsRemoteInstanceAndReleasesLease(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")
	store := controlplane.NewMemoryStore("test-fabric")

	ctx := context.Background()
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-a",
		State:           controlplane.NodeStateReady,
		Labels:          map[string]string{"role": "node", "region": "us-east"},
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterNode node-a: %v", err)
	}
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-b",
		State:           controlplane.NodeStateReady,
		Labels:          map[string]string{"role": "node", "region": "us-west"},
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterNode node-b: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterInstance ready: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-b/developer/1",
		Profile:         "developer",
		NodeID:          "node-b",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
		State:           controlplane.InstanceStateBusy,
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterInstance busy: %v", err)
	}

	graph := &taskgraph.TaskGraph{
		RequestID: "req-placement",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "developer",
				Description: "Implement the API",
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	s := &Scheduler{
		Comm:         conductorComm,
		ControlPlane: store,
		RequestID:    "req-placement",
		LeaseOwnerID: "conductor-test",
		Agents: []runtime.AgentDefinition{
			{Name: "developer"},
		},
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(runCtx, graph.Tasks[0], graph)
	}()

	select {
	case msg := <-agentComm.Receive(runCtx):
		if msg.Metadata["assigned_instance"] != "node-a/developer/1" {
			t.Fatalf("assigned instance = %q, want node-a/developer/1", msg.Metadata["assigned_instance"])
		}
		if msg.Metadata["execution_node"] != "node-a" {
			t.Fatalf("execution node = %q, want node-a", msg.Metadata["execution_node"])
		}
		if msg.Metadata["lease_epoch"] == "" {
			t.Fatal("expected lease_epoch metadata to be set")
		}

		agentComm.Send(runCtx, &message.Message{
			ID:        "r1",
			RequestID: "req-placement",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts:     []message.Part{message.TextPart{Text: "done"}},
			Metadata:  map[string]string{"task_id": "t1"},
			Timestamp: time.Now(),
		})
	case <-runCtx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-runCtx.Done():
		t.Fatal("timeout waiting for dispatchTask")
	}

	task := graph.Tasks[0]
	if task.AssignedInstance != "node-a/developer/1" {
		t.Fatalf("task assigned instance = %q, want node-a/developer/1", task.AssignedInstance)
	}
	if task.ExecutionNode != "node-a" {
		t.Fatalf("task execution node = %q, want node-a", task.ExecutionNode)
	}
	if task.LeaseEpoch == 0 {
		t.Fatal("expected task lease epoch to be set")
	}
	if _, ok, err := store.GetTaskLease(ctx, "req-placement", "t1"); err != nil {
		t.Fatalf("GetTaskLease: %v", err)
	} else if ok {
		t.Fatal("expected task lease to be released after dispatch completion")
	}
	if _, ok, err := store.GetProfileLease(ctx, "developer"); err != nil {
		t.Fatalf("GetProfileLease: %v", err)
	} else if ok {
		t.Fatal("expected profile lease to be released after dispatch completion")
	}
}

func TestSchedulerRedispatchesAfterAssignedInstanceExpires(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")
	store := controlplane.NewMemoryStore("test-fabric")

	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-a",
		State:           controlplane.NodeStateReady,
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterNode node-a: %v", err)
	}
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-b",
		State:           controlplane.NodeStateReady,
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterNode node-b: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstance node-a: %v", err)
	}
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-b/developer/1",
		Profile:         "developer",
		NodeID:          "node-b",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstance node-b: %v", err)
	}

	graph := &taskgraph.TaskGraph{
		RequestID: "req-recovery",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "developer",
				Description: "Implement the API",
				Status:      taskgraph.StatusPending,
			},
		},
	}

	s := &Scheduler{
		Comm:         conductorComm,
		ControlPlane: store,
		RequestID:    "req-recovery",
		LeaseOwnerID: "conductor-test",
		Agents: []runtime.AgentDefinition{
			{Name: "developer"},
		},
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Execute(runCtx, graph)
	}()

	msgCh := agentComm.Receive(runCtx)

	var firstMsg *message.Message
	select {
	case firstMsg = <-msgCh:
	case <-runCtx.Done():
		t.Fatal("timeout waiting for first dispatch")
	}
	if firstMsg.Metadata["assigned_instance"] != "node-a/developer/1" {
		t.Fatalf("first assigned instance = %q, want node-a/developer/1", firstMsg.Metadata["assigned_instance"])
	}

	staleAt := time.Now().UTC().Add(-controlplane.InstanceHeartbeatTTL - time.Second)
	if err := store.RegisterInstance(ctx, controlplane.AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
		State:           controlplane.InstanceStateReady,
		LastHeartbeatAt: staleAt,
	}); err != nil {
		t.Fatalf("RegisterInstance stale node-a: %v", err)
	}

	var secondMsg *message.Message
	select {
	case secondMsg = <-msgCh:
	case <-runCtx.Done():
		t.Fatal("timeout waiting for redispatch")
	}
	if secondMsg.Metadata["assigned_instance"] != "node-b/developer/1" {
		t.Fatalf("second assigned instance = %q, want node-b/developer/1", secondMsg.Metadata["assigned_instance"])
	}

	agentComm.Send(runCtx, &message.Message{
		ID:        "r1",
		RequestID: "req-recovery",
		From:      "developer",
		To:        "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "done"}},
		Metadata: map[string]string{
			"task_id":        "t1",
			"dispatch_nonce": secondMsg.Metadata["dispatch_nonce"],
		},
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-runCtx.Done():
		t.Fatal("timeout waiting for Execute")
	}

	task := graph.Tasks[0]
	if task.Status != taskgraph.StatusCompleted {
		t.Fatalf("task status = %q, want completed", task.Status)
	}
	if task.AssignedInstance != "node-b/developer/1" {
		t.Fatalf("task assigned instance = %q, want node-b/developer/1", task.AssignedInstance)
	}
}

func TestSchedulerSelectTaskInstanceHonorsRequiredNodeLabels(t *testing.T) {
	store := controlplane.NewMemoryStore("test-fabric")
	ctx := context.Background()

	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-a",
		State:           controlplane.NodeStateReady,
		Labels:          map[string]string{"region": "us-east", "accelerator": "gpu"},
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterNode node-a: %v", err)
	}
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-b",
		State:           controlplane.NodeStateReady,
		Labels:          map[string]string{"region": "us-west"},
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterNode node-b: %v", err)
	}

	instances := []controlplane.AgentInstance{
		{
			ID:              "node-a/developer/1",
			Profile:         "developer",
			NodeID:          "node-a",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: time.Now().UTC(),
		},
		{
			ID:              "node-b/developer/1",
			Profile:         "developer",
			NodeID:          "node-b",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: time.Now().UTC(),
		},
	}

	s := &Scheduler{
		ControlPlane: store,
		Agents: []runtime.AgentDefinition{
			{
				Name: "developer",
				RequiredNodeLabels: map[string]string{
					"accelerator": "gpu",
				},
			},
		},
	}

	instance, ok, err := s.selectTaskInstance(context.Background(), &taskgraph.TaskNode{Agent: "developer"}, instances)
	if err != nil {
		t.Fatalf("selectTaskInstance: %v", err)
	}
	if !ok {
		t.Fatal("expected instance selection to succeed")
	}
	if instance.ID != "node-a/developer/1" {
		t.Fatalf("selected instance = %q, want node-a/developer/1", instance.ID)
	}
}

func TestSchedulerSelectTaskInstanceHonorsNodeTaskCapacity(t *testing.T) {
	store := controlplane.NewMemoryStore("test-fabric")
	ctx := context.Background()

	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-a",
		State:           controlplane.NodeStateReady,
		Capacity:        controlplane.NodeCapacity{MaxTasks: 1},
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterNode node-a: %v", err)
	}
	if err := store.RegisterNode(ctx, controlplane.Node{
		ID:              "node-b",
		State:           controlplane.NodeStateReady,
		Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterNode node-b: %v", err)
	}

	instances := []controlplane.AgentInstance{
		{
			ID:              "node-a/developer/1",
			Profile:         "developer",
			NodeID:          "node-a",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: time.Now().UTC(),
		},
		{
			ID:              "node-b/developer/1",
			Profile:         "developer",
			NodeID:          "node-b",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: time.Now().UTC(),
		},
	}

	s := &Scheduler{
		ControlPlane:     store,
		instanceInFlight: map[string]int{"node-a/developer/1": 1},
		nodeInFlight:     map[string]int{"node-a": 1},
	}

	instance, ok, err := s.selectTaskInstance(context.Background(), &taskgraph.TaskNode{Agent: "developer"}, instances)
	if err != nil {
		t.Fatalf("selectTaskInstance: %v", err)
	}
	if !ok {
		t.Fatal("expected instance selection to succeed")
	}
	if instance.ID != "node-b/developer/1" {
		t.Fatalf("selected instance = %q, want node-b/developer/1", instance.ID)
	}
}

func TestSchedulerSelectTaskInstanceSpreadsRequestAcrossNodes(t *testing.T) {
	store := controlplane.NewMemoryStore("test-fabric")
	ctx := context.Background()
	now := time.Now().UTC()

	for _, nodeID := range []string{"node-a", "node-b"} {
		if err := store.RegisterNode(ctx, controlplane.Node{
			ID:              nodeID,
			State:           controlplane.NodeStateReady,
			Capacity:        controlplane.NodeCapacity{MaxTasks: 2},
			LastHeartbeatAt: now,
		}); err != nil {
			t.Fatalf("RegisterNode %s: %v", nodeID, err)
		}
	}

	instances := []controlplane.AgentInstance{
		{
			ID:              "node-a/designer/1",
			Profile:         "designer",
			NodeID:          "node-a",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: now,
		},
		{
			ID:              "node-b/designer/1",
			Profile:         "designer",
			NodeID:          "node-b",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: now,
		},
	}

	graph := &taskgraph.TaskGraph{
		RequestID: "req-spread",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:            "t1",
				Agent:         "architect",
				Status:        taskgraph.StatusCompleted,
				ExecutionNode: "node-a",
			},
			{
				ID:     "t2",
				Agent:  "designer",
				Status: taskgraph.StatusPending,
			},
		},
	}

	s := &Scheduler{
		ControlPlane: store,
		graph:        graph,
	}

	instance, ok, err := s.selectTaskInstance(context.Background(), graph.Tasks[1], instances)
	if err != nil {
		t.Fatalf("selectTaskInstance: %v", err)
	}
	if !ok {
		t.Fatal("expected instance selection to succeed")
	}
	if instance.ID != "node-b/designer/1" {
		t.Fatalf("selected instance = %q, want node-b/designer/1", instance.ID)
	}
}

type blockingListNodesStore struct {
	controlplane.Store
}

func (s *blockingListNodesStore) ListNodes(ctx context.Context) ([]controlplane.Node, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSchedulerSelectTaskInstanceHonorsContextDuringNodeLookup(t *testing.T) {
	store := &blockingListNodesStore{
		Store: controlplane.NewMemoryStore("test-fabric"),
	}

	instances := []controlplane.AgentInstance{
		{
			ID:              "node-a/developer/1",
			Profile:         "developer",
			NodeID:          "node-a",
			Endpoint:        runtime.Endpoint{Address: "10.0.0.1:6001"},
			State:           controlplane.InstanceStateReady,
			LastHeartbeatAt: time.Now().UTC(),
		},
	}

	s := &Scheduler{
		ControlPlane: store,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := s.selectTaskInstance(ctx, &taskgraph.TaskNode{Agent: "developer"}, instances)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("selectTaskInstance error = %v, want context canceled", err)
	}
}

func TestCommonDirPrefixMixed(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  string
	}{
		{
			name:  "shared_dir",
			paths: []string{"a/b/c.txt", "a/b/d.txt"},
			want:  "a/b/",
		},
		{
			name:  "no_shared_dir",
			paths: []string{"x/foo.txt", "y/bar.txt"},
			want:  "",
		},
		{
			name:  "single_path",
			paths: []string{"a/b/c.txt"},
			want:  "a/b/",
		},
		{
			name:  "deep_vs_shallow",
			paths: []string{"a/b/c/d.txt", "a/b/e.txt"},
			want:  "a/b/",
		},
		{
			name:  "empty",
			paths: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() {
				done <- commonDirPrefix(tt.paths)
			}()
			select {
			case got := <-done:
				if got != tt.want {
					t.Errorf("commonDirPrefix(%v) = %q, want %q", tt.paths, got, tt.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("commonDirPrefix(%v) hung", tt.paths)
			}
		})
	}
}

func TestDispatchTaskTruncatesResult(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	longResult := strings.Repeat("x", 5000)

	// No ArtifactFiles → filterArtifacts returns nil → hits the fallback DataPart path
	// where truncation logic applies.
	graph := &taskgraph.TaskGraph{
		RequestID: "req-trunc",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "architect",
				Description: "Design the API",
				Status:      taskgraph.StatusCompleted,
				Result:      longResult,
				ArtifactURI: "artifacts/architect/",
			},
			{
				ID:          "t2",
				Agent:       "developer",
				Description: "Implement the API",
				DependsOn:   []string{"t1"},
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-trunc",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(ctx, graph.Tasks[1], graph)
	}()

	select {
	case msg := <-agentComm.Receive(ctx):
		// Find the dependency DataPart.
		for _, p := range msg.Parts {
			if dp, ok := p.(message.DataPart); ok {
				if dp.Data["dependency_id"] == "t1" {
					result, _ := dp.Data["result"].(string)
					if len(result) >= len(longResult) {
						t.Errorf("result should be truncated, got %d chars (original %d)", len(result), len(longResult))
					}
					if !strings.Contains(result, "truncated") {
						t.Error("truncated result should contain truncation marker")
					}
					// Verify artifact_uri is present.
					uri, _ := dp.Data["artifact_uri"].(string)
					if uri != "artifacts/architect/" {
						t.Errorf("artifact_uri: got %q", uri)
					}
				}
			}
		}

		// Send back result to complete dispatch.
		agentComm.Send(ctx, &message.Message{
			ID:        "r1",
			RequestID: "req-trunc",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts:     []message.Part{message.TextPart{Text: "done"}},
			Metadata:  map[string]string{"task_id": "t2"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for dispatchTask")
	}
}

func TestDispatchTaskPassesArtifactFilesInDataPart(t *testing.T) {
	hub := local.NewHub()
	conductorComm := hub.Register("conductor")
	agentComm := hub.Register("developer")

	// Use only summary.md — it gets filtered out by filterArtifacts,
	// falling through to the fallback DataPart path where artifact_files is populated.
	graph := &taskgraph.TaskGraph{
		RequestID: "req-af",
		Tasks: []*taskgraph.TaskNode{
			{
				ID:          "t1",
				Agent:       "architect",
				Description: "Design the API",
				Status:      taskgraph.StatusCompleted,
				Result:      "short result",
				ArtifactURI: "artifacts/architect/",
				ArtifactFiles: []taskgraph.ArtifactFile{
					{URI: "artifacts/architect/summary.md", MimeType: "text/markdown", Name: "summary.md"},
				},
			},
			{
				ID:          "t2",
				Agent:       "developer",
				Description: "Implement the API",
				DependsOn:   []string{"t1"},
				Status:      taskgraph.StatusRunning,
			},
		},
	}

	s := &Scheduler{
		Comm:      conductorComm,
		RequestID: "req-af",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.dispatchTask(ctx, graph.Tasks[1], graph)
	}()

	select {
	case msg := <-agentComm.Receive(ctx):
		for _, p := range msg.Parts {
			if dp, ok := p.(message.DataPart); ok {
				if dp.Data["dependency_id"] == "t1" {
					files, ok := dp.Data["artifact_files"]
					if !ok {
						t.Error("expected artifact_files in DataPart")
					} else {
						names, ok := files.([]string)
						if !ok || len(names) == 0 {
							t.Error("artifact_files should be []string with entries")
						} else if names[0] != "summary.md" {
							t.Errorf("artifact_files[0]: got %q, want summary.md", names[0])
						}
					}
				}
			}
		}

		agentComm.Send(ctx, &message.Message{
			ID:        "r1",
			RequestID: "req-af",
			From:      "developer",
			To:        "conductor",
			Type:      message.TypeTaskResult,
			Parts:     []message.Part{message.TextPart{Text: "done"}},
			Metadata:  map[string]string{"task_id": "t2"},
			Timestamp: time.Now(),
		})
	case <-ctx.Done():
		t.Fatal("timeout waiting for task dispatch")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dispatchTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for dispatchTask")
	}
}

func TestFilterArtifactsKeepsMockupWhenCompanionsExist(t *testing.T) {
	dep := &taskgraph.TaskNode{
		ArtifactFiles: []taskgraph.ArtifactFile{
			{URI: "artifacts/designer/mockup.html", MimeType: "text/html", Name: "mockup.html"},
			{URI: "artifacts/designer/spec.md", MimeType: "text/markdown", Name: "spec.md"},
			{URI: "artifacts/designer/screenshots/mockup.png", MimeType: "image/png", Name: "screenshots/mockup.png"},
		},
	}

	got := filterArtifacts(dep)
	names := make([]string, 0, len(got))
	for _, fp := range got {
		names = append(names, fp.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "mockup.html") {
		t.Fatalf("expected mockup.html to remain when companions exist, got %v", names)
	}
	if !strings.Contains(joined, "spec.md") {
		t.Fatalf("expected spec.md to remain, got %v", names)
	}
}

func TestFilterArtifactsKeepsMockupAsFallback(t *testing.T) {
	dep := &taskgraph.TaskNode{
		ArtifactFiles: []taskgraph.ArtifactFile{
			{URI: "artifacts/designer/mockup.html", MimeType: "text/html", Name: "mockup.html"},
		},
	}

	got := filterArtifacts(dep)
	if len(got) != 1 || got[0].Name != "mockup.html" {
		t.Fatalf("expected mockup.html fallback to remain, got %+v", got)
	}
}

func TestEnrichLoopGuidelines(t *testing.T) {
	s := &Scheduler{
		Agents: []runtime.AgentDefinition{
			{Name: "architect", ReviewGuidelines: "- Check imports\n- Verify structure"},
			{Name: "developer"},
		},
	}

	def := &loop.LoopDefinition{
		ID: "test-loop",
		States: []loop.StateConfig{
			{Name: "WORKING", Agent: "developer"},
			{Name: "REVIEWING", Agent: "architect"},
			{Name: "APPROVED"},
		},
	}

	s.enrichLoopGuidelines(def)

	// Architect state should have guidelines.
	if def.States[1].ReviewGuidelines != "- Check imports\n- Verify structure" {
		t.Errorf("architect state: got %q", def.States[1].ReviewGuidelines)
	}

	// Developer state should remain empty (no ReviewGuidelines in definition).
	if def.States[0].ReviewGuidelines != "" {
		t.Errorf("developer state: expected empty, got %q", def.States[0].ReviewGuidelines)
	}

	// Terminal state should remain empty (no agent).
	if def.States[2].ReviewGuidelines != "" {
		t.Errorf("terminal state: expected empty, got %q", def.States[2].ReviewGuidelines)
	}
}

func TestBuildKnowledgeContextDecisionBypass(t *testing.T) {
	// Simulates the cross-request scenario:
	// Request 1: "Build a to-do app" — architect decides "Use Material Design 3"
	// Request 2: "Add notifications" — developer task has ZERO keyword overlap with MD3
	// The MD3 decision must still appear in the developer's knowledge context.

	sharedGraph := knowledge.NewGraph()
	sharedGraph.Nodes["architect/md3-decision"] = &knowledge.Node{
		ID:      "architect/md3-decision",
		Agent:   "architect",
		Title:   "Use Material Design 3",
		Summary: "All UI components must follow Material Design 3 guidelines",
		Tags:    []string{"decision", "design-system"},
		TTLDays: 0, // decisions never expire
	}
	sharedGraph.Nodes["architect/api-design"] = &knowledge.Node{
		ID:      "architect/api-design",
		Agent:   "architect",
		Title:   "REST API Design",
		Summary: "REST API with endpoints for todo CRUD",
		Tags:    []string{"api"},
	}

	s := &Scheduler{
		KnowledgeGraph: sharedGraph,
	}

	// Developer task with zero keyword overlap with "Material Design 3".
	task := &taskgraph.TaskNode{
		ID:          "t-dev",
		Agent:       "developer",
		Description: "Add notifications feature with push alerts",
	}

	parts := s.buildKnowledgeContext(task)
	if len(parts) == 0 {
		t.Fatal("expected knowledge context parts, got none")
	}

	// Extract the data part and check for the decision node.
	dp, ok := parts[0].(message.DataPart)
	if !ok {
		t.Fatal("expected DataPart")
	}
	relatedRaw, ok := dp.Data["related_knowledge"]
	if !ok {
		t.Fatal("expected related_knowledge key in data")
	}
	related, ok := relatedRaw.([]map[string]any)
	if !ok {
		t.Fatalf("unexpected type for related_knowledge: %T", relatedRaw)
	}

	foundMD3 := false
	for _, entry := range related {
		if id, _ := entry["id"].(string); id == "architect/md3-decision" {
			foundMD3 = true
			break
		}
	}
	if !foundMD3 {
		t.Error("MD3 decision node should appear despite zero keyword overlap — decision bypass failed")
	}
}

func TestBuildKnowledgeContextSupersededDecisionExcluded(t *testing.T) {
	sharedGraph := knowledge.NewGraph()
	sharedGraph.Nodes["arch/bootstrap"] = &knowledge.Node{
		ID:      "arch/bootstrap",
		Agent:   "architect",
		Title:   "Use Bootstrap",
		Summary: "Bootstrap CSS framework",
		Tags:    []string{"decision", "design-system"},
		TTLDays: 0,
	}
	sharedGraph.Nodes["arch/md3"] = &knowledge.Node{
		ID:      "arch/md3",
		Agent:   "architect",
		Title:   "Use Material Design 3",
		Summary: "MD3 for all UI",
		Tags:    []string{"decision", "design-system"},
		TTLDays: 0,
	}
	sharedGraph.Edges = []*knowledge.Edge{
		{From: "arch/md3", To: "arch/bootstrap", Relation: "supersedes"},
	}

	s := &Scheduler{
		KnowledgeGraph: sharedGraph,
	}

	task := &taskgraph.TaskNode{
		ID:          "t-dev",
		Agent:       "developer",
		Description: "Add notifications feature",
	}

	parts := s.buildKnowledgeContext(task)
	if len(parts) == 0 {
		t.Fatal("expected knowledge context parts")
	}

	dp := parts[0].(message.DataPart)
	relatedRaw, _ := dp.Data["related_knowledge"]
	related, _ := relatedRaw.([]map[string]any)

	for _, entry := range related {
		if id, _ := entry["id"].(string); id == "arch/bootstrap" {
			t.Error("superseded decision (Bootstrap) should NOT appear in knowledge context")
		}
	}

	foundMD3 := false
	for _, entry := range related {
		if id, _ := entry["id"].(string); id == "arch/md3" {
			foundMD3 = true
		}
	}
	if !foundMD3 {
		t.Error("active decision (MD3) should appear in knowledge context")
	}
}
