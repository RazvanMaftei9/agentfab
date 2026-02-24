# AgentFab Architecture

## Overview

AgentFab is a multi-agent orchestration framework. You define a set of specialized agents — each with its own LLM, prompt, tools, and persistent knowledge — and the framework handles decomposition, scheduling, communication, and state management.

```
┌──────────────────────────────────────────────────────────┐
│  Fabric                                                  │
│                                                          │
│  ┌───────────┐              ┌───────────┐                │
│  │ Conductor │◄────────────►│ Agent A   │                │
│  │ (user I/O)│              └───────────┘                │
│  └─────┬─────┘                                           │
│        │                    ┌───────────┐                │
│        └───────────────────►│ Agent B   │                │
│                             └─────┬─────┘                │
│                                   │                      │
│                             ┌─────▼─────┐                │
│                             │ Agent C   │                │
│                             └───────────┘                │
│                                                          │
│  ┌──────────────────────────────────────────────────┐    │
│  │ Shared Volume (logs, artifacts, definition file) │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
        ▲
        │ text, images, files
        ▼
      User
```

The Conductor is the only user-facing agent. It decomposes requests into task graphs and dispatches work to specialist agents. Agents communicate peer-to-peer — the Conductor orchestrates, it does not relay.

AgentFab ships with four default agents (conductor, architect, designer, developer) for software fabrication, but the framework is not limited to these roles. You define agents in YAML and the system adapts.

---

## Agents

### Definition

Every agent is defined in YAML:

```yaml
name: "data-analyst"
purpose: "Analyze datasets and produce visualizations"
capabilities: ["data_analysis", "chart_generation"]
model: "anthropic/claude-sonnet-4-5-20250929"
escalation_target: "architect"
tools:
  - name: python
    command: "python3 $ARGS"
    mode: live
review_guidelines: "Check statistical validity and chart labeling"
budget:
  max_total_tokens: 500000
  max_cost_usd: 2.00
verify:
  command: "cd $SCRATCH_DIR && python3 -m py_compile *.py 2>&1"
  max_retries: 2
  bonus_iterations: 5
```

Key fields:

| Field | Description |
|---|---|
| `name` | Unique identifier. Lowercase, alphanumeric + hyphens. |
| `purpose` | Injected into the agent's system prompt. |
| `model` | `provider/model-id` format. Each agent can use a different LLM. |
| `capabilities` | Tool/action identifiers. Used by the Conductor to match tasks to agents. |
| `tools` | Shell commands the LLM can invoke. Modes: `live`, `post_process`, `both`. |
| `escalation_target` | Where to defer on uncertainty. |
| `review_guidelines` | What this agent checks when reviewing peers. |
| `review_prompt` | Custom prompt override for when this agent reviews peers. |
| `verify` | Post-loop verification command. See [Verification Gates](#verification-gates). |
| `special_knowledge_file` | Markdown file loaded into context on every invocation. |
| `max_concurrent_tasks` | Concurrency limit for this agent. 0 = unlimited. |
| `budget` | Token and cost limits: `max_input_tokens`, `max_output_tokens`, `max_total_tokens`, `max_cost_usd`. |
| `task_timeout` | Maximum duration for a single task execution. |
| `max_knowledge_nodes` | Cap on active knowledge graph nodes. Default 200. |
| `curation_threshold` | Node count that triggers LLM-based graph consolidation. Default 50. |
| `cold_storage_retention_days` | Days before permanently purging cold-stored nodes. Default 1095 (3 years). |
| `model_tiers` | Ordered list of models for adaptive routing (e.g., try cheaper model first). |
| `shell_only` | If true, the agent edits files via shell commands only, with no file artifacts. |
| `address` | gRPC address for distributed mode. |

### LLM Providers

Four provider adapters via [Eino](https://github.com/cloudwego/eino):

| Adapter | Covers |
|---|---|
| Anthropic | Claude (Opus, Sonnet, Haiku) |
| OpenAI | GPT, o-series |
| Google | Gemini |
| OpenAI-compatible | Any provider with the OpenAI chat completions API (Groq, Together, Ollama, etc.) |

Model format: `provider/model-id` (e.g., `anthropic/claude-opus-4-5-20251101`, `openai/gpt-5.2`, `google/gemini-3.1-pro-preview`). Swap a model by changing one line in the YAML.

### Default Profiles

`agentfab init` creates a system with four built-in agents:

| Profile | Role | Default Model |
|---|---|---|
| `conductor` | Orchestration, user I/O | Claude Sonnet |
| `architect` | Technical authority, design decisions | GPT-5.2 |
| `designer` | UI/UX design, visual review | Claude Haiku |
| `developer` | Code generation, testing | Claude Opus |

These are starting points. Override models, add agents, remove agents, or replace all of them.

### Custom Agents

`agentfab agent compile` generates YAML agent definitions from plain Markdown descriptions:

1. Write one `.md` file per agent in a directory (e.g., `./agents/`). Each filename becomes the agent name.
2. Run `agentfab agent compile --input ./agents/ --output ./agents/`.
3. The compiler reads each `.md`, generates a structured YAML definition (capabilities, tools, escalation targets), copies the `.md` as a special knowledge file, writes `agents.yaml`, and generates a manifest.

A conductor agent is auto-added if not present in the input. Use `--dry-run` to preview generated definitions without writing files.

---

## Task Graphs

The Conductor decomposes user requests into a DAG of tasks. Each task is assigned to an agent, has dependencies, and can optionally participate in a review loop.

```
User: "Build a REST API with authentication and a dashboard"

┌──────────────────┐
│ T1: Design auth  │
│ (agent: arch)    │
└────────┬─────────┘
    ┌────┴────┐
    ▼         ▼
┌────────┐ ┌────────────┐
│ T2:    │ │ T3: Design │
│ Review │ │ dashboard  │
│ (loop) │ │ (agent: B) │
└────┬───┘ └──────┬─────┘
     │            │
     ▼            ▼
  ┌──────────────────┐
  │ T4: Implement    │
  │ (agent: dev)     │
  └──────────────────┘
```

### Decomposition

The Conductor uses LLM-driven structured decomposition:
1. Parse the request into discrete outcomes.
2. Match each outcome to an agent from the definition file.
3. Determine dependencies.
4. Annotate tasks needing iteration with orchestration loops.
5. Assign timeouts and budgets.

**Templates** (`defaults/templates/`) provide starting-point patterns (e.g., `build-app`, `fix-bug`, `design-change`). The LLM adapts these rather than generating graphs from scratch.

**Fast path**: Trivial requests skip decomposition entirely — a synthetic single-task graph is created.

**Disambiguation**: Before decomposition, the Conductor evaluates whether the request is clear enough. Ambiguous requests get a clarifying question before proceeding.

### Scheduling

The scheduler executes the DAG:
- Tasks run in parallel when dependencies allow.
- Per-agent concurrency limits are enforced.
- Failed tasks cascade: dependents are marked failed via BFS.
- Deadlock detection catches circular or stuck graphs.

During execution, users can **pause**, **resume**, **cancel**, or **amend** tasks. Amendments can trigger graph restructuring — completed work is preserved and new tasks are added.

---

## Orchestration Loops

When a task needs iteration between agents (e.g., implement → review → revise), it runs as a bounded FSM.

```
     max_transitions = 5
     ┌──────────────────────────────┐
     │                              │
     ▼                              │
┌─────────┐    ┌──────────┐    ┌────┴────┐
│ WORKING │───►│ REVIEWING│───►│ REVISING│
│(agent A)│    │(agent B) │    │(agent A)│
└─────────┘    └────┬─────┘    └─────────┘
                    │
                    ▼
              ┌──────────┐
              │ APPROVED │  (terminal)
              └──────────┘

  If max_transitions exceeded:
              ┌──────────┐
              │ ESCALATED│  (terminal → user)
              └──────────┘
```

**Key properties:**
- Every loop has a hard `max_transitions` cap. Infinite loops are structurally impossible.
- Agents route directly within loops — the Conductor only sees the initial dispatch and terminal result.
- `ESCALATED` is a graceful outcome, not an error. The Conductor presents the impasse to the user.
- Reviewers must use tools before approving (zero-tool approvals are downgraded to "revise").
- Task scope controls iteration depth: `small` (no loop), `standard` (max 3), `large` (max 5).

See [orchestration-internals.md](orchestration-internals.md) for the full FSM implementation.

---

## Knowledge System

Each agent maintains a persistent knowledge graph on disk. After every request, the system extracts knowledge from task results and merges it into the graph.

### What gets stored

Knowledge nodes with provenance metadata:

| Field | Description |
|---|---|
| `confidence` | 0–1, LLM-assigned. Only raised on merge, never lowered. |
| `source` | `task_result`, `inferred`, or `user_provided`. |
| `ttl_days` | Days until stale. 0 = never expires. |
| `hits` / `last_hit_at` | Access frequency tracking. Drives cold storage eviction. |
| `tags` | Including `decision` for project-wide constraints. |

Edges connect nodes: `depends_on`, `implements`, `related_to`, `supersedes`.

### Context injection

Before each task, relevant knowledge is injected into the agent's context:
- BFS traversal from the agent's own nodes, scored by depth + keyword overlap + confidence. Capped at 15 nodes.
- **Decision injection**: Decision-tagged nodes are scored by keyword relevance against the current task or request. Only decisions above a relevance threshold are injected. This keeps project-wide constraints available without flooding unrelated tasks with irrelevant context.

### Lifecycle

Knowledge nodes move through three tiers:

```
Active graph  →  Cold storage  →  Permanent purge
(frequently       (hits < 3,        (older than
 accessed)         30+ days old)     3 years)
```

- **Cold storage**: Infrequently accessed nodes are archived. Decision-tagged nodes are exempt.
- **Curation**: When the graph exceeds 50 nodes, LLM-based consolidation merges overlapping nodes. Validation ensures decisions and high-confidence nodes are preserved. Removed nodes go to cold storage, not deletion.
- **Supersession**: When decisions evolve, a `supersedes` edge retires the old decision. Superseded nodes are excluded from lookups.

---

## Storage

Three tiers, scoped by visibility and lifetime:

| Tier | Scope | Lifetime | Contents |
|---|---|---|---|
| Shared | All agents | Persistent | Logs, artifacts, definition file |
| Agent | One agent | Persistent | Knowledge graph, special knowledge file, cold storage |
| Scratch | One agent | Ephemeral | Script execution temp files |

```
Shared Volume
├── agents.yaml                    (definition file)
├── logs/{request_id}.jsonl        (interaction logs)
└── artifacts/{agent}/             (produced files)

Agent Volume (per agent)
├── special_knowledge.md           (user-managed, read-only to agent)
├── knowledge/graph.json           (persistent knowledge graph)
├── cold_storage/                  (archived nodes)
└── docs/                          (knowledge node documents)

Scratch (ephemeral)
└── /tmp/agent-xxx/                (destroyed after task)
```

`Delete` is only permitted on Scratch. Agent and Shared tiers block programmatic deletion.

### Deployment modes

| Tier | Local | Docker | Kubernetes |
|---|---|---|---|
| Shared | `{data-dir}/shared/` | Named volume | PVC (ReadWriteMany) |
| Agent | `{data-dir}/agents/{name}/` | Volume per agent | PVC per agent |
| Scratch | OS `$TMPDIR` | tmpfs | `emptyDir` |

---

## Sandboxing

All tool and script execution runs through `sandbox.Run`:

**Environment isolation**: Stripped env (`PATH=/usr/bin:/bin`), isolated `HOME` and `TMPDIR`, no API keys or credentials leaked.

**OS-level filesystem restrictions**:
- **macOS**: `sandbox-exec` with deny-default SBPL profile. Read-only access to system paths and detected toolchains. Read-write access to the work directory only. Note: `sandbox-exec` is deprecated by Apple; see [tech-debt.md](tech-debt.md) for migration plans.
- **Linux**: Landlock filesystem restrictions (kernel >= 5.13). Enforced via a self-reexec pattern: the binary re-invokes itself with a sentinel argument, applies Landlock to the child process, then `exec`s the real command under the restricted policy. On older kernels without Landlock support, a warning is logged and execution proceeds unsandboxed.
- Toolchain auto-detection (NVM, Pyenv, Rustup, Go, Bun, etc.) adds development tool paths as read-only.

**Timeout enforcement**: Hard kill via process group signal on timeout.

**Write scoping**: Agents can only write to `artifacts/{own-name}/` on the shared volume, enforced at the Storage interface level.

---

## Verification Gates

Agents can define a `verify` block to run a command after the tool loop ends:

```yaml
verify:
  command: "cd $SCRATCH_DIR && npm run build 2>&1"
  max_retries: 2
  bonus_iterations: 5
  timeout: 120s
```

On failure, errors are injected back into the conversation and the agent gets extra iterations to fix them. After exhausting retries, a `VERIFICATION FAILED` header is prepended to the response for upstream visibility.

---

## Escalation

Each agent has an `escalation_target`. When an agent cannot produce a confident answer, it escalates rather than guessing.

```
Agent A ──► Agent B ──► Conductor ──► User
```

The chain is configurable per agent. The Conductor always escalates to the user.

---

## Communication

- **Local mode**: In-process Go channels via `local.Hub`. Each agent has a buffered channel (cap 64). `Send()` drops a `*message.Message` onto the target agent's channel; `Receive()` returns the agent's own inbound channel. No network I/O, no serialization overhead.
- **Distributed mode**: Each agent runs as a standalone gRPC server (`agentfab agent serve`). Communication uses protobuf over gRPC with mTLS. The conductor generates a CA and per-agent certificates at startup. Agent discovery uses a static peers file written by the conductor after all agents are up. A cluster monitor detects dead members and can trigger conductor re-election.
- **Messages**: Multipart format with `TextPart`, `FilePart` (URI to shared volume), and `DataPart` (structured JSON).
- **Logging**: All messages are logged to `logs/{request_id}.jsonl` on the shared volume.

---

## Cost Control

**Metering**: Every LLM call records input/output tokens, duration, and cost. Aggregated per agent, per task, and per request.

**Budgets**: Per-agent limits on input tokens, output tokens, total tokens, and cost (USD). Checked before every LLM call; on breach, the agent stops and sends a failed result.

**Circuit breakers**: Budget exceeded, consecutive errors, nonsensical output, or timeout. All trigger escalation.

**Metrics**: `agentfab metrics` produces per-agent and per-model cost reports from debug logs.

---

## Integrity

`agentfab init` generates a manifest (`agents/manifest.json`) with SHA-256 checksums of all agent definition files. On startup, `agentfab run` verifies the manifest and reports any modifications, additions, or deletions. `agentfab verify` runs this check standalone.

---

## Further Reading

- [Orchestration Internals](orchestration-internals.md) — code-level request lifecycle, scheduler implementation, FSM routing details
- [Glossary](glossary.md) — terminology reference
- [Tech Debt](tech-debt.md) — known gaps
