# Orchestration Internals

Implementation-level documentation of AgentFab orchestration flows. Covers the full path from user input to result output, linking directly to source files and functions. Written for anyone (human or AI) who needs to understand or modify the system without prior context.

## Key Source Files

| File | What it does |
|---|---|
| `internal/conductor/conductor.go` | HandleRequest lifecycle, pause/resume/cancel delegation, graph restructuring |
| `internal/conductor/scheduler.go` | Dispatch loop, demux, task/loop dispatch, pause gate, cancel logic, enrichLoopGuidelines |
| `internal/conductor/decompose.go` | LLM-based request → TaskGraph conversion with templates |
| `internal/conductor/templates.go` | DecomposeTemplate loading from embedded FS |
| `internal/conductor/chat.go` | Agent chat, amendment/restructure/escalation marker parsing |
| `internal/agent/agent.go` | Agent Run loop, task execution, loop step completion, user queries |
| `internal/agent/envelope.go` | ResponseEnvelope type, extractEnvelope, extractVerdictFromEnvelope |
| `internal/agent/toolexec.go` | Live tool execution during LLM generation (ToolExecutor); `SandboxEnv()` and `SandboxConfig()` helpers shared with verification |
| `internal/agent/tools.go` | Post-processing tool execution, routed through sandbox.Run |
| `internal/agent/toolbind.go` | Tool binding for LLM tool-calling interface |
| `internal/agent/artifact.go` | Artifact handling and file block accumulation |
| `internal/loop/types.go` | LoopDefinition, StateConfig (with ReviewGuidelines), Transition, LoopState |
| `internal/loop/fsm.go` | FSM state machine: Transition(), Validate(), max_transitions enforcement |
| `internal/loop/resolve.go` | ResolveNextState(), IsReviewState(), BuildStatePrompt(decisionContext) |
| `internal/loop/context.go` | LoopContext encode/decode as DataPart |
| `internal/taskgraph/types.go` | TaskNode, TaskStatus constants, TaskGraph, ArtifactFile |
| `internal/taskgraph/dag.go` | ReadyTasks(), AllDone(), FailDependents(), TopologicalSort() |
| `internal/event/event.go` | Event types, Bus, Emit() |
| `internal/knowledge/types.go` | Graph, Node, Edge, Manifest, ManifestNode, SupersededBy |
| `internal/knowledge/graph.go` | Knowledge graph load/save/merge/query/summary, IsSuperseded |
| `internal/knowledge/generate.go` | LLM-based knowledge extraction from task results |
| `internal/knowledge/lookup.go` | BFS knowledge lookup + keyword-filtered decision injection for context |
| `internal/knowledge/write.go` | Apply(): write docs to disk, merge manifest into graph |
| `internal/structuredoutput/` | Per-provider JSON extraction strategies (claude, openai, gemini) |
| `internal/metrics/metrics.go` | Debug log analysis, per-agent/per-model cost reporting |
| `internal/runtime/context.go` | Context key helpers: WithRequestID, WithTaskID, WithLoopID, WithLoopState |
| `internal/sandbox/local.go` | Sandboxed command execution: stripped env, timeout, AllowedDirs |
| `internal/sandbox/restrict.go` | Policy type for OS-level filesystem restrictions |
| `internal/sandbox/restrict_darwin.go` | macOS sandbox-exec SBPL profile generation |
| `internal/sandbox/restrict_linux.go` | Linux Landlock filesystem restrictions |
| `internal/sandbox/toolchains.go` | Auto-detection of installed development toolchain paths |
| `internal/knowledge/cold.go` | Cold storage: MoveToCold, RestoreFromCold, LookupCold, purgeColdExpired |
| `internal/knowledge/curate.go` | LLM-based curation: Curate, ValidateCurated, SwapCurated |
| `internal/conductor/disambiguate.go` | Request clarity evaluation before decomposition |
| `internal/config/system.go` | FabricDef (agents.yaml) schema and parsing |
| `internal/config/manifest.go` | Agent definition integrity: GenerateManifest, VerifyManifest |
| `internal/config/datadir.go` | Platform-aware default data directory |
| `internal/ui/renderer.go` | Event-driven TUI rendering |
| `cmd/agentfab/main.go` | REPL, stdin handling, pause/resume/cancel commands, agent chat, metrics cmd |

