# AgentFab

A distributed platform where specialized AI agents collaborate through task graphs, persistent knowledge, and bounded review loops. Define agents in YAML, each with its own LLM model, tools, and knowledge base. The framework handles decomposition, scheduling, sandboxed execution, and inter-agent communication.

## Platforms

| Platform | Sandbox | Notes |
|----------|---------|-------|
| macOS 10.15+ | `sandbox-exec` (SBPL) | Full filesystem isolation |
| Linux (kernel 5.13+) | Landlock LSM | Full filesystem isolation |
| Linux (older kernels) | None | Warning logged, runs unsandboxed |

Requires Go 1.24+.

## Quick start

```sh
# Build
make build

# Initialize a fabric with default agents (conductor, architect, designer, developer)
agentfab init

# Set API keys for your providers
export ANTHROPIC_API_KEY="..."
export OPENAI_API_KEY="..."

# Run
agentfab run
```

Each agent uses a `provider/model-id` format (e.g., `anthropic/claude-sonnet-4-5-20250929`, `openai/gpt-5.2`). Set the API key for whichever providers your agents use.

## CLI

| Command | Description |
|---------|-------------|
| `init` | Create a fabric with default or custom agents |
| `run` | Interactive mode |
| `agent compile` | Generate YAML definitions from Markdown agent descriptions |
| `agent serve` | Run an agent as a standalone gRPC server (distributed mode) |
| `status` | Show agents and their models |
| `metrics` | Per-agent, per-model cost report (requires `--debug` on previous run) |
| `verify` | Check agent definition integrity against manifest |
| `bench` | Run benchmark scenarios |

Key flags for `run`: `--debug` (log LLM calls), `--distributed` (gRPC mode), `--skip-verify`, `--data <dir>`.

## Custom agents

Write a Markdown file per agent describing its role, then compile:

```sh
mkdir agents
# Create agents/security-auditor.md, agents/api-designer.md, etc.

agentfab agent compile --input ./agents --output ./agents
agentfab run
```

The compiler generates YAML definitions with capabilities, tools, and escalation targets. A conductor is auto-added if absent. Use `--dry-run` to preview without writing files.

Alternatively, write YAML definitions directly:

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
  bonus_iterations: 5
```

## Providers

Four LLM provider adapters via [Eino](https://github.com/cloudwego/eino):

| Provider | Env var | Example model |
|----------|---------|---------------|
| Anthropic | `ANTHROPIC_API_KEY` | `anthropic/claude-opus-4-5-20251101` |
| OpenAI | `OPENAI_API_KEY` | `openai/gpt-5.2` |
| Google | `GOOGLE_API_KEY` | `google/gemini-3.1-pro-preview` |
| OpenAI-compatible | `OPENAI_COMPAT_API_KEY` | `openai-compat/local-model` |

Each agent can use a different provider and model.

## Documentation

- [Architecture](docs/architecture.md) -- system design, agent definitions, task graphs, knowledge system, sandboxing
- [Orchestration Internals](docs/orchestration-internals.md) -- code-level request lifecycle, scheduler, FSM routing
- [Glossary](docs/glossary.md) -- terminology reference
- [Tech Debt](docs/tech-debt.md) -- known gaps and planned work
