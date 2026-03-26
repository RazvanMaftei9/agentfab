# agentfab

A platform for running collaborative AI agent clusters. Define specialized agents in YAML, each with its own model, tools, and persistent knowledge. AgentFab decomposes work into task DAGs, schedules agents in parallel, runs bounded review loops, and sandboxes every shell command at the OS level.

Today it runs locally as a single, lightweight binary with zero serialization overhead via in-process Go channels. Pass `--distributed` and it runs in microservice mode where each agent spins up its own independent gRPC server with TLS.

**Try it out:**

```sh
make build
agentfab init
agentfab run
```

Once the interactive prompt loads, enter your project idea to watch the Conductor decompose the task into a bounded FSM:

![AgentFab decomposition demo](https://razvanwebsite.blob.core.windows.net/blog-article-images/3/1774343815595-taskgraph.gif)

## How agentfab stands out

**Curated Knowledge Graphs (Not just Vector RAG).** Other frameworks dump chat logs into a vector database. `agentfab` agents extract a directed graph of their decisions. Nodes carry confidence scores, TTLs, and typed edges (e.g., `supersedes`, `depends_on`). An autonomous LLM-driven curator merges redundant nodes and decays stale ones to cold storage, meaning the agent truly learns your architecture rather than just retrieving old text.

**Structurally Bounded FSMs (Not just loop counters).** Most frameworks rely on a global `recursion_limit` to stop infinite loops. `agentfab` models review cycles as strict Finite State Machines. Reviewers *must* execute verification tools before approving; zero-tool approvals are structurally downgraded to "revise." If the FSM doesn't converge within its transition cap, it escalates to you.

**OS-Level Sandboxing.** Giving an LLM full filesystem access or relying on Docker is a security risk. Every tool call in `agentfab` runs inside `sandbox-exec` (macOS) or Landlock (Linux) with a deny-default policy. It provides syscall-level enforcement on every command, ensuring agents cannot touch your host system outside their workspace.

**Pure Config.** `agentfab` agents are entirely YAML/MD. The Conductor dynamically handles the orchestration. You can add agents, swap models, and change review rules without writing a single line of orchestration code.

**Built for the Cluster.** Most multi-agent frameworks run in a single process. In distributed mode, `agentfab` agents communicate over gRPC with mTLS and Conductor-generated certificates, with the Conductor handling peer discovery. It runs locally via in-process channels today, but the architecture is designed to scale across distributed infrastructure.

[Full article with architecture walkthrough and demos](https://razvanmaftei.me/article?slug=agentfab-stateful-multi-agent-orchestration)

## Default Agents

Besides the Conductor, agentfab provides three default agents out of the box: `Architect`, `Designer`, and `Developer`.

Agents use `provider/model-id` format (e.g. `anthropic/claude-sonnet-4-5-20250929`, `openai/gpt-5.2`). Set the key for whichever providers your agents reference.

## Custom agents

agentfab uses *fabrics* made up of various agents. The Conductor is always present for decomposition and orchestration, but you are free to add your custom agents to your fabric and on a per-project basis.

![litedaw-taskgraph](https://razvanwebsite.blob.core.windows.net/blog-article-images/3/1774344084389-litedaw-taskgraph.gif)

```sh
mkdir agents
# write agents/security-auditor.md, agents/api-designer.md, etc.

agentfab agent compile --input ./agents --output ./agents
agentfab run
```

The compiler generates capabilities, tools, and escalation targets. A conductor is auto-added if missing. Use `--dry-run` to preview.

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

## CLI

| Command | Description |
|---------|-------------|
| `init` | Create a fabric with default or custom agents |
| `run` | Interactive mode (`--distributed` for cluster mode) |
| `agent compile` | Generate YAML definitions from Markdown descriptions |
| `agent serve` | Run an agent as a standalone gRPC service |
| `status` | Show agents and their models |
| `metrics` | Per-agent, per-model cost report (requires `--debug`) |
| `verify` | Check agent definition integrity against manifest |
| `bench` | Run benchmark scenarios |

Key flags for `run`: `--debug`, `--distributed`, `--skip-verify`, `--data <dir>`.

## Providers

Four adapters via [Eino](https://github.com/cloudwego/eino):

| Provider | Env var | Example model |
|----------|---------|---------------|
| Anthropic | `ANTHROPIC_API_KEY` | `anthropic/claude-opus-4-5-20251101` |
| OpenAI | `OPENAI_API_KEY` | `openai/gpt-5.2` |
| Google | `GOOGLE_API_KEY` | `google/gemini-3.1-pro-preview` |
| OpenAI-compatible | `OPENAI_COMPAT_API_KEY` | `openai-compat/local-model` |

## Requirements

Go 1.24+

| Platform | Sandbox |
|----------|---------|
| macOS 10.15+ | `sandbox-exec` (SBPL) |
| Linux (kernel 5.13+) | Landlock LSM |
| Linux (older kernels) | Runs unsandboxed with warning |

## Docs

- [Architecture](docs/architecture.md) - system design, task graphs, knowledge system, sandboxing
- [Internals](docs/orchestration-internals.md) - request lifecycle, scheduler, FSM routing
- [Glossary](docs/glossary.md)
- [Full article with architecture walkthrough and demos](https://razvanmaftei.me/article?slug=agentfab-stateful-multi-agent-orchestration)

## License

[MIT](LICENSE)