---

## 1. Request Lifecycle (End-to-End)

A user request flows through these phases in `Conductor.HandleRequest`:

```
User input → REPL (mainLoop in main.go)
  → Conductor.HandleRequest
    1. reqCtx/reqCancel creation
       Store reqCancel on Conductor for pre-scheduler cancellation
       reqCtx = runtime.WithRequestID(reqCtx, requestID)
    2. Screen --classify as actionable or not
       Non-actionable → emit RequestScreened + RequestComplete, return early
       Trivial → fast path: synthetic 1-task graph, skip decomposition
       Screen failure → log warning, proceed to disambiguate (fail-open)
    2b. Disambiguate --evaluate request clarity
        Clear → proceed to decompose
        Ambiguous → present clarifying question to user, loop until clear
    3. Load conductor special knowledge + knowledge graph summary + active decisions
    4. Decompose --LLM generates TaskGraph DAG (with templates)
       Emit DecomposeStart before, DecomposeEnd after
    5. Create Scheduler, store on Conductor under lock
    6. Scheduler.Execute --dispatch loop
    7. On return: check if all tasks cancelled → ErrRequestCancelled
    8. Knowledge.Generate --background goroutine
    9. writeArtifacts → per-task .md files + combined results.md
    10. Emit RequestComplete, return collectResults
  → REPL renders results
```

**Context cancellation**: `reqCtx` is a child of the REPL's root context. `reqCancel` is stored on the Conductor so `CancelExecution()` can cancel even during Screen or Decompose (before the Scheduler exists). Every phase checks `reqCtx.Err()` and returns `ErrRequestCancelled` if set.

**Event emission points**:
- `DecomposeStart` --before LLM call
- `DecomposeEnd` --after graph parsed, includes task summaries and token usage
- `RequestScreened` --if input is non-actionable
- `RequestComplete` --after all tasks done, includes total duration and system-wide token usage

---

## 2. Scheduler Dispatch Loop

`Scheduler.Execute()`:

```go
for !graph.AllDone() {
    // 1. Pause gate
    select { case <-pauseCh: ... <-resumeCh / <-ctx.Done() }

    // 2. Graph replacement check (non-blocking)
    select { case newGraph := <-graphReplace: swap }

    // 3. ReadyTasks()
    ready := graph.ReadyTasks()

    // 4. Deadlock detection
    if len(ready) == 0 && !graph.AllDone() → error

    // 5. Per-agent concurrency limit filtering
    for each ready task:
        if !tryAcquireAgent(task.Agent) → skip (stays pending for next iteration)

    // 6. Parallel dispatch goroutines (only tasks that passed limit check)
    for each dispatching task:
        task.Status = StatusRunning
        emit TaskStart
        go func(t) {
            defer releaseAgent(t.Agent)
            taskCtx, taskCancel = context.WithCancel(ctx)
            registerTaskCancel(t.ID, taskCancel)
            if t.LoopID != "" → dispatchLoopTask()
            else              → dispatchTask()
            // handle error / emit TaskComplete or TaskFailed
        }

    // 7. If no tasks dispatched (all at capacity), backoff 50ms

    // 8. wg.Wait()

    // 9. Failure cascade
    for each failed ready task:
        graph.FailDependents(task.ID) --BFS
        emit TaskFailed per cascaded task
}
```

### Task Status State Machine

Defined in `taskgraph/types.go`:

```
pending → running → completed
                  → failed
                  → escalated  (loop max_transitions exceeded)
                  → cancelled  (user cancel or pause cleanup)
```

- `ReadyTasks()`: returns tasks with `StatusPending` whose all `DependsOn` are `StatusCompleted`.
- `AllDone()`: true when no task is `StatusPending` or `StatusRunning`.

