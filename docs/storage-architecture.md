# Storage Architecture

This document describes how agentfab manages cross-task and cross-agent data
exchange, the consistency model that follows from the current implementation,
and the direction the platform is moving toward.

## Tiers and what each is for

Every agent's runtime sees three storage tiers, exposed as filesystem paths
through environment variables:

| Tier | Env var | Lifetime | Visible to |
|---|---|---|---|
| Scratch | `$SCRATCH_DIR` | Single task. Cleared on task completion. | The task instance only. |
| Agent | `$AGENT_DIR` | Persistent per agent across tasks. | All tasks belonging to the same agent. |
| Shared | `$SHARED_DIR` | Persistent across the whole fabric. | All agents, across all tasks. |

Upstream artifacts that a downstream agent consumes are conventionally located
at `$SHARED_DIR/artifacts/<upstream-agent>/`. The conductor's task assignment
references upstream task IDs so the receiving agent knows which paths to read.

## Implementation today (May 2026)

Each agent runtime owns a `runtime.Workspace`:

```go
type Workspace struct {
    Scratch MaterializedTier
    Agent   MaterializedTier
    Shared  MaterializedTier
}
```

Every `MaterializedTier` is backed by a `stagedTier`
(`internal/runtime/materialize.go`). A `stagedTier` is a **per-agent
filesystem snapshot** copied from the underlying storage backend at the
moment that agent's workspace is opened. Concretely:

```
newStagedTier(storage, tier):
  dir := os.MkdirTemp("", "agentfab-tier-*")   # per-agent local temp dir
  files := storage.ListAll(tier, "")           # one-shot listing
  for each file: copy storage → dir            # one-shot copy
  return stagedTier{dir, ...}
```

The agent's `$SHARED_DIR` is `dir` — not the underlying storage path. Writes
go to the local snapshot. `workspace.Sync()` walks the snapshot, reads every
file, and writes it back to the storage backend.

**This is the same on every topology.** Local single-process, local-bootstrap
multi-node, kind cluster with a host-mounted shared volume, multi-cluster with
S3-backed storage — in every case each agent has its own `agentfab-tier-*`
local directory, populated by an initial snapshot from storage.

## The race we caught and what closes it

### Symptom

In the us-energy-grid dryrun on 2026-05-25, `demand-forecaster` (t3,
dispatched immediately after `data-engineer`'s t1 task_result) ran
`find $SHARED_DIR/artifacts -maxdepth 2 -type f` as its first tool call
and received `(no output)` — the directory was an empty snapshot. The
agent then surfaced the absence honestly:

> "`$SHARED_DIR/artifacts` was not present during this task, so no upstream
> machine-readable artifacts were available to load."

The host-visible `.data/shared/artifacts/data-engineer/` was populated.
But t3's snapshot was taken before t1's sync had completed.

### Cause

The materialize → sync cycle had no happens-before relationship across
task boundaries:

```
t1: LLM done → emit task_result → … → workspace.Sync (async / delayed)
t3: dispatched → workspace materialize (ListAll + copy) → first LLM call
```

If `t3.materialize` ran before `t1.Sync` settled, `ListAll` returned a stale
file set and the per-agent snapshot was missing the new outputs.

### Mitigation: Option B (current)

`Workspace.Sync()` is now called at the start of `Agent.sendResult`, before
the `task_result` message is emitted. `MaterializedTier.Refresh()` (the
shared tier specifically) is called at the start of `Agent.executeTask`,
before any LLM call.

Together this gives a per-task happens-before:

```
t1: LLM done → workspace.Sync (synchronous) → emit task_result
t3: dispatched → workspace.Shared.Refresh (re-ListAll + re-copy from storage) → first LLM call
```

The conductor still dispatches tasks in parallel where the DAG allows it.
The barrier is at each task's own boundary, not at the DAG level. Within a
task, the agent sees a consistent shared-tier view that reflects everything
its upstream task results have settled in storage.

See `internal/agent/agent.go` — `executeTask` and `sendResult` carry inline
comments referencing this document.

## Where this is still imperfect

- **Refresh granularity is full-tier.** Each refresh re-lists the entire
  shared tier and re-copies every file. For a fabric with many large
  artifacts, that's O(tier) work per task even when the task only needs
  one upstream agent's outputs.
- **Sync is best-effort, not transactional.** If `Sync()` fails partway,
  some files made it to storage and others did not. We log and continue.
- **Snapshot semantics, not stream semantics.** Inside a task, the agent
  sees a single point-in-time view. If a parallel task in the same DAG
  layer writes during the agent's run, the agent does not see those
  writes (would require a within-task refresh, which we deliberately do
  not do — agents should reason against stable dependency inputs).
- **No artifact versioning.** Two runs that produce the same logical
  artifact at the same path will overwrite each other in storage. There
  is no immutability guarantee.

These are acceptable for local-bootstrap and single-cluster deployments.
They become structural problems at multi-cluster scale.

## Long-term direction: Option D — content-addressable artifacts

The end-state design replaces the filesystem-globbing convention with
explicit artifact URIs in task assignment messages.

```
task_assignment {
  agent: "demand-forecaster"
  inputs: [
    "shared://agents/data-engineer/run-abc/manifest_v1.json@sha256:<hash>"
    "shared://agents/data-engineer/run-abc/ercot_load_zones.geojson@sha256:<hash>"
  ]
  ...
}
```

Properties this would give us:

1. **Immutability by construction.** Every write produces a new version
   with a stable content-addressed identifier. There is no overwrite.
2. **No race possible.** A URI either resolves or it does not. The
   conductor does not dispatch a downstream task until all its declared
   inputs are committed and addressable.
3. **No per-agent snapshot copy.** Agents resolve URIs lazily through a
   small artifact-fetch tool. Most agents only need a small fraction of
   their upstream agent's outputs; we stop copying everything.
4. **Multi-cluster ready.** S3, GCS, and Azure Blob all natively support
   the access pattern. Cross-region replication is a separate property
   layered on top.
5. **Provenance graph for free.** Every artifact carries the URIs of the
   inputs that produced it. Reproducibility, debugging, audit, and the
   "why is this number what it is" question all become tractable.

This is the same pattern that Spark, Airflow, Tekton, Kubernetes Jobs, and
modern data pipelines use. It is the boring industry-standard answer for
distributed task data exchange.

## Roadmap

| Stage | Status | Scope |
|---|---|---|
| Option A — Sync barrier on task_result | Implemented (`agent.go: sendResult`) | All topologies |
| Option B — Refresh shared tier on task start | Implemented (`agent.go: executeTask`) | All topologies |
| Per-dependency refresh (refresh only the specific upstream agent's subtree) | Not yet | Performance optimization on top of B |
| Option C — Live mount for single-host topologies | Not yet | Local-bootstrap and kind optimization |
| Option D — Content-addressable artifact URIs | Not yet | The structural fix; required before serious multi-cluster work |

Option D is the right end state. Options B and C are the bridges that let
us run reliably while we get there. The current code path should be treated
as a known-imperfect intermediate, and any future work that touches
`internal/runtime/materialize.go` should be evaluated against whether it
moves us toward D or merely paves over the existing model.
