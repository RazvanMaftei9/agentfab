# agentfab Glossary

**Fabric**: One agentfab execution domain. Contains a Conductor, logical agent profiles, zero or more concrete agent instances, shared storage, and control-plane state.

**Conductor**: The primary orchestration entry point. Handles screening, disambiguation, decomposition, scheduling, persistence, and leadership. Users can also chat directly with individual agents through the Conductor UI surface.

**Agent**: A specialist worker defined by agentfab configuration. In current native mode, an agent has a model, prompt, tools, verification rules, and persistent knowledge.

**Agent Profile**: The logical scheduling target for work such as `developer` or `architect`. A profile describes what kind of work should be done, independent of which concrete instance runs it.

**Agent Instance**: One runnable execution unit for a profile on a specific node. Represented in the control plane as `AgentInstance`.

**Node**: A compute host that can register capacity and run one or more agent instances. Today this is typically an external node host process started with `agentfab node serve`.

**Agent Definition**: YAML configuration for a native agentfab agent profile. Includes fields like `name`, `purpose`, `capabilities`, `model`, `tools`, `escalation_target`, `verify`, budgets, and review settings.

**Default Agents**: The built-in starter fabric created by `agentfab init`: `conductor`, `architect`, `designer`, and `developer`.

**Task Graph**: DAG of work produced by the Conductor. Nodes are tasks, edges are dependencies, and task metadata can carry profile, instance, node, and lease information.

**Task Node**: One unit of work in the graph. Targets a logical profile and can optionally bind to a specific instance for execution.

**Orchestration Loop**: A bounded finite state machine used for iterative flows such as implement-review-revise. Prevents infinite review cycles by construction.

**LoopContext**: Metadata carried through loop messages. Includes the loop definition, current state, original task context, and prior summaries. Excluded from the LLM prompt surface where appropriate.

**Control Plane**: The coordination layer that tracks nodes, agent instances, leader leases, task leases, request records, and task records.

**Leader Lease**: The durable record granting one Conductor instance active leadership for a fabric.

**Task Lease**: A durable ownership record for an in-flight task. Used to fence stale owners and support safe recovery semantics.

**Request Record**: Control-plane state for a user request, including whether it is pending, running, completed, failed, cancelled, or interrupted.

**Interrupted Request**: A request that was running when the prior leader or runtime stopped and has been reconciled into a safe non-running state on recovery.

**Discovery**: Runtime abstraction for resolving a logical name to an endpoint. Today agentfab supports local discovery, static gRPC discovery, and control-plane-backed discovery.

**Lifecycle**: Runtime abstraction that starts, waits for, and tears down agents or agent-hosting runtimes.

**Communicator**: Runtime abstraction for sending and receiving agent messages.

**Storage**: Runtime abstraction over the shared, agent, and scratch tiers, with configurable tier roots and staged local workspaces so deployment clients can use mounted volumes or non-POSIX backing stores.

**Workspace Materialization**: The runtime step that stages a local filesystem view of the storage tiers onto each agent host so tools and sandboxed commands can read and write through normal file I/O. The agent's shell tools can then run the same way regardless of whether the underlying storage backend is a local disk, a mounted volume, or an object store.

**Shared Tier**: Fabric-wide persistent storage for artifacts, logs, control-plane state, and shared configuration.

**Agent Tier**: Persistent storage owned by an agent profile or execution owner, typically used for knowledge and special knowledge files.

**Scratch Tier**: Ephemeral task-local storage used for intermediate files and tool execution.

**Knowledge Graph**: Per-agent persistent graph of structured knowledge with confidence, provenance, TTL, and typed edges.

**Cold Storage**: Archive for stale or infrequently used knowledge that should not stay in the active graph.

**Curation**: Consolidation pass over the active knowledge graph to merge or retire redundant knowledge safely.

**Decision Injection**: Selective injection of decision-tagged knowledge into a new task when it is relevant to that task or request.

**Supersession**: A relationship where one knowledge node replaces another, typically modeled with a `supersedes` edge.

**Special Knowledge File**: Markdown content loaded into an agent's context on every invocation.

**Verification Gate**: Optional verification command that runs after the main tool loop and can force retries or surface explicit failure.

**Escalation Chain**: Configured path for uncertainty or failure. Agents escalate to their configured target, and the Conductor escalates to the user.

**Budget**: Limits on token usage and cost. Checked before or during LLM execution to stop runaway work.

**Circuit Breaker**: Runtime stop condition such as budget exhaustion, repeated failures, or timeout.

**Sandbox**: The restricted environment used for tool execution, including stripped environment variables, write scoping, and OS-level filesystem restrictions.

**Local Mode**: Default execution mode where all agents run in one process and communicate over in-memory channels.

**External-Node Mode**: Distributed mode where external node hosts register themselves and serve agent instances for the Conductor over the control plane and gRPC.

**Enrollment Token**: Bootstrap credential used by an external node to attest itself before joining the fabric. Tokens can also bind expected bundle and binary measurements.

**Bundle Digest**: Content-addressed hash of the active fabric's agent definition files. A node host submits its bundle digest during enrollment. The control plane rejects registration if the digest does not match the active fabric, so a host cannot join with a divergent agent set.

**Profile Digest**: Content-addressed hash of one agent profile's definition, including the YAML and any special-knowledge files it references. The control plane uses the profile digest alongside the bundle digest during admission, so an instance whose per-profile config has drifted is rejected.

**Measured Claims**: The set of content-addressed measurements a node presents during attestation, typically the bundle digest, the profile digests, and a hash of the agentfab binary. Combined with the enrollment token, these bind a node's join to a specific fabric state, not just to any valid token holder.

**Identity Provider**: Component that issues workload identity material such as certificates and trust bundles. agentfab currently includes a shared local development provider.

**Attestor**: Component that validates bootstrap evidence and converts it into an attested node identity. agentfab currently includes a local development join-token attestor, and the control plane enforces its attestation decisions at registration time.

**Workload Identity**: The identity an agentfab runtime participant uses for mTLS and authorization after bootstrap succeeds.

**Bootstrap Identity**: The identity evidence presented during initial enrollment, before workload certificates are used for normal runtime communication.

**Pluggable Agent Implementation**: The architectural direction where a logical profile can be backed by different execution implementations, not only the built-in native agentfab loop.

**Agent Manifest**: Integrity manifest for generated agent definition files. Used to detect unexpected changes before running a fabric.

**Agent Compilation**: The process of generating structured agent definitions from Markdown descriptions via `agentfab agent compile`.

**Disambiguation**: The Conductor's request-clarity step before decomposition. If the request is ambiguous, the user is asked to clarify before work starts.

**Decomposition Template**: A template used to guide task-graph generation for common request shapes.

**Eino**: The Go library agentfab uses for LLM provider integrations.