### Per-Agent Concurrency Limits

The scheduler enforces `MaxConcurrentTasks` (from `AgentDefinition`, default 0 = unlimited). Before dispatching, each ready task must pass `tryAcquireAgent`. Tasks that exceed the limit stay `StatusPending` and retry next iteration.

### taskCancels Map

`map[string]context.CancelFunc` protected by `taskCancelMu`. Each dispatch goroutine registers its `taskCancel` via `registerTaskCancel` and unregisters in its deferred cleanup. `CancelAllRunning()` iterates and calls all cancel functions.

### graphReplace Swap

Buffered channel of capacity 1. `ReplaceGraph()` does a non-blocking send. The dispatch loop checks it non-blocking on each iteration. When received, `graph` and `s.graph` are swapped and the loop continues from the top.

---

## 3. Demux Message Router

The demux system routes messages from the conductor's single receive channel to per-task waiters.

**Structure**:

```go
type demux struct {
    mu       sync.Mutex
    waiters  map[string]chan *message.Message  // task_id → waiter channel
    overflow []*message.Message                // messages arriving before subscription
}
```

**Goroutine**: `runDemux()` reads from `s.Comm.Receive(ctx)` in a loop. Each message is passed to `demux.route()`, which extracts `task_id` from `msg.Metadata["task_id"]` and delivers to the matching waiter, or appends to overflow if no waiter is registered for that task ID.

**subscribe(taskID)**: Creates a buffered channel (cap 16) for a task ID. Drains matching overflow messages into the new channel. Used by both `waitForResult()` and `waitForLoopCompletion()`.

**route(msg)**: Extracts `task_id` from `msg.Metadata["task_id"]` and delivers to the matching waiter, or appends to overflow. All agent result paths propagate `task_id` in metadata.

**Overflow buffer**: Messages arriving before subscription (or when a channel is full) are buffered and drained on the next matching `subscribe`.

**Lifecycle**: `ensureDemux()` starts the goroutine exactly once via `sync.Once`. `stopDemux()` cancels it when `Execute()` returns.

---

## 4. Task Dispatch (Regular Tasks)

`dispatchTask()`:

### Message Parts Assembly

Parts are built in order:

1. **Task description** --`TextPart` with `task.Description`
2. **User request** --`DataPart` with key `user_request`
3. **Pipeline context** --`buildPipelineContext()`: DAG visualization showing all tasks, their agents, dependency arrows, and output targeting guidance. Highlights the current task with `[YOU]`.
4. **Knowledge context** --`buildKnowledgeContext()`: BFS over knowledge graph edges + keyword-filtered decision injection (see Section 8)
5. **Existing artifacts** --`buildExistingArtifacts()`: lists prior artifacts from shared storage so agents can iterate
6. **Dependency results** --for each completed dependency:
   - First try `filterArtifacts()` for selective artifact passing
   - Fall back to `artifact_uri` + inline `result`

Context propagation: `ctx = runtime.WithTaskID(ctx, task.ID)` is set before dispatch, threading task_id into LLM metering and tool call logs.

### filterArtifacts

Heuristic-based artifact selection for specific producer→consumer pairs:
- `designer → developer`: include screenshots (.png/.jpg), spec.md, summary.md; exclude .html/.css build artifacts
- `architect → developer`: include design.md, decision records, summary.md
- All others: no filtering (fall back to full artifact URI)

### Message Send and Wait

Send `TypeTaskAssignment` to the agent, then call `waitForResult()`. On return, extract text, artifact URI, and artifact files from the result message. If `metadata["status"] == "failed"`, mark task as `StatusFailed`; otherwise `StatusCompleted`.

---

## 5. Orchestration Loops (FSM)

### 5.1 Data Model

**LoopDefinition** (`loop/types.go`):
```go
type LoopDefinition struct {
    ID             string
    Participants   []string       // agent names involved
    States         []StateConfig  // name + agent + review_guidelines
    Transitions    []Transition   // from/to/condition
    InitialState   string
    TerminalStates []string       // e.g. ["APPROVED", "ESCALATED"]
    MaxTransitions int
}
```

