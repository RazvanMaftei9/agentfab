# AgentFab 0.2.0

## Highlights

- reframed AgentFab as a distributed platform, not just a local multi-agent runtime
- added a standalone control-plane service with durable request, task, membership, and lease state
- added external node hosts for running agent instances outside the Conductor process
- added `etcd` as a consensus-backed control-plane backend
- added replica-aware scheduling, per-request anti-affinity, and task redispatch after node or instance loss
- added mTLS across distributed runtime traffic
- added authenticated node enrollment with join tokens and measured claims
- added signed bundle enforcement and fabric-wide bundle/profile digest checks
- added mounted external identity support for production-shaped deployments
- added certificate renewal and local-dev CA rollover for long-running distributed runtimes

## New Commands

- `agentfab control-plane serve`
- `agentfab node serve`
- `agentfab node token create`
- `agentfab run --external-nodes`

## Runtime Changes

- `external-node` mode is now the main distributed runtime path
- the Conductor, control plane, and node hosts can run as separate workloads
- control-plane discovery now resolves live conductors and agent instances through the control plane instead of local peer files
- task placement now targets concrete instances, not just logical profiles
- loop execution now preserves placement metadata across hops
- control-plane state can be backed by memory, files, or `etcd`

## Security and Identity

- distributed traffic now uses workload mTLS
- node admission now validates enrollment server-side
- nodes and instances must match the active fabric’s bundle and profile digests
- signed bundles are supported through trusted public keys
- production deployments can use mounted workload identity and trust bundles projected by external infrastructure

## Developer Experience

- `agentfab run --external-nodes` can auto-bootstrap a local control plane and local node hosts for fast distributed testing
- added local Kubernetes test assets under [`deploy/kind/`](/Users/razvanmaftei/Documents/Projects/agentfab/deploy/kind)
- improved distributed UI feedback with runtime mode, control-plane visibility, node-aware task labels, and cleaner knowledge panels

## Notable Changes From 0.1.0

- the old standalone `agent serve` path has been removed from the main distributed story
- distributed operation now centers on the control plane plus external node hosts
- agentfab now has an explicit separation between control plane and data plane

## Current Limits

- local and `kind`-based distributed testing work with the built-in `local-dev` identity provider
- production deployments use the mounted identity provider, which expects a cert, key, and trust bundle to be projected by external infrastructure
- direct in-tree integrations with SPIFFE, SPIRE, or a corporate CA are still future work
- no autoscaler yet, so the scheduler tracks capacity but does not create or retire instances based on load
- no automatic mid-request continuation after Conductor takeover, so interrupted requests still need to be retriggered by the operator
- the current file and `etcd` backends cover local and single-cluster distributed deployments, but high-contention shared profile state still needs a stronger transactional backend
