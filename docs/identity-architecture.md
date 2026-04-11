# Identity Architecture

agentfab's identity layer issues workload credentials for every conductor, node, and agent instance, validates peer identity during mTLS, and authorizes behavior at the control-plane boundary. The layer is platform-neutral: the core runtime does not embed cloud-specific identity flows, so the same agentfab code runs across laptops, VMs, containers, and Kubernetes clusters.

## Identity Model

agentfab treats identities as subjects inside a trust domain.

Subject kinds:

- conductor
- node
- agent profile
- agent instance

Identity URIs follow a SPIFFE-style shape:

```
spiffe://agentfab.example/fabric/demo/conductor/conductor-a
spiffe://agentfab.example/fabric/demo/node/node-a
spiffe://agentfab.example/fabric/demo/node/node-a/agent/developer/instance/node-a-developer-1
```

The URI components carry:

- a trust domain that identifies the security boundary
- a fabric that identifies the agentfab deployment
- a subject kind that identifies the runtime role
- subject segments that identify the concrete workload

## Trust Model

agentfab trusts an identity provider, not a cloud provider. The provider is responsible for:

- issuing workload certificates
- returning the trust bundle
- validating or deriving workload identity from bootstrap evidence

agentfab itself is responsible for:

- requesting identities for conductors, nodes, and agent instances
- presenting certificates for mTLS
- validating peers against the trust bundle
- authorizing behavior based on subject identity

## Bootstrap Versus Workload Identity

These are separate concerns.

**Bootstrap identity** proves what environment a node or workload came from. Examples include a Kubernetes projected service account token, a VM machine identity document, a cloud-managed workload token, or a local development join token. agentfab core does not hard-code the bootstrap flows for each environment; they live in the attestor implementation.

**Workload identity** is the certificate the running agentfab component uses for mTLS and authorization. agentfab standardizes on this layer, which keeps the core stable even when bootstrap evidence differs across Kubernetes, VMs, and containers.

## Identity Interfaces

agentfab core depends on a narrow identity abstraction in `internal/identity`:

- `CertificateProvider`
- `Attestor`

`CertificateProvider` covers issuing a certificate for a subject, renewing a certificate for a long-running subject, and returning the trust bundle.

There are two provider shapes:

- **issuer-backed providers** that mint child identities for the current process
- **subject-bound providers** that expose the current workload identity from an external system

agentfab supports both shapes for multiplexed runtimes. External node hosts represent hosted agent instances under the node identity when the configured provider is subject-bound, so one workload certificate covers every instance the node serves.

`Attestor` covers validating bootstrap evidence, deriving an attested node identity, and validating measured runtime claims.

## Provider Implementations

### Local Development Provider

The built-in `local-dev` provider in `internal/identity/localdev.go`:

- mints leaf certificates from a persisted cluster CA
- rotates leaf certificates automatically through `ManagedCertificate`
- ships a join-token attestor that validates enrollment tokens and measured claims (bundle digest, binary digest)

It serves local mode, external-node development, and shared-volume testing.

### Mounted External Provider

Production deployments use a mounted external provider that reads the workload's certificate, key, and trust bundle from files written by infrastructure. The provider works with:

- SPIFFE helper processes
- CSI-mounted workload identities
- sidecars or agents that write SVIDs and trust bundles to disk
- any external identity plane with file projection

The mounted provider is subject-bound: it supplies the identity for the current workload and does not mint arbitrary child certificates. For multiplexed node hosts, agentfab runs the hosted agent instances under the node's workload identity while the control plane keeps per-instance authorization and scheduling state.

See [Production Identity Deployment](production-identity-deployment.md) for the deployment model.

## Authorization

After mTLS establishes peer identity, agentfab authorizes based on subject type and scope:

- only a subject with conductor identity claims conductor leadership
- only a node identity registers node capacity
- only an agent instance identity or the hosting node identity registers an instance endpoint
- a hosting node identity only advertises instances that belong to that node
- only nodes and instances with bundle and profile digests matching the active fabric are admitted
- only bundles signed by trusted operator keys are treated as authoritative when signed-bundle enforcement is enabled
- only nodes with fresh successful attestation are allowed to register capacity or instances

This prevents a valid certificate from being used to impersonate a different runtime role.

## Security Properties

- certificates are short-lived and rotated automatically
- private keys stay inside their owning workload
- trust bundles are explicit and rotatable
- all distributed traffic uses mTLS
- node and instance registration are authenticated
- fabric config compatibility is enforced before a node or instance can participate
- signed bundle trust roots are explicit and operator-controlled
- peer authorization is role-aware
- leader election and control-plane writes use authenticated identities
- local development identity material is isolated from production behavior
- measured attestation decisions are enforced at the control-plane boundary

## What agentfab Core Does Not Do

agentfab core does not:

- embed separate EKS, AKS, ECS, or VM identity stacks
- make the conductor the root CA
- rely on long-lived shared secrets between nodes
- treat process-local identity as a production trust model

The identity plane lives outside agentfab core or behind the narrow attestation boundary described above.