`StateConfig` includes `ReviewGuidelines string` --populated at dispatch time from the agent's YAML definition via `enrichLoopGuidelines()`. `ReviewGuidelinesForState(state)` looks up guidelines for a given state.

**LoopState** (`loop/types.go`): Tracks `CurrentState` and `TransitionCount`.

**LoopContext** (`loop/context.go`): Travels as a `DataPart` with key `loop_context`. Contains the full `LoopDefinition`, current `LoopState`, `TaskID`, `Conductor` name, `OriginalTask`, `UserRequest`, `DepParts`, and `WorkSummary`. Excluded from LLM input by `buildTaskInput()`.

**FSM** (`loop/fsm.go`): Holds `LoopDefinition` + `LoopState`. `Transition()` returns `(TransitionResult, error)`:
- `(Transitioned, nil)` — normal state change. Validates the transition is in the allowed set and increments `TransitionCount`.
- `(Escalated, nil)` — `MaxTransitions` exceeded. Auto-sets state to `"ESCALATED"`. This is a graceful budget exhaustion outcome, not an error.
- `(0, err)` — actual programming error: already in a terminal state, or invalid transition.

### 5.2 Conductor-Side: dispatchLoopTask

`dispatchLoopTask()`:

1. Look up `LoopDefinition` from `graph.GetLoop(task.LoopID)`
2. `enrichLoopGuidelines(loopDef)` --copies `review_guidelines` from agent definitions into state configs
3. Create and validate FSM via `loop.NewFSM()`
4. Build initial `LoopContext` with `InitialState`, task dependencies, user request
5. Build decision context from active knowledge graph decisions
6. Find first agent via `loopDef.AgentForState(loopDef.InitialState)`
7. Build prompt via `loop.BuildStatePrompt(taskDesc, def, state, lastVerdict, lastOutput, decisionContext)`
8. Append `loop.EncodeContext(lc)` as the last part (DataPart)
9. Send `TypeTaskAssignment` to first agent
10. Call `waitForLoopCompletion()`

**waitForLoopCompletion**:
- `subscribe(task.ID)` --subscribes by task ID (not participant group)
- Timeout = base timeout x `loopDef.MaxTransitions`
- Wait for `TypeTaskResult` --indicates loop reached terminal state
- If `metadata["loop_escalated"] == "true"`, task gets `StatusEscalated`; otherwise `StatusCompleted`
- All non-`TypeTaskResult` messages are ignored (agents route among themselves)

### 5.3 Agent-Side: Loop Routing

`completeLoopStep()`:

```
1. Extract verdict (for review states):
   - IsReviewState() checks if state has condition-based transitions
   - extractVerdictFromEnvelope() tries JSON response envelope first,
     falls back to legacy "VERDICT: approved" parsing and heuristics
   - If verdict == "approved" && toolCallCount == 0 → downgrade to "revise"
     (enforces tool usage: reviewers must read files before approving)

2. ResolveNextState():
   - Find transitions from current state
   - Single unconditional transition → follow it
   - Multiple transitions → match verdict against conditions
   - No match → error (force escalation)

3. FSM.Transition() → (TransitionResult, error):
   - If already terminal → return (0, error) — programming error
   - If TransitionCount >= MaxTransitions → auto-escalate to "ESCALATED", return (Escalated, nil)
   - Validate transition is in allowed set; if invalid → return (0, error)
   - Apply transition, increment TransitionCount → return (Transitioned, nil)
   - Caller branches: error → slog.Error + escalate; Escalated → emit event + escalate; Transitioned → continue loop

4. Terminal state? (fsm.IsTerminal()):
   YES → buildResultParts() + sendLoopResult() back to conductor
   NO  → buildResultParts() (persist artifacts) + parseSummary() + forwardLoopMessage()

5. Emit LoopTransition event on every state change
```

