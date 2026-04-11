# agentfab

A distributed platform where specialized AI agents collaborate through task graphs, persistent knowledge, and bounded review loops. Define agents in YAML with their own model, tools, and knowledge. agentfab decomposes requests into a DAG, schedules agents in parallel, runs review loops with hard caps, persists state through a durable control plane, and sandboxes shell commands at the OS level.

For a full breakdown and demos, see: https://razvanmaftei.me/article?slug=agentfab-stateful-multi-agent-orchestration

![decomposition demo](https://razvanwebsite.blob.core.windows.net/blog-article-images/3/1774343815595-taskgraph.gif)

## Quick Start

```sh
make build
agentfab init
agentfab run
```

Type your project idea at the prompt. agentfab decomposes it into a task graph, dispatches specialist agents, and writes artifacts under the data directory.

## Fabrics

agentfab uses *fabrics* made up of a Conductor plus whatever specialist agents you configure. The Conductor is always present for decomposition and orchestration. Every other agent is defined in YAML, per project.

![fabric taskgraph](https://razvanwebsite.blob.core.windows.net/blog-article-images/3/1774344084389-litedaw-taskgraph.gif)

`agentfab init` creates a default fabric with `conductor`, `architect`, `designer`, and `developer`. Replace them or add your own.

## Custom Agents

Compile Markdown descriptions into YAML:

```sh
mkdir agents
# write agents/security-auditor.md, agents/api-designer.md, etc.
agentfab agent compile --input ./agents --output ./agents
agentfab run
```

Or write YAML directly:

```yaml
name: "security-auditor"
purpose: "Audit code for vulnerabilities and compliance"
capabilities: ["security_analysis", "compliance_check"]
model: "anthropic/claude-sonnet-4-5-20250929"
escalation_target: "architect"
tools:
  - name: semgrep
    command: "semgrep --config auto $ARGS"
    mode: live
verify:
  command: "cd $SCRATCH_DIR && semgrep --config auto --error . 2>&1"
  max_retries: 2
```

## Runtime Modes

### Local mode

```sh
agentfab run
```

All agents run in one process with in-memory transport. This is the fastest dev loop and the default for editing YAML, prompts, and tools.

### External-node mode

```sh
agentfab run --external-nodes
```

The Conductor, the control-plane service, and one or more node hosts run as separate processes. They can live on separate machines. This is the deployment shape agentfab targets for VMs, containers, and Kubernetes clusters.

What you get:

- **mTLS everywhere.** All gRPC traffic uses workload certificates issued by the configured identity provider. Local development uses a built-in provider; production uses a mounted external provider (SPIFFE, corporate CA, or similar).
- **Authenticated node enrollment.** Node hosts present an enrollment token plus measured claims (bundle digest, profile digests, binary digest) at registration. The control-plane API validates attestation; the node does not self-authorize.
- **Durable control plane.** A Conductor leader lease, per-task leases, and request and task records persist through a file backend or etcd. Restarting the Conductor does not lose in-flight state.
- **Control-plane discovery.** Instance endpoints resolve through the control plane's registry. Node hosts update it as they bring up hosted instances.
- **Recovery without duplicate execution.** When a node host dies, stale heartbeats mark its records unavailable and task leases release. The scheduler redispatches affected tasks to another healthy instance, and the task lease prevents two instances from running the same task.
- **Signed, version-matched fabrics.** Bundle and per-profile digests are enforced at admission. A node cannot join with a different agent set than the active fabric, and signed bundles can be required through trusted operator keys.

Fast local path (no extra config required):

```sh
# auto-bootstraps a local control-plane service and one local node host
agentfab run --external-nodes

# spin up more local node hosts under the same command
agentfab run --external-nodes --bootstrap-nodes 3
```

For the manual production-like setup (separate control-plane, Conductor, and node host processes, optional etcd backend, enrollment tokens, signed bundles), see [Runtime Modes](docs/runtime-modes.md).

## CLI

| Command | Description |
|---|---|
| `init` | Create a fabric with default or custom agents |
| `run` | Start the Conductor and enter interactive mode |
| `setup` | Configure API keys and install the binary |
| `agent compile` | Generate YAML definitions from Markdown descriptions |
| `node serve` | Run an external node host |
| `node token create` | Create a node enrollment token |
| `status` | Show agent status and token usage |
| `metrics` | Cost and usage report from debug logs |
| `verify` | Check bundle integrity and sign bundle metadata |

Key `run` flags: `--debug`, `--external-nodes`, `--bootstrap-nodes N`, `--skip-verify`, `--data-dir <dir>`, `--listen <addr>`, `--control-plane-address <addr>`.

## Providers

agentfab uses [Eino](https://github.com/cloudwego/eino) adapters:

| Provider | Env var | Example model |
|---|---|---|
| Anthropic | `ANTHROPIC_API_KEY` | `anthropic/claude-sonnet-4-6` |
| OpenAI | `OPENAI_API_KEY` | `openai/gpt-5.4` |
| Google | `GOOGLE_API_KEY` | `google/gemini-3.1-pro-preview` |
| OpenAI-compatible | `OPENAI_COMPAT_API_KEY` | `openai-compat/local-model` |

Each agent can use a different provider.

## Requirements

Go 1.24+.

| Platform | Sandbox |
|---|---|
| macOS 10.15+ | `sandbox-exec` |
| Linux kernel 5.13+ | Landlock |
| Older Linux kernels | Runs unsandboxed with a warning |

## Docs

- [Architecture](docs/architecture.md)
- [Runtime Modes](docs/runtime-modes.md)
- [Orchestration Internals](docs/orchestration-internals.md)
- [Identity Architecture](docs/identity-architecture.md)
- [Production Identity Deployment](docs/production-identity-deployment.md)
- [Roadmap](docs/roadmap.md)
- [Glossary](docs/glossary.md)

## License

[MIT](LICENSE)
