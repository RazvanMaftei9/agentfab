# agentfab Architecture

agentfab is a distributed orchestration platform for collaborative agent fabrics. This document describes the runtime architecture: the Conductor, the control plane, the data plane tiers, the node hosts that run agent instances, and the identity and recovery mechanisms that let the fabric run safely across process, machine, and cluster boundaries.

## Overview

A fabric is one agentfab execution domain. Inside a fabric:

- the Conductor is the primary orchestration entry point. It screens user requests, decomposes them into task graphs, and drives them to completion.
- specialist agents execute tasks, reviews, and tool workflows on their assigned model.
- the control plane tracks nodes, agent instances, requests, tasks, and leases. It is durable across restarts.
- storage is tiered into shared, agent, and scratch scopes so cross-agent artifacts, per-profile memory, and per-task scratch space each have their own cleanup and write-safety rules.

```text
User
  |
  v
Conductor  ---> decomposition ---> Scheduler ---> control plane writes
  |
  +--> agent instances
         - local goroutines   (local mode)
         - external node hosts (external-node mode, including Kubernetes deployments)
```

## Core Concepts

### Fabric

A fabric is one agentfab execution domain. It contains:

- one active Conductor leader
- zero or more standby Conductors
- one or more logical agent profiles
- zero or more concrete agent instances running those profiles
- shared storage for artifacts and control-plane state

### Conductor

The Conductor is responsible for:

- user interaction
- request screening and disambiguation
- task graph decomposition
- scheduler lifecycle
- request and task persistence
- control-plane leadership
- recovery and stale-work reconciliation on restart

### Agent Profile Versus Agent Instance

agentfab splits the concept of an agent into two:

- `AgentProfile`: the logical role the scheduler targets, such as `developer` or `architect`. A profile is defined by its YAML configuration.
- `AgentInstance`: one runnable execution unit for that profile on a specific node, registered in the control plane with its own endpoint and capacity.

The scheduler dispatches work against profiles but places each task on a specific instance. Placement is least-loaded first, with per-request anti-affinity, so a fan-out workload is distributed across every eligible instance and no single instance collects the whole request. The control plane tracks instance lifecycle through registration, heartbeat, and recovery, so node loss does not kill the request in flight.

### Node

A node is a compute host that can register capacity and host one or more agent instances. External node hosts are started with `agentfab node serve` and register themselves with the control plane.

## Runtime Modes

agentfab supports:

- local mode
- external-node mode

The exact behavior and commands are documented in [Runtime Modes](runtime-modes.md).

## Agent Definitions

Agents are defined in YAML. A definition describes:

- name
- purpose
- capabilities
- model
- tools
- escalation target
- review behavior
- verification rules
- budgets and timeouts

Representative example:

```yaml
name: "developer"
purpose: "Implement changes and validate them"
capabilities: ["coding", "testing"]
model: "anthropic/claude-sonnet-4-6"
escalation_target: "architect"
tools:
  - name: go-test
    command: "go test ./..."
    mode: live
verify:
  command: "cd $SCRATCH_DIR && go test ./..."
  max_retries: 2
budget:
  max_total_tokens: 500000
  max_cost_usd: 5.00
```

An agent definition describes the native agentfab implementation path. A logical profile is the scheduler-facing identity of an agent role; the implementation below it is what runs the agent's prompt, tools, and loop.

## Request Lifecycle

When the user submits a request:

1. The Conductor evaluates whether the request is actionable.
2. If needed, the Conductor asks clarifying questions.
3. The Conductor decomposes the request into a task DAG.
4. The scheduler executes ready tasks subject to dependency ordering and per-profile concurrency limits.
5. Tasks may run bounded review loops.
6. Request and task state are persisted through the control plane.
7. Results, artifacts, and knowledge updates are written to storage.

Implementation details live in [Orchestration Internals](orchestration-internals.md).

## Task Graphs

The Conductor decomposes requests into DAGs of `TaskNode`s.

Each task carries:

- a task ID
- a logical target profile
- an optional assigned instance
- an optional execution node
- dependency edges
- status and lease metadata

Scheduler properties:

- ready tasks run in parallel when dependencies allow
- per-agent concurrency limits are enforced
- failed tasks cascade failure to dependents
- task metadata represents profile-aware and instance-aware execution

## Orchestration Loops

Review and revise flows are modeled as bounded finite state machines.

Properties:

- explicit states and transitions
- hard `max_transitions` cap
- tool-less approvals are downgraded during review
- terminal states include approved and escalated outcomes

This is one of the core architectural differences between agentfab and frameworks that rely on ad hoc recursive prompting.

## Control Plane

agentfab now has an explicit control-plane layer.

The core records are:

- `Node`
- `AgentInstance`
- `LeaderLease`
- `TaskLease`
- `RequestRecord`
- `TaskRecord`

Current control-plane responsibilities:

- conductor leadership
- node registration and heartbeat
- instance registration and heartbeat
- request persistence
- task persistence
- task lease tracking

Current backends:

- in-memory store
- file-backed durable store
- etcd-backed consensus store

The file-backed store is appropriate for:

- local development
- shared-volume testing of external-node mode

The etcd-backed store is appropriate for:

- multi-node distributed execution
- resilient Conductor leadership
- hybrid-cloud deployments that need consensus-backed metadata and lease coordination

This gives agentfab a real production-grade metadata-plane option now, while leaving room for future backend expansion.

## Recovery Model

On startup, the Conductor:

- registers as a control-plane node
- acquires the leader lease
- reconciles stale requests from prior runs

Recovery semantics:

- requests still marked running are moved to `interrupted`
- active tasks are marked interrupted
- stale task leases are released
- stale node and instance heartbeats become `unavailable`
- tasks and loop executions are redispatched after active replica loss when another healthy instance is available

## Identity And Security

Distributed communication uses a platform-neutral identity layer.

Security model:

- local and distributed traffic use a shared development certificate provider or a mounted external provider
- external nodes require authenticated enrollment using join tokens
- distributed workload traffic uses mTLS
- bootstrap identity and workload identity are separate concepts in the design

See [Identity Architecture](identity-architecture.md) for the full subject model and provider contract, and [Production Identity Deployment](production-identity-deployment.md) for the current external-provider path.

## Communication

The two runtime modes differ in how agents find each other, how identity is issued, and how failures are detected and handled. Each mode is described separately below.

### Local Mode

Everything runs in one process. The communicator is an in-process hub, messages travel over Go channels, and discovery is a local registry populated at startup. There is no network transport, no TLS, and no liveness checking. A crash in any agent crashes the host process.

### External-Node Mode

The conductor, the control-plane service, and one or more node hosts run as separate processes that can live on separate machines. Node hosts are started with `agentfab node serve` and register themselves with the control plane after presenting an enrollment token and measured claims. The control-plane service can either be embedded in the conductor for fast local testing or run as a standalone process via `agentfab control-plane serve`, backed by a file store or an etcd cluster.

All traffic uses gRPC with mTLS. Identity comes from the shared local-development provider for development work or from a mounted external provider for production, such as SPIFFE or SPIRE. `ManagedCertificate` rotation applies in both cases, so long-running node hosts and conductors reissue their leaf certificates before expiry without operator intervention.

Discovery is control-plane-backed. Instance endpoints resolve through the control plane's instance registry, which node hosts update as they bring up the agent instances they host. There is no peers file in this mode.

Liveness is also control-plane-backed. Node hosts and hosted instances heartbeat through the control plane, and the control plane marks records unavailable once heartbeats expire. The scheduler reacts by redispatching affected tasks to another eligible instance, with task leases preventing duplicate execution across the recovery boundary.

Conductor leadership is managed through a durable leader lease in the control plane, and a standby conductor can take over when an incumbent's lease expires.

## Storage

agentfab uses three storage tiers:

| Tier | Scope | Lifetime | Typical contents |
|---|---|---|---|
| Shared | Fabric-wide | Persistent | artifacts, logs, cross-agent handoff, `agents.yaml` |
| Agent | Per profile | Persistent | knowledge graph, special knowledge, cold storage |
| Scratch | Per task | Ephemeral | working files, tool outputs |

Each tier has its own cleanup rules. Scratch can be wiped without consequence. The per-agent tier can be wiped to reset one profile's accumulated memory without touching cross-agent artifacts. The shared tier is the one the fabric treats as ground truth when agents hand work off to each other.

When many parallel instances of the same profile write concurrently, they write to task-scoped paths under the shared tier. Each task writes to a unique path, so concurrent writers do not collide. Cross-profile shared state that needs coordination, such as the merged knowledge graph, is written in one place by the Conductor in a single background pass, not by each agent in parallel.

Two other properties of the storage layer are worth knowing:

- Tier roots are configurable, so deployment clients can back them with mounted cloud volumes (EBS, CSI, `hostPath`) without touching core runtime code.
- The runtime materializes local workspaces from the storage abstraction for tools and sandboxed execution. The backend does not have to be a POSIX filesystem. An object store or a service-backed storage implementation plugs into the same contract as long as it can produce a staged local workspace on demand.

## Knowledge System

Each agent keeps a persistent knowledge graph.

The system supports:

- confidence-scored nodes
- typed edges such as `depends_on` and `supersedes`
- decision injection into future tasks
- cold storage for stale knowledge
- graph curation when the active graph grows too large

The goal is to preserve durable architectural context, not just retrieve old transcripts.

## Verification And Budgeting

Agents can define:

- verification commands
- retry counts
- timeout limits
- token budgets
- cost budgets

The runtime meters LLM usage and can stop work when budgets are exceeded.

## Sandboxing

Tool execution runs inside the sandbox layer.

Current model:

- stripped environment
- write restrictions
- timeout enforcement
- OS-specific filesystem controls

Platform behavior:

- macOS: `sandbox-exec`
- Linux: Landlock where supported

## Further Reading

- [Runtime Modes](runtime-modes.md)
- [Orchestration Internals](orchestration-internals.md)
- [Identity Architecture](identity-architecture.md)
- [Roadmap](roadmap.md)
- [Glossary](glossary.md)