**WorkSummary**: Carried in `LoopContext.WorkSummary`. Updated by working agents (non-review states) in `forwardLoopMessage()`. In `sendLoopResult()`, if the terminal agent is a reviewer and `WorkSummary` exists, the first TextPart is replaced with the worker's summary so the task result reflects what was built, not the review verdict.

**Message type routing**: `TypeTaskAssignment` for work states, `TypeReviewRequest` for review states (determined by `IsReviewState()`).

### 5.4 Example Flow

```
Conductor → dev (WORKING, TypeTaskAssignment)
  dev completes → verdict="" → next state: REVIEWING
  Emit LoopTransition(WORKING → REVIEWING)
  dev persists artifacts, forwards summary

dev → arch (REVIEWING, TypeReviewRequest)
  arch reviews → verdict="revise" → next state: REVISING
  Emit LoopTransition(REVIEWING → REVISING)
  arch forwards review feedback

arch → dev (REVISING, TypeTaskAssignment)
  dev revises → verdict="" → next state: REVIEWING
  Emit LoopTransition(REVISING → REVIEWING)
  dev persists artifacts, forwards summary

dev → arch (REVIEWING, TypeReviewRequest)
  arch reviews → verdict="approved" → next state: APPROVED (terminal)
  Emit LoopTransition(REVIEWING → APPROVED)
  arch calls sendLoopResult() → conductor

arch → conductor (TypeTaskResult)
  Conductor marks task StatusCompleted
```

---

## 6. Circuit Breakers and Escalation

All the ways execution can be interrupted:

### 6.1 FSM max_transitions

When `TransitionCount >= MaxTransitions`, `FSM.Transition()` auto-sets state to `"ESCALATED"` and returns `(Escalated, nil)` --a graceful outcome, not an error. The agent detects `result == loop.Escalated` in `completeLoopStep()`, emits a `LoopTransition` event with verdict `"escalated"`, and sends terminal result with `loop_escalated: "true"` metadata. Actual errors (invalid transition, already terminal) are logged at `slog.Error` level and also result in escalation, but indicate programming bugs rather than expected budget exhaustion.

### 6.2 Agent Budget Check

`Meter.CheckBudget()` called before every LLM call. On breach, agent sends failed result with `"Budget exceeded"` message. No escalation --just a failed result to unblock the task graph.

### 6.3 Agent LLM Failure

Failed `Generate()` call → `escalate()`. Sends `TypeEscalation` to `escalation_target` with the failure reason and original task context. Also sends a failed result back to the conductor so the scheduler isn't stuck waiting. The escalation is informational; the result is what unblocks the task graph.

### 6.4 Task Timeout

`waitForResult()` uses a configurable timeout (default 5min, overridden by `task.Timeout`). On timeout, returns error `"task %q timed out"`. Task marked `StatusFailed`, cascade fires.

### 6.5 Scheduler Deadlock Detection

If `ReadyTasks()` returns empty but `AllDone()` is false → returns `"deadlock: no ready tasks but graph not done"`. This catches circular dependencies or states where all remaining tasks are blocked by failed dependencies.

### 6.6 Failure Cascade

`FailDependents()`: BFS from failed task through dependency edges. All pending/running descendants marked `StatusFailed` with result `"dependency %q failed"`. A `TaskFailed` event is emitted per cascaded task.

### 6.7 Automatic Verification Gate

Agents with a `verify` block in their definition run an automatic verification command when the tool loop ends naturally (LLM returns no tool calls). Implemented in `agent.go`:

1. `runVerify(ctx)` executes `Def.Verify.Command` via `sandbox.Run` using the same `SandboxConfig`/`SandboxEnv` helpers as tool execution.
2. On failure, error output (truncated to 4000 chars) is injected as a `[AUTOMATIC VERIFICATION FAILED]` user message, `maxIter` is extended by `verifyBonusIterations`, and the tool loop continues.
3. After `max_retries` failures, or when the agent exhausts iterations, a post-loop verification runs once more. If it fails, a `## VERIFICATION FAILED` header is prepended to the response for upstream visibility.
4. When `Def.Verify` is nil, all verification paths are no-ops.

