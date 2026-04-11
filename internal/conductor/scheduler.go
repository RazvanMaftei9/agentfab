package conductor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/loop"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

type UserQuery struct {
	AgentName  string
	TaskID     string
	Question   string
	ResponseCh chan string
}

// Scheduler dispatches tasks from a DAG and collects results.
type Scheduler struct {
	Comm           message.MessageCommunicator
	ControlPlane   controlplane.Store
	Logger         *message.Logger
	Storage        runtime.Storage
	Meter          runtime.ExtendedMeter
	StorageFactory func(string) runtime.Storage
	RequestID      string
	Events         event.Bus
	Agents         []runtime.AgentDefinition
	UserRequest    string
	LeaseOwnerID   string
	KnowledgeGraph *knowledge.Graph
	AgentGraphs    map[string]*knowledge.Graph
	// KnowledgeGenerate is used for incremental knowledge extraction while a request
	// is still running. It should point to the conductor's generate function.
	KnowledgeGenerate func(context.Context, []*schema.Message) (*schema.Message, error)
	// Router is an optional adaptive router for tier-based model escalation on failure.
	Router interface {
		Escalate(agent, taskID string) bool
		RecordOutcome(agent, taskID string, success bool)
	}

	demux       *demux             // routes incoming messages to per-agent waiters
	demuxOnce   sync.Once          // ensures demux starts exactly once
	demuxCancel context.CancelFunc // stops the demux goroutine

	graph        *taskgraph.TaskGraph // stored reference for external access
	taskCancelMu sync.Mutex
	taskCancels  map[string]context.CancelFunc
	UserQueryCh  chan *UserQuery           // agent queries forwarded here
	graphReplace chan *taskgraph.TaskGraph // for restructuring

	pauseCh       chan struct{}      // closed to signal pause
	resumeCh      chan struct{}      // closed to signal resume
	reqCancel     context.CancelFunc // cancels the per-request context
	paused        atomic.Bool
	restructuring atomic.Bool // set during graph restructuring to suppress error logging

	knowledgeMu        sync.RWMutex
	knowledgeRefreshMu sync.Mutex

	inflightMu       sync.Mutex
	inflight         map[string]int // agent name → count of currently dispatched tasks
	instanceInFlight map[string]int
	nodeInFlight     map[string]int

	nonceMu    sync.Mutex
	taskNonces map[string]string // task ID → expected dispatch nonce
}

const (
	taskLeaseTTL              = 30 * time.Second
	taskLeaseRenewInterval    = 10 * time.Second
	taskHealthCheckInterval   = 2 * time.Second
	profileLeaseTTL           = 30 * time.Second
	profileLeaseRenewInterval = 10 * time.Second
	taskProgressHeartbeat     = 6 * time.Second
	nodeLookupTimeout         = 2 * time.Second
)

type recoverableDispatchError struct {
	reason string
}

func (e recoverableDispatchError) Error() string {
	return e.reason
}

func isRecoverableDispatchError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(recoverableDispatchError)
	return ok
}

// demux routes messages to per-task subscribers by task_id (not agent name)
// to prevent deadlocks when concurrent tasks target the same agent.
type demux struct {
	mu       sync.Mutex
	waiters  map[string]chan *message.Message // task_id → waiter channel
	overflow []*message.Message               // messages received before a waiter registered
}

func newDemux() *demux {
	return &demux{waiters: make(map[string]chan *message.Message)}
}

func (d *demux) subscribe(taskID string) chan *message.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch := make(chan *message.Message, 16)
	d.waiters[taskID] = ch
	remaining := d.overflow[:0]
	for _, msg := range d.overflow {
		if msg.Metadata != nil && msg.Metadata["task_id"] == taskID {
			ch <- msg
		} else {
			remaining = append(remaining, msg)
		}
	}
	d.overflow = remaining
	return ch
}

func (d *demux) unsubscribe(taskID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.waiters, taskID)
	remaining := d.overflow[:0]
	for _, msg := range d.overflow {
		if msg.Metadata == nil || msg.Metadata["task_id"] != taskID {
			remaining = append(remaining, msg)
		}
	}
	d.overflow = remaining
}

func (d *demux) clearOverflow(taskID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	remaining := d.overflow[:0]
	for _, msg := range d.overflow {
		if msg.Metadata == nil || msg.Metadata["task_id"] != taskID {
			remaining = append(remaining, msg)
		}
	}
	d.overflow = remaining
}

func (d *demux) route(msg *message.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	taskID := ""
	if msg.Metadata != nil {
		taskID = msg.Metadata["task_id"]
	}
	if taskID != "" {
		if ch, ok := d.waiters[taskID]; ok {
			select {
			case ch <- msg:
			default:
				d.overflow = append(d.overflow, msg)
			}
			return
		}
	}
	d.overflow = append(d.overflow, msg)
}

func (s *Scheduler) ensureDemux(ctx context.Context) {
	s.demuxOnce.Do(func() {
		s.demux = newDemux()
		demuxCtx, cancel := context.WithCancel(ctx)
		s.demuxCancel = cancel
		go s.runDemux(demuxCtx)
	})
}

func (s *Scheduler) stopDemux() {
	if s.demuxCancel != nil {
		s.demuxCancel()
	}
}

func (s *Scheduler) runDemux(ctx context.Context) {
	ch := s.Comm.Receive(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if msg.Type == message.TypeStatusUpdate && s.Events != nil {
				for _, p := range msg.Parts {
					if tp, ok := p.(message.TextPart); ok && tp.Text != "" {
						s.Events.Emit(event.Event{
							Type:         event.TaskProgress,
							TaskAgent:    msg.From,
							ProgressText: tp.Text,
						})
						break
					}
				}
			}
			s.demux.route(msg)
		}
	}
}

// tryAcquireAgent returns true (and increments) if the agent is below its concurrency limit.
func (s *Scheduler) tryAcquireAgent(agent string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	limit := s.agentLimit(agent)
	if limit > 0 && s.inflight[agent] >= limit {
		return false
	}
	s.inflight[agent]++
	return true
}

func (s *Scheduler) releaseAgent(agent string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight[agent] > 0 {
		s.inflight[agent]--
	}
}

func (s *Scheduler) agentLimit(agent string) int {
	for _, def := range s.Agents {
		if def.Name == agent {
			return def.MaxConcurrentTasks
		}
	}
	return 0
}

func taskProfile(task *taskgraph.TaskNode) string {
	if task == nil {
		return ""
	}
	return task.TargetProfile()
}

func taskExecutionTarget(task *taskgraph.TaskNode) string {
	if task == nil {
		return ""
	}
	return task.ExecutionTarget()
}

// enrichLoopGuidelines copies ReviewPrompt and ReviewGuidelines from agent
// definitions into loop state configs for review context injection.
func (s *Scheduler) enrichLoopGuidelines(def *loop.LoopDefinition) {
	for i, sc := range def.States {
		if sc.Agent == "" {
			continue
		}
		for _, ad := range s.Agents {
			if ad.Name != sc.Agent {
				continue
			}
			var parts []string
			if ad.ReviewPrompt != "" {
				parts = append(parts, ad.ReviewPrompt)
			}
			if ad.ReviewGuidelines != "" {
				parts = append(parts, ad.ReviewGuidelines)
			}
			if len(parts) > 0 {
				def.States[i].ReviewGuidelines = strings.Join(parts, "\n")
			}
			break
		}
	}
}

