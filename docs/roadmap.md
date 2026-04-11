# Roadmap

This is the single place agentfab tracks future work. Every other doc in this tree describes the platform as it exists today; this one describes where it is going. When a roadmap item lands, its bullet moves out of this file and into the relevant architecture or reference doc.

## Elasticity and Scale

- Elastic autoscaling across pools of interchangeable instances of the same profile. The scheduler is already instance-aware and the control plane tracks per-node capacity, but there is no autoscaler that creates or retires instances based on load.
- Automatic mid-request resumption after a conductor takeover. The leader lease model supports a standby conductor taking over when the incumbent's lease expires. What is still open is picking up in-flight work mid-request; recovery currently marks interrupted requests and relies on the operator to retrigger them.
- Cross-cluster execution, where a single fabric spans multiple Kubernetes clusters or cloud regions with consistent control-plane state.

## Storage

- Transactional backend for high-rate concurrent writes to shared profile state. The current file and etcd backends cover local and single-cluster distributed deployments. Multi-cluster fabrics with high write contention on the merged knowledge graph will want a stronger contract behind the same storage interface.

## Identity and Attestation

- First-class production certificate provider beyond the mounted-files path. The mounted provider is the current production-deployment shape; a first-class implementation that talks directly to SPIRE, a corporate workload CA, or another short-lived-certificate issuer is still open.
- Production bootstrap attestor that validates cloud-native bootstrap evidence (Kubernetes projected service account tokens, VM machine identity, cloud-managed workload tokens) outside the local development join-token path.
- External measured attestation validated outside the node binary. The control plane currently enforces measured claims (bundle digest, profile digests, binary digest) that the node presents about itself. Moving verification of those measurements outside the node process is the next step for hostile-runtime resistance.
- Subject-aware authorization across every distributed runtime surface. The control plane currently authorizes subject roles at admission and at node and instance registration. Extending the same subject-aware authorization to task dispatch, artifact writes, and every control-plane mutation API is still open.

## Pluggable Agent Implementations

- Config-level split between `AgentProfile` and `AgentImplementation`. Today an agent definition bundles the logical profile (name, purpose, capabilities, escalation policy, budgets) with the concrete native implementation contract (model, tools, prompt, tool loop). Splitting these into two nested concepts lets a profile point at one or more compatible implementation kinds.
- Native driver extracted as one implementation kind among several. The built-in agentfab loop becomes a plugin rather than the only execution path.
- Additional implementation kinds: externally hosted gRPC agents, containerized workers, customer-provided runtimes, and third-party agentic systems wrapped inside the agentfab harness.
- Explicit feature contracts so the Conductor and scheduler know which behaviors an implementation supports (peer review loops, checkpoint resume, knowledge curation, sandboxed tools) and degrade safely when an implementation does not support a feature.

## Deployment

- First-class in-tree Docker and Kubernetes runtime implementations. Both are supported deployment targets today and the deployment tooling lives outside the core runtime. Moving them in-tree is still open.