---

## 7. User Interaction During Execution

### 7.1 Agent User Queries

1. Agent detects `ASK_USER:` marker in LLM output --`extractUserQuery()`
2. Agent sends `TypeUserQuery` to conductor --`askUser()`
3. `waitForResult()` in scheduler intercepts `TypeUserQuery`:
   - Creates a `UserQuery` struct with a `ResponseCh`
   - Sends it on `s.UserQueryCh`
   - Blocks waiting for answer on `ResponseCh`
   - Sends `TypeUserResponse` back to agent
   - Resets the timeout timer
4. REPL reads from `GetUserQueryCh()`, calls `handleUserQuery()`:
   - Displays question via `ui.RenderAgentQuery()`
   - Reads answer from stdin
   - Sends answer on `query.ResponseCh`
5. Agent receives `TypeUserResponse` in `askUser()`, returns answer
6. Agent re-generates with user answer appended to conversation

### 7.2 Agent Chat (! command)

1. REPL detects `!` prefix → `handleAgentChat()`
2. Shows agent picker via `ui.PickAgent()`
3. Reads chat message from stdin
4. Calls `Conductor.Chat()` with agent name, message, and optional task context
5. Chat builds agent's system prompt + amendment/escalation detection instructions
6. Knowledge context injected via `buildChatKnowledge()`
7. LLM response parsed for `AMEND_TASK:` / `RESTRUCTURE:` / `ESCALATE:` markers
8. If amendment detected:
   - Non-structural → `AmendTask()`: updates task description, resets to pending, cancels running instance
   - Structural → `RestructureGraph()` (see 7.4)
9. If `ESCALATE:` detected → escalation notice displayed, suggests coordinated work mode

### 7.3 Pause / Resume / Cancel

**Pause** (`stop`/`pause` command):
- `Conductor.PauseExecution()` → `Scheduler.Pause()`
- CAS `paused` false→true, close `pauseCh`, `CancelAllRunning()`
- Dispatch goroutines see cancelled context and exit silently (cancelled tasks keep their status; paused tasks are left as-is)
- Dispatch loop blocks on `pauseCh` select

**Resume** (`resume` command):
- `Conductor.ResumeExecution()` → `Scheduler.Resume()`
- CAS `paused` true→false
- Reset all `StatusRunning` tasks to `StatusPending`
- Close `resumeCh` (unblocks dispatch loop), re-create both channels

**Cancel** (`cancel` command):
- `Conductor.CancelExecution()` → `Scheduler.Cancel()` if scheduler exists, otherwise `reqCancel()` directly
- Mark all pending/running tasks as `StatusCancelled`
- `CancelAllRunning()` + `reqCancel()`
- If paused, unblock the pause gate so Execute can exit
- `HandleRequest` detects all-cancelled → returns `ErrRequestCancelled`

### 7.4 Graph Restructuring

`RestructureGraph()`:

1. Cancel all running tasks via `sched.CancelAllRunning()`
2. Build context from completed tasks --summaries of what's already done
3. Build modified request: original request + user's amendment + completed work context
4. Re-decompose with `Decompose()` using the modified request (with templates)
5. Merge: keep completed tasks from old graph + add new tasks from decomposition
6. Re-ID new tasks to avoid collisions with completed task IDs
7. Swap graph on Conductor under lock + `sched.ReplaceGraph()`
8. Emit `TaskAmended` event
9. Scheduler picks up new graph on next loop iteration via `graphReplace` channel

---

## 8. Knowledge System Integration

### Node Provenance

Knowledge nodes carry provenance metadata:

| Field | Type | Description |
|---|---|---|
| `confidence` | float64 (0-1) | LLM-assigned certainty score. On merge, only raised (never lowered). |
| `source` | string | `"task_result"`, `"inferred"`, or `"user_provided"`. |
| `ttl_days` | int | Days until stale. 0 = never expires. Not overwritten by zero on merge. |

`Node.IsStale()` returns true when `time.Since(UpdatedAt) > TTLDays * 24h`.

