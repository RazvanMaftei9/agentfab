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
	Logger         *message.Logger
	Storage        runtime.Storage
	Meter          runtime.ExtendedMeter
	StorageFactory func(string) runtime.Storage
	RequestID      string
	Events         event.Bus
	Agents         []runtime.AgentDefinition
	UserRequest    string
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

	inflightMu sync.Mutex
	inflight   map[string]int // agent name → count of currently dispatched tasks

	nonceMu    sync.Mutex
	taskNonces map[string]string // task ID → expected dispatch nonce
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
			// StatusUpdate messages carry streaming progress from distributed
			// agents. Convert to TaskProgress events for the UI instead of
			// routing through the demux (they don't need task-based routing).
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
				continue
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
			if !s.tryAcquireAgent(task.Agent) {
				continue // agent at capacity, try next iteration
			}
			dispatched[task.ID] = true
			newDispatches++
			pending++
			task.Status = taskgraph.StatusRunning
			s.logTaskStatus(ctx, task, "")
			s.writeGraphStatus(ctx, graph)
			startEvt := event.Event{
				Type:            event.TaskStart,
				TaskID:          task.ID,
				TaskAgent:       task.Agent,
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
				defer s.releaseAgent(t.Agent)
				defer func() {
					tCancel()
					s.unregisterTaskCancel(t.ID)
				}()
				start := time.Now()
				inBefore, outBefore := s.agentUsageSnapshot(ctx, t.Agent)
				var err error
				if t.LoopID != "" {
					err = s.dispatchLoopTask(tCtx, t, graph)
				} else {
					err = s.dispatchTask(tCtx, t, graph)
				}
				inAfter, outAfter := s.agentUsageSnapshot(ctx, t.Agent)
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
					s.logTaskStatus(ctx, t, err.Error())
					s.writeGraphStatus(ctx, graph)
					failEvt := event.Event{
						Type:              event.TaskFailed,
						TaskID:            t.ID,
						TaskAgent:         t.Agent,
						Duration:          time.Since(start),
						ErrMsg:            err.Error(),
						AgentInputTokens:  inAfter - inBefore,
						AgentOutputTokens: outAfter - outBefore,
					}
					s.attachUsage(ctx, &failEvt)
					s.Events.Emit(failEvt)
					return
				}
				s.logTaskStatus(ctx, t, "")
				s.writeGraphStatus(ctx, graph)
				if t.Status == taskgraph.StatusFailed {
					// Adaptive routing: try escalating to a higher-tier model.
					if s.Router != nil {
						s.Router.RecordOutcome(t.Agent, t.ID, false)
						if s.Router.Escalate(t.Agent, t.ID) {
							slog.Info("adaptive routing: escalating model tier", "task", t.ID, "agent", t.Agent)
							t.Status = taskgraph.StatusPending
							t.Result = ""
							return // task will be retried on next iteration
						}
					}
					failEvt := event.Event{
						Type:              event.TaskFailed,
						TaskID:            t.ID,
						TaskAgent:         t.Agent,
						Duration:          time.Since(start),
						ErrMsg:            t.Result,
						AgentInputTokens:  inAfter - inBefore,
						AgentOutputTokens: outAfter - outBefore,
					}
					s.attachUsage(ctx, &failEvt)
					s.Events.Emit(failEvt)
				} else {
					if s.Router != nil {
						s.Router.RecordOutcome(t.Agent, t.ID, true)
					}
					doneEvt := event.Event{
						Type:              event.TaskComplete,
						TaskID:            t.ID,
						TaskAgent:         t.Agent,
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
						s.logTaskStatus(ctx, dep, dep.Result)
						cascadeEvt := event.Event{
							Type:      event.TaskFailed,
							TaskID:    id,
							TaskAgent: dep.Agent,
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
							s.logTaskStatus(ctx, dep, dep.Result)
							cascadeEvt := event.Event{
								Type:      event.TaskFailed,
								TaskID:    id,
								TaskAgent: dep.Agent,
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
		To:        task.Agent,
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
	ID          string `json:"id"`
	Agent       string `json:"agent"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ArtifactURI string `json:"artifact_uri,omitempty"`
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
			ID:          t.ID,
			Agent:       t.Agent,
			Description: t.Description,
			Status:      string(t.Status),
			ArtifactURI: t.ArtifactURI,
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
	if existingParts := s.buildExistingArtifacts(ctx, task.Agent); len(existingParts) > 0 {
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
				"dependency_agent": dep.Agent,
			}})
			continue
		}

		data := map[string]any{
			"dependency_id":    dep.ID,
			"dependency_agent": dep.Agent,
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
		To:        task.Agent,
		Type:      message.TypeTaskAssignment,
		Parts:     parts,
		Metadata: map[string]string{
			"task_id":        task.ID,
			"task_scope":     string(task.Scope.EffectiveScope()),
			"dispatch_nonce": nonce,
		},
		Timestamp: time.Now(),
	}

	if s.Logger != nil {
		s.Logger.Log(ctx, msg)
	}

	if err := s.Comm.Send(ctx, msg); err != nil {
		return fmt.Errorf("send task to %q: %w", task.Agent, err)
	}

	// Wait for result.
	result, err := s.waitForResult(ctx, task, nonce)
	if err != nil {
		return err
	}

	task.Result = extractText(result)
	task.ArtifactURI = extractArtifactURI(result)
	task.ArtifactFiles = extractArtifactFiles(result)

	// In distributed mode, agents record LLM usage in their own meters.
	// Feed the reported usage into the conductor's meter so aggregate
	// totals include all agents.
	s.recordRemoteUsage(ctx, result)

	if result.Metadata != nil && result.Metadata["status"] == "failed" {
		task.Status = taskgraph.StatusFailed
	} else {
		task.Status = taskgraph.StatusCompleted
	}

	return nil
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
	s.CancelTask(taskID)
	s.Events.Emit(event.Event{
		Type:          event.TaskAmended,
		AmendedTaskID: taskID,
		AmendedAgent:  task.Agent,
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

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timerC:
			return nil, fmt.Errorf("task %q timed out", task.ID)
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

		line := fmt.Sprintf("- %s (%s)%s: %q", t.ID, t.Agent, marker, t.Description)

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
	lc := &loop.LoopContext{
		Definition: *loopDef,
		State: loop.LoopState{
			LoopID:       loopDef.ID,
			CurrentState: loopDef.InitialState,
		},
		TaskID:        task.ID,
		Conductor:     "conductor",
		OriginalTask:  task.Description,
		UserRequest:   s.UserRequest,
		DepParts:      depParts,
		DispatchNonce: nonce,
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timerC:
			return fmt.Errorf("loop task %q timed out", task.ID)
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("channel closed while waiting for loop completion")
			}
			if msg.Type == message.TypeUserQuery {
				if err := s.forwardUserQuery(ctx, msg); err != nil {
					return err
				}
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

				if msg.Metadata != nil && msg.Metadata["loop_escalated"] == "true" {
					task.Status = taskgraph.StatusEscalated
				} else {
					task.Status = taskgraph.StatusCompleted
				}
				return nil
			}
			// Ignore non-terminal messages (agents route among themselves).
		}
	}
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
			"dependency_agent": dep.Agent,
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

	sharedCount := 0
	if sharedGraph != nil {
		sharedCount = len(sharedGraph.Nodes)
	}
	agentCount := 0
	if agentGraphs != nil {
		if ag := agentGraphs[task.Agent]; ag != nil {
			agentCount = len(ag.Nodes)
		}
	}
	slog.Debug("buildKnowledgeContext", "agent", task.Agent, "task", task.ID,
		"shared_nodes", sharedCount, "agent_nodes", agentCount)

	if sharedGraph == nil || len(sharedGraph.Nodes) == 0 {
		// Even without a shared graph, check agent graph.
		if agentGraphs == nil {
			slog.Debug("no knowledge graphs available", "agent", task.Agent)
			return nil
		}
		ag := agentGraphs[task.Agent]
		if ag == nil || len(ag.Nodes) == 0 {
			slog.Debug("no knowledge nodes for agent", "agent", task.Agent)
			return nil
		}
	}

	var result knowledge.LookupResult
	if agentGraphs != nil {
		agentGraph := agentGraphs[task.Agent]
		result = knowledge.LookupDual(agentGraph, sharedGraph, task.Agent, task.Description, knowledge.LookupOpts{})
	} else {
		result = knowledge.Lookup(sharedGraph, task.Agent, task.Description, knowledge.LookupOpts{})
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

	slog.Debug("knowledge lookup result", "agent", task.Agent, "own", len(result.Own), "related", len(result.Related))

	// Emit KnowledgeLookup event for CLI visualization.
	if s.Events != nil && (len(result.Own) > 0 || len(result.Related) > 0) {
		var ownInfos []event.KnowledgeNodeInfo
		for _, n := range result.Own {
			ownInfos = append(ownInfos, event.KnowledgeNodeInfo{
				ID: n.ID, Agent: n.Agent, Title: n.Title,
			})
		}
		var relInfos []event.KnowledgeRelInfo
		for _, rn := range result.Related {
			relInfos = append(relInfos, event.KnowledgeRelInfo{
				KnowledgeNodeInfo: event.KnowledgeNodeInfo{
					ID: rn.ID, Agent: rn.Agent, Title: rn.Title,
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
			collectEdges(agentGraphs[task.Agent])
		}
		s.Events.Emit(event.Event{
			Type:                 event.KnowledgeLookup,
			KnowledgeLookupAgent: task.Agent,
			KnowledgeLookupTask:  task.ID,
			KnowledgeLookupOwnNodes: ownInfos,
			KnowledgeLookupRelNodes: relInfos,
			KnowledgeLookupEdges: edgeInfos,
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
		Agent:       task.Agent,
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

	baseDir := s.baseDir()
	if baseDir == "" {
		return fmt.Errorf("cannot resolve storage base dir")
	}

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
	if max := s.maxKnowledgeNodes(task.Agent); max > 0 {
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
	updatedAgent, _ := knowledge.LoadFromTier(ctx, s.StorageFactory(task.Agent), runtime.TierAgent)
	if updatedShared != nil {
		s.knowledgeMu.Lock()
		s.KnowledgeGraph = updatedShared
		if s.AgentGraphs == nil {
			s.AgentGraphs = make(map[string]*knowledge.Graph)
		}
		if updatedAgent != nil {
			s.AgentGraphs[task.Agent] = updatedAgent
		}
		s.knowledgeMu.Unlock()
	}

	slog.Info("incremental knowledge refreshed",
		"task", task.ID,
		"agent", task.Agent,
		"nodes", len(manifest.Nodes),
		"edges", len(manifest.Edges))
	return nil
}

func (s *Scheduler) baseDir() string {
	if s.Storage == nil {
		return ""
	}
	return filepath.Dir(s.Storage.SharedDir())
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
		if def.Name == task.Agent {
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
	baseDir := s.baseDir()
	if baseDir == "" {
		return
	}
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
