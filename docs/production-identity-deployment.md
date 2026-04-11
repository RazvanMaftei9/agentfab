# Production Identity Deployment

## Purpose

This document describes the production identity path for agentfab across AWS, Azure, GCP, and on-prem environments.

The goal is simple:

- keep agentfab cloud-neutral
- provision identity in infrastructure, not in application code
- let the runtime consume short-lived workload credentials through a stable contract

## Operating Model

Production agentfab should treat identity as a platform service.

That means:

- Terraform, CloudFormation, Helm, or another infrastructure layer provisions the identity system
- the platform supplies workload certificates and trust bundles to agentfab
- agentfab uses those credentials for mTLS, authorization, and admission

agentfab should not contain separate AWS, Azure, or GCP identity logic in its core runtime.

## Recommended Shape

The recommended production shape is SPIFFE or SPIRE aligned:

- a platform identity plane issues short-lived X.509 workload identities
- workload subjects map to agentfab control-plane, conductor, node, and agent roles
- trust bundles rotate outside agentfab and are consumed by the runtime

The cloud-specific pieces stay at the bootstrap layer:

- Kubernetes service account or workload identity
- VM or instance identity
- platform-specific node attestation
- workload selectors and registration policy

From agentfab's perspective, those differences disappear behind the external identity system.

## Current Runtime Support

agentfab now supports two certificate-provider shapes:

### Issuer-backed provider

This provider can mint arbitrary child identities for the current process.

Today this is the built-in `local-dev` provider.

It works for:

- local mode
- node hosts that multiplex several agent instances inside one process

### Mounted external provider

This provider reads the current certificate, key, and trust bundle from mounted files.

It works well when the infrastructure already rotates those files, for example through:

- SPIFFE helper processes
- CSI-mounted workload identities
- sidecars or agents that write SVIDs and trust bundles to disk
- another external identity plane with file projection

It is cloud-neutral and fits Terraform-managed deployments well.

## Ambient Node-Host Support

The mounted provider is subject-bound.

It supplies the identity for the current workload and does not mint arbitrary child certificates.

agentfab now supports that model for multiplexed node hosts.

The node host keeps one workload identity for itself and uses control-plane authorization plus agent-message metadata to represent the hosted agent instances running on that node.

That means:

- the control-plane service can run with a mounted workload identity
- the conductor can run with a mounted workload identity
- a node host can run with a mounted workload identity while still hosting several agent instances
- hosted agent instances keep their own profile and instance IDs in the scheduler and control plane, even though transport authentication is anchored in the node identity

## Deployment Options

### Option 1: Dedicated workload per agentfab role

Run:

- one control-plane workload
- one conductor workload
- one workload per agent runtime

Each workload receives its own mounted production identity.

This matches ambient workload identity cleanly.

### Option 2: Multiplexed node host with issuer-backed identity

Keep the current node host model, but use an issuer-backed external provider that can mint identities for hosted agent instances.

This remains a valid option and is close to the local-dev provider model.

### Option 3: Multiplexed node host with ambient identity

Keep multiplexed node hosts, but run hosted agent instances under the node's workload identity while the control plane authorizes which instance IDs that node may represent.

This is the best fit for cloud-neutral SPIFFE or SPIRE style deployment.

## Recommended Near-Term Production Path

For a first production rollout that stays cloud-neutral:

1. Provision an external identity plane in infrastructure.
2. Mount workload certificates and trust bundles into the control-plane service, conductor, and node hosts.
3. Use the mounted identity provider for those workloads.
4. Let node hosts represent their hosted agent instances through node-backed transport identity plus control-plane authorization.

This gives agentfab a real production identity path that stays aligned with ambient workload identity systems.

## Configuration

agentfab accepts a fabric-level identity block:

```yaml
identity:
  provider: mounted
  trust_domain: spiffe.example.internal
  mounted:
    cert_file: /var/run/agentfab/tls/cert.pem
    key_file: /var/run/agentfab/tls/key.pem
    bundle_file: /var/run/agentfab/tls/bundle.pem
```

Use `provider: local-dev` for development and tests.

Use `provider: mounted` when the environment projects the current workload's certificate and trust bundle into the runtime filesystem.

## What Infrastructure Owns

The infrastructure layer should own:

- trust domain setup
- workload registration and selectors
- root and intermediate CA rollover
- workload cert renewal
- trust bundle distribution
- attestation policy
- network policy around the identity plane

agentfab should only own:

- loading the configured provider
- using short-lived workload certs for mTLS
- renewing leaf identities through the provider contract
- reacting correctly to rotated trust bundles
- authorizing behavior based on authenticated workload identity
