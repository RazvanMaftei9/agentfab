# Orchestration Internals

Implementation-level documentation for the current agentfab orchestration path.

This document focuses on what the code does today:

- startup and runtime wiring
- request lifecycle
- scheduler behavior
- loop routing
- control-plane persistence and recovery

## Key Source Files

| File | Role |
|---|---|
| `internal/conductor/conductor.go` | Conductor startup, runtime wiring, request lifecycle, control-plane leadership, recovery |
| `internal/conductor/setup.go` | Fabric setup, agent spawn path, external-node readiness checks |
| `internal/conductor/scheduler.go` | DAG execution, task dispatch, demux, task persistence, loop dispatch |
| `internal/conductor/decompose.go` | Request decomposition into task graphs |
| `internal/conductor/disambiguate.go` | Request clarity evaluation |
| `internal/conductor/chat.go` | Direct agent chat and amendment routing |
| `internal/taskgraph/types.go` | TaskNode schema, profile and instance metadata |
| `internal/taskgraph/dag.go` | Ready task resolution and dependency handling |
| `internal/controlplane/interfaces.go` | Control-plane contracts |
| `internal/controlplane/types.go` | Nodes, instances, leader leases, task leases, request and task records |
| `internal/controlplane/file.go` | Durable file-backed control-plane backend |
| `internal/controlplane/discovery.go` | Control-plane-backed endpoint discovery |
| `internal/nodehost/host.go` | External node runtime and hosted agent instances |
| `internal/identity/*` | Workload identity, trust bundle, local development enrollment and certificates |
| `internal/grpc/*` | gRPC server, communicator, TLS helpers, process lifecycle |
| `internal/agent/agent.go` | Native agent runtime loop |
| `internal/loop/*` | FSM loop model and routing |
| `internal/knowledge/*` | Knowledge generation, lookup, merge, cold storage, curation |
| `internal/sandbox/*` | Tool sandboxing |
| `cmd/agentfab/main.go` | CLI, REPL, runtime selection |
| `cmd/agentfab/node.go` | External node runtime commands and enrollment token management |

## 1. Startup Paths

### Local Mode

`conductor.New()` defaults to:

- local communicator hub
- local discovery
- local lifecycle
- local meter
- local filesystem-backed storage

`Conductor.Start()` then:

1. registers the Conductor locally
2. initializes conductor LLM helpers
3. starts the control plane
4. runs fabric setup

### External-Node Mode

When `WithExternalAgents()` is enabled:

1. `setupDistributed()` wires gRPC transport, mTLS, and control-plane-backed discovery
2. the Conductor obtains workload identity from the configured certificate provider
3. the Conductor starts its own gRPC server
4. `runtime.NoopLifecycle` replaces process spawning, since agent instances run on external node hosts
5. setup waits for ready external agent instances registered through the control plane

External nodes are started independently through `agentfab node serve`.

## 2. Control Plane During Startup

`startControlPlane()` is part of `Conductor.Start()`.

It currently does all of the following:

1. register the Conductor as a control-plane node
2. acquire the leader lease
3. reconcile stale running requests from prior runs
4. start node heartbeat renewal
5. start leader lease renewal

If leadership cannot be acquired, startup fails.

## 3. Recovery Semantics

`reconcileRecoveredRequests()` handles stale in-flight work after restart.

Current behavior:

- requests still marked `running` become `interrupted`
- active tasks become `interrupted`
- existing task leases are released

## 4. Request Lifecycle

The main request path is `Conductor.HandleRequest()`.

At a high level:

1. create a request-scoped context and cancellation handle
2. screen the request for actionability
3. disambiguate if the request is unclear
4. load conductor knowledge and context
5. decompose the request into a task graph
6. create a scheduler
7. persist request and task records to the control plane
8. execute the graph
9. generate knowledge updates
10. write artifacts and return the final result

Important property:

- cancellation is wired before the scheduler exists, so the Conductor can abort during screening or decomposition as well as during execution

## 5. Task Graph Execution

The scheduler executes one task graph at a time.

Core loop shape:

1. gate on pause and cancellation
2. apply graph replacement if an amendment arrived
3. compute ready tasks from dependency state
4. enforce per-profile concurrency limits
5. mark dispatching tasks as running
6. persist updated task state
7. dispatch tasks in parallel
8. wait for task completions
9. cascade failure to dependents where required

Current task metadata can carry:

- profile
- assigned instance
- execution node
- lease epoch

That means the scheduler model is instance-aware and replica-aware. Placement is least-loaded first with per-request anti-affinity, so a fan-out workload spreads across every eligible instance and no single instance collects every task. Write safety for concurrent instances comes from two places: each task writes to a unique output path under the shared tier, and the Conductor reconciles shared knowledge in one background pass. It does not come from per-profile dispatch serialization.

## 6. Task Dispatch

For non-loop tasks, the scheduler:

1. assembles prompt parts
2. includes task description and user request
3. includes pipeline context and dependency outputs
4. includes relevant knowledge and prior artifacts
5. sends a `task_assignment` message
6. waits for a result through the demux layer

On completion, it:

- updates the task record in the graph
- updates the control-plane task record
- releases local scheduler bookkeeping

While waiting for completion, it also:

- renews the task lease
- watches the assigned node and instance for liveness loss
- resets the task to `pending` and redispatches when the active replica disappears or the lease is lost

## 7. Demultiplexing

The conductor has one inbound message channel, but many tasks can be waiting concurrently.

The demux layer:

- subscribes waiters by task ID
- routes incoming messages by `task_id`
- buffers overflow when messages arrive before a waiter subscribes

This keeps the scheduler simple while still supporting concurrent task execution over a shared inbound transport.

## 8. Orchestration Loops

Loop tasks use the FSM machinery under `internal/loop`.

Current flow:

1. the scheduler loads the loop definition
2. review guidelines are enriched from agent definitions
3. the initial loop context is encoded into the task assignment
4. active loop participants send status updates back to the Conductor as the loop moves between states
5. agents route among themselves until the loop reaches a terminal state
6. the terminal result returns to the Conductor as one task result

Key invariants:

- loops are bounded by `MaxTransitions`
- invalid transition paths are treated as errors
- exhausted transition budgets become escalations rather than silent infinite churn
- the Conductor tracks the currently active loop replica from status updates so loop recovery follows the live hop, not only the initial assignment

## 9. Agent Runtime

The native agent runtime in `internal/agent/agent.go` is still the primary execution implementation.

The agent runtime is responsible for:

- receiving assignments
- building prompt input
- invoking the model
- executing live tools where configured
- handling review flows
- writing artifacts
- updating knowledge and logs

In external-node mode, node hosts create these same native agent runtimes and expose them over gRPC.

## 10. External Node Runtime

`internal/nodehost/host.go` is the external node host.

Startup flow:

1. load or create shared local-dev workload identity
2. attest the node against the control-plane API using the configured attestor and measured claims
3. register the node with the control plane
4. start one gRPC server per hosted profile
5. issue workload identity for each hosted instance
6. register each instance with the control plane
7. heartbeat nodes and instances

The control plane treats stale heartbeats as authoritative unavailability. That means the scheduler can recover from node loss even when a node does not shut down cleanly.

Current attestation path:

- `agentfab node token create` issues an enrollment token
- `agentfab node serve` presents that token plus measured claims such as bundle and binary digests
- the control plane validates attestation before accepting node or instance registration

## 11. Identity And Transport

The current identity model is intentionally abstracted from any specific cloud provider.

Today the implementation includes:

- a shared local development certificate provider
- a local development join-token attestor
- mTLS between distributed participants

The control plane authorizes node and instance registration based on workload identity, attestation outcome, and bundle compatibility.

## 12. Knowledge And Artifacts

After execution:

- task results are written as artifacts
- combined request results are assembled
- knowledge extraction runs in the background
- graph updates merge into agent knowledge stores

Artifacts and knowledge go through the storage abstraction described in the Architecture doc, so the backing store can be a local filesystem, a mounted cloud volume, or a service-backed implementation behind the same contract.

## Related Docs

- [Architecture](architecture.md)
- [Runtime Modes](runtime-modes.md)
- [Identity Architecture](identity-architecture.md)
- [Glossary](glossary.md)