// Execute runs the task graph to completion.
func (s *Scheduler) Execute(ctx context.Context, graph *taskgraph.TaskGraph) error {
	defer s.stopDemux()
	slog.Info("scheduler starting", "request_id", graph.RequestID, "tasks", len(graph.Tasks))

	s.graph = graph
	s.taskCancels = make(map[string]context.CancelFunc)
	s.inflight = make(map[string]int)
	s.instanceInFlight = make(map[string]int)
	s.nodeInFlight = make(map[string]int)
	s.taskNonces = make(map[string]string)
	if s.UserQueryCh == nil {
		s.UserQueryCh = make(chan *UserQuery, 1)
	}
	if s.graphReplace == nil {
		s.graphReplace = make(chan *taskgraph.TaskGraph, 1)
	}
	s.pauseCh = make(chan struct{})
	s.resumeCh = make(chan struct{})

	// Start demux goroutine to fan out messages to per-agent waiters.
	s.ensureDemux(ctx)
	s.syncGraphState(ctx, graph)

	// Write initial graph status.
	s.writeGraphStatus(ctx, graph)

	// Channel-based async dispatch: tasks complete individually and newly-
	// unlocked tasks dispatch immediately (no batch synchronization).
	doneCh := make(chan string, len(graph.Tasks))
	pending := 0                    // goroutines in flight
	dispatched := map[string]bool{} // tracks already-dispatched task IDs

	for !graph.AllDone() {
		// Pause gate: if paused, block until resumed or context cancelled.
		// We read s.pauseCh and s.resumeCh into locals because Resume()
		// replaces both fields after closing the old channels. If we read
		// the struct fields inside nested selects, we can observe the NEW
		// (open) channel and deadlock.
		pauseCh := s.pauseCh
		resumeCh := s.resumeCh
		select {
		case <-pauseCh:
			select {
			case <-resumeCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			// Drain all in-flight goroutines before re-dispatching.
			// Their contexts were cancelled by Pause's CancelAllRunning,
			// so they exit quickly.
			for pending > 0 {
				select {
				case <-doneCh:
					pending--
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			dispatched = map[string]bool{}
		default:
		}

		// Check for graph replacement (non-blocking).
		select {
		case newGraph := <-s.graphReplace:
			graph = newGraph
			s.graph = graph
			s.restructuring.Store(false)
			s.writeGraphStatus(ctx, graph)
			dispatched = map[string]bool{}
			continue
		default:
		}

		// Dispatch any ready tasks.
		ready := graph.ReadyTasks()
		newDispatches := 0
		for _, task := range ready {
			if dispatched[task.ID] {
				continue
			}
			profile := taskProfile(task)
			if !s.tryAcquireAgent(profile) {
				continue // agent at capacity, try next iteration
			}
			dispatched[task.ID] = true
			newDispatches++
			pending++
			task.Status = taskgraph.StatusRunning
			s.syncTaskState(ctx, task)
			s.logTaskStatus(ctx, task, "")
			s.writeGraphStatus(ctx, graph)
			startEvt := event.Event{
				Type:            event.TaskStart,
				TaskID:          task.ID,
				TaskAgent:       profile,
				ExecutionNode:   task.ExecutionNode,
				TaskDescription: task.Description,
			}
			s.attachUsage(ctx, &startEvt)
			s.Events.Emit(startEvt)
			// Register the task cancel BEFORE launching the goroutine so
			// CancelAllRunning (called by Pause) is guaranteed to find it
			// even if the goroutine hasn't started yet.
			taskCtx, taskCancel := context.WithCancel(ctx)
			s.registerTaskCancel(task.ID, taskCancel)
			go func(t *taskgraph.TaskNode, tCtx context.Context, tCancel context.CancelFunc) {
				defer func() { doneCh <- t.ID }()
				profile := taskProfile(t)
				defer s.releaseAgent(profile)
				defer func() {
					tCancel()
					s.unregisterTaskCancel(t.ID)
				}()
				start := time.Now()
				inBefore, outBefore := s.agentUsageSnapshot(ctx, profile)
				var err error
				if t.LoopID != "" {
					err = s.dispatchLoopTask(tCtx, t, graph)
				} else {
					err = s.dispatchTask(tCtx, t, graph)
				}
				inAfter, outAfter := s.agentUsageSnapshot(ctx, profile)
				if err != nil {
					if t.Status == taskgraph.StatusCancelled {
						return
					}
					if t.Status == taskgraph.StatusPending {
						return
					}
					if s.restructuring.Load() {
						return
					}
					if s.paused.Load() {
						return
					}
					slog.Error("task dispatch failed", "task", t.ID, "error", err)
					t.Status = taskgraph.StatusFailed
					t.Result = err.Error()
					s.syncTaskState(ctx, t)
					s.logTaskStatus(ctx, t, err.Error())
					s.writeGraphStatus(ctx, graph)
					failEvt := event.Event{
						Type:              event.TaskFailed,
						TaskID:            t.ID,
						TaskAgent:         profile,
						Duration:          time.Since(start),
						ErrMsg:            err.Error(),
						AgentInputTokens:  inAfter - inBefore,
						AgentOutputTokens: outAfter - outBefore,
					}
					s.attachUsage(ctx, &failEvt)
					s.Events.Emit(failEvt)
					return
				}
				s.syncTaskState(ctx, t)
				s.logTaskStatus(ctx, t, "")
				s.writeGraphStatus(ctx, graph)
				if t.Status == taskgraph.StatusFailed {
					// Adaptive routing: try escalating to a higher-tier model.
					if s.Router != nil {
						s.Router.RecordOutcome(profile, t.ID, false)
						if s.Router.Escalate(profile, t.ID) {
							slog.Info("adaptive routing: escalating model tier", "task", t.ID, "agent", profile)
							t.Status = taskgraph.StatusPending
							t.Result = ""
							s.syncTaskState(ctx, t)
							return // task will be retried on next iteration
						}
					}
					failEvt := event.Event{
						Type:              event.TaskFailed,
						TaskID:            t.ID,
						TaskAgent:         profile,
						Duration:          time.Since(start),
						ErrMsg:            t.Result,
						AgentInputTokens:  inAfter - inBefore,
						AgentOutputTokens: outAfter - outBefore,
					}
					s.attachUsage(ctx, &failEvt)
					s.Events.Emit(failEvt)
				} else {
					if s.Router != nil {
						s.Router.RecordOutcome(profile, t.ID, true)
					}
					doneEvt := event.Event{
						Type:              event.TaskComplete,
						TaskID:            t.ID,
						TaskAgent:         profile,
						Duration:          time.Since(start),
						AgentInputTokens:  inAfter - inBefore,
						AgentOutputTokens: outAfter - outBefore,
						ResultSummary:     taskResultSummary(t.Result, 0),
					}
					s.attachUsage(ctx, &doneEvt)
					s.Events.Emit(doneEvt)
					s.scheduleKnowledgeRefresh(ctx, graph, t)
				}
			}(task, taskCtx, taskCancel)
		}

		// If nothing in flight and no ready tasks were dispatched, check for deadlock.
		if pending == 0 && newDispatches == 0 && !graph.AllDone() {
			if len(ready) > 0 {
				// All ready tasks blocked by concurrency limits — back off.
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return fmt.Errorf("deadlock: no ready tasks but graph not done")
		}

		// If nothing new dispatched and there are in-flight tasks, wait for
		// at least one to complete before re-checking ready tasks.
		if pending > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case newGraph := <-s.graphReplace:
				graph = newGraph
				s.graph = graph
				s.restructuring.Store(false)
				s.writeGraphStatus(ctx, graph)
				dispatched = map[string]bool{}
				continue
			case taskID := <-doneCh:
				pending--
				delete(dispatched, taskID)
				// Cascade failures for the completed task.
				if t := graph.Get(taskID); t != nil && t.Status == taskgraph.StatusFailed {
					for _, id := range graph.FailDependents(taskID) {
						dep := graph.Get(id)
						s.syncTaskState(ctx, dep)
						s.logTaskStatus(ctx, dep, dep.Result)
						cascadeEvt := event.Event{
							Type:      event.TaskFailed,
							TaskID:    id,
							TaskAgent: dep.TargetProfile(),
							ErrMsg:    dep.Result,
						}
						s.attachUsage(ctx, &cascadeEvt)
						s.Events.Emit(cascadeEvt)
					}
					s.writeGraphStatus(ctx, graph)
				}
			}

			// Drain any other completed tasks (non-blocking).
		drainLoop:
			for {
				select {
				case taskID := <-doneCh:
					pending--
					delete(dispatched, taskID)
					if t := graph.Get(taskID); t != nil && t.Status == taskgraph.StatusFailed {
						for _, id := range graph.FailDependents(taskID) {
							dep := graph.Get(id)
							s.syncTaskState(ctx, dep)
							s.logTaskStatus(ctx, dep, dep.Result)
							cascadeEvt := event.Event{
								Type:      event.TaskFailed,
								TaskID:    id,
								TaskAgent: dep.TargetProfile(),
								ErrMsg:    dep.Result,
							}
							s.attachUsage(ctx, &cascadeEvt)
							s.Events.Emit(cascadeEvt)
						}
						s.writeGraphStatus(ctx, graph)
					}
				default:
					break drainLoop
				}
			}
		}
	}

	// Write final graph status.
	s.writeGraphStatus(ctx, graph)
	s.syncGraphState(ctx, graph)

	slog.Info("scheduler complete", "request_id", graph.RequestID, "has_failures", graph.HasFailures())
	return nil
}

// logTaskStatus writes a status_update message to the JSONL interaction log.
func (s *Scheduler) logTaskStatus(ctx context.Context, task *taskgraph.TaskNode, errMsg string) {
	if s.Logger == nil {
		return
	}

	metadata := map[string]string{
		"task_id": task.ID,
		"status":  string(task.Status),
	}
	if errMsg != "" {
		metadata["error"] = errMsg
	}
	if task.ArtifactURI != "" {
		metadata["artifact_uri"] = task.ArtifactURI
	}

	msg := &message.Message{
		ID:        uuid.New().String(),
		RequestID: s.RequestID,
		From:      "conductor",
		To:        taskProfile(task),
		Type:      message.TypeStatusUpdate,
		Parts: []message.Part{
			message.TextPart{Text: fmt.Sprintf("Task %s: %s", task.ID, task.Status)},
		},
		Metadata:  metadata,
		Timestamp: time.Now(),
	}

	if err := s.Logger.Log(ctx, msg); err != nil {
		slog.Warn("failed to log task status", "task", task.ID, "error", err)
	}
}

// graphStatusEntry is the JSON structure for the status file.
type graphStatusEntry struct {
	RequestID string            `json:"request_id"`
	UpdatedAt time.Time         `json:"updated_at"`
	Tasks     []taskStatusEntry `json:"tasks"`
}

type taskStatusEntry struct {
	ID               string `json:"id"`
	Agent            string `json:"agent"`
	AssignedInstance string `json:"assigned_instance,omitempty"`
	ExecutionNode    string `json:"execution_node,omitempty"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	ArtifactURI      string `json:"artifact_uri,omitempty"`
}

// writeGraphStatus writes the current task graph state to a status.json file
// that can be polled to monitor progress mid-execution.
func (s *Scheduler) writeGraphStatus(ctx context.Context, graph *taskgraph.TaskGraph) {
	if s.Storage == nil {
		return
	}

	entry := graphStatusEntry{
		RequestID: s.RequestID,
		UpdatedAt: time.Now(),
		Tasks:     make([]taskStatusEntry, len(graph.Tasks)),
	}
	for i, t := range graph.Tasks {
		te := taskStatusEntry{
			ID:               t.ID,
			Agent:            t.TargetProfile(),
			AssignedInstance: t.AssignedInstance,
			ExecutionNode:    t.ExecutionNode,
			Description:      t.Description,
			Status:           string(t.Status),
			ArtifactURI:      t.ArtifactURI,
		}
		if t.Status == taskgraph.StatusFailed && t.Result != "" {
			te.Error = t.Result
		}
		entry.Tasks[i] = te
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		slog.Warn("failed to marshal graph status", "error", err)
		return
	}

	path := fmt.Sprintf("logs/%s_status.json", s.RequestID)
	if err := s.Storage.Write(ctx, runtime.TierShared, path, data); err != nil {
		slog.Warn("failed to write graph status", "error", err)
	}
}

// attachUsage populates cumulative token fields on an event from the Meter.
func (s *Scheduler) attachUsage(ctx context.Context, e *event.Event) {
	if s.Meter == nil {
		return
	}
	usage, _ := s.Meter.AggregateUsage(ctx)
	e.InputTokens = usage.InputTokens
	e.OutputTokens = usage.OutputTokens
	e.CacheReadTokens = usage.CacheReadTokens
	e.TotalCalls = usage.TotalCalls
}

// recordRemoteUsage feeds token usage from a distributed agent's result message
// into the conductor's meter so that aggregate totals include all agents.
// In local mode, the agent shares the conductor's meter, so usage is already
// recorded — the TokenUsage on the message will match what's in the meter and
// this becomes a harmless no-op (we check for duplicates via TotalCalls).
func (s *Scheduler) recordRemoteUsage(ctx context.Context, result *message.Message) {
	if s.Meter == nil || result.TokenUsage == nil || result.TokenUsage.TotalCalls == 0 {
		return
	}
	tu := result.TokenUsage

	// In local mode, the agent's LLM calls are already recorded in the shared
	// meter. Skip to avoid double-counting.
	existing, _ := s.Meter.Usage(ctx, result.From)
	if existing.TotalCalls > 0 {
		return
	}

	// Record a synthetic LLMCallRecord representing the agent's cumulative usage.
	s.Meter.Record(ctx, runtime.LLMCallRecord{
		AgentName:       result.From,
		RequestID:       result.RequestID,
		Model:           tu.Model,
		InputTokens:     tu.InputTokens,
		OutputTokens:    tu.OutputTokens,
		CacheReadTokens: tu.CacheReadTokens,
		Timestamp:       result.Timestamp,
	})

	// If the agent made multiple calls, record additional empty records to get
	// the call count right. The first record above carries all the tokens.
	for i := int64(1); i < tu.TotalCalls; i++ {
		s.Meter.Record(ctx, runtime.LLMCallRecord{
			AgentName: result.From,
			RequestID: result.RequestID,
			Model:     tu.Model,
			Timestamp: result.Timestamp,
		})
	}
}

// agentUsageSnapshot returns the current usage for an agent (for computing deltas).
func (s *Scheduler) agentUsageSnapshot(ctx context.Context, agent string) (in, out int64) {
	if s.Meter == nil {
		return 0, 0
	}
	usage, _ := s.Meter.Usage(ctx, agent)
	return usage.InputTokens, usage.OutputTokens
}

func (s *Scheduler) dispatchTask(ctx context.Context, task *taskgraph.TaskNode, graph *taskgraph.TaskGraph) error {
	ctx = runtime.WithTaskID(ctx, task.ID)
	s.ensureDemux(ctx)

	// Generate dispatch nonce and clear stale overflow from prior dispatches.
	nonce := uuid.New().String()[:8]
	s.setNonce(task.ID, nonce)
	s.demux.clearOverflow(task.ID)
	profile := taskProfile(task)
	placement, err := s.prepareTaskPlacement(ctx, task)
	if err != nil {
		if isRecoverableDispatchError(err) {
			s.prepareTaskForRedispatch(ctx, task)
		}
		return err
	}
	if placement != nil {
		defer s.finishTaskPlacement(context.WithoutCancel(ctx), task, placement)
	}
	target := taskExecutionTarget(task)

	// Build message parts: task description + pipeline context + dependency results.
	parts := []message.Part{
		message.TextPart{Text: task.Description},
	}

	// Include the original user request so agents can anchor to it.
	if s.UserRequest != "" {
		parts = append(parts, message.DataPart{Data: map[string]any{
			"user_request": s.UserRequest,
		}})
	}

	// Include chat context from amendment so the agent knows what was discussed.
	if task.ChatContext != "" {
		parts = append(parts, message.DataPart{Data: map[string]any{
			"chat_context": task.ChatContext,
		}})
	}

	// Add pipeline context so the agent knows where it sits in the DAG.
	if pctx := buildPipelineContext(task, graph, s.Agents); pctx != "" {
		parts = append(parts, message.DataPart{Data: map[string]any{
			"pipeline_context": pctx,
		}})
	}

	// Inject prior knowledge context for this agent.
	if knParts := s.buildKnowledgeContext(task); len(knParts) > 0 {
		parts = append(parts, knParts...)
	}

	// Include existing artifacts so agents can iterate on prior work.
	if existingParts := s.buildExistingArtifacts(ctx, profile); len(existingParts) > 0 {
		parts = append(parts, existingParts...)
	}

	if len(task.DependsOn) == 0 {
		// First task in the pipeline — no upstream artifacts exist yet.
		// Anchor the agent to the provided context so it doesn't hallucinate files.
		parts = append(parts, message.DataPart{Data: map[string]any{
			"no_upstream": "You are the first task in the pipeline. There are no upstream artifacts or dependency files to read. All requirements come from the user request and task description above.",
		}})
	}

	for _, depID := range task.DependsOn {
		dep := graph.Get(depID)
		if dep == nil || dep.Status != taskgraph.StatusCompleted {
			continue
		}

		// Use filtered artifact files when available, otherwise fall back to URI.
		filtered := filterArtifacts(dep)
		if len(filtered) > 0 {
			for _, fp := range filtered {
				parts = append(parts, fp)
			}
			// Also include a DataPart so buildTaskInput sees dependency metadata.
			parts = append(parts, message.DataPart{Data: map[string]any{
				"dependency_id":    dep.ID,
				"dependency_agent": dep.TargetProfile(),
			}})
			continue
		}

		data := map[string]any{
			"dependency_id":    dep.ID,
			"dependency_agent": dep.TargetProfile(),
		}
		if dep.ArtifactURI != "" {
			data["artifact_uri"] = dep.ArtifactURI
			if len(dep.ArtifactFiles) > 0 {
				names := make([]string, len(dep.ArtifactFiles))
				for i, f := range dep.ArtifactFiles {
					names[i] = f.Name
				}
				data["artifact_files"] = names
			}
		}
		if dep.Result != "" {
			if dep.ArtifactURI != "" && len(dep.Result) > 4000 {
				data["result"] = dep.Result[:4000] + "\n[... truncated, full content in artifact_uri]"
			} else {
				data["result"] = dep.Result
			}
		}
		if dep.ArtifactURI != "" || dep.Result != "" {
			parts = append(parts, message.DataPart{Data: data})
		}
	}

	// Send task assignment.
	msg := &message.Message{
		ID:        uuid.New().String(),
		RequestID: s.RequestID,
		From:      "conductor",
		To:        profile,
		Type:      message.TypeTaskAssignment,
		Parts:     parts,
		Metadata: map[string]string{
			"task_id":        task.ID,
			"task_scope":     string(task.Scope.EffectiveScope()),
			"dispatch_nonce": nonce,
		},
		Timestamp: time.Now(),
	}
	if profile != "" {
		msg.Metadata["profile"] = profile
	}
	if task.AssignedInstance != "" {
		msg.Metadata["assigned_instance"] = task.AssignedInstance
	}
	if task.ExecutionNode != "" {
		msg.Metadata["execution_node"] = task.ExecutionNode
	}
	if task.LeaseEpoch > 0 {
		msg.Metadata["lease_epoch"] = fmt.Sprintf("%d", task.LeaseEpoch)
	}

	if s.Logger != nil {
		s.Logger.Log(ctx, msg)
	}

	if err := s.Comm.Send(ctx, msg); err != nil {
		return fmt.Errorf("send task to %q: %w", target, err)
	}

	// Wait for result.
	result, err := s.waitForResult(ctx, task, nonce)
	if err != nil {
		if isRecoverableDispatchError(err) {
			s.prepareTaskForRedispatch(ctx, task)
		}
		return err
	}

	task.Result = extractText(result)
	task.ArtifactURI = extractArtifactURI(result)
	task.ArtifactFiles = extractArtifactFiles(result)

	// In gRPC-based runtimes, agents record LLM usage in their own meters.
	// Feed the reported usage into the conductor's meter so aggregate totals
	// include all agents.
	s.recordRemoteUsage(ctx, result)

	if result.Metadata != nil && result.Metadata["status"] == "failed" {
		task.Status = taskgraph.StatusFailed
	} else {
		task.Status = taskgraph.StatusCompleted
	}
	s.syncTaskState(ctx, task)

	return nil
}

type taskPlacement struct {
	instanceID         string
	nodeID             string
	taskLease          controlplane.TaskLease
	cancelLeaseRenewal context.CancelFunc
}

type loopPlacement struct {
	leases             []controlplane.ProfileLease
	cancelLeaseRenewal context.CancelFunc
}

func (s *Scheduler) prepareTaskPlacement(ctx context.Context, task *taskgraph.TaskNode) (*taskPlacement, error) {
	if s.ControlPlane == nil || task == nil || task.LoopID != "" {
		return nil, nil
	}

	profile := task.TargetProfile()
	if profile == "" {
		return nil, nil
	}

	instances, err := s.ControlPlane.ListInstances(ctx, controlplane.InstanceFilter{Profile: profile})
	if err != nil {
		return nil, fmt.Errorf("list instances for profile %q: %w", profile, err)
	}
	if len(instances) == 0 {
		task.AssignedInstance = ""
		task.ExecutionNode = ""
		task.LeaseEpoch = 0
		return nil, nil
	}

	// Profile leases used to pin every dispatch for a profile to a single
	// instance, which serialised parallel fan-out workloads. The profile lease
	// guarded nothing real -- per-task artifacts are written to task-scoped
	// paths, knowledge updates are already serialised by knowledgeRefreshMu
	// and the conductor's single background apply goroutine, and same-task
	// double-execution is prevented by the per-task lease below. Skip the
	// profile lease entirely and let the scheduler's least-loaded sort do
	// its job.
	instance, ok, err := s.selectTaskInstance(ctx, task, instances)
	if err != nil {
		return nil, fmt.Errorf("select instance for profile %q: %w", profile, err)
	}
	if !ok {
		// Every eligible instance is at capacity. This is a transient
		// condition for fan-out workloads larger than current cluster
		// headroom: as in-flight tasks complete, capacity frees up and the
		// scheduler can retry. Surface as recoverable so the dispatcher
		// requeues instead of failing the task hard.
		return nil, recoverableDispatchError{reason: fmt.Sprintf("no routable instance available for profile %q (all at capacity)", profile)}
	}

	task.AssignedInstance = instance.ID
	task.ExecutionNode = instance.NodeID
	if s.Events != nil {
		progress := "Running..."
		if task.AssignedInstance != "" && task.ExecutionNode != "" {
			progress = fmt.Sprintf("Running on %s via %s", task.AssignedInstance, task.ExecutionNode)
		} else if task.ExecutionNode != "" {
			progress = fmt.Sprintf("Running on node %s", task.ExecutionNode)
		}
		s.Events.Emit(event.Event{
			Type:          event.TaskProgress,
			TaskID:        task.ID,
			TaskAgent:     profile,
			ExecutionNode: task.ExecutionNode,
			ProgressText:  progress,
		})
	}

	taskLease, acquired, err := s.ControlPlane.AcquireTaskLease(ctx, controlplane.TaskLease{
		RequestID:        s.RequestID,
		TaskID:           task.ID,
		Profile:          profile,
		AssignedInstance: instance.ID,
		ExecutionNode:    instance.NodeID,
		OwnerID:          s.leaseOwnerID(),
	}, taskLeaseTTL)
	if err != nil {
		return nil, fmt.Errorf("acquire task lease for %q: %w", task.ID, err)
	}
	if !acquired {
		return nil, fmt.Errorf("task lease for %q is already held by %q", task.ID, taskLease.OwnerID)
	}

	task.LeaseEpoch = taskLease.Epoch
	s.acquireInstance(instance.ID, instance.NodeID)

	renewCtx, cancelRenew := context.WithCancel(ctx)
	go s.runPlacementLeaseHeartbeat(renewCtx, task, taskLease)

	return &taskPlacement{
		instanceID:         instance.ID,
		nodeID:             instance.NodeID,
		taskLease:          taskLease,
		cancelLeaseRenewal: cancelRenew,
	}, nil
}

func (s *Scheduler) finishTaskPlacement(ctx context.Context, task *taskgraph.TaskNode, placement *taskPlacement) {
	if placement == nil {
		return
	}
	if placement.cancelLeaseRenewal != nil {
		placement.cancelLeaseRenewal()
	}
	if placement.instanceID != "" {
		s.releaseInstance(placement.instanceID, placement.nodeID)
	}
	if s.ControlPlane != nil {
		if err := s.ControlPlane.ReleaseTaskLease(ctx, placement.taskLease); err != nil {
			slog.Warn("control plane task lease release failed",
				"request_id", placement.taskLease.RequestID,
				"task_id", placement.taskLease.TaskID,
				"epoch", placement.taskLease.Epoch,
				"error", err)
		}
	}
}

func (s *Scheduler) acquireProfilePlacement(ctx context.Context, profile string, instances []controlplane.AgentInstance) (controlplane.AgentInstance, controlplane.ProfileLease, error) {
	if profile == "" {
		return controlplane.AgentInstance{}, controlplane.ProfileLease{}, nil
	}

	lease, ok, err := s.ControlPlane.GetProfileLease(ctx, profile)
	if err != nil {
		return controlplane.AgentInstance{}, controlplane.ProfileLease{}, fmt.Errorf("get profile lease for %q: %w", profile, err)
	}
	if ok {
		instance, found, err := findSchedulableInstanceByID(s.selectTaskInstance, ctx, &taskgraph.TaskNode{Agent: profile}, instances, lease.AssignedInstance)
		if err != nil {
			return controlplane.AgentInstance{}, controlplane.ProfileLease{}, fmt.Errorf("select leased instance for profile %q: %w", profile, err)
		}
		if !found {
			return controlplane.AgentInstance{}, controlplane.ProfileLease{}, recoverableDispatchError{reason: fmt.Sprintf("profile %q is leased to unavailable instance %q", profile, lease.AssignedInstance)}
		}
		if lease.OwnerID != s.leaseOwnerID() {
			return controlplane.AgentInstance{}, controlplane.ProfileLease{}, recoverableDispatchError{reason: fmt.Sprintf("profile %q is currently reserved by %q", profile, lease.OwnerID)}
		}
		renewed, err := s.ControlPlane.RenewProfileLease(ctx, lease, profileLeaseTTL)
		if err != nil {
			return controlplane.AgentInstance{}, controlplane.ProfileLease{}, recoverableDispatchError{reason: fmt.Sprintf("profile lease for %q could not be renewed: %v", profile, err)}
		}
		return instance, renewed, nil
	}

	instance, ok, err := s.selectTaskInstance(ctx, &taskgraph.TaskNode{Agent: profile}, instances)
	if err != nil {
		return controlplane.AgentInstance{}, controlplane.ProfileLease{}, fmt.Errorf("select instance for profile %q: %w", profile, err)
	}
	if !ok {
		return controlplane.AgentInstance{}, controlplane.ProfileLease{}, nil
	}

	acquiredLease, acquired, err := s.ControlPlane.AcquireProfileLease(ctx, controlplane.ProfileLease{
		Profile:          profile,
		AssignedInstance: instance.ID,
		ExecutionNode:    instance.NodeID,
		OwnerID:          s.leaseOwnerID(),
	}, profileLeaseTTL)
	if err != nil {
		return controlplane.AgentInstance{}, controlplane.ProfileLease{}, fmt.Errorf("acquire profile lease for %q: %w", profile, err)
	}
	if !acquired {
		return controlplane.AgentInstance{}, controlplane.ProfileLease{}, recoverableDispatchError{reason: fmt.Sprintf("profile %q is currently reserved by %q", profile, acquiredLease.OwnerID)}
	}
	return instance, acquiredLease, nil
}

func (s *Scheduler) selectTaskInstance(ctx context.Context, task *taskgraph.TaskNode, instances []controlplane.AgentInstance) (controlplane.AgentInstance, bool, error) {
	profile := ""
	if task != nil {
		profile = task.TargetProfile()
	}
	def := s.agentDefinition(profile)
	nodes, err := s.instanceNodeMap(ctx, instances)
	if err != nil {
		return controlplane.AgentInstance{}, false, err
	}

	candidates := make([]controlplane.AgentInstance, 0, len(instances))
	for _, instance := range instances {
		if !isSchedulableInstanceState(instance.State) || instance.Endpoint.Address == "" {
			continue
		}
		node, hasNode := nodes[instance.NodeID]
		if !isEligibleNodeForAgent(def, node, hasNode) {
			continue
		}
		if nodeAtCapacity(node, hasNode, s.nodeLoad(instance.NodeID)) {
			continue
		}
		candidates = append(candidates, instance)
	}
	if len(candidates) == 0 {
		return controlplane.AgentInstance{}, false, nil
	}

	if task != nil && task.AssignedInstance != "" {
		for _, instance := range candidates {
			if instance.ID == task.AssignedInstance {
				return instance, true, nil
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		leftNode := nodes[candidates[i].NodeID]
		rightNode := nodes[candidates[j].NodeID]
		leftNodeLoad := s.nodeLoad(candidates[i].NodeID)
		rightNodeLoad := s.nodeLoad(candidates[j].NodeID)
		if leftNodeLoad != rightNodeLoad {
			return leftNodeLoad < rightNodeLoad
		}
		leftInflight := s.instanceLoad(candidates[i].ID)
		rightInflight := s.instanceLoad(candidates[j].ID)
		if leftInflight != rightInflight {
			return leftInflight < rightInflight
		}
		leftRequestUsage := s.requestNodeUsage(task, candidates[i].NodeID)
		rightRequestUsage := s.requestNodeUsage(task, candidates[j].NodeID)
		if leftRequestUsage != rightRequestUsage {
			return leftRequestUsage < rightRequestUsage
		}
		leftNodeCapacity := effectiveNodeTaskCapacity(leftNode)
		rightNodeCapacity := effectiveNodeTaskCapacity(rightNode)
		if leftNodeCapacity != rightNodeCapacity {
			return leftNodeCapacity > rightNodeCapacity
		}
		leftPriority := schedulableInstancePriority(candidates[i].State)
		rightPriority := schedulableInstancePriority(candidates[j].State)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if !candidates[i].LastHeartbeatAt.Equal(candidates[j].LastHeartbeatAt) {
			return candidates[i].LastHeartbeatAt.After(candidates[j].LastHeartbeatAt)
		}
		return candidates[i].ID < candidates[j].ID
	})

	return candidates[0], true, nil
}

func findSchedulableInstanceByID(
	selectFn func(context.Context, *taskgraph.TaskNode, []controlplane.AgentInstance) (controlplane.AgentInstance, bool, error),
	ctx context.Context,
	task *taskgraph.TaskNode,
	instances []controlplane.AgentInstance,
	instanceID string,
) (controlplane.AgentInstance, bool, error) {
	if strings.TrimSpace(instanceID) == "" {
		return controlplane.AgentInstance{}, false, nil
	}
	taskCopy := &taskgraph.TaskNode{}
	if task != nil {
		*taskCopy = *task
	}
	taskCopy.AssignedInstance = instanceID
	return selectFn(ctx, taskCopy, instances)
}

func (s *Scheduler) agentDefinition(profile string) runtime.AgentDefinition {
	for _, def := range s.Agents {
		if def.Name == profile {
			return def
		}
	}
	return runtime.AgentDefinition{}
}

func (s *Scheduler) instanceNodeMap(ctx context.Context, instances []controlplane.AgentInstance) (map[string]controlplane.Node, error) {
	nodes := make(map[string]controlplane.Node)
	if s.ControlPlane == nil {
		return nodes, nil
	}
	nodeIDs := make(map[string]struct{})
	for _, instance := range instances {
		if instance.NodeID == "" {
			continue
		}
		nodeIDs[instance.NodeID] = struct{}{}
	}
	if len(nodeIDs) == 0 {
		return nodes, nil
	}

	lookupCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		lookupCtx, cancel = context.WithTimeout(ctx, nodeLookupTimeout)
		defer cancel()
	}

	registeredNodes, err := s.ControlPlane.ListNodes(lookupCtx)
	if err != nil {
		return nil, err
	}
	for _, node := range registeredNodes {
		if _, exists := nodeIDs[node.ID]; !exists {
			continue
		}
		nodes[node.ID] = node
	}
	return nodes, nil
}

func isEligibleNodeForAgent(def runtime.AgentDefinition, node controlplane.Node, hasNode bool) bool {
	if hasNode && !isSchedulableNodeState(node.State) {
		return false
	}
	if len(def.RequiredNodeLabels) == 0 {
		return true
	}
	if !hasNode {
		return false
	}
	for key, value := range def.RequiredNodeLabels {
		if node.Labels[key] != value {
			return false
		}
	}
	return true
}

func isSchedulableNodeState(state controlplane.NodeState) bool {
	switch state {
	case controlplane.NodeStateReady, controlplane.NodeStateDegraded:
		return true
	default:
		return false
	}
}

func isRunningNodeState(state controlplane.NodeState) bool {
	switch state {
	case controlplane.NodeStateReady, controlplane.NodeStateDegraded, controlplane.NodeStateDraining:
		return true
	default:
		return false
	}
}

func nodeAtCapacity(node controlplane.Node, hasNode bool, currentLoad int) bool {
	if !hasNode {
		return false
	}
	if node.Capacity.MaxTasks <= 0 {
		return false
	}
	return currentLoad >= node.Capacity.MaxTasks
}

func effectiveNodeTaskCapacity(node controlplane.Node) int {
	if node.Capacity.MaxTasks <= 0 {
		return 1 << 30
	}
	return node.Capacity.MaxTasks
}

func (s *Scheduler) prepareLoopAssignments(ctx context.Context, loopDef *loop.LoopDefinition) (map[string]controlplane.AgentInstance, *loopPlacement, controlplane.AgentInstance, bool, error) {
	if s.ControlPlane == nil || loopDef == nil {
		return nil, nil, controlplane.AgentInstance{}, false, nil
	}

	assignments := make(map[string]controlplane.AgentInstance)
	leases := make([]controlplane.ProfileLease, 0, len(loopDef.States))
	var firstInstance controlplane.AgentInstance
	firstAgent := loopDef.AgentForState(loopDef.InitialState)
	foundAny := false

	for _, state := range loopDef.States {
		if state.Agent == "" {
			continue
		}
		if _, exists := assignments[state.Agent]; exists {
			continue
		}

		instances, err := s.ControlPlane.ListInstances(ctx, controlplane.InstanceFilter{Profile: state.Agent})
		if err != nil {
			s.releaseLoopProfileLeases(context.WithoutCancel(ctx), leases)
			return nil, nil, controlplane.AgentInstance{}, false, fmt.Errorf("list loop instances for profile %q: %w", state.Agent, err)
		}
		if len(instances) == 0 {
			continue
		}

		instance, lease, err := s.acquireProfilePlacement(ctx, state.Agent, instances)
		if err != nil {
			s.releaseLoopProfileLeases(context.WithoutCancel(ctx), leases)
			return nil, nil, controlplane.AgentInstance{}, false, err
		}
		if instance.ID == "" {
			continue
		}
		assignments[state.Agent] = instance
		leases = append(leases, lease)
		foundAny = true
		if state.Agent == firstAgent {
			firstInstance = instance
		}
	}

	if !foundAny {
		return nil, nil, controlplane.AgentInstance{}, false, nil
	}
	if firstAgent != "" && firstInstance.ID == "" {
		s.releaseLoopProfileLeases(context.WithoutCancel(ctx), leases)
		return nil, nil, controlplane.AgentInstance{}, false, fmt.Errorf("no routable instance available for initial loop agent %q", firstAgent)
	}
	renewCtx, cancel := context.WithCancel(ctx)
	for _, lease := range leases {
		go s.runProfileLeaseHeartbeat(renewCtx, lease)
	}
	return assignments, &loopPlacement{leases: leases, cancelLeaseRenewal: cancel}, firstInstance, true, nil
}

func (s *Scheduler) finishLoopPlacement(ctx context.Context, placement *loopPlacement) {
	if placement == nil {
		return
	}
	if placement.cancelLeaseRenewal != nil {
		placement.cancelLeaseRenewal()
	}
	s.releaseLoopProfileLeases(ctx, placement.leases)
}

func (s *Scheduler) releaseLoopProfileLeases(ctx context.Context, leases []controlplane.ProfileLease) {
	for _, lease := range leases {
		if err := s.ControlPlane.ReleaseProfileLease(ctx, lease); err != nil {
			slog.Warn("control plane profile lease release failed",
				"profile", lease.Profile,
				"assigned_instance", lease.AssignedInstance,
				"epoch", lease.Epoch,
				"error", err)
		}
	}
}

func loopAssignedInstances(assignments map[string]controlplane.AgentInstance) map[string]string {
	if len(assignments) == 0 {
		return nil
	}
	result := make(map[string]string, len(assignments))
	for profile, instance := range assignments {
		if instance.ID != "" {
			result[profile] = instance.ID
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func loopExecutionNodes(assignments map[string]controlplane.AgentInstance) map[string]string {
	if len(assignments) == 0 {
		return nil
	}
	result := make(map[string]string, len(assignments))
	for profile, instance := range assignments {
		if instance.NodeID != "" {
			result[profile] = instance.NodeID
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func isSchedulableInstanceState(state controlplane.InstanceState) bool {
	switch state {
	case controlplane.InstanceStateReady, controlplane.InstanceStateBusy:
		return true
	default:
		return false
	}
}

func isRunningInstanceState(state controlplane.InstanceState) bool {
	switch state {
	case controlplane.InstanceStateReady, controlplane.InstanceStateBusy, controlplane.InstanceStateDraining:
		return true
	default:
		return false
	}
}

func schedulableInstancePriority(state controlplane.InstanceState) int {
	switch state {
	case controlplane.InstanceStateReady:
		return 0
	case controlplane.InstanceStateBusy:
		return 1
	default:
		return 2
	}
}

func (s *Scheduler) runPlacementLeaseHeartbeat(ctx context.Context, task *taskgraph.TaskNode, taskLease controlplane.TaskLease) {
	ticker := time.NewTicker(taskLeaseRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewedTask, err := s.ControlPlane.RenewTaskLease(ctx, taskLease, taskLeaseTTL)
			if err != nil {
				slog.Warn("control plane task lease renew failed",
					"request_id", taskLease.RequestID,
					"task_id", taskLease.TaskID,
					"epoch", taskLease.Epoch,
					"error", err)
				return
			}
			taskLease = renewedTask
			if task != nil {
				task.LeaseEpoch = renewedTask.Epoch
			}
		}
	}
}

func (s *Scheduler) runProfileLeaseHeartbeat(ctx context.Context, profileLease controlplane.ProfileLease) {
	ticker := time.NewTicker(profileLeaseRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, err := s.ControlPlane.RenewProfileLease(ctx, profileLease, profileLeaseTTL)
			if err != nil {
				slog.Warn("control plane profile lease renew failed",
					"profile", profileLease.Profile,
					"assigned_instance", profileLease.AssignedInstance,
					"epoch", profileLease.Epoch,
					"error", err)
				return
			}
			profileLease = renewed
		}
	}
}

func (s *Scheduler) leaseOwnerID() string {
	if strings.TrimSpace(s.LeaseOwnerID) != "" {
		return s.LeaseOwnerID
	}
	return "conductor"
}

func (s *Scheduler) acquireInstance(instanceID, nodeID string) {
	if instanceID == "" {
		return
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.instanceInFlight == nil {
		s.instanceInFlight = make(map[string]int)
	}
	s.instanceInFlight[instanceID]++
	if nodeID != "" {
		if s.nodeInFlight == nil {
			s.nodeInFlight = make(map[string]int)
		}
		s.nodeInFlight[nodeID]++
	}
}

func (s *Scheduler) releaseInstance(instanceID, nodeID string) {
	if instanceID == "" {
		return
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.instanceInFlight[instanceID] > 0 {
		s.instanceInFlight[instanceID]--
	}
	if nodeID != "" && s.nodeInFlight[nodeID] > 0 {
		s.nodeInFlight[nodeID]--
	}
}

func (s *Scheduler) instanceLoad(instanceID string) int {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.instanceInFlight[instanceID]
}

func (s *Scheduler) nodeLoad(nodeID string) int {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.nodeInFlight[nodeID]
}

func (s *Scheduler) requestNodeUsage(task *taskgraph.TaskNode, nodeID string) int {
	if s.graph == nil || nodeID == "" {
		return 0
	}
	currentTaskID := ""
	if task != nil {
		currentTaskID = task.ID
	}
	usage := 0
	for _, existing := range s.graph.Tasks {
		if existing == nil || existing.ID == currentTaskID {
			continue
		}
		if existing.ExecutionNode != nodeID {
			continue
		}
		switch existing.Status {
		case taskgraph.StatusFailed, taskgraph.StatusCancelled:
			continue
		default:
			usage++
		}
	}
	return usage
}

func (s *Scheduler) registerTaskCancel(taskID string, cancel context.CancelFunc) {
	s.taskCancelMu.Lock()
	defer s.taskCancelMu.Unlock()
	if s.taskCancels != nil {
		s.taskCancels[taskID] = cancel
	}
}

func (s *Scheduler) unregisterTaskCancel(taskID string) {
	s.taskCancelMu.Lock()
	defer s.taskCancelMu.Unlock()
	delete(s.taskCancels, taskID)
}

// CancelTask cancels a running task's context.
func (s *Scheduler) CancelTask(taskID string) {
	s.taskCancelMu.Lock()
	defer s.taskCancelMu.Unlock()
	if cancel, ok := s.taskCancels[taskID]; ok {
		cancel()
	}
}

// CancelAllRunning cancels all running task contexts.
func (s *Scheduler) CancelAllRunning() {
	s.taskCancelMu.Lock()
	defer s.taskCancelMu.Unlock()
	for _, cancel := range s.taskCancels {
		cancel()
	}
}

// CancelAllRunningForRestructure cancels all running tasks and sets the
// restructuring flag so that dispatchers exit quietly without logging errors.
func (s *Scheduler) CancelAllRunningForRestructure() {
	s.restructuring.Store(true)
	s.CancelAllRunning()
}

// Pause pauses the scheduler: cancels all running tasks and blocks the dispatch loop.
// Running tasks are reset to pending so they re-dispatch on resume.
func (s *Scheduler) Pause() {
	if !s.paused.CompareAndSwap(false, true) {
		return
	}
	close(s.pauseCh)
	s.CancelAllRunning()
}

// Resume resumes the scheduler after a pause. Running tasks are reset to pending
// and both pause/resume channels are re-created for the next cycle.
func (s *Scheduler) Resume() {
	if !s.paused.CompareAndSwap(true, false) {
		return
	}
	// Reset running tasks to pending so they re-dispatch.
	if s.graph != nil {
		for _, t := range s.graph.Tasks {
			if t.Status == taskgraph.StatusRunning {
				t.Status = taskgraph.StatusPending
				t.Result = ""
				s.syncTaskState(context.Background(), t)
			}
		}
	}
	close(s.resumeCh)
	// Re-create channels for next pause/resume cycle.
	s.pauseCh = make(chan struct{})
	s.resumeCh = make(chan struct{})
}

// Cancel marks all pending/running tasks as cancelled and cancels the per-request context.
// If paused, it also unblocks the dispatch loop so Execute can return.
func (s *Scheduler) Cancel() {
	if s.graph != nil {
		for _, t := range s.graph.Tasks {
			if t.Status == taskgraph.StatusPending || t.Status == taskgraph.StatusRunning {
				t.Status = taskgraph.StatusCancelled
				t.Result = "cancelled by user"
				s.syncTaskState(context.Background(), t)
			}
		}
	}
	s.CancelAllRunning()
	if s.reqCancel != nil {
		s.reqCancel()
	}
	// If paused, unblock the pause gate so Execute can exit.
	if s.paused.CompareAndSwap(true, false) {
		close(s.resumeCh)
	}
}

// AmendTask updates a task description and resets it for re-dispatch.
// chatContext carries the chat conversation that led to the amendment so the
// re-dispatched agent has full context of what was discussed with the user.
func (s *Scheduler) AmendTask(taskID, newDesc, chatContext string) error {
	if s.graph == nil {
		return fmt.Errorf("no active graph")
	}
	task := s.graph.Get(taskID)
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	task.Description = newDesc
	task.ChatContext = chatContext
	task.Status = taskgraph.StatusPending
	task.Result = ""
	task.ArtifactURI = ""
	task.ArtifactFiles = nil
	s.syncTaskState(context.Background(), task)
	s.CancelTask(taskID)
	s.Events.Emit(event.Event{
		Type:          event.TaskAmended,
		AmendedTaskID: taskID,
		AmendedAgent:  task.TargetProfile(),
	})
	return nil
}

// setNonce stores the expected dispatch nonce for a task.
func (s *Scheduler) setNonce(taskID, nonce string) {
	s.nonceMu.Lock()
	defer s.nonceMu.Unlock()
	if s.taskNonces == nil {
		s.taskNonces = make(map[string]string)
	}
	s.taskNonces[taskID] = nonce
}

// ReplaceGraph sends a new graph for the scheduler to swap.
func (s *Scheduler) ReplaceGraph(g *taskgraph.TaskGraph) {
	if s.graphReplace != nil {
		select {
		case s.graphReplace <- g:
		default:
		}
	}
}

func (s *Scheduler) waitForResult(ctx context.Context, task *taskgraph.TaskNode, expectedNonce string) (*message.Message, error) {
	ch := s.demux.subscribe(task.ID)
	defer s.demux.unsubscribe(task.ID)

	timeout := s.resolveTaskTimeout(task)

	// timeout == 0 means no timeout — wait indefinitely (only ctx cancellation applies).
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timerC = timer.C
		defer timer.Stop()
	}
	healthTicker := time.NewTicker(taskHealthCheckInterval)
	defer healthTicker.Stop()
	lastProgressAt := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timerC:
			return nil, fmt.Errorf("task %q timed out", task.ID)
		case <-healthTicker.C:
			if err := s.checkTaskExecutionHealth(ctx, task); err != nil {
				return nil, err
			}
			if time.Since(lastProgressAt) >= taskProgressHeartbeat {
				s.emitTaskHeartbeat(task)
				lastProgressAt = time.Now()
			}
		case msg, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("channel closed while waiting for task %q", task.ID)
			}
			// Discard stale results from prior dispatches (e.g., after amendment).
			// A result is stale when it carries a dispatch_nonce that doesn't match.
			// Results without a nonce are accepted (backward compatibility).
			if expectedNonce != "" && msg.Type == message.TypeTaskResult {
				if resultNonce := msg.Metadata["dispatch_nonce"]; resultNonce != "" && resultNonce != expectedNonce {
					slog.Debug("discarding stale result", "task", task.ID, "expected_nonce", expectedNonce, "got_nonce", resultNonce)
					if timer != nil {
						timer.Reset(timeout)
					}
					continue
				}
			}
			if msg.Type == message.TypeUserQuery {
				if err := s.forwardUserQuery(ctx, msg); err != nil {
					return nil, err
				}
				lastProgressAt = time.Now()
				if timer != nil {
					timer.Reset(timeout)
				}
				continue
			}
			if msg.Type == message.TypeTaskResult {
				return msg, nil
			}
			if msg.Type == message.TypeEscalation {
				slog.Warn("escalation received during task execution",
					"from", msg.From, "task", task.ID)
				return msg, nil
			}
			// Heartbeat: any other message type (status update, review response
			// being forwarded, etc.) resets the timeout — the agent is alive.
			lastProgressAt = time.Now()
			if timer != nil {
				timer.Reset(timeout)
			}
		}
	}
}

// forwardUserQuery forwards an agent's user_query to the interactive UI and
// relays the user's answer back to the requesting agent.
func (s *Scheduler) forwardUserQuery(ctx context.Context, msg *message.Message) error {
	if s.UserQueryCh == nil {
		s.UserQueryCh = make(chan *UserQuery, 1)
	}

	answerCh := make(chan string, 1)
	query := &UserQuery{
		AgentName:  msg.From,
		TaskID:     extractMetaTaskID(msg),
		Question:   extractText(msg),
		ResponseCh: answerCh,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.UserQueryCh <- query:
	}

	var answer string
	select {
	case <-ctx.Done():
		return ctx.Err()
	case answer = <-answerCh:
	}

	resp := &message.Message{
		ID:        uuid.New().String(),
		RequestID: s.RequestID,
		From:      "conductor",
		To:        msg.From,
		Type:      message.TypeUserResponse,
		Parts:     []message.Part{message.TextPart{Text: answer}},
		Metadata: map[string]string{
			"task_id": query.TaskID,
		},
		Timestamp: time.Now(),
	}
	return s.Comm.Send(ctx, resp)
}

// extractMetaTaskID extracts task_id from message metadata.
func extractMetaTaskID(msg *message.Message) string {
	if msg.Metadata != nil {
		return msg.Metadata["task_id"]
	}
	return ""
}

func extractText(msg *message.Message) string {
	for _, p := range msg.Parts {
		if tp, ok := p.(message.TextPart); ok {
			return tp.Text
		}
	}
	return ""
}

func extractArtifactURI(msg *message.Message) string {
	var uris []string
	for _, p := range msg.Parts {
		if fp, ok := p.(message.FilePart); ok {
			uris = append(uris, fp.URI)
		}
	}
	if len(uris) == 0 {
		return ""
	}
	if len(uris) == 1 {
		return uris[0]
	}
	// Multiple files — return their common directory prefix.
	return commonDirPrefix(uris)
}

// extractArtifactFiles collects individual FileParts from a result message
// as ArtifactFile entries for fine-grained downstream filtering.
func extractArtifactFiles(msg *message.Message) []taskgraph.ArtifactFile {
	var files []taskgraph.ArtifactFile
	for _, p := range msg.Parts {
		if fp, ok := p.(message.FilePart); ok {
			files = append(files, taskgraph.ArtifactFile{
				URI:      fp.URI,
				MimeType: fp.MimeType,
				Name:     fp.Name,
			})
		}
	}
	return files
}

// buildPipelineContext produces a text summary of the task graph for the agent.
// It shows all tasks, their agents, descriptions, and dependency arrows,
// highlighting the current task and its downstream consumers.
func buildPipelineContext(task *taskgraph.TaskNode, graph *taskgraph.TaskGraph, agents []runtime.AgentDefinition) string {
	if graph == nil || len(graph.Tasks) <= 1 {
		return ""
	}

	// Build agent purpose lookup.
	agentPurpose := make(map[string]string, len(agents))
	for _, a := range agents {
		agentPurpose[a.Name] = a.Purpose
	}

	// Build reverse dependency map: task ID → list of tasks that depend on it.
	downstream := make(map[string][]string, len(graph.Tasks))
	for _, t := range graph.Tasks {
		for _, dep := range t.DependsOn {
			downstream[dep] = append(downstream[dep], t.ID)
		}
	}

	var b strings.Builder
	b.WriteString("## Task Pipeline\n")
	b.WriteString(fmt.Sprintf("You are executing task %s. Here is the full pipeline:\n", task.ID))

	for _, t := range graph.Tasks {
		marker := ""
		if t.ID == task.ID {
			marker = " [YOU]"
		}

		line := fmt.Sprintf("- %s (%s)%s: %q", t.ID, t.TargetProfile(), marker, t.Description)

		if ds := downstream[t.ID]; len(ds) > 0 {
			line += " → feeds into: " + strings.Join(ds, ", ")
		}

		b.WriteString(line + "\n")
	}

	// Output targeting: tell the agent what downstream consumers need.
	if ds := downstream[task.ID]; len(ds) > 0 {
		b.WriteString("\n## Output Targeting\n")
		b.WriteString("Your output will be passed to downstream tasks. Produce ONLY what they need:\n")
		for _, dsID := range ds {
			dsTask := graph.Get(dsID)
			if dsTask == nil {
				continue
			}
			purpose := agentPurpose[dsTask.Agent]
			if purpose == "" {
				purpose = dsTask.Agent
			}
			b.WriteString(fmt.Sprintf("- %s (%s): %s\n", dsID, dsTask.Agent, purpose))
		}
		b.WriteString("Every token in your output costs downstream budget.\n")
	}

	return b.String()
}

// filterArtifacts selects which artifact files from a dependency should be
// passed downstream. Excludes redundant files to reduce token usage:
// summary.md (prose restating the task), decision docs when design.md is
// present.
func filterArtifacts(dep *taskgraph.TaskNode) []message.FilePart {
	if len(dep.ArtifactFiles) == 0 {
		return nil
	}

	hasDesignMD := false
	for _, f := range dep.ArtifactFiles {
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, "design.md") {
			hasDesignMD = true
		}
	}

	var result []message.FilePart
	for _, f := range dep.ArtifactFiles {
		name := strings.ToLower(f.Name)

		// Always skip summary.md — it's prose that restates the task.
		if strings.HasSuffix(name, "summary.md") {
			continue
		}

		// Skip decision records when design.md is present (it already contains them).
		if hasDesignMD && strings.Contains(name, "decision") {
			continue
		}

		result = append(result, message.FilePart{URI: f.URI, MimeType: f.MimeType, Name: f.Name})
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// dispatchLoopTask handles a task driven by an FSM loop.
// It sends the initial task to the first agent with a LoopContext attached.
// Agents route among themselves; the conductor waits for the terminal result.
func (s *Scheduler) dispatchLoopTask(ctx context.Context, task *taskgraph.TaskNode, graph *taskgraph.TaskGraph) error {
	ctx = runtime.WithTaskID(ctx, task.ID)
	s.ensureDemux(ctx)

	// Generate dispatch nonce and clear stale overflow from prior dispatches.
	nonce := uuid.New().String()[:8]
	s.setNonce(task.ID, nonce)
	s.demux.clearOverflow(task.ID)

	loopDef := graph.GetLoop(task.LoopID)
	if loopDef == nil {
		return fmt.Errorf("task %q references unknown loop %q", task.ID, task.LoopID)
	}

	// Enrich loop state configs with per-agent review guidelines from definitions.
	s.enrichLoopGuidelines(loopDef)

	if _, err := loop.NewFSM(*loopDef); err != nil {
		return fmt.Errorf("create FSM for loop %q: %w", loopDef.ID, err)
	}

	// Build LoopContext for the initial dispatch.
	depParts := s.buildDepPartsData(task, graph)
	assignments, loopPlacement, firstInstance, ok, err := s.prepareLoopAssignments(ctx, loopDef)
	if err != nil {
		if isRecoverableDispatchError(err) {
			s.prepareTaskForRedispatch(ctx, task)
		}
		return err
	}
	if loopPlacement != nil {
		defer s.finishLoopPlacement(context.WithoutCancel(ctx), loopPlacement)
	}
	if ok {
		task.AssignedInstance = firstInstance.ID
		task.ExecutionNode = firstInstance.NodeID
		task.LeaseEpoch = 0
	}
	lc := &loop.LoopContext{
		Definition: *loopDef,
		State: loop.LoopState{
			LoopID:       loopDef.ID,
			CurrentState: loopDef.InitialState,
		},
		TaskID:            task.ID,
		Conductor:         "conductor",
		OriginalTask:      task.Description,
		UserRequest:       s.UserRequest,
		DepParts:          depParts,
		AssignedInstances: loopAssignedInstances(assignments),
		ExecutionNodes:    loopExecutionNodes(assignments),
		DispatchNonce:     nonce,
	}

	// Build decision context for review enforcement across the loop.
	s.knowledgeMu.RLock()
	decisionCtx := decomposeDecisions(s.KnowledgeGraph, task.Description)
	s.knowledgeMu.RUnlock()
	lc.DecisionContext = decisionCtx

	// Build initial message.
	firstAgent := loopDef.AgentForState(loopDef.InitialState)
	if firstAgent == "" {
		return fmt.Errorf("loop %q: no agent for initial state %q", loopDef.ID, loopDef.InitialState)
	}

	prompt := loop.BuildStatePrompt(task.Description, loopDef, loopDef.InitialState, "", "", decisionCtx, nil)

	parts := []message.Part{message.TextPart{Text: prompt}}
	if s.UserRequest != "" {
		parts = append(parts, message.DataPart{Data: map[string]any{
			"user_request": s.UserRequest,
		}})
	}

	// Include prior knowledge context so loop agents have the same context
	// as regular task agents.
	if knParts := s.buildKnowledgeContext(task); len(knParts) > 0 {
		parts = append(parts, knParts...)
	}

	// Include existing artifacts so loop agents can iterate on prior work
	// instead of creating new directories.
	if existingParts := s.buildExistingArtifacts(ctx, firstAgent); len(existingParts) > 0 {
		parts = append(parts, existingParts...)
	}

	for _, dp := range depParts {
		parts = append(parts, message.DataPart{Data: dp})
	}
	parts = append(parts, loop.EncodeContext(lc))

	msg := &message.Message{
		ID:        uuid.New().String(),
		RequestID: s.RequestID,
		From:      "conductor",
		To:        firstAgent,
		Type:      message.TypeTaskAssignment,
		Parts:     parts,
		Metadata: map[string]string{
			"task_id":        task.ID,
			"task_scope":     string(task.Scope.EffectiveScope()),
			"loop_id":        loopDef.ID,
			"loop_state":     loopDef.InitialState,
			"dispatch_nonce": nonce,
		},
		Timestamp: time.Now(),
	}
	if assigned := lc.AssignedInstances[firstAgent]; assigned != "" {
		msg.Metadata["assigned_instance"] = assigned
	}
	if executionNode := lc.ExecutionNodes[firstAgent]; executionNode != "" {
		msg.Metadata["execution_node"] = executionNode
	}

	if s.Logger != nil {
		s.Logger.Log(ctx, msg)
	}

	if err := s.Comm.Send(ctx, msg); err != nil {
		return fmt.Errorf("send initial loop message to %q: %w", firstAgent, err)
	}

	// Wait for the terminal result from any participant.
	return s.waitForLoopCompletion(ctx, task, loopDef, nonce)
}

// waitForLoopCompletion subscribes to the task ID and waits for
// a TypeTaskResult indicating the loop has reached a terminal state.
// Loop terminal results carry task_id in metadata (set by sendLoopResult).
func (s *Scheduler) waitForLoopCompletion(ctx context.Context, task *taskgraph.TaskNode, loopDef *loop.LoopDefinition, expectedNonce string) error {
	ch := s.demux.subscribe(task.ID)
	defer s.demux.unsubscribe(task.ID)

	// Scale timeout by max transitions.
	timeout := s.resolveTaskTimeout(task)
	if timeout > 0 {
		timeout = timeout * time.Duration(loopDef.MaxTransitions)
	}

	// timeout == 0 means no timeout — wait indefinitely (only ctx cancellation applies).
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timerC = timer.C
		defer timer.Stop()
	}
	healthTicker := time.NewTicker(taskHealthCheckInterval)
	defer healthTicker.Stop()
	lastProgressAt := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timerC:
			return fmt.Errorf("loop task %q timed out", task.ID)
		case <-healthTicker.C:
			if err := s.checkTaskExecutionHealth(ctx, task); err != nil {
				s.prepareTaskForRedispatch(ctx, task)
				return err
			}
			if time.Since(lastProgressAt) >= taskProgressHeartbeat {
				s.emitTaskHeartbeat(task)
				lastProgressAt = time.Now()
			}
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("channel closed while waiting for loop completion")
			}
			if msg.Type == message.TypeStatusUpdate {
				s.updateLoopTaskExecution(task, msg)
				lastProgressAt = time.Now()
				if timer != nil {
					timer.Reset(timeout)
				}
				continue
			}
			if msg.Type == message.TypeUserQuery {
				if err := s.forwardUserQuery(ctx, msg); err != nil {
					return err
				}
				lastProgressAt = time.Now()
				if timer != nil {
					timer.Reset(timeout)
				}
				continue
			}
			if msg.Type == message.TypeTaskResult {
				// Discard stale results from prior dispatches.
				// A result is stale when it carries a nonce that doesn't match.
				if expectedNonce != "" {
					if resultNonce := msg.Metadata["dispatch_nonce"]; resultNonce != "" && resultNonce != expectedNonce {
						slog.Debug("discarding stale loop result", "task", task.ID, "expected_nonce", expectedNonce, "got_nonce", resultNonce)
						if timer != nil {
							timer.Reset(timeout)
						}
						continue
					}
				}
				task.Result = extractText(msg)
				task.ArtifactURI = extractArtifactURI(msg)
				task.ArtifactFiles = extractArtifactFiles(msg)
				s.recordRemoteUsage(ctx, msg)

				if msg.Metadata != nil && msg.Metadata["status"] == "failed" {
					task.Status = taskgraph.StatusFailed
				} else if msg.Metadata != nil && msg.Metadata["loop_escalated"] == "true" {
					task.Status = taskgraph.StatusEscalated
				} else {
					task.Status = taskgraph.StatusCompleted
				}
				s.syncTaskState(ctx, task)
				return nil
			}
			// Ignore non-terminal messages (agents route among themselves).
		}
	}
}

func (s *Scheduler) emitTaskHeartbeat(task *taskgraph.TaskNode) {
	if s.Events == nil || task == nil {
		return
	}
	progress := "Still working..."
	if task.AssignedInstance != "" && task.ExecutionNode != "" {
		progress = fmt.Sprintf("Still working on %s via %s", task.AssignedInstance, task.ExecutionNode)
	} else if task.AssignedInstance != "" {
		progress = fmt.Sprintf("Still working on %s", task.AssignedInstance)
	} else if task.ExecutionNode != "" {
		progress = fmt.Sprintf("Still working on node %s", task.ExecutionNode)
	}
	s.Events.Emit(event.Event{
		Type:          event.TaskProgress,
		TaskID:        task.ID,
		TaskAgent:     task.TargetProfile(),
		ExecutionNode: task.ExecutionNode,
		ProgressText:  progress,
	})
}

func (s *Scheduler) syncGraphState(ctx context.Context, graph *taskgraph.TaskGraph) {
	if s.ControlPlane == nil || graph == nil {
		return
	}
	for _, task := range graph.Tasks {
		s.syncTaskState(ctx, task)
	}
}

func (s *Scheduler) syncTaskState(ctx context.Context, task *taskgraph.TaskNode) {
	if s.ControlPlane == nil || task == nil || s.RequestID == "" {
		return
	}

	record := controlplane.TaskRecord{
		RequestID:        s.RequestID,
		TaskID:           task.ID,
		Profile:          task.TargetProfile(),
		AssignedInstance: task.AssignedInstance,
		ExecutionNode:    task.ExecutionNode,
		Status:           string(task.Status),
		LeaseEpoch:       task.LeaseEpoch,
	}
	if err := s.ControlPlane.UpsertTask(ctx, record); err != nil {
		slog.Warn("control plane task upsert failed",
			"request_id", s.RequestID,
			"task_id", task.ID,
			"error", err)
	}
}

func (s *Scheduler) updateLoopTaskExecution(task *taskgraph.TaskNode, msg *message.Message) {
	if task == nil || msg == nil || msg.Metadata == nil {
		return
	}
	if msg.Metadata["task_id"] != task.ID || msg.Metadata["loop_id"] == "" {
		return
	}

	updated := false
	if assigned := strings.TrimSpace(msg.Metadata["assigned_instance"]); assigned != "" && assigned != task.AssignedInstance {
		task.AssignedInstance = assigned
		updated = true
	}
	if executionNode := strings.TrimSpace(msg.Metadata["execution_node"]); executionNode != "" && executionNode != task.ExecutionNode {
		task.ExecutionNode = executionNode
		updated = true
	}
	if updated {
		s.syncTaskState(context.Background(), task)
	}
}

func (s *Scheduler) prepareTaskForRedispatch(ctx context.Context, task *taskgraph.TaskNode) {
	if task == nil {
		return
	}
	task.Status = taskgraph.StatusPending
	task.Result = ""
	task.ArtifactURI = ""
	task.ArtifactFiles = nil
	task.AssignedInstance = ""
	task.ExecutionNode = ""
	task.LeaseEpoch = 0
	s.syncTaskState(ctx, task)
}

func (s *Scheduler) checkTaskExecutionHealth(ctx context.Context, task *taskgraph.TaskNode) error {
	if s.ControlPlane == nil || task == nil {
		return nil
	}

	if task.ExecutionNode != "" {
		node, ok, err := s.ControlPlane.GetNode(ctx, task.ExecutionNode)
		if err == nil {
			if !ok {
				return recoverableDispatchError{reason: fmt.Sprintf("execution node %q disappeared", task.ExecutionNode)}
			}
			if !isRunningNodeState(node.State) {
				return recoverableDispatchError{reason: fmt.Sprintf("execution node %q became unavailable", task.ExecutionNode)}
			}
		}
	}

	if task.AssignedInstance != "" {
		instance, ok, err := s.ControlPlane.GetInstance(ctx, task.AssignedInstance)
		if err == nil {
			if !ok {
				return recoverableDispatchError{reason: fmt.Sprintf("assigned instance %q disappeared", task.AssignedInstance)}
			}
			if !isRunningInstanceState(instance.State) || instance.Endpoint.Address == "" {
				return recoverableDispatchError{reason: fmt.Sprintf("assigned instance %q became unavailable", task.AssignedInstance)}
			}
		}
	}

	if task.LeaseEpoch == 0 {
		return nil
	}

	lease, ok, err := s.ControlPlane.GetTaskLease(ctx, s.RequestID, task.ID)
	if err != nil {
		return nil
	}
	if !ok {
		return recoverableDispatchError{reason: fmt.Sprintf("task lease for %q expired", task.ID)}
	}
	if lease.OwnerID != s.leaseOwnerID() || lease.Epoch != task.LeaseEpoch {
		return recoverableDispatchError{reason: fmt.Sprintf("task lease for %q moved to a different owner", task.ID)}
	}
	return nil
}

// buildDepPartsData builds dependency context as raw maps (for LoopContext serialization).
func (s *Scheduler) buildDepPartsData(task *taskgraph.TaskNode, graph *taskgraph.TaskGraph) []map[string]any {
	var parts []map[string]any
	for _, depID := range task.DependsOn {
		dep := graph.Get(depID)
		if dep == nil || dep.Status != taskgraph.StatusCompleted {
			continue
		}
		data := map[string]any{
			"dependency_id":    dep.ID,
			"dependency_agent": dep.TargetProfile(),
		}
		if dep.ArtifactURI != "" {
			data["artifact_uri"] = dep.ArtifactURI
			if len(dep.ArtifactFiles) > 0 {
				names := make([]string, len(dep.ArtifactFiles))
				for i, f := range dep.ArtifactFiles {
					names[i] = f.Name
				}
				data["artifact_files"] = names
			}
		}
		if dep.Result != "" {
			if dep.ArtifactURI != "" && len(dep.Result) > 4000 {
				data["result"] = dep.Result[:4000] + "\n[... truncated, full content in artifact_uri]"
			} else {
				data["result"] = dep.Result
			}
		}
		if dep.ArtifactURI != "" || dep.Result != "" {
			parts = append(parts, data)
		}
	}
	return parts
}

// buildKnowledgeContext returns message parts with prior knowledge for the task's agent.
// Uses LookupDual when per-agent graphs are available (own nodes from agent graph,
// cross-agent BFS from shared graph). Falls back to single-graph Lookup otherwise.
func (s *Scheduler) buildKnowledgeContext(task *taskgraph.TaskNode) []message.Part {
	s.knowledgeMu.RLock()
	sharedGraph := s.KnowledgeGraph
	agentGraphs := s.AgentGraphs
	s.knowledgeMu.RUnlock()
	profile := taskProfile(task)

	sharedCount := 0
	if sharedGraph != nil {
		sharedCount = len(sharedGraph.Nodes)
	}
	agentCount := 0
	if agentGraphs != nil {
		if ag := agentGraphs[profile]; ag != nil {
			agentCount = len(ag.Nodes)
		}
	}
	slog.Debug("buildKnowledgeContext", "agent", profile, "task", task.ID,
		"shared_nodes", sharedCount, "agent_nodes", agentCount)

	if sharedGraph == nil || len(sharedGraph.Nodes) == 0 {
		// Even without a shared graph, check agent graph.
		if agentGraphs == nil {
			slog.Debug("no knowledge graphs available", "agent", profile)
			return nil
		}
		ag := agentGraphs[profile]
		if ag == nil || len(ag.Nodes) == 0 {
			slog.Debug("no knowledge nodes for agent", "agent", profile)
			return nil
		}
	}

	var result knowledge.LookupResult
	if agentGraphs != nil {
		agentGraph := agentGraphs[profile]
		result = knowledge.LookupDual(agentGraph, sharedGraph, profile, task.Description, knowledge.LookupOpts{})
	} else {
		result = knowledge.Lookup(sharedGraph, profile, task.Description, knowledge.LookupOpts{})
	}
	// Inject decision-tagged nodes unconditionally — regardless of keyword overlap.
	// This ensures project-wide decisions (e.g., "use Material Design 3") propagate
	// to every agent even when the task description has zero keyword match.
	if sharedGraph != nil {
		foundIDs := make(map[string]bool, len(result.Own)+len(result.Related))
		for _, n := range result.Own {
			foundIDs[n.ID] = true
		}
		for _, rn := range result.Related {
			foundIDs[rn.ID] = true
		}
		for id, n := range sharedGraph.Nodes {
			if foundIDs[id] || n.IsStale() || sharedGraph.IsSuperseded(id) || !n.HasTag("decision") {
				continue
			}
			result.Related = append(result.Related, &knowledge.RelatedNode{
				Node:      n,
				Depth:     0,
				Score:     1.0,
				Relations: []string{"decision"},
			})
		}
		// Inject user-request nodes so agents can reference what the user originally asked.
		// Re-read foundIDs to include any decisions just added above.
		for _, rn := range result.Related {
			foundIDs[rn.ID] = true
		}
		for id, n := range sharedGraph.Nodes {
			if foundIDs[id] || n.IsStale() || !n.HasTag("user-request") {
				continue
			}
			result.Related = append(result.Related, &knowledge.RelatedNode{
				Node:      n,
				Depth:     0,
				Score:     0.8,
				Relations: []string{"user-request"},
			})
		}
	}

	slog.Debug("knowledge lookup result", "agent", profile, "own", len(result.Own), "related", len(result.Related))

	// Emit KnowledgeLookup event for CLI visualization.
	if s.Events != nil && (len(result.Own) > 0 || len(result.Related) > 0) {
		var ownInfos []event.KnowledgeNodeInfo
		for _, n := range result.Own {
			ownInfos = append(ownInfos, event.KnowledgeNodeInfo{
				ID: n.ID, Agent: n.Agent, Title: n.Title, Summary: n.Summary, Tags: append([]string(nil), n.Tags...),
			})
		}
		var relInfos []event.KnowledgeRelInfo
		for _, rn := range result.Related {
			relInfos = append(relInfos, event.KnowledgeRelInfo{
				KnowledgeNodeInfo: event.KnowledgeNodeInfo{
					ID: rn.ID, Agent: rn.Agent, Title: rn.Title, Summary: rn.Summary, Tags: append([]string(nil), rn.Tags...),
				},
				Depth:     rn.Depth,
				Relations: rn.Relations,
			})
		}
		// Collect edges connecting returned nodes.
		returnedIDs := make(map[string]bool, len(ownInfos)+len(relInfos))
		for _, n := range ownInfos {
			returnedIDs[n.ID] = true
		}
		for _, n := range relInfos {
			returnedIDs[n.ID] = true
		}
		var edgeInfos []event.KnowledgeEdgeInfo
		collectEdges := func(g *knowledge.Graph) {
			if g == nil {
				return
			}
			for _, e := range g.Edges {
				if returnedIDs[e.From] && returnedIDs[e.To] {
					edgeInfos = append(edgeInfos, event.KnowledgeEdgeInfo{
						From: e.From, To: e.To, Relation: e.Relation,
					})
				}
			}
		}
		collectEdges(sharedGraph)
		if agentGraphs != nil {
			collectEdges(agentGraphs[profile])
		}
		s.Events.Emit(event.Event{
			Type:                    event.KnowledgeLookup,
			KnowledgeLookupAgent:    profile,
			KnowledgeLookupTask:     task.ID,
			KnowledgeLookupOwnNodes: ownInfos,
			KnowledgeLookupRelNodes: relInfos,
			KnowledgeLookupEdges:    edgeInfos,
		})
	}

	if len(result.Own) == 0 && len(result.Related) == 0 {
		return nil
	}

	// Own nodes: backward-compatible "knowledge_context" key.
	var ownEntries []map[string]any
	for _, n := range result.Own {
		ownEntries = append(ownEntries, map[string]any{
			"id":       n.ID,
			"title":    n.Title,
			"summary":  n.Summary,
			"tags":     n.Tags,
			"file":     n.FilePath,
			"from_req": n.RequestName,
		})
	}

	// Related nodes: new "related_knowledge" key with provenance.
	var relatedEntries []map[string]any
	for _, rn := range result.Related {
		relatedEntries = append(relatedEntries, map[string]any{
			"id":           rn.ID,
			"title":        rn.Title,
			"summary":      rn.Summary,
			"tags":         rn.Tags,
			"file":         rn.FilePath,
			"from_req":     rn.RequestName,
			"source_agent": rn.Agent,
			"relations":    rn.Relations,
			"depth":        rn.Depth,
		})
	}

	data := make(map[string]any)
	if len(ownEntries) > 0 {
		data["knowledge_context"] = ownEntries
	}
	if len(relatedEntries) > 0 {
		data["related_knowledge"] = relatedEntries
	}

	return []message.Part{
		message.DataPart{Data: data},
	}
}

// scheduleKnowledgeRefresh incrementally updates shared and per-agent knowledge
// after a task produces material artifacts. Refresh runs asynchronously and is
// serialized across tasks to avoid graph write races.
func (s *Scheduler) scheduleKnowledgeRefresh(ctx context.Context, graph *taskgraph.TaskGraph, task *taskgraph.TaskNode) {
	if task == nil || task.Status != taskgraph.StatusCompleted {
		return
	}
	if len(task.ArtifactFiles) == 0 {
		return
	}
	if s.KnowledgeGenerate == nil || s.Storage == nil {
		return
	}

	requestName := ""
	if graph != nil {
		requestName = graph.Name
	}

	// Copy task content to decouple from scheduler mutations.
	tcopy := &taskgraph.TaskNode{
		ID:          task.ID,
		Agent:       task.TargetProfile(),
		Profile:     task.TargetProfile(),
		Description: task.Description,
		Result:      task.Result,
		Status:      taskgraph.StatusCompleted,
	}

	go func() {
		bgCtx := context.WithoutCancel(ctx)
		if err := s.refreshKnowledgeFromTask(bgCtx, requestName, tcopy); err != nil {
			slog.Warn("incremental knowledge refresh failed",
				"task", tcopy.ID,
				"agent", tcopy.Agent,
				"error", err)
		}
	}()
}

func (s *Scheduler) refreshKnowledgeFromTask(ctx context.Context, requestName string, task *taskgraph.TaskNode) error {
	s.knowledgeRefreshMu.Lock()
	defer s.knowledgeRefreshMu.Unlock()
	profile := taskProfile(task)

	sharedStorage := s.StorageFactory("conductor")
	sharedGraph, err := knowledge.Load(ctx, sharedStorage)
	if err != nil {
		return fmt.Errorf("load shared knowledge graph: %w", err)
	}
	if sharedGraph == nil {
		sharedGraph = knowledge.NewGraph()
	}

	miniGraph := &taskgraph.TaskGraph{
		RequestID: s.RequestID,
		Name:      requestName,
		Tasks:     []*taskgraph.TaskNode{task},
	}

	manifest, _, err := knowledge.Generate(ctx, s.KnowledgeGenerate, miniGraph, sharedGraph, requestName)
	if err != nil {
		return fmt.Errorf("generate incremental manifest: %w", err)
	}
	if manifest == nil || (len(manifest.Nodes) == 0 && len(manifest.Edges) == 0) {
		return nil
	}

	pruneOpts := knowledge.PruneOpts{}
	if max := s.maxKnowledgeNodes(profile); max > 0 {
		pruneOpts.MaxNodes = max
	}

	storageFor := func(name string) knowledge.StorageHandle {
		return s.StorageFactory(name)
	}
	if err := knowledge.Apply(ctx, storageFor, sharedStorage, manifest, sharedGraph, s.RequestID, requestName, pruneOpts); err != nil {
		return fmt.Errorf("apply incremental manifest: %w", err)
	}

	// Rehydrate scheduler memory so subsequent dispatches see fresh knowledge.
	updatedShared, _ := knowledge.Load(ctx, sharedStorage)
	updatedAgent, _ := knowledge.LoadFromTier(ctx, s.StorageFactory(profile), runtime.TierAgent)
	if updatedShared != nil {
		s.knowledgeMu.Lock()
		s.KnowledgeGraph = updatedShared
		if s.AgentGraphs == nil {
			s.AgentGraphs = make(map[string]*knowledge.Graph)
		}
		if updatedAgent != nil {
			s.AgentGraphs[profile] = updatedAgent
		}
		s.knowledgeMu.Unlock()
	}

	slog.Info("incremental knowledge refreshed",
		"task", task.ID,
		"agent", profile,
		"nodes", len(manifest.Nodes),
		"edges", len(manifest.Edges))
	return nil
}

func (s *Scheduler) maxKnowledgeNodes(agent string) int {
	for _, def := range s.Agents {
		if def.Name == agent {
			return def.MaxKnowledgeNodes
		}
	}
	return 0
}

// resolveTaskTimeout determines the effective timeout for a task.
// Priority: task-level override > agent-level config > default (5 min).
// A value of 0 means no timeout (wait indefinitely).
func (s *Scheduler) resolveTaskTimeout(task *taskgraph.TaskNode) time.Duration {
	// Task-level override takes highest priority.
	if task.Timeout > 0 {
		return task.Timeout
	}

	// Check agent-level config.
	for _, def := range s.Agents {
		if def.Name == taskProfile(task) {
			return def.TaskTimeout // 0 = no timeout
		}
	}

	// Fallback default.
	return 5 * time.Minute
}

// buildExistingArtifacts lists material artifacts already present for the agent
// from prior requests. This allows agents to iterate on existing code rather than
// starting from scratch. Trace directories (_requests/) are excluded.
func (s *Scheduler) buildExistingArtifacts(ctx context.Context, agent string) []message.Part {
	if s.Storage == nil {
		return nil
	}

	// List artifacts in the shared volume. Any agent can see any other
	// agent's output in system-data/shared/artifacts/.
	pattern := "artifacts/"
	var allFiles []string
	for depth := 1; depth <= 5; depth++ {
		p := pattern + strings.Repeat("*/", depth-1) + "*"
		files, err := s.Storage.List(ctx, runtime.TierShared, p)
		if err != nil {
			break
		}
		allFiles = append(allFiles, files...)
	}

	// Deduplicate and filter out noise.
	seen := make(map[string]bool, len(allFiles))
	var filePaths []string
	for _, f := range allFiles {
		if seen[f] {
			continue
		}
		seen[f] = true

		if isArtifactNoise(f) {
			continue
		}

		filePaths = append(filePaths, f)
	}

	if len(filePaths) == 0 {
		return nil
	}

	// Build a human-readable directory tree so agents can see what exists.
	tree := buildDirectoryTree(filePaths)

	return []message.Part{
		message.TextPart{Text: "## Shared Artifacts\n" +
			"These files already exist in the shared workspace. " +
			"Inspect the tree to identify repositories, packages, and local dependencies before writing code. " +
			"Use shell commands to read specific files when you need more detail.\n\n" +
			"```\n" + tree + "```"},
		message.DataPart{Data: map[string]any{
			"existing_artifacts": true,
			"agent":              agent,
			"files":              filePaths,
		}},
	}
}

// buildDirectoryTree renders a sorted list of file paths as an indented tree.
func buildDirectoryTree(paths []string) string {
	sort.Strings(paths)

	var b strings.Builder
	prevParts := []string{}
	for _, p := range paths {
		parts := strings.Split(p, "/")

		// Find common prefix depth with previous path.
		common := 0
		for common < len(prevParts) && common < len(parts) && prevParts[common] == parts[common] {
			common++
		}

		// Print new directory components.
		for i := common; i < len(parts)-1; i++ {
			indent := strings.Repeat("  ", i)
			b.WriteString(fmt.Sprintf("%s%s/\n", indent, parts[i]))
		}

		// Print the file.
		indent := strings.Repeat("  ", len(parts)-1)
		b.WriteString(fmt.Sprintf("%s%s\n", indent, parts[len(parts)-1]))

		prevParts = parts
	}
	return b.String()
}

// isArtifactNoise returns true for paths that should be excluded from the
// existing artifacts listing: build outputs, dependency directories, trace
// files, and other noise that would bloat the context window.
func isArtifactNoise(path string) bool {
	// Normalize separators for matching.
	p := strings.ToLower(path)

	noiseSegments := []string{
		"/node_modules/",
		"/_requests/",
		"/.git/",
		"/dist/",
		"/build/",
		"/.next/",
		"/__pycache__/",
		"/vendor/",
		"/.cache/",
		"/coverage/",
		"/.vite/",
	}
	for _, seg := range noiseSegments {
		if strings.Contains(p, seg) {
			return true
		}
	}

	// Skip lock files and binary assets.
	noiseFiles := []string{
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		".ds_store",
	}
	base := filepath.Base(p)
	for _, nf := range noiseFiles {
		if base == nf {
			return true
		}
	}

	return false
}

// mimeForPath returns a MIME type based on file extension (scheduler-local copy).
func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "text/x-go"
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "text/yaml"
	case ".js":
		return "text/javascript"
	case ".ts":
		return "text/typescript"
	case ".py":
		return "text/x-python"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".png":
		return "image/png"
	default:
		return "text/plain"
	}
}

// taskResultSummary collapses whitespace for display. When maxLen > 0, truncates.
func taskResultSummary(result string, maxLen int) string {
	s := strings.Join(strings.Fields(result), " ")
	if maxLen > 0 && len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// persistHitCounters saves all modified agent knowledge graphs so that
// in-memory hit counters (Hits, LastHitAt) survive across requests.
func (s *Scheduler) persistHitCounters(ctx context.Context) {
	s.knowledgeMu.RLock()
	graphs := s.AgentGraphs
	s.knowledgeMu.RUnlock()

	for agentName, g := range graphs {
		if g == nil || len(g.Nodes) == 0 {
			continue
		}
		agentStorage := s.StorageFactory(agentName)
		if err := knowledge.SaveToTier(ctx, agentStorage, runtime.TierAgent, g); err != nil {
			slog.Debug("persistHitCounters: failed to save", "agent", agentName, "error", err)
		}
	}
}

// commonDirPrefix returns the longest common directory prefix of the given paths,
// ending with "/". Returns empty string if no common prefix exists.
func commonDirPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	prefix := paths[0]
	for _, p := range paths[1:] {
		for !strings.HasPrefix(p, prefix) {
			// Strip trailing "/" before searching so we find the parent
			// directory separator, not the trailing one (which would loop forever).
			i := strings.LastIndex(strings.TrimSuffix(prefix, "/"), "/")
			if i < 0 {
				return ""
			}
			prefix = prefix[:i+1]
		}
	}
	// Ensure it ends with "/" (directory).
	if !strings.HasSuffix(prefix, "/") {
		i := strings.LastIndex(prefix, "/")
		if i < 0 {
			return ""
		}
		prefix = prefix[:i+1]
	}
	return prefix
}