**Supersession**: `Graph.IsSuperseded(nodeID)` returns true if another non-stale node has a `supersedes` edge pointing to this node. Superseded nodes are treated as stale and excluded from lookup results. During `Merge`, if two decision nodes share the same domain tag, a `supersedes` edge is auto-created from newer to older.

### Post-Execution: Generate

After `Scheduler.Execute()` returns, `knowledge.Generate()` runs in a background goroutine:

1. LLM produces a `Manifest` (nodes + edges) from task results + existing graph summary, including `confidence`, `source`, and `ttl_days` per node
2. `knowledge.Apply()` writes node markdown files, merges manifest into graph, saves to `shared/knowledge/graph.json`

### Pre-Execution: Load and Inject

1. `knowledge.Load()` reads graph from shared storage
2. `Graph.Summary()` → injected into conductor context for `Decompose()`
3. `buildDecomposeDecisionContext()` scores decision-tagged nodes by keyword relevance against the user request and includes only those above a relevance threshold

### Per-Task: Knowledge Context

`buildKnowledgeContext()` calls `knowledge.Lookup()`:

1. BFS from agent's non-stale own nodes, up to `MaxDepth` (default 2) hops, skipping stale and superseded nodes
2. Score: `1/depth + keyword_overlap + (confidence * 0.5)`, cap at `MaxNodes` (default 15)
3. **Decision injection**: After BFS, decision-tagged nodes are scored by keyword relevance against the task description. Only decisions above a relevance threshold are injected, keeping context focused while still propagating relevant project-wide constraints.
4. Injected as `DataPart`: `knowledge_context` (own nodes) + `related_knowledge` (cross-agent nodes via edges + decision nodes)

### Cold Storage

`internal/knowledge/cold.go` manages archiving infrequently-accessed nodes:

**MoveToCold(ctx, storage, activeGraph, opts)**:
1. Iterate active nodes; skip decision-tagged nodes
2. `IsCold(minHits, coldDays)` checks: `Hits < MinHits` AND last access > `ColdDays` ago
3. Move cold nodes + their docs to `cold_storage/graph.json` and `cold_storage/docs/`
4. Move edges referencing cold nodes to cold graph; keep only fully-active edges in active graph
5. `purgeColdExpired()` removes cold nodes older than `RetentionDays` and cleans dangling edges
6. Save cold graph

**RestoreFromCold(ctx, storage, active, cold, nodeIDs)**: Moves specific nodes back to active graph, restores docs, moves connecting edges back.

**LookupCold(coldGraph, query, maxResults)**: Keyword-overlap scoring over cold nodes, returns top matches (default 10).

### Knowledge Curation

`internal/knowledge/curate.go` provides LLM-based graph consolidation:

**Curate(ctx, generate, agentName, graph, opts)**:
1. Checks graph exceeds `threshold` (default 50 nodes)
2. Sends graph summary to LLM with consolidation instructions
3. LLM produces a reduced manifest preserving all decision and high-confidence (≥ 0.8) nodes
4. Returns `CurateResult` with curated graph, `NodesIn`, `NodesOut`

**ValidateCurated(original, curated)**: Checks:
- Node count did not increase
- All active decision nodes preserved
- All high-confidence (≥ 0.8) nodes preserved
- No dangling edges

**SwapCurated(ctx, storage, original, curated, coldOpts)**:
1. Moves removed nodes to cold storage (not deleted)
2. Moves edges referencing removed nodes to cold graph
3. Purges expired cold nodes
4. Increments graph version

**Conductor integration** (`conductor.go`): Curation runs asynchronously during idle periods. Per-agent curation status tracked via `curationRunning` map.

---

## 9. Event Bus

**Type**: `event.Bus` --`chan Event` with buffer 128.

**Emit**: Non-blocking send with nil-safe + closed-channel recovery. Drops events if buffer is full.

### Event Flow

Events flow from conductor/scheduler/agents → REPL → renderer:

