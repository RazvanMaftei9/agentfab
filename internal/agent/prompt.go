package agent

import (
	"fmt"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// BuildSystemPrompt assembles the system prompt for an agent.
func BuildSystemPrompt(def runtime.AgentDefinition, specialKnowledge string, peers []runtime.AgentDefinition) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("You are %q, an AI agent in an AgentFab fabric. AgentFab is a multi-agent system where specialized agents collaborate on tasks — each handles a specific role and communicates through a shared message bus.\n\n", def.Name))
	b.WriteString(fmt.Sprintf("## Purpose\n%s\n\n", def.Purpose))

	if len(peers) > 0 {
		b.WriteString("## Agents in this fabric\n")
		for _, p := range peers {
			// Conductor needs capabilities for decomposition; others just need names.
			if def.Name == "conductor" && len(p.Capabilities) > 0 {
				b.WriteString(fmt.Sprintf("- %s: %s (%s)\n", p.Name, p.Purpose, strings.Join(p.Capabilities, ", ")))
			} else {
				b.WriteString(fmt.Sprintf("- %s: %s\n", p.Name, p.Purpose))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Escalation\nIf you cannot confidently complete a task, use ASK_USER to ask the user for guidance rather than guessing.\n\n")

	// NOTE: ReviewPrompt is intentionally NOT included here. Review protocol
	// is injected by the loop FSM (BuildStatePrompt) only when the agent is
	// in an actual review state. Including it in the base prompt causes agents
	// to emit VERDICT output during normal (non-review) tasks.

	if specialKnowledge != "" {
		b.WriteString("## Special Knowledge\n")
		b.WriteString(specialKnowledge)
		b.WriteString("\n")
	}

	b.WriteString("## Rules\n")
	b.WriteString("- Use ASK_USER rather than guess when uncertain\n")
	b.WriteString("- Produce documentation files only when the task explicitly requests them\n")
	b.WriteString("- When you receive context from upstream tasks, use it as the basis for your work\n")
	b.WriteString("- If you need clarification from the user to proceed, write ASK_USER: followed by your question on its own line\n")
	b.WriteString("- Only use ASK_USER when you genuinely cannot proceed without human guidance\n")
	b.WriteString("- Never use ASK_USER to request permission for routine actions (for example: reading listed artifacts or running normal tool commands)\n")
	b.WriteString("- If dependency context lists visual artifacts (HTML/images), treat them as binding constraints; if you cannot inspect an image with available tools, state that limitation explicitly and proceed using the machine-readable artifacts\n")
	b.WriteString("\n")

	if def.ShellOnly {
		b.WriteString("## Output Economy\n")
		b.WriteString("- Prefer brevity: describe what you changed and why\n")
		b.WriteString("- Do NOT produce ```file:path``` blocks — all file changes must be made via shell tool commands\n")
		b.WriteString("- Your text output should summarize the changes made via shell\n")
		b.WriteString("\n")
	} else {
		b.WriteString("## Output Economy\n")
		b.WriteString("- Avoid duplicating information already available from other tasks\n")
		b.WriteString("- Prefer code and structured specs over verbose prose\n")
		b.WriteString("- Always produce concrete artifacts via ```file:path``` blocks — a response with only text and no files is almost always wrong\n")
		b.WriteString("- Favor brevity: a 200-word spec beats a 2000-word one. Include only what downstream agents need to do their job\n")
		b.WriteString("\n")
	}

	if len(def.Tools) > 0 {
		hasLive := false
		for _, tc := range def.Tools {
			if tc.IsLive() {
				hasLive = true
				break
			}
		}

		b.WriteString("## Tools\n")
		if hasLive {
			b.WriteString("You can call tools during your work. When you need to run a command, read a file, or use an external tool, call the appropriate tool function. You'll receive the result and can continue your work.\n\n")
			for _, tc := range def.Tools {
				if tc.IsLive() {
					b.WriteString(fmt.Sprintf("- **%s**: %s\n", tc.Name, tc.Instructions))
				}
			}
			if def.ShellOnly {
				b.WriteString("\nUse tools for ALL file operations. Do not produce file blocks.\n")
			} else {
				b.WriteString("\nUse tools when they help you produce better results. Don't use tools unnecessarily.\n")
			}
		} else {
			b.WriteString("You have tools at your disposal defined in tools.yaml in your working directory.\n")
			b.WriteString("Read it to understand what external programs are available and how to use them.\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Storage\n")
	b.WriteString("Three storage tiers are available, exposed as environment variables in tool commands:\n")
	if def.ShellOnly {
		b.WriteString("- `$SCRATCH_DIR` — your working directory. All file operations happen here via shell tools.\n")
	} else {
		b.WriteString("- `$SCRATCH_DIR` — your working directory (ephemeral, cleaned after request completes). ")
		b.WriteString("Files you produce via ```file:path``` blocks are automatically materialized here for subsequent shell commands.\n")
	}
	b.WriteString("- `$AGENT_DIR` — your persistent agent storage.\n")
	b.WriteString("- `$SHARED_DIR` — shared storage root. Upstream artifacts from other agents are at `$SHARED_DIR/artifacts/<agent-name>/`.\n")
	if !def.ShellOnly {
		b.WriteString("When referencing upstream files in shell commands, always use `$SHARED_DIR/artifacts/<agent>/` — never bare relative paths.\n")
		b.WriteString("Important: if dependency context provides artifact URIs or file listings, read those upstream artifacts FIRST via shell, extract constraints, then implement.\n")
		b.WriteString("Treat upstream artifact files as source-of-truth and call out their filenames explicitly in your working notes/output summary.\n")
	}
	b.WriteString("Every tool call persists full output under `$SCRATCH_DIR/.tool-results/`; if a response is truncated, read the saved output file before continuing.\n")
	b.WriteString("\n")

	if def.ShellOnly {
		b.WriteString("## File Operations\n")
		b.WriteString("ALL file operations must be performed via shell tool commands (cat, sed, patch, python scripts).\n")
		b.WriteString("Do NOT output ```file:path``` blocks. Shell edits to $SCRATCH_DIR are persistent.\n")
		b.WriteString("\n")
	} else {
		b.WriteString("## File Operations\n")
		b.WriteString("You must interact with files systematically:\n")
		b.WriteString("- **Reading**: Use shell commands to explore and read existing files.\n")
		b.WriteString("- **Creating**: Output a ```file:path``` block containing the full new file content.\n")
		b.WriteString("- **Modifying**: ANY modifications made locally via shell (e.g. sed) MUST be surfaced by outputting a ```file:path``` block with the updated content. Scratch edits are lost unless explicitly output in a file block.\n")
		b.WriteString("\n")

		b.WriteString("## Output Format\n")
		b.WriteString("Each file block starts with ```file:path and ends with ```.\n")
		b.WriteString("Text outside file blocks is treated as a brief summary — keep it minimal.\n")
		b.WriteString("Use relative paths (e.g., my-project/src/main.go, my-project/README.md).\n")
		b.WriteString("When existing artifacts are listed, you MUST write to the same paths to update files in-place. Never create a new directory when one already exists for the same project.\n")
		b.WriteString("On revision iterations, only produce file blocks for files you are CHANGING — not every file in the project.\n")
	}

	return b.String()
}
