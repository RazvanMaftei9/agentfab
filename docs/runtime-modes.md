# Runtime Modes

agentfab supports two runtime modes. They share the same binary, the same fabric definition, and the same control-plane abstractions. A deployment chooses its mode by which subcommand it starts, not by which artifacts it ships.

- **Local mode** runs everything in one process with in-memory transport and filesystem storage. This is the developer loop.
- **External-node mode** runs the Conductor, the control-plane service, and one or more node hosts as separate processes. Those processes can live on separate machines, which makes this the real distributed path and the substrate agentfab targets for production deployment on VMs, containers, and Kubernetes clusters.

## 1. Local Mode

Command:

```sh
agentfab run
```

Behavior:

- all agents run in one process
- communication uses in-memory channels
- storage is filesystem-backed under the configured data directory
- there is no network transport in the task path

This is the primary developer experience and the fastest iteration loop.

## 2. External-Node Mode

Command:

```sh
agentfab run --external-nodes
```

This is the default local development path for distributed node testing. When the fabric does not already point at an external control-plane API, the CLI automatically bootstraps:

- a local control-plane service
- one local node host
- measured enrollment for that node

To simulate more than one local node:

```sh
agentfab run --external-nodes --bootstrap-nodes 2
```

Manual production-like setup is still available:

```sh
agentfab node token create --config agents.yaml --data-dir ./system-data --node-id node-a

agentfab node serve \
  --config agents.yaml \
  --data-dir ./system-data \
  --control-plane-address 127.0.0.1:50051 \
  --node-id node-a \
  --listen-host 127.0.0.1 \
  --advertise-host 127.0.0.1 \
  --enrollment-token <token>

agentfab control-plane serve \
  --config agents.yaml \
  --data-dir ./system-data \
  --listen :50051

agentfab run \
  --config agents.yaml \
  --data-dir ./system-data \
  --external-nodes \
  --listen 127.0.0.1:50050 \
  --control-plane-address 127.0.0.1:50051
```

Behavior:

- the Conductor runs independently
- the control-plane API can run independently from the Conductor
- one or more node hosts run separately and serve agent instances
- node hosts register themselves and their instances through the control-plane service
- control-plane discovery resolves the active Conductor and live agent instances
- runtime traffic uses mTLS
- node startup requires a valid enrollment token
- node startup verifies the local agent bundle by default
- node startup sends measured claims such as bundle and binary digests to the control plane
- signed bundles are supported through trusted Ed25519 public keys configured in `agents.yaml`
- control-plane admission rejects nodes and instances whose bundle or profile digests do not match the active fabric
- `--bootstrap-nodes N` starts local node hosts automatically when no external control-plane API is configured

Security model:

- node enrollment uses a local development join-token attestor
- node attestation is validated server-side by the control-plane API
- workload certificates come from the shared local development identity provider, or from a mounted external provider in production
- bootstrap and workload identity are separate concepts even in development mode
- node and instance registration are authorized by workload identity at the control-plane API boundary
- config compatibility is enforced across the fabric with bundle and per-profile digests
- measured enrollment tokens can bind a node to expected bundle and binary digests
- signed bundles protect the stock runtime against modified files plus modified local manifests

What this mode is good for:

- local multi-process simulation of distributed execution
- hybrid-cloud workflow testing
- validating node lifecycle, enrollment, shared discovery, and instance routing

## Control Plane

agentfab has an explicit control-plane model with:

- `Node`
- `AgentInstance`
- `LeaderLease`
- `TaskLease`
- `RequestRecord`
- `TaskRecord`

Backends:

- in-memory store for tests and local wiring
- file-backed durable store for local and shared-volume development
- etcd-backed consensus store for distributed deployments

Behavior:

- the Conductor registers itself as a node
- the Conductor acquires a leader lease
- request and task state are persisted through the control plane
- on restart, recovered in-flight requests are marked interrupted and stale task leases are released
- stale node and instance heartbeats are treated as unavailability by the control plane
- tasks assigned to an expired instance are reset and redispatched to another healthy replica when possible
- loop transitions send status updates back to the Conductor so loop recovery tracks the currently active replica, not only the initial one

The etcd backend provides consensus-backed metadata for distributed deployments. The file backend is the default for local and shared-volume development.

## Identity

agentfab has a platform-neutral identity abstraction in `internal/identity`:

- shared local development certificate provider
- workload mTLS for distributed communication
- local development node enrollment authority
- join-token attestation for external node startup
- mounted external provider path for production deployments

See [Identity Architecture](identity-architecture.md) for the subject model and provider contract, and [Production Identity Deployment](production-identity-deployment.md) for the external-provider path.

## Storage

The runtime uses filesystem-backed storage with three tiers:

- shared
- agent
- scratch

agentfab stages local workspaces from the storage abstraction when agents need shell tools or sandboxed commands, so deployment clients are not forced to expose the backing store as a mounted POSIX filesystem. Mounted volumes are the easiest operational path, but object-store or service-backed storage implementations plug into the same contract.

Parallel instances of the same profile avoid write conflicts by writing to task-scoped paths under the shared tier. Each task writes to a unique path, so concurrent writers on different pods do not collide. Cross-profile shared state that needs coordination, such as the merged knowledge graph, is reconciled by the Conductor in one background pass rather than by each agent on its own.

## What To Use When

Use local mode for everyday development. It has the fastest loop, the cheapest iteration, and no network in the task path.

Use external-node mode for realistic distributed development, pre-cluster validation, and production deployment. The same mode covers a one-machine smoke test (`agentfab run --external-nodes` auto-bootstraps a local control-plane service and node host) and a multi-machine production rollout.

See [Architecture](architecture.md) for the complete design, [Identity Architecture](identity-architecture.md) for the workload-identity model, and [Production Identity Deployment](production-identity-deployment.md) for the external-provider path.