1. **Per-request bus**: REPL creates a new `event.NewBus()` for each request, passes to conductor via `SetEvents()`. Renderer reads from this bus in a goroutine.
2. **Startup bus**: Separate bus for `AgentReady` / `AllAgentsReady` events during `Start()`.

### Key Event Sequence (Happy Path)

```
DecomposeStart
DecomposeEnd (includes task summaries)
TaskStart (per task, as they become ready)
  TaskProgress* (streaming LLM output snippets, ~80 chars)
TaskComplete | TaskFailed (per task)
  LoopTransition* (per FSM state change in loop tasks)
KnowledgeStart → KnowledgeEnd (background, after scheduler completes)
AgentSleep* → CurationStarted → CurationComplete → AgentWake* (idle curation)
RequestComplete (total duration, has_failures, cumulative token usage, per-model breakdown)
```

### Event Types

| Type | When | Key Fields |
|---|---|---|
| `AgentReady` | Agent goroutine started | AgentName, AgentModel |
| `AllAgentsReady` | All non-conductor agents ready | --|
| `RequestScreened` | Input classified as non-actionable | ScreenMessage |
| `DecomposeStart` | Before decompose LLM call | --|
| `DecomposeEnd` | Task graph ready | Tasks (summaries), token usage |
| `TaskStart` | Task status → running | TaskID, TaskAgent |
| `TaskProgress` | Streaming LLM chunk | ProgressText (~80 char tail) |
| `TaskComplete` | Task succeeded | TaskID, TaskAgent, Duration, ResultSummary |
| `TaskFailed` | Task failed (direct or cascade) | TaskID, TaskAgent, ErrMsg |
| `LoopTransition` | FSM state change | LoopID, FromState, ToState, Verdict, LoopCount |
| `TaskAmended` | Task description changed via chat | AmendedAgent |
| `AgentQueryReceived` | Agent asked user question | QueryAgent, QueryText |
| `AgentQueryAnswered` | User answered | AnswerText |
| `RequestPaused` | Execution paused | --|
| `RequestResumed` | Execution resumed | --|
| `RequestCancelled` | Execution cancelled | CancelReason |
| `CurationStarted` | Agent curation begins | CurationAgent |
| `CurationComplete` | Agent curation done | CurationAgent, CurationNodesIn, CurationNodesOut, ColdStorageMoved, ColdStoragePurged |
| `ColdStorageLookup` | Cold storage search | --|
| `AgentSleep` | Agent entering idle curation | AgentName |
| `AgentWake` | Agent waking from idle | AgentName |
| `RequestComplete` | All tasks done | TotalDuration, HasFailures, token usage, ModelUsages |

---

## 10. Observability

### Context Propagation

`internal/runtime/context.go` provides context key helpers that thread correlation IDs through the call chain:

- `WithRequestID(ctx, id)` / `RequestIDFrom(ctx)` --set by conductor in `HandleRequest`
- `WithTaskID(ctx, id)` / `TaskIDFrom(ctx)` --set by scheduler in `dispatchTask` / `dispatchLoopTask`
- `WithLoopID(ctx, id)` / `WithLoopState(ctx, state)` --set during loop execution

These propagate into `MeteredModel.Generate()` which logs structured `slog.Info("llm call", ...)` with all fields. `ToolExecutor.Execute()` similarly logs `slog.Info("tool call", ...)` with agent, tool, duration, exit code, output size.

### Metrics

`internal/metrics/metrics.go` reads debug output.jsonl files and produces:
- Per-agent summaries: call count, total/prompt/completion tokens, latency (p50/p95/p99), estimated cost
- Per-model summaries: same aggregations grouped by model
- Model pricing table for Anthropic, OpenAI, and Google models

Accessible via the `agentfab metrics` CLI command.

### LLM Call Records

`LLMCallRecord` (in `runtime/types.go`) captures per-call data: agent, model, input/output tokens, duration, request_id, task_id, and `FinishReason`. `local.Meter` provides `RequestUsage(requestID)`, `AllAgentUsage()`, and `AllRecords()` for queries.
