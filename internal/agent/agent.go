package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/loop"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/sandbox"
)

// Agent is a single AI agent in a fabric.
type Agent struct {
	Def                runtime.AgentDefinition
	Comm               message.MessageCommunicator
	Storage            runtime.Storage
	Meter              runtime.Meter
	Logger             *message.Logger
	Generate           func(ctx context.Context, input []*schema.Message) (*schema.Message, error)
	SystemPrompt       string
	Events             event.Bus
	ToolExecutor       *ToolExecutor // nil if no live tools
	PromptCacheEnabled bool          // When true, skip trimOldToolResults to preserve cache prefix.

	// OnProgress is called with progress text (streaming snippets, tool names).
	// In local mode this is wired to the event bus; in distributed mode it sends
	// a StatusUpdate message to the conductor so progress reaches the UI.
	OnProgress func(text string)
}

// Run is the main agent loop. It listens for messages and processes them.
func (a *Agent) Run(ctx context.Context) error {
	slog.Debug("agent started", "agent", a.Def.Name)
	ch := a.Comm.Receive(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("agent shutting down", "agent", a.Def.Name)
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if err := a.handleMessage(ctx, msg); err != nil {
				slog.Error("agent handle message failed", "agent", a.Def.Name, "error", err)
			}
		}
	}
}

// contextBudget returns the token budget available for input context,
// accounting for the output token reservation. Returns 0 if unconfigured
// or if MaxOutputTokens exceeds ContextLimit.
func (a *Agent) contextBudget() int {
	if a.ToolExecutor == nil || a.ToolExecutor.ContextLimit <= 0 {
		return 0
	}
	b := a.ToolExecutor.ContextLimit - a.ToolExecutor.MaxOutputTokens
	if b < 0 {
		return 0
	}
	return b
}

// maxIterationsForScope returns the tool loop iteration cap for a given scope.
// Large tasks need enough iterations for: read specs (2-3), implement (4-6),
// verify build (2-3), and handle review feedback (3-5).
func maxIterationsForScope(scope string) int {
	switch scope {
	case "small":
		return 3
	case "large":
		return 20
	default:
		return 10
	}
}

// scratchListing walks the scratch directory and returns a formatted tree of
// existing files, filtering noise directories (node_modules, .git, etc.).
// Returns an empty string if the directory is empty or doesn't exist.
func scratchListing(scratchDir string) string {
	const maxDepth = 5
	const maxEntries = 50

	var paths []string
	_ = filepath.WalkDir(scratchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		rel, relErr := filepath.Rel(scratchDir, path)
		if relErr != nil || rel == "." {
			return nil
		}

		if strings.Count(rel, string(filepath.Separator)) >= maxDepth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Noise filtering (same patterns as scheduler's isArtifactNoise).
		lower := strings.ToLower(rel)
		noiseSegments := []string{
			"node_modules/", "_requests/", ".git/", "dist/", "build/",
			".next/", "__pycache__/", "vendor/", ".cache/", "coverage/",
			".vite/", ".tool-results/",
		}
		for _, seg := range noiseSegments {
			if strings.Contains(lower+"/", seg) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		noiseFiles := []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", ".ds_store"}
		base := strings.ToLower(d.Name())
		for _, nf := range noiseFiles {
			if base == nf {
				return nil
			}
		}

		display := filepath.ToSlash(rel)
		if d.IsDir() {
			display += "/"
		}
		paths = append(paths, display)

		if len(paths) >= maxEntries {
			return fs.SkipAll
		}
		return nil
	})

	sort.Strings(paths)

	var b strings.Builder
	prevParts := []string{}
	for _, p := range paths {
		isDir := strings.HasSuffix(p, "/")
		clean := strings.TrimSuffix(p, "/")
		parts := strings.Split(clean, "/")

		common := 0
		for common < len(prevParts) && common < len(parts) && prevParts[common] == parts[common] {
			common++
		}
		for i := common; i < len(parts)-1; i++ {
			b.WriteString(strings.Repeat("  ", i))
			b.WriteString(parts[i] + "/\n")
		}
		b.WriteString(strings.Repeat("  ", len(parts)-1))
		if isDir {
			b.WriteString(parts[len(parts)-1] + "/\n")
		} else {
			b.WriteString(parts[len(parts)-1] + "\n")
		}
		prevParts = parts
	}
	return b.String()
}

// generateWithTools wraps Generate with a tool-calling loop. If the LLM returns
// tool calls, they are executed and the results fed back until the LLM produces
// a final response with no tool calls, or the iteration limit is reached.
//
// Returns the final response, the total number of tool calls executed, whether
// verification failed (post-loop verify command returned non-zero), and any error.
//
// File blocks (```file:path```) produced in intermediate responses (alongside
// tool calls) are accumulated and prepended to the final response so that
// buildResultParts sees ALL files the agent produced across the entire loop.
func (a *Agent) generateWithTools(ctx context.Context, input []*schema.Message, maxIter int) (*schema.Message, int, bool, error) {
	if a.ToolExecutor == nil {
		a.emitModelProgress("Thinking...")
		resp, err := a.Generate(ctx, input)
		return resp, 0, false, err
	}

	// Kill any background processes (servers, etc.) when the tool loop ends.
	defer a.ToolExecutor.Cleanup()

	if maxIter <= 0 {
		maxIter = 7
	}

	// Inject scratch listing so the model sees existing files and avoids path confusion.
	if listing := scratchListing(a.ToolExecutor.TierPaths[0]); listing != "" {
		var hint string
		if a.Def.ShellOnly {
			hint = "## Working Directory ($SCRATCH_DIR)\n" +
				"These files already exist in your working directory.\n\n" +
				"```\n" + listing + "```"
		} else {
			hint = "## Working Directory ($SCRATCH_DIR)\n" +
				"These files already exist in your working directory. " +
				"Use paths relative to this root when producing ```file:path``` blocks.\n\n" +
				"```\n" + listing + "```"
		}
		input = append(input, schema.UserMessage(hint))
		slog.Debug("injected scratch listing", "agent", a.Def.Name, "files", strings.Count(listing, "\n"))
	}

	var accumulatedBlocks []FileBlock // file blocks from intermediate iterations
	verifyStreak := 0                 // consecutive verification-only iterations
	toolCallCount := 0                // total tool calls executed across all iterations
	verifyAttempt := 0                // verification retry counter
	shellRedirCount := 0           // ShellOnly file-block redirect counter (max 2)

	for i := 0; i < maxIter; i++ {
		// Budget check between iterations.
		if i > 0 && a.Meter != nil {
			if err := a.Meter.CheckBudget(ctx, a.Def.Name); err != nil {
				return nil, toolCallCount, false, fmt.Errorf("budget exceeded during tool loop: %w", err)
			}
		}

		// Trim old tool results that the LLM has already seen.
		// Keeps the most recent exchange intact; truncates older ones.
		// Skipped when prompt caching is enabled (e.g., Anthropic) because
		// mutating the message prefix breaks prefix-based cache hits.
		if i > 0 && !a.PromptCacheEnabled {
			trimOldToolResults(input)
		}

		// Proactive compaction: after 4 iterations, aggressively compact old
		// exchanges even if we're not near the context limit. This prevents
		// history from growing unbounded and reduces input tokens per call.
		if i >= 4 && a.contextBudget() > 0 {
			budget := a.contextBudget()
			target := budget * 3 / 4 // keep 75% headroom
			if estimateTokens(input) > target {
				input = compactToolHistory(input, target)
			}
		}

		// Context budget check: compact old tool exchanges if approaching limit.
		if budget := a.contextBudget(); budget > 0 {
			if estimateTokens(input) > budget {
				input = compactToolHistory(input, budget)
			}
		}

		a.emitModelProgress("Thinking...")
		resp, err := a.Generate(ctx, input)
		if err != nil {
			// Retry once on context overflow: compact and re-generate.
			if a.contextBudget() > 0 && isContextOverflowError(err) {
				budget := a.contextBudget()
				compacted := compactToolHistory(input, budget*3/4) // aggressive: 75% budget
				if len(compacted) < len(input) {
					slog.Info("retrying after context overflow compaction",
						"agent", a.Def.Name, "before", len(input), "after", len(compacted))
					a.emitModelProgress("Thinking...")
					resp, err = a.Generate(ctx, compacted)
					if err == nil {
						input = compacted
					}
				}
			}
			if err != nil {
				return nil, toolCallCount, false, err
			}
		}

		// Warn if the response was truncated due to output token limit.
		if isResponseTruncated(resp) {
			slog.Warn("model response truncated (max_tokens reached)",
				"agent", a.Def.Name, "iteration", i,
				"content_len", len(resp.Content))
		}

		if len(resp.ToolCalls) == 0 {
			// ShellOnly redirect: if the agent produced file blocks or no tool calls,
			// nudge it to use the shell tool. This handles models that default to
			// producing ```file:path``` blocks despite system prompt instructions.
			// Fires when: (a) no tool calls at all, or (b) model explored via shell
			// but then wrote the fix as file blocks instead of shell edits.
			// Limited to 2 redirects to avoid infinite loops.
			if a.Def.ShellOnly && shellRedirCount < 2 {
				hasFileBlocks := false
				var blockPaths string
				if fbs, _ := ParseFileBlocks(resp.Content); len(fbs) > 0 {
					hasFileBlocks = true
					blockPaths = fileBlockPaths(fbs)
				}
				if hasFileBlocks || toolCallCount == 0 {
					shellRedirCount++
					nudge := "[SHELL-ONLY MODE] You MUST use the shell tool to make all file changes. " +
						"Do NOT produce ```file:path``` blocks — they will be IGNORED. " +
						"Use shell commands like python3 -c or sed -i '' to edit files directly in $SCRATCH_DIR/repo."
					if hasFileBlocks {
						nudge = "[SHELL-ONLY MODE] You produced file blocks (" + blockPaths + ") but those are IGNORED. " +
							"You MUST use the shell tool to edit files directly in $SCRATCH_DIR/repo. " +
							"Use python3 -c or sed -i '' to apply the same changes via shell commands. " +
							"Do NOT produce ```file:path``` blocks."
						slog.Info("shell-only redirect: agent produced file blocks instead of shell commands",
							"agent", a.Def.Name, "iteration", i, "blocks", blockPaths)
					} else {
						slog.Info("shell-only redirect: agent produced no tool calls, nudging to use shell",
							"agent", a.Def.Name, "iteration", i)
					}
					input = append(input, resp)
					input = append(input, schema.UserMessage(nudge))
					// Grant bonus iterations so the redirect doesn't eat
					// into the agent's working budget.
					maxIter = i + 1 + 5
					continue
				}
			}

			// Automatic verification gate: if configured and retries remain,
			// run the verify command. On failure, inject errors and grant bonus iterations.
			if a.Def.Verify != nil && verifyAttempt < verifyMaxRetries(a.Def.Verify) {
				passed, output := a.runVerify(ctx)
				if !passed {
					verifyAttempt++
					input = append(input, resp)
					input = append(input, schema.UserMessage(fmt.Sprintf(
						"[AUTOMATIC VERIFICATION FAILED — attempt %d/%d]\n"+
							"Your output has errors. Fix them before finalizing.\n\n"+
							"Verification command: %s\nOutput:\n%s\n\n"+
							"Fix the errors above. Do not explore docs or add features — "+
							"focus exclusively on fixing these errors.",
						verifyAttempt, verifyMaxRetries(a.Def.Verify),
						a.Def.Verify.Command, output)))
					maxIter = i + 1 + verifyBonusIterations(a.Def.Verify)
					continue // re-enter tool loop
				}
			}

			if len(accumulatedBlocks) > 0 {
				captureFromScratch(a.ToolExecutor.TierPaths[0], accumulatedBlocks)
				// Strip redundant file blocks from the final response. The
				// accumulated blocks (refreshed from scratch) are authoritative
				// and already include any modifications from subsequent tool calls.
				// Keeping the model's re-dump wastes thousands of tokens downstream.
				resp = stripRedundantBlocks(resp, accumulatedBlocks)
			}
			return mergeAccumulatedBlocks(resp, accumulatedBlocks), toolCallCount, false, nil
		}

		// Extract file blocks from this intermediate response before they're
		// lost to conversation history. Later blocks for the same path override
		// earlier ones (the agent may revise a file after seeing tool output).
		// In ShellOnly mode, skip file block extraction entirely — file blocks
		// are not part of the expected workflow.
		newBlocksThisIteration := false
		if !a.Def.ShellOnly {
			if blocks, summary := ParseFileBlocks(resp.Content); len(blocks) > 0 {
				newBlocksThisIteration = true
				slog.Debug("collected file blocks from tool iteration",
					"agent", a.Def.Name, "iteration", i, "blocks", len(blocks))
				accumulatedBlocks = append(accumulatedBlocks, blocks...)
				// Materialize file blocks into scratch so subsequent tool calls
				// (builds, tests, git) can see the code the agent just produced.
				materializeBlocksToScratch(a.ToolExecutor.TierPaths[0], blocks)
				// Replace file block content with a compact summary in conversation
				// history to avoid re-sending thousands of tokens on every subsequent
				// tool call. The full content is preserved in accumulatedBlocks and
				// merged back into the final response.
				resp = shallowCopyMessage(resp)
				resp.Content = summary + "\n\n[Produced " + fmt.Sprintf("%d", len(blocks)) + " file(s): " + fileBlockPaths(blocks) + " — materialized to scratch. Only produce file blocks for files you need to CHANGE.]"
			}
		}

		input = append(input, resp)

		for _, tc := range resp.ToolCalls {
			a.emitToolProgress(tc)
			result, execErr := a.ToolExecutor.Execute(ctx, tc)
			if execErr != nil {
				result = fmt.Sprintf("Error: %v", execErr)
			}
			toolCallCount++
			// Keep per-iteration tool payload bounded; long logs explode token usage.
			result = truncateToolResult(result, maxRecentToolResult)
			input = append(input, schema.ToolMessage(result, tc.ID))
		}

		// Track verification stagnation: the developer running builds without
		// making code changes. Iterations that produce new file blocks are
		// progress (the developer is fixing errors), NOT stagnation.
		if len(accumulatedBlocks) > 0 && isVerificationOnly(resp.ToolCalls) && !newBlocksThisIteration {
			verifyStreak++
		} else {
			verifyStreak = 0
		}
		if verifyStreak >= 3 {
			slog.Info("verification stagnation detected, forcing finalization",
				"agent", a.Def.Name, "iteration", i)
			break // fall through to post-loop finalization
		}

		// Nudge the LLM to batch commands if it's making single-command calls
		// after the first iteration. Costs ~20 tokens but can prevent the
		// 6-call verification pattern that wastes thousands of input tokens.
		if i > 0 && len(resp.ToolCalls) == 1 {
			lastIdx := len(input) - 1
			input[lastIdx] = &schema.Message{
				Role:       schema.Tool,
				Content:    input[lastIdx].Content + "\n\n[Tip: batch multiple commands in one shell call to save time, e.g., cmd1 && cmd2 && cmd3]",
				ToolCallID: input[lastIdx].ToolCallID,
			}
		}
	}

	// Max iterations or stagnation — force finalization.
	slog.Warn("forcing tool loop finalization", "agent", a.Def.Name, "iterations", maxIter)

	// Post-loop verification: run once more to tag the output if still failing.
	// No retries here — the agent already exhausted its budget.
	var verifyFailureTag string
	if a.Def.Verify != nil {
		passed, output := a.runVerify(ctx)
		if !passed {
			slog.Error("verification failed at finalization", "agent", a.Def.Name)
			verifyFailureTag = fmt.Sprintf(
				"## VERIFICATION FAILED\n\nVerification command: %s\nOutput:\n%s\n\n---\n\n",
				a.Def.Verify.Command, output)
		}
	}

	verifyFailed := verifyFailureTag != ""

	// If we already have concrete file artifacts from earlier iterations,
	// finalize deterministically from scratch-captured content instead of
	// asking the model to regenerate from memory.
	if len(accumulatedBlocks) > 0 {
		captureFromScratch(a.ToolExecutor.TierPaths[0], accumulatedBlocks)
		result := synthesizeFinalFromAccumulated(accumulatedBlocks)
		if verifyFailureTag != "" {
			result.Content = verifyFailureTag + result.Content
		}
		return result, toolCallCount, verifyFailed, nil
	}

	if a.Def.ShellOnly {
		// ShellOnly mode: don't ask for file blocks — the work is in the repo.
		// Instead, give the agent one last chance to apply its fix via shell.
		input = append(input, schema.UserMessage(
			"You have reached the iteration limit. You have ONE final chance to apply your fix. "+
				"Use the shell tool NOW to make the edit (python3 -c or sed -i ''). "+
				"If you already made the edit, run `git diff` to confirm. "+
				"Do NOT produce ```file:path``` blocks — they are ignored."))
	} else {
		input = append(input, schema.UserMessage(
			"You have reached the iteration limit. Submit your best work now — do not call tools. "+
				"Return a concise summary of what you implemented and what remains incomplete, "+
				"plus ```file:path``` blocks for all files you created or modified. "+
				"Partial working output is always better than no output."))
	}

	a.emitModelProgress("Thinking...")
	resp, err := a.Generate(ctx, input)
	if err != nil {
		return nil, toolCallCount, false, err
	}

	// If LLM still returns tool calls, handle gracefully.
	if len(resp.ToolCalls) > 0 {
		if a.Def.ShellOnly {
			// ShellOnly finalization: execute the tool calls — this is the
			// agent's last chance to apply its fix to the repo.
			slog.Info("executing finalization tool calls in shell-only mode",
				"agent", a.Def.Name, "calls", len(resp.ToolCalls))
			input = append(input, resp)
			for _, tc := range resp.ToolCalls {
				result, execErr := a.ToolExecutor.Execute(ctx, tc)
				toolCallCount++
				_ = execErr // best effort
				input = append(input, &schema.Message{
					Role:       schema.Tool,
					Content:    result,
					ToolCallID: tc.ID,
				})
			}
			// Get final summary.
			a.emitModelProgress("Thinking...")
			finalResp, finalErr := a.Generate(ctx, input)
			if finalErr == nil {
				resp = finalResp
			}
		} else {
			slog.Warn("stripping tool calls from forced finalization", "agent", a.Def.Name)
			if len(accumulatedBlocks) > 0 {
				resp = &schema.Message{
					Role:    schema.Assistant,
					Content: resp.Content,
				}
			} else {
				return nil, toolCallCount, false, fmt.Errorf("tool loop did not converge after %d iterations", maxIter)
			}
		}
	}

	if len(accumulatedBlocks) > 0 {
		captureFromScratch(a.ToolExecutor.TierPaths[0], accumulatedBlocks)
	}
	result := mergeAccumulatedBlocks(resp, accumulatedBlocks)
	if verifyFailureTag != "" {
		result = shallowCopyMessage(result)
		result.Content = verifyFailureTag + result.Content
	}
	return result, toolCallCount, verifyFailed, nil
}

func (a *Agent) emitModelProgress(label string) {
	a.emitProgress(label)
}

func (a *Agent) emitToolProgress(tc schema.ToolCall) {
	preview := tc.Function.Name
	if tc.Function.Name == "shell" {
		// Extract the command string from JSON args for a readable preview.
		var args struct{ Command string }
		if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil && args.Command != "" {
			preview = args.Command
			if len(preview) > 72 {
				preview = preview[:72] + "..."
			}
		}
	}
	a.emitProgress("$ " + preview)
}

// emitProgress sends progress text via the OnProgress callback (distributed mode)
// or the Events bus (local mode). At least one must be set for progress to appear.
func (a *Agent) emitProgress(text string) {
	if a.OnProgress != nil {
		a.OnProgress(text)
		return
	}
	if a.Events != nil {
		a.Events.Emit(event.Event{
			Type:         event.TaskProgress,
			TaskAgent:    a.Def.Name,
			ProgressText: text,
		})
	}
}

func verifyTimeout(vc *runtime.VerifyConfig) time.Duration {
	if vc.Timeout > 0 {
		return vc.Timeout
	}
	return 120 * time.Second
}

// runVerify executes the verification command defined in a.Def.Verify and
// returns whether it passed plus the (truncated) output. Returns (true, "")
// when no verify config is set.
func (a *Agent) runVerify(ctx context.Context) (passed bool, output string) {
	if a.Def.Verify == nil || a.Def.Verify.Command == "" || a.ToolExecutor == nil {
		return true, ""
	}

	cfg := a.ToolExecutor.SandboxConfig(verifyTimeout(a.Def.Verify))
	env := a.ToolExecutor.SandboxEnv()

	// Auto-detect project root within scratch directory. Agents often create
	// a subdirectory (e.g., "todo-app/") so $SCRATCH_DIR itself may not
	// contain the build manifest. Export PROJECT_DIR for the verify command.
	scratchDir := a.ToolExecutor.TierPaths[0]
	projectDir := detectProjectRoot(scratchDir)
	env = append(env, "PROJECT_DIR="+projectDir)

	slog.Info("running verification", "agent", a.Def.Name, "command", a.Def.Verify.Command, "project_dir", projectDir)
	a.emitModelProgress("Verifying...")

	raw, _, err := sandbox.Run(ctx, cfg, env, "sh", "-c", a.Def.Verify.Command)
	out := string(raw)
	if err != nil {
		if out == "" {
			out = "Error: " + err.Error()
		}
		out = truncateToolResult(out, 4000)
		slog.Warn("verification failed", "agent", a.Def.Name, "error", err)
		return false, out
	}
	slog.Info("verification passed", "agent", a.Def.Name)
	return true, ""
}

// detectProjectRoot finds the directory containing the project's build manifest
// (package.json, go.mod, etc.) within scratchDir. If the manifest is at the
// root, returns scratchDir. If it's in a single subdirectory, returns that
// subdirectory. Falls back to scratchDir if nothing is found.
func detectProjectRoot(scratchDir string) string {
	manifests := []string{
		"package.json", "go.mod", "Cargo.toml", "pyproject.toml",
		"requirements.txt", "pom.xml", "build.gradle", "Makefile",
	}

	for _, m := range manifests {
		if _, err := os.Stat(filepath.Join(scratchDir, m)); err == nil {
			return scratchDir
		}
	}

	entries, err := os.ReadDir(scratchDir)
	if err != nil {
		return scratchDir
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		sub := filepath.Join(scratchDir, entry.Name())
		for _, m := range manifests {
			if _, err := os.Stat(filepath.Join(sub, m)); err == nil {
				return sub
			}
		}
	}

	return scratchDir
}

func verifyMaxRetries(vc *runtime.VerifyConfig) int {
	if vc.MaxRetries > 0 {
		return vc.MaxRetries
	}
	return 2
}

func verifyBonusIterations(vc *runtime.VerifyConfig) int {
	if vc.BonusIterations > 0 {
		return vc.BonusIterations
	}
	return 5
}

// safeScratchPath resolves a relative path within scratchDir and returns
// an error if the result escapes the directory (e.g., via "..").
func safeScratchPath(scratchDir, relPath string) (string, error) {
	full := filepath.Join(scratchDir, relPath)
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	base, err := filepath.Abs(scratchDir)
	if err != nil {
		return "", fmt.Errorf("resolve base: %w", err)
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes scratch directory", relPath)
	}
	return abs, nil
}

// materializeBlocksToScratch writes file blocks to the scratch directory so
// subsequent shell tool calls can see the code (for builds, tests, git, etc.).
func materializeBlocksToScratch(scratchDir string, blocks []FileBlock) {
	for _, b := range blocks {
		dest, err := safeScratchPath(scratchDir, b.Path)
		if err != nil {
			slog.Warn("scratch materialize path rejected", "path", b.Path, "error", err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			slog.Warn("scratch materialize mkdir failed", "path", dest, "error", err)
			continue
		}
		if err := os.WriteFile(dest, []byte(b.Content), 0644); err != nil {
			slog.Warn("scratch materialize write failed", "path", dest, "error", err)
		}
	}
}

// populateScratchWithArtifacts copies the agent's existing artifacts from shared
// storage into the scratch directory so the agent can build, test, and modify
// its own prior work. This is critical for bug-fix and iteration tasks where the
// project already exists in shared storage but scratch starts empty.
func (a *Agent) populateScratchWithArtifacts(ctx context.Context) {
	if a.Storage == nil || a.ToolExecutor == nil || len(a.ToolExecutor.TierPaths) < 3 {
		return
	}
	scratchDir := a.ToolExecutor.TierPaths[0]

	prefix := "artifacts/" + a.Def.Name + "/"

	// List all artifacts for this agent across multiple depths.
	var allFiles []string
	for depth := 1; depth <= 5; depth++ {
		pattern := prefix + strings.Repeat("*/", depth-1) + "*"
		files, err := a.Storage.List(ctx, runtime.TierShared, pattern)
		if err != nil {
			break
		}
		allFiles = append(allFiles, files...)
	}

	if len(allFiles) == 0 {
		return
	}

	seen := make(map[string]bool, len(allFiles))
	copied := 0
	for _, f := range allFiles {
		if seen[f] {
			continue
		}
		seen[f] = true

		// Skip trace/request directories, noise files, and internal markers.
		if !strings.HasPrefix(f, prefix) {
			continue
		}
		relPath := f[len(prefix):]
		if strings.Contains(relPath, "_requests/") {
			continue
		}
		if isArtifactNoise(relPath) {
			continue
		}

		dest, err := safeScratchPath(scratchDir, relPath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(dest); err == nil {
			continue
		}

		data, err := a.Storage.Read(ctx, runtime.TierShared, f)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			continue
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			continue
		}
		copied++
	}

	if copied > 0 {
		slog.Info("populated scratch with existing artifacts",
			"agent", a.Def.Name, "files", copied)
	}
}

// isArtifactNoise returns true for paths that should not be copied to scratch
// (build outputs, dependency directories, lock files, etc.).
func isArtifactNoise(path string) bool {
	p := strings.ToLower(path)

	noiseSegments := []string{
		"node_modules/", ".git/", "dist/", "build/", ".next/",
		"__pycache__/", "vendor/", ".cache/", "coverage/", ".vite/",
	}
	for _, seg := range noiseSegments {
		if strings.Contains(p+"/", seg) {
			return true
		}
	}

	noiseFiles := []string{
		"package-lock.json", "yarn.lock", "pnpm-lock.yaml", ".ds_store",
	}
	base := strings.ToLower(filepath.Base(p))
	for _, nf := range noiseFiles {
		if base == nf {
			return true
		}
	}

	// Tool executor command logs (.cmd_*.log).
	if strings.HasPrefix(base, ".cmd_") && strings.HasSuffix(base, ".log") {
		return true
	}

	return false
}

// persistScratchToShared copies files from the scratch directory to shared
// storage that weren't already persisted via file blocks. This catches files
// created by shell commands (npm init, npx tsc --init, framework CLIs, etc.)
// that would otherwise be lost when scratch is destroyed.
func (a *Agent) persistScratchToShared(ctx context.Context, rp *resultParts, existingBlocks []FileBlock) int {
	if a.ToolExecutor == nil || len(a.ToolExecutor.TierPaths) == 0 {
		return 0
	}
	scratchDir := a.ToolExecutor.TierPaths[0]
	if rp.dir == "" {
		return 0
	}

	persisted := make(map[string]bool, len(existingBlocks))
	for _, b := range existingBlocks {
		persisted[filepath.Clean(b.Path)] = true
	}

	const maxFileSize = 1 << 20 // 1MB per file — skip large binaries
	extra := 0

	skipDirs := map[string]bool{
		"node_modules": true, ".git": true, "dist": true, "build": true,
		".next": true, "__pycache__": true, "vendor": true, ".cache": true,
		"coverage": true, ".vite": true, "_requests": true, ".tool-results": true,
	}

	_ = filepath.WalkDir(scratchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(scratchDir, path)
		if relErr != nil || rel == "." {
			return nil
		}

		if d.IsDir() {
			if skipDirs[strings.ToLower(d.Name())] {
				return fs.SkipDir
			}
			return nil
		}

		if persisted[filepath.Clean(rel)] {
			return nil
		}
		if isArtifactNoise(rel) {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil || info.Size() > maxFileSize || info.Size() == 0 {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		storagePath := rp.dir + "/" + filepath.ToSlash(rel)
		if err := a.Storage.Write(ctx, runtime.TierShared, storagePath, data); err != nil {
			slog.Warn("failed to persist scratch file", "path", rel, "error", err)
		} else {
			rp.files = append(rp.files, rel)
			extra++
		}
		return nil
	})

	if extra > 0 {
		slog.Info("persisted scratch files to shared storage",
			"agent", a.Def.Name, "files", extra)
	}
	return extra
}

// ensureScratchFromShared copies work artifact files from shared storage into
// scratch if they are missing. This handles the case where WORKING produced
// file blocks in the final response (written to shared but never in scratch),
// ensuring REVISING finds the files on disk.
func (a *Agent) ensureScratchFromShared(ctx context.Context, lc *loop.LoopContext) {
	if len(lc.WorkArtifactFiles) == 0 || lc.WorkArtifactURI == "" {
		return
	}
	scratchDir := a.ToolExecutor.TierPaths[0]
	for _, relPath := range lc.WorkArtifactFiles {
		dest, err := safeScratchPath(scratchDir, relPath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(dest); err == nil {
			continue // already there
		}
		data, err := a.Storage.Read(ctx, runtime.TierShared, lc.WorkArtifactURI+relPath)
		if err != nil {
			continue
		}
		os.MkdirAll(filepath.Dir(dest), 0755)
		os.WriteFile(dest, data, 0644)
	}
}

// captureFromScratch re-reads accumulated file block paths from the scratch
// directory, picking up any in-place edits the agent made via shell commands
// (e.g., sed -i). If a file was deleted or is unreadable, the original block
// content is kept.
func captureFromScratch(scratchDir string, blocks []FileBlock) {
	for i, b := range blocks {
		path, err := safeScratchPath(scratchDir, b.Path)
		if err != nil {
			slog.Warn("scratch capture path rejected", "path", b.Path, "error", err)
			continue // keep original block content
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // file deleted or missing; keep original block content
		}
		blocks[i].Content = string(data)
	}
}

// maxTrimmedToolResult is the max characters kept in old tool results.
// 80 chars is enough to show the key outcome (e.g., "PASS\nok pkg 0.5s" or
// "Error: build failed: undefined reference to 'foo'").
const maxTrimmedToolResult = 80

// maxRecentToolResult is the max characters kept in the most recent batch of
// tool results (the results the LLM hasn't processed yet). Must be large enough
// to hold typical file reads (HTML mockups ~4KB, source files ~10KB) so agents
// can faithfully read upstream artifacts. Extreme outputs (huge build logs) are
// already bounded by maxToolOutputBytes at the sandbox level.
const maxRecentToolResult = 12_000

// trimOldToolResults truncates tool result messages that the LLM has already
// processed (i.e., all tool messages except the most recent batch).
// This prevents tool outputs from accumulating unbounded in the conversation.
func trimOldToolResults(msgs []*schema.Message) {
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.Assistant {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 {
		return
	}

	for i := 0; i < lastAssistant; i++ {
		if msgs[i].Role == schema.Tool && len(msgs[i].Content) > maxTrimmedToolResult {
			trimmed := msgs[i].Content[:maxTrimmedToolResult] + "\n[... truncated]"
			msgs[i] = &schema.Message{
				Role:       schema.Tool,
				Content:    trimmed,
				ToolCallID: msgs[i].ToolCallID,
			}
		}
	}
}

func truncateToolResult(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// Keep both ends: errors typically appear at the tail of shell output.
	marker := "\n[... truncated middle ...]\n"
	headSize := max / 3
	tailSize := max - headSize - len(marker)
	if tailSize < 0 {
		tailSize = 0
	}
	return s[:headSize] + marker + s[len(s)-tailSize:]
}

// shallowCopyMessage creates a copy of a Message so we can modify Content
// without mutating the original (which may be referenced by tool call IDs).
func shallowCopyMessage(m *schema.Message) *schema.Message {
	cp := *m
	return &cp
}

func fileBlockPaths(blocks []FileBlock) string {
	const maxListed = 8
	paths := make([]string, len(blocks))
	for i, b := range blocks {
		paths[i] = b.Path
	}
	if len(paths) <= maxListed {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:maxListed], ", ") + fmt.Sprintf(", +%d more", len(paths)-maxListed)
}

// stripRedundantBlocks removes file blocks from the final response that are
// identical to what's already in accumulated blocks (i.e., unchanged re-dumps).
// Blocks with genuinely updated content are kept. This prevents the model from
// re-dumping files it produced in earlier iterations without losing real updates.
func stripRedundantBlocks(resp *schema.Message, accumulated []FileBlock) *schema.Message {
	finalBlocks, summary := ParseFileBlocks(resp.Content)
	if len(finalBlocks) == 0 {
		return resp
	}

	// Build map of accumulated content by path (latest version).
	accumContent := make(map[string]string, len(accumulated))
	for _, b := range accumulated {
		accumContent[b.Path] = b.Content
	}

	// Keep blocks that are new (not in accumulated) or genuinely updated.
	var kept []FileBlock
	for _, b := range finalBlocks {
		prev, exists := accumContent[b.Path]
		if !exists || strings.TrimSpace(b.Content) != strings.TrimSpace(prev) {
			kept = append(kept, b)
		}
	}

	stripped := len(finalBlocks) - len(kept)
	if stripped == 0 {
		return resp
	}

	slog.Info("stripped redundant file blocks from final response",
		"agent_blocks", len(finalBlocks), "stripped", stripped, "kept", len(kept))

	// Rebuild content: summary text + any genuinely new/updated file blocks.
	var b strings.Builder
	if summary != "" {
		b.WriteString(strings.TrimSpace(summary))
		b.WriteString("\n\n")
	}
	for _, fb := range kept {
		b.WriteString("```file:")
		b.WriteString(fb.Path)
		b.WriteByte('\n')
		b.WriteString(fb.Content)
		b.WriteString("\n```\n\n")
	}

	out := shallowCopyMessage(resp)
	out.Content = strings.TrimSpace(b.String())
	return out
}

// mergeAccumulatedBlocks prepends file blocks collected from intermediate tool
// loop iterations into the final response content. Blocks from the final response
// take precedence over accumulated ones for the same path.
func mergeAccumulatedBlocks(resp *schema.Message, accumulated []FileBlock) *schema.Message {
	if len(accumulated) == 0 {
		return resp
	}

	finalBlocks, _ := ParseFileBlocks(resp.Content)
	finalPaths := make(map[string]bool, len(finalBlocks))
	for _, b := range finalBlocks {
		finalPaths[b.Path] = true
	}

	var prefix strings.Builder
	merged := 0
	for _, b := range accumulated {
		if finalPaths[b.Path] {
			continue // final response has a newer version
		}
		finalPaths[b.Path] = true // deduplicate within accumulated
		prefix.WriteString("```file:")
		prefix.WriteString(b.Path)
		prefix.WriteByte('\n')
		prefix.WriteString(b.Content)
		prefix.WriteString("\n```\n\n")
		merged++
	}

	if merged == 0 {
		return resp
	}

	slog.Info("merged accumulated file blocks into final response",
		"merged", merged, "final_blocks", len(finalBlocks))

	resp.Content = prefix.String() + resp.Content
	return resp
}

// synthesizeFinalFromAccumulated builds a deterministic final response from
// accumulated file blocks. This avoids an extra LLM call that may hallucinate
// or rewrite large projects after tool-loop convergence failures.
func synthesizeFinalFromAccumulated(accumulated []FileBlock) *schema.Message {
	files := uniqueFilePaths(accumulated)
	summary := "Summary: Finalized from materialized scratch files after tool-loop convergence.\n"
	summary += "Files: " + strings.Join(files, ", ") + "\n"
	summary += "Verify: Previously executed in tool loop; no additional finalization commands run.\n"
	summary += "Commit: no-op"

	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n")
	for _, f := range files {
		content := latestBlockContent(accumulated, f)
		b.WriteString("```file:")
		b.WriteString(f)
		b.WriteByte('\n')
		b.WriteString(content)
		b.WriteString("\n```\n\n")
	}

	return &schema.Message{
		Role:    schema.Assistant,
		Content: strings.TrimSpace(b.String()),
	}
}

func uniqueFilePaths(blocks []FileBlock) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range blocks {
		if b.Path == "" || seen[b.Path] {
			continue
		}
		seen[b.Path] = true
		out = append(out, b.Path)
	}
	return out
}

func latestBlockContent(blocks []FileBlock, path string) string {
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Path == path {
			return blocks[i].Content
		}
	}
	return ""
}

// estimateTokens returns a rough token count for a slice of messages.
// Uses len(content)/4 as approximation (conservative for code/English text).
func estimateTokens(msgs []*schema.Message) int {
	total := 0
	for _, msg := range msgs {
		total += len(msg.Content)/4 + 4 // +4 for role/framing overhead
		for _, tc := range msg.ToolCalls {
			total += len(tc.Function.Arguments) / 4
			total += len(tc.ID)
		}
	}
	return total
}

// compactToolHistory replaces older tool exchanges with a compact summary to
// fit within the token budget. System (index 0) and user (index 1) messages
// are always preserved. Recent messages are kept intact; older tool exchanges
// are replaced with a single summary message.
func compactToolHistory(msgs []*schema.Message, budget int) []*schema.Message {
	if len(msgs) <= 2 {
		return msgs
	}

	prefixCost := 0
	for _, m := range msgs[:2] {
		prefixCost += len(m.Content)/4 + 4
	}

	remaining := budget - prefixCost
	keepFrom := len(msgs) // index where we start keeping
	for i := len(msgs) - 1; i >= 2; i-- {
		cost := len(msgs[i].Content)/4 + 4
		for _, tc := range msgs[i].ToolCalls {
			cost += len(tc.Function.Arguments) / 4
			cost += len(tc.ID)
		}
		if remaining-cost < 0 {
			break
		}
		remaining -= cost
		keepFrom = i
	}

	if keepFrom <= 2 {
		return msgs
	}

	var toolNames []string
	droppedIterations := 0
	for _, m := range msgs[2:keepFrom] {
		if m.Role == schema.Assistant {
			droppedIterations++
		}
		for _, tc := range m.ToolCalls {
			toolNames = append(toolNames, tc.Function.Name)
		}
	}

	toolNames = dedupStrings(toolNames)

	summary := fmt.Sprintf("[%d earlier iteration(s)", droppedIterations)
	if len(toolNames) > 0 {
		summary += " using: " + strings.Join(toolNames, ", ")
	}
	summary += " — history compacted to stay within context budget]"

	result := make([]*schema.Message, 0, 2+1+len(msgs)-keepFrom)
	result = append(result, msgs[0], msgs[1])
	result = append(result, &schema.Message{
		Role:    schema.User,
		Content: summary,
	})
	result = append(result, msgs[keepFrom:]...)
	return result
}

func dedupStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func isResponseTruncated(resp *schema.Message) bool {
	if resp == nil || resp.ResponseMeta == nil {
		return false
	}
	return resp.ResponseMeta.FinishReason == "length"
}

func isContextOverflowError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "context limit") ||
		strings.Contains(msg, "context length") ||
		strings.Contains(msg, "maximum context") ||
		strings.Contains(msg, "max_tokens") && strings.Contains(msg, "exceed")
}

func isVerificationOnly(toolCalls []schema.ToolCall) bool {
	if len(toolCalls) == 0 {
		return false
	}
	for _, tc := range toolCalls {
		if tc.Function.Name != "shell" {
			return false
		}
		cmd := extractShellCommand(tc.Function.Arguments)
		if !isReadOnlyCommand(cmd) {
			return false
		}
	}
	return true
}

func extractShellCommand(args string) string {
	var parsed struct{ Command string }
	if json.Unmarshal([]byte(args), &parsed) == nil {
		return parsed.Command
	}
	return ""
}

func isReadOnlyCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	segments := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '\n'
	})
	if len(segments) == 0 {
		return false
	}

	readOnly := []string{
		"ls", "find", "grep", "rg", "head", "tail", "wc", "cat", "curl", "test",
		"file", "diff", "stat", "pwd", "sleep", "true",
	}
	for _, seg := range segments {
		for _, part := range strings.Split(seg, "&&") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			for _, p := range strings.Split(part, "||") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				if hasWriteRedirection(p) {
					return false
				}
				fields := strings.Fields(p)
				if len(fields) == 0 {
					continue
				}
				base := filepath.Base(fields[0])
				if base == "cd" {
					continue
				}
				matched := false
				for _, r := range readOnly {
					if base == r {
						matched = true
						break
					}
				}
				if !matched {
					return false
				}
			}
		}
	}
	return true
}

func hasWriteRedirection(cmd string) bool {
	// Treat here-doc as mutating for safety in stagnation detection.
	if strings.Contains(cmd, "<<") {
		return true
	}
	for _, tok := range strings.Fields(cmd) {
		if !strings.Contains(tok, ">") {
			continue
		}
		switch tok {
		case "2>&1", "1>&2", "2>/dev/null", "1>/dev/null", ">/dev/null", "&>/dev/null":
			continue
		default:
			return true
		}
	}
	return false
}

func (a *Agent) handleMessage(ctx context.Context, msg *message.Message) error {
	switch msg.Type {
	case message.TypeTaskAssignment:
		return a.executeTask(ctx, msg)
	case message.TypeEscalation:
		slog.Info("received escalation", "agent", a.Def.Name, "from", msg.From)
		return a.handleEscalation(ctx, msg)
	case message.TypeEscalationResponse:
		// Process escalation response — for now just log it.
		slog.Info("received escalation response", "agent", a.Def.Name, "from", msg.From)
		return nil
	case message.TypeReviewRequest:
		return a.executeTask(ctx, msg)
	default:
		slog.Warn("unhandled message type", "agent", a.Def.Name, "type", msg.Type)
		return nil
	}
}

func (a *Agent) executeTask(ctx context.Context, msg *message.Message) error {
	scope := ""
	if msg.Metadata != nil {
		scope = msg.Metadata["task_scope"]
	}

	// Populate scratch with the agent's existing artifacts from shared storage.
	// This is essential for bug-fix and iteration tasks where the project already
	// exists from prior work but scratch starts empty.
	// Skip in ShellOnly mode — the agent works on an external repo.
	if !a.Def.ShellOnly {
		a.populateScratchWithArtifacts(ctx)
	}

	// Re-materialize work artifacts into scratch if they exist in shared
	// storage but are missing locally (e.g., WORKING wrote them only via
	// file blocks in the final response).
	if lc, ok := loop.DecodeContext(msg); ok && a.ToolExecutor != nil && a.Storage != nil {
		a.ensureScratchFromShared(ctx, lc)
	}

	taskInput := a.buildTaskInput(ctx, msg, scope)
	input := []*schema.Message{
		schema.SystemMessage(a.SystemPrompt),
		schema.UserMessage(taskInput),
	}

	if a.Meter != nil {
		if err := a.Meter.CheckBudget(ctx, a.Def.Name); err != nil {
			return a.sendResult(ctx, msg, fmt.Sprintf("Budget exceeded: %v", err), false, nil, taskMeta(msg))
		}
	}

	maxIter := maxIterationsForScope(scope)
	if a.Def.ShellOnly && maxIter < 15 {
		maxIter = 15 // ShellOnly needs more iterations — each file op is a tool call
	}
	resp, toolCallCount, verifyFailed, err := a.generateWithTools(ctx, input, maxIter)
	if err != nil {
		return a.escalate(ctx, msg, fmt.Sprintf("model call failed: %v", err))
	}

	// Materialize file blocks to scratch so revision iterations can find them.
	// generateWithTools only materializes during intermediate tool-call responses;
	// if all blocks are in the final response, scratch stays empty without this.
	if a.ToolExecutor != nil {
		if blocks, _ := ParseFileBlocks(resp.Content); len(blocks) > 0 {
			materializeBlocksToScratch(a.ToolExecutor.TierPaths[0], blocks)
		}
	}

	for askAttempt := 0; askAttempt < 3; askAttempt++ {
		question, ok := extractUserQuery(resp.Content)
		if !ok {
			break
		}
		answer, askErr := a.askUser(ctx, msg, question)
		if askErr != nil || answer == "" {
			slog.Warn("ASK_USER auto-proceeding",
				"agent", a.Def.Name, "question", question, "attempt", askAttempt, "error", askErr)
			answer = "Proceed without waiting. You have all listed artifacts and tools available. " +
				"Read the upstream artifacts via shell and implement the task."
		}
		input = append(input, schema.AssistantMessage(resp.Content, nil))
		input = append(input, schema.UserMessage("User's answer: "+answer))
		resp, toolCallCount, verifyFailed, err = a.generateWithTools(ctx, input, maxIterationsForScope(scope))
		if err != nil {
			return a.escalate(ctx, msg, fmt.Sprintf("model call failed after user answer: %v", err))
		}
	}

	resp.Content = stripUserQueryLines(resp.Content)

	// If this message carries loop context, route within the loop instead of
	// sending a result back to the original sender.
	if lc, ok := loop.DecodeContext(msg); ok {
		return a.completeLoopStep(ctx, msg, resp.Content, a.cumulativeUsage(), lc, toolCallCount)
	}

	return a.sendResult(ctx, msg, resp.Content, verifyFailed, a.cumulativeUsage(), taskMeta(msg))
}

type resultParts struct {
	parts         []message.Part
	dir   string
	files []string
}

// buildResultParts writes artifacts to shared storage and returns the message parts.
func (a *Agent) buildResultParts(ctx context.Context, requestID, content string, failed bool, taskMeta ...map[string]string) resultParts {
	rp := resultParts{
		parts: []message.Part{message.TextPart{Text: content}},
	}

	if failed || a.Storage == nil {
		return rp
	}

	rp.dir = fmt.Sprintf("artifacts/%s", a.Def.Name)
	traceDir := fmt.Sprintf("%s/_requests/%s", rp.dir, requestID)

	// In ShellOnly mode, skip file block parsing — the agent's work is in the
	// external repo via shell commands, not in file artifacts.
	var blocks []FileBlock
	var summary string
	if !a.Def.ShellOnly {
		blocks, summary = ParseFileBlocks(content)
	}

	if len(blocks) > 0 {
		for _, block := range blocks {
			path := fmt.Sprintf("%s/%s", rp.dir, block.Path)
			if err := a.Storage.Write(ctx, runtime.TierShared, path, []byte(block.Content)); err != nil {
				slog.Warn("failed to write artifact file", "agent", a.Def.Name, "path", path, "error", err)
				continue
			}
			rp.files = append(rp.files, block.Path)
			rp.parts = append(rp.parts, message.FilePart{
				URI:      path,
				MimeType: mimeForPath(block.Path),
				Name:     block.Path,
			})
		}
		if summary != "" {
			summaryPath := fmt.Sprintf("%s/summary.md", traceDir)
			if err := a.Storage.Write(ctx, runtime.TierShared, summaryPath, []byte(summary)); err != nil {
				slog.Warn("failed to write summary", "agent", a.Def.Name, "path", summaryPath, "error", err)
			} else {
				rp.parts = append(rp.parts, message.FilePart{
					URI:      summaryPath,
					MimeType: "text/markdown",
					Name:     "summary.md",
				})
			}
		}
		if summary == "" {
			if len(blocks) > 0 {
				names := make([]string, len(blocks))
				for i, b := range blocks {
					names[i] = b.Path
				}
				summary = "Produced " + strings.Join(names, ", ")
			}
		}
		rp.parts[0] = message.TextPart{Text: summary}
	} else {
		resultPath := fmt.Sprintf("%s/result.md", traceDir)
		if err := a.Storage.Write(ctx, runtime.TierShared, resultPath, []byte(content)); err != nil {
			slog.Warn("failed to write result trace", "agent", a.Def.Name, "path", resultPath, "error", err)
		} else {
			rp.parts = append(rp.parts, message.FilePart{
				URI:      resultPath,
				MimeType: "text/markdown",
				Name:     "result.md",
			})
		}
	}

	// Persist scratch files that were created via shell commands (npm init,
	// framework CLIs, etc.) but not as file blocks. Without this, config files
	// like package.json, tsconfig.json, vite.config.ts are lost when scratch
	// is destroyed.
	// Skip in ShellOnly mode — the agent works on an external repo, not
	// scratch-based artifacts.
	if !a.Def.ShellOnly {
		a.persistScratchToShared(ctx, &rp, blocks)
	}

	// Run post-processing tools with commands on the produced artifacts.
	if len(a.Def.Tools) > 0 && len(rp.files) > 0 {
		var meta map[string]string
		if len(taskMeta) > 0 {
			meta = taskMeta[0]
		}
		a.runTools(ctx, &rp, requestID, meta)
	}

	return rp
}

func (a *Agent) sendResult(ctx context.Context, original *message.Message, content string, failed bool, usage *message.TokenUsage, extraMeta map[string]string) error {
	// Pass task metadata for post-processing tools (git commit messages, etc.).
	meta := map[string]string{}
	if tid, ok := original.Metadata["task_id"]; ok {
		meta["task_id"] = tid
	}
	if original.Type == message.TypeTaskAssignment {
		for _, p := range original.Parts {
			if tp, ok := p.(message.TextPart); ok && tp.Text != "" {
				meta["task_description"] = tp.Text
				break
			}
		}
	}
	rp := a.buildResultParts(ctx, original.RequestID, content, failed, meta)

	msgType := message.TypeTaskResult
	if original.Type == message.TypeReviewRequest {
		msgType = message.TypeReviewResponse
	}

	result := &message.Message{
		ID:         uuid.New().String(),
		RequestID:  original.RequestID,
		From:       a.Def.Name,
		To:         original.From,
		Type:       msgType,
		Parts:      rp.parts,
		Metadata:   map[string]string{},
		TokenUsage: usage,
		Timestamp:  time.Now(),
	}
	if failed {
		result.Metadata["status"] = "failed"
	}

	if msgType == message.TypeReviewResponse && !failed {
		if v := extractVerdict(content); v != "" {
			result.Metadata["verdict"] = v
		}
	}

	for k, v := range extraMeta {
		result.Metadata[k] = v
	}

	if a.Logger != nil {
		a.Logger.Log(ctx, result)
	}

	return a.Comm.Send(ctx, result)
}

func (a *Agent) escalate(ctx context.Context, original *message.Message, reason string) error {
	if a.Def.EscalationTarget == "" {
		extraMeta := taskMeta(original)
		return a.sendResult(ctx, original, fmt.Sprintf("Cannot complete task: %s (no escalation target)", reason), true, nil, extraMeta)
	}

	esc := &message.Message{
		ID:        uuid.New().String(),
		RequestID: original.RequestID,
		From:      a.Def.Name,
		To:        a.Def.EscalationTarget,
		Type:      message.TypeEscalation,
		Parts: []message.Part{
			message.TextPart{Text: reason},
			message.DataPart{Data: map[string]any{
				"original_from": original.From,
				"original_task": extractText(original),
			}},
		},
		Timestamp: time.Now(),
	}

	if a.Logger != nil {
		a.Logger.Log(ctx, esc)
	}

	if err := a.Comm.Send(ctx, esc); err != nil {
		slog.Warn("failed to send escalation", "agent", a.Def.Name, "target", a.Def.EscalationTarget, "error", err)
	}

	// Also send a failed result back to the conductor so the scheduler
	// isn't stuck waiting. The escalation is informational; the result
	// is what unblocks the task graph.
	extraMeta := taskMeta(original)
	return a.sendResult(ctx, original, fmt.Sprintf("Escalated to %s: %s", a.Def.EscalationTarget, reason), true, nil, extraMeta)
}

func (a *Agent) handleEscalation(ctx context.Context, msg *message.Message) error {
	taskInput := a.buildTaskInput(ctx, msg, "")
	input := []*schema.Message{
		schema.SystemMessage(a.SystemPrompt),
		schema.UserMessage(fmt.Sprintf("Another agent has escalated an issue to you for guidance:\n\n%s", taskInput)),
	}

	a.emitModelProgress("Thinking...")
	resp, err := a.Generate(ctx, input)
	if err != nil {
		slog.Warn("escalation model call failed", "agent", a.Def.Name, "error", err)
		return nil // Don't cascade — escalation handling is best-effort.
	}

	response := &message.Message{
		ID:         uuid.New().String(),
		RequestID:  msg.RequestID,
		From:       a.Def.Name,
		To:         msg.From,
		Type:       message.TypeEscalationResponse,
		Parts:      []message.Part{message.TextPart{Text: resp.Content}},
		TokenUsage: extractTokenUsage(resp),
		Timestamp:  time.Now(),
	}

	if a.Logger != nil {
		a.Logger.Log(ctx, response)
	}

	return a.Comm.Send(ctx, response)
}

// completeLoopStep handles FSM routing after an agent finishes its work within a loop.
// It resolves the next state and either forwards to the next agent or sends the
// terminal result back to the conductor.
func (a *Agent) completeLoopStep(ctx context.Context, msg *message.Message, content string, usage *message.TokenUsage, lc *loop.LoopContext, toolCallCount int) error {
	// Extract verdict for review states (envelope-first, fallback to heuristic).
	verdict := ""
	if loop.IsReviewState(&lc.Definition, lc.State.CurrentState) {
		verdict = extractVerdictFromEnvelope(content)

		// Enforce tool usage: a reviewer that approves without reading any
		// files (zero tool calls) gets downgraded to revise. Only enforced
		// when the agent has tools available (ToolExecutor != nil).
		if verdict == "approved" && toolCallCount == 0 && a.ToolExecutor != nil {
			slog.Warn("forcing review verdict to revise due to zero tool calls",
				"agent", a.Def.Name,
				"state", lc.State.CurrentState,
				"task_id", lc.TaskID)
			verdict = "revise"
			content = "VERDICT: revise\n\nYou must use shell tools to read implementation files before approving. Read the files, compare values, then issue your verdict."
		}

		if verdict == "approved" {
			if forcedFeedback, ok := requireReviewEvidenceForApproval(parseSummary(content), lc); ok {
				slog.Warn("forcing review verdict to revise due to missing file evidence",
					"agent", a.Def.Name,
					"state", lc.State.CurrentState,
					"task_id", lc.TaskID)
				verdict = "revise"
				content = forcedFeedback
			}
		}
	}

	if loop.IsReviewState(&lc.Definition, lc.State.CurrentState) && verdict == "" {
		slog.Warn("no verdict extracted from review response, will escalate",
			"agent", a.Def.Name, "state", lc.State.CurrentState,
			"content_preview", truncateLog(content, 200))
	}

	nextState, err := loop.ResolveNextState(&lc.Definition, lc.State.CurrentState, verdict)
	if err != nil {
		// Can't resolve -- force escalate. Terminal path: write artifacts.
		slog.Warn("loop escalated: failed to resolve next state",
			"agent", a.Def.Name, "state", lc.State.CurrentState,
			"verdict", verdict, "error", err)
		rp := a.buildResultParts(ctx, msg.RequestID, content, false)
		return a.sendLoopResult(ctx, msg, rp, usage, lc, true)
	}

	fsm := loop.RestoreFSM(lc.Definition, lc.State)
	fromState := lc.State.CurrentState
	result, err := fsm.Transition(nextState)
	if err != nil {
		// Actual error: invalid transition or already terminal — log and escalate.
		slog.Error("loop transition failed",
			"agent", a.Def.Name, "state", lc.State.CurrentState,
			"next", nextState, "error", err)
		rp := a.buildResultParts(ctx, msg.RequestID, content, false)
		return a.sendLoopResult(ctx, msg, rp, usage, lc, true)
	}
	if result == loop.Escalated {
		// Graceful: max transitions reached, FSM auto-escalated.
		a.Events.Emit(event.Event{
			Type:      event.LoopTransition,
			TaskID:    lc.TaskID,
			LoopID:    lc.Definition.ID,
			FromState: fromState,
			ToState:   fsm.State().CurrentState,
			Verdict:   "escalated",
			LoopCount: fsm.State().TransitionCount,
		})
		rp := a.buildResultParts(ctx, msg.RequestID, content, false)
		return a.sendLoopResult(ctx, msg, rp, usage, lc, true)
	}

	a.Events.Emit(event.Event{
		Type:      event.LoopTransition,
		TaskID:    lc.TaskID,
		LoopID:    lc.Definition.ID,
		FromState: fromState,
		ToState:   nextState,
		Verdict:   verdict,
		LoopCount: fsm.State().TransitionCount,
	})

	if fsm.IsTerminal() {
		rp := a.buildResultParts(ctx, msg.RequestID, content, false)
		return a.sendLoopResult(ctx, msg, rp, usage, lc, false)
	}

	// Non-terminal: persist this agent's artifacts (so they're visible in the
	// shared volume), then forward a lightweight summary to the next agent.
	rp := a.buildResultParts(ctx, msg.RequestID, content, false)
	summary := parseSummary(content)
	if loop.IsReviewState(&lc.Definition, lc.State.CurrentState) {
		summary = reviewFeedbackSummary(summary, verdict)
	}
	if err := a.forwardLoopMessage(ctx, msg, summary, &rp, verdict, fsm.State(), lc, nextState); err != nil {
		// Forward failed (e.g., next agent unreachable). Escalate to the
		// conductor so the task doesn't hang forever as "working".
		slog.Error("loop forward failed, escalating result to conductor",
			"agent", a.Def.Name, "next_state", nextState, "error", err)
		return a.sendLoopResult(ctx, msg, rp, usage, lc, true)
	}
	return nil
}

// forwardLoopMessage builds and sends a message to the next agent in the loop.
// rp carries artifact metadata from buildResultParts so the forwarded message
// can include artifact_uri and artifact_files for targeted reading downstream.
func (a *Agent) forwardLoopMessage(ctx context.Context, original *message.Message, summary string, rp *resultParts, verdict string, newState loop.LoopState, lc *loop.LoopContext, nextStateName string) error {
	nextAgent := lc.Definition.AgentForState(nextStateName)
	if nextAgent == "" {
		return fmt.Errorf("no agent for state %q", nextStateName)
	}

	var manifestFiles []string
	if verdict == "revise" {
		manifestFiles = lc.WorkArtifactFiles
	}
	prompt := loop.BuildStatePrompt(lc.OriginalTask, &lc.Definition, nextStateName, verdict, summary, lc.DecisionContext, manifestFiles)

	parts := []message.Part{message.TextPart{Text: prompt}}

	if lc.UserRequest != "" {
		parts = append(parts, message.DataPart{Data: map[string]any{
			"user_request": lc.UserRequest,
		}})
	}

	filteredDeps := loop.FilterDepParts(lc.DepParts, &lc.Definition, nextStateName, verdict, nextAgent)
	for _, dp := range filteredDeps {
		parts = append(parts, message.DataPart{Data: dp})
	}

	isWorkerState := !loop.IsReviewState(&lc.Definition, lc.State.CurrentState)
	workSummary := lc.WorkSummary
	workArtifactURI := lc.WorkArtifactURI
	workArtifactFiles := lc.WorkArtifactFiles
	if isWorkerState {
		workSummary = mergeWorkSummary(workSummary, summary)
		if rp != nil && len(rp.files) > 0 {
			workArtifactURI = rp.dir + "/"
			workArtifactFiles = mergeArtifactFiles(workArtifactFiles, rp.files)
		}
	}

	// Forward cumulative summary so reviewers see the full implementation.
	depResult := summary
	if isWorkerState {
		depResult = workSummary
	}
	depData := map[string]any{
		"dependency_id":    lc.TaskID,
		"dependency_agent": a.Def.Name,
		"context_kind":     "loop_feedback",
		"result":           depResult,
	}
	if workArtifactURI != "" && len(workArtifactFiles) > 0 {
		depData["artifact_uri"] = workArtifactURI
		depData["artifact_files"] = workArtifactFiles
	}
	parts = append(parts, message.DataPart{Data: depData})

	updatedLC := &loop.LoopContext{
		Definition:        lc.Definition,
		State:             newState,
		TaskID:            lc.TaskID,
		Conductor:         lc.Conductor,
		OriginalTask:      lc.OriginalTask,
		UserRequest:       lc.UserRequest,
		DepParts:          lc.DepParts,
		WorkSummary:       workSummary,
		WorkArtifactURI:   workArtifactURI,
		WorkArtifactFiles: workArtifactFiles,
	}
	parts = append(parts, loop.EncodeContext(updatedLC))

	msgType := message.TypeTaskAssignment
	if loop.IsReviewState(&lc.Definition, nextStateName) {
		msgType = message.TypeReviewRequest
	}

	fwdMeta := map[string]string{
		"task_id":    lc.TaskID,
		"loop_id":    lc.Definition.ID,
		"loop_state": nextStateName,
	}
	// Carry task_scope through loop hops so agents keep the right iteration cap.
	if original.Metadata != nil {
		if scope := original.Metadata["task_scope"]; scope != "" {
			fwdMeta["task_scope"] = scope
		}
	}

	fwd := &message.Message{
		ID:        uuid.New().String(),
		RequestID: original.RequestID,
		From:      a.Def.Name,
		To:        nextAgent,
		Type:      msgType,
		Parts:     parts,
		Metadata:  fwdMeta,
		Timestamp: time.Now(),
	}

	if a.Logger != nil {
		a.Logger.Log(ctx, fwd)
	}

	return a.Comm.Send(ctx, fwd)
}

// sendLoopResult sends the terminal loop result back to the conductor.
// If the terminal agent is a reviewer and a WorkSummary exists, the first
// TextPart is replaced with the worker's summary so the task result reflects
// what was actually built rather than the review verdict.
func (a *Agent) sendLoopResult(ctx context.Context, original *message.Message, rp resultParts, usage *message.TokenUsage, lc *loop.LoopContext, escalated bool) error {
	meta := map[string]string{
		"task_id": lc.TaskID,
		"loop_id": lc.Definition.ID,
	}
	if lc.DispatchNonce != "" {
		meta["dispatch_nonce"] = lc.DispatchNonce
	}
	if escalated {
		meta["loop_escalated"] = "true"
	}

	parts := rp.parts
	if lc.WorkSummary != "" && loop.IsReviewState(&lc.Definition, lc.State.CurrentState) {
		// Replace reviewer's text with the worker's summary.
		parts = make([]message.Part, len(rp.parts))
		copy(parts, rp.parts)
		parts[0] = message.TextPart{Text: lc.WorkSummary}

		// Ensure worker artifacts are preserved on the terminal message so
		// downstream consumers don't lose the implementation file references.
		if lc.WorkArtifactURI != "" && len(lc.WorkArtifactFiles) > 0 {
			existing := map[string]bool{}
			for _, p := range parts {
				if fp, ok := p.(message.FilePart); ok {
					existing[fp.Name] = true
				}
			}
			for _, name := range lc.WorkArtifactFiles {
				if existing[name] {
					continue
				}
				parts = append(parts, message.FilePart{
					URI:      lc.WorkArtifactURI + name,
					MimeType: mimeForPath(name),
					Name:     name,
				})
			}
		}
	}

	result := &message.Message{
		ID:         uuid.New().String(),
		RequestID:  original.RequestID,
		From:       a.Def.Name,
		To:         lc.Conductor,
		Type:       message.TypeTaskResult,
		Parts:      parts,
		Metadata:   meta,
		TokenUsage: usage,
		Timestamp:  time.Now(),
	}

	if a.Logger != nil {
		a.Logger.Log(ctx, result)
	}

	return a.Comm.Send(ctx, result)
}

// parseSummary extracts the summary text from LLM output. If the output contains
// file blocks, the summary is the text outside those blocks. Otherwise returns
// the full content. This avoids the I/O of buildResultParts for intermediate hops.
func parseSummary(content string) string {
	_, summary := ParseFileBlocks(content)
	if summary != "" {
		return summary
	}
	return content
}

// reviewFeedbackSummary normalizes reviewer output so revision agents receive
// actionable findings first. It strips preamble before VERDICT when present.
func reviewFeedbackSummary(summary, verdict string) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		if verdict == "" {
			return ""
		}
		return "VERDICT: " + verdict
	}

	lower := strings.ToLower(s)
	if idx := strings.Index(lower, "verdict:"); idx >= 0 {
		s = strings.TrimSpace(s[idx:])
	} else if verdict != "" {
		// Ensure an explicit verdict anchor exists for downstream revision.
		s = "VERDICT: " + verdict + "\n\n" + s
	}

	const maxFeedbackChars = 5000
	if len(s) > maxFeedbackChars {
		return s[:maxFeedbackChars] + "\n[... truncated feedback]"
	}
	return s
}

// requireReviewEvidenceForApproval enforces file-backed review rigor for
// approval verdicts when both upstream artifact files and implementation files
// are available in loop context. Returns (feedback, true) when approval should
// be downgraded to revise.
func requireReviewEvidenceForApproval(summary string, lc *loop.LoopContext) (string, bool) {
	if lc == nil {
		return "", false
	}
	upstreamFiles := collectDepArtifactFiles(lc.DepParts)
	workFiles := dedupeNonEmpty(lc.WorkArtifactFiles)
	if len(upstreamFiles) == 0 || len(workFiles) == 0 {
		return "", false
	}
	if reviewHasFileEvidence(summary, upstreamFiles, workFiles) {
		return "", false
	}

	if len(upstreamFiles) > 4 {
		upstreamFiles = upstreamFiles[:4]
	}
	if len(workFiles) > 4 {
		workFiles = workFiles[:4]
	}
	previewUpstream := strings.Join(upstreamFiles, ", ")
	previewWork := strings.Join(workFiles, ", ")
	feedback := "VERDICT: revise\n\n" +
		"Approval lacks file-level evidence. Re-review against upstream artifacts and implementation files, then cite concrete file paths.\n" +
		"Expected upstream files: " + previewUpstream + "\n" +
		"Observed implementation files: " + previewWork + "\n" +
		"Only approve after confirming fidelity with explicit file references."
	return feedback, true
}

func collectDepArtifactFiles(depParts []map[string]any) []string {
	var out []string
	for _, dp := range depParts {
		files := toStringSlice(dp["artifact_files"])
		for _, f := range files {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			out = append(out, f)
		}
	}
	return dedupeNonEmpty(out)
}

func reviewHasFileEvidence(summary string, upstreamFiles, workFiles []string) bool {
	s := strings.ToLower(summary)
	if strings.TrimSpace(s) == "" {
		return false
	}
	return containsAnyFileRef(s, upstreamFiles) && containsAnyFileRef(s, workFiles)
}

func containsAnyFileRef(summaryLower string, files []string) bool {
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		full := strings.ToLower(f)
		base := strings.ToLower(filepath.Base(f))
		if full != "" && strings.Contains(summaryLower, full) {
			return true
		}
		if base != "" && base != full && strings.Contains(summaryLower, base) {
			return true
		}
	}
	return false
}

func dedupeNonEmpty(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func mergeWorkSummary(existing, latest string) string {
	existing = strings.TrimSpace(existing)
	latest = strings.TrimSpace(latest)
	switch {
	case existing == "":
		return latest
	case latest == "":
		return existing
	case strings.Contains(existing, latest):
		return existing
	case strings.Contains(latest, existing):
		return latest
	default:
		const maxSummaryChars = 12_000
		merged := existing + "\n\n## Revision Update\n" + latest
		if len(merged) <= maxSummaryChars {
			return merged
		}
		// Keep the newest information when truncating.
		keepTail := maxSummaryChars - len(existing) - 32
		if keepTail < 300 {
			keepTail = 300
		}
		if len(latest) > keepTail {
			latest = latest[len(latest)-keepTail:]
		}
		return existing + "\n\n## Revision Update\n[... truncated older details]\n" + latest
	}
}

func mergeArtifactFiles(existing, latest []string) []string {
	if len(existing) == 0 {
		return append([]string{}, latest...)
	}
	out := append([]string{}, existing...)
	seen := make(map[string]bool, len(existing)+len(latest))
	for _, f := range out {
		seen[f] = true
	}
	for _, f := range latest {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// extractVerdict parses "VERDICT: approved" or "VERDICT: revise" from LLM output.
// Returns the normalized verdict string ("approved" or "revise"), or empty if not found.
// Falls back to heuristic keyword matching when no explicit VERDICT line is present.
func extractVerdict(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "verdict:") {
			v := strings.TrimSpace(line[len("verdict:"):])
			v = strings.ToLower(v)
			switch {
			case strings.HasPrefix(v, "approved"):
				return "approved"
			case strings.HasPrefix(v, "revise"):
				return "revise"
			}
		}
	}

	// Fallback: heuristic scan for approval/revision language.
	lower := strings.ToLower(content)
	approveSignals := []string{
		"approved", "approve", "looks good", "lgtm", "well done",
		"passes review", "no issues", "ready for", "all good",
	}
	for _, sig := range approveSignals {
		if strings.Contains(lower, sig) {
			slog.Warn("verdict inferred heuristically", "verdict", "approved", "signal", sig)
			return "approved"
		}
	}

	reviseSignals := []string{
		"needs revision", "needs revising", "needs work", "must fix",
		"please fix", "please revise", "send back for revision",
		"not ready", "critical issues",
	}
	for _, sig := range reviseSignals {
		if strings.Contains(lower, sig) {
			slog.Warn("verdict inferred heuristically", "verdict", "revise", "signal", sig)
			return "revise"
		}
	}

	return ""
}

// taskMeta builds the standard result metadata from an incoming assignment,
// carrying forward task_id and dispatch_nonce for the scheduler's nonce filter.
func taskMeta(msg *message.Message) map[string]string {
	m := map[string]string{"task_id": msg.Metadata["task_id"]}
	if nonce := msg.Metadata["dispatch_nonce"]; nonce != "" {
		m["dispatch_nonce"] = nonce
	}
	return m
}

func extractText(msg *message.Message) string {
	for _, p := range msg.Parts {
		if tp, ok := p.(message.TextPart); ok {
			return tp.Text
		}
	}
	return ""
}

func stripUserQueryLines(content string) string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "ASK_USER:") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func extractUserQuery(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "ASK_USER:") {
			question := strings.TrimSpace(trimmed[len("ASK_USER:"):])
			if question != "" {
				return question, true
			}
		}
	}
	return "", false
}

// askUser sends a TypeUserQuery to the conductor and blocks until TypeUserResponse arrives.
func (a *Agent) askUser(ctx context.Context, original *message.Message, question string) (string, error) {
	queryMsg := &message.Message{
		ID:        uuid.New().String(),
		RequestID: original.RequestID,
		From:      a.Def.Name,
		To:        "conductor",
		Type:      message.TypeUserQuery,
		Parts:     []message.Part{message.TextPart{Text: question}},
		Metadata: map[string]string{
			"task_id": original.Metadata["task_id"],
		},
		Timestamp: time.Now(),
	}

	if err := a.Comm.Send(ctx, queryMsg); err != nil {
		return "", fmt.Errorf("send user query: %w", err)
	}

	// Wait for TypeUserResponse on the comm channel.
	// The agent's Run() loop is blocked in handleMessage, so we can safely
	// read from the comm channel here.
	timeout := 5 * time.Minute
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ch := a.Comm.Receive(ctx)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", fmt.Errorf("user query timed out")
		case msg, ok := <-ch:
			if !ok {
				return "", fmt.Errorf("channel closed while waiting for user response")
			}
			if msg.Type == message.TypeUserResponse {
				return extractText(msg), nil
			}
			slog.Warn("unexpected message while waiting for user response",
				"agent", a.Def.Name, "type", msg.Type)
		}
	}
}

func extractTokenUsage(resp *schema.Message) *message.TokenUsage {
	if resp == nil || resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return nil
	}
	u := resp.ResponseMeta.Usage
	return &message.TokenUsage{
		InputTokens:  int64(u.PromptTokens),
		OutputTokens: int64(u.CompletionTokens),
		TotalTokens:  int64(u.PromptTokens + u.CompletionTokens),
	}
}

// cumulativeUsage returns the agent's total token usage from the meter.
// This captures all LLM calls made during task execution, not just the last one.
func (a *Agent) cumulativeUsage() *message.TokenUsage {
	if a.Meter == nil {
		return nil
	}
	usage, err := a.Meter.Usage(context.Background(), a.Def.Name)
	if err != nil || usage.TotalCalls == 0 {
		return nil
	}
	return &message.TokenUsage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		TotalTokens:     usage.TotalTokens,
		TotalCalls:      usage.TotalCalls,
		CacheReadTokens: usage.CacheReadTokens,
		Model:           a.Def.Model,
	}
}

func scopeHint(scope string, shellOnly bool) string {
	if shellOnly {
		switch scope {
		case "small":
			return "## Scope: Small\nThis is a small task. Be concise. Do not over-engineer.\nMake all changes via shell tool commands.\n\n"
		case "large":
			return "## Scope: Large\nThis is a large task. Be thorough with the implementation.\nMake all changes via shell tool commands.\n\n"
		default:
			return "## Scope: Standard\nThis is a standard task. Produce working code.\nMake all changes via shell tool commands.\n\n"
		}
	}
	switch scope {
	case "small":
		return "## Scope: Small\nThis is a small task. Be concise. Do not over-engineer.\nWhen fixing errors, only produce file blocks for files that need changes.\n\n"
	case "large":
		return "## Scope: Large\nThis is a large task. Be thorough with the implementation.\nWhen fixing errors, only produce file blocks for files that need changes.\n\n"
	default:
		return "## Scope: Standard\nThis is a standard task. Produce working code.\nWhen fixing errors, only produce file blocks for files that need changes.\n\n"
	}
}

// buildTaskInput formats all message parts into a single LLM user message.
// TextParts become the task description; DataParts with dependency context
// are formatted as labeled sections so the LLM sees upstream results.
// Artifacts are passed by reference (URI + optional file list); the agent
// should use tools to inspect files as needed rather than inlining file bodies.
func (a *Agent) buildTaskInput(ctx context.Context, msg *message.Message, scope string) string {
	_ = ctx
	var taskDesc string
	var pipelineCtx string
	var userRequest string
	var chatContext string
	var deps []string
	currentTaskID := ""
	if msg.Metadata != nil {
		currentTaskID = msg.Metadata["task_id"]
	}

	for _, p := range msg.Parts {
		switch v := p.(type) {
		case message.TextPart:
			if taskDesc == "" {
				taskDesc = v.Text
			}
		case message.FilePart:
			kind := "text"
			if isBinaryMime(v.MimeType) {
				kind = "binary"
			}
			deps = append(deps, fmt.Sprintf(
				"--- File Reference: %s (%s) ---\nURI: %s\nUse shell tools to inspect this file from shared artifacts when needed.",
				v.Name, kind, v.URI,
			))
		case message.DataPart:
			if _, ok := v.Data["loop_context"]; ok {
				continue
			}
			if ur, ok := v.Data["user_request"].(string); ok {
				userRequest = ur
				continue
			}
			if pc, ok := v.Data["pipeline_context"].(string); ok {
				pipelineCtx = pc
				continue
			}
			if cc, ok := v.Data["chat_context"].(string); ok {
				chatContext = cc
				continue
			}
			handled := false
			if kc, ok := v.Data["knowledge_context"]; ok {
				deps = append(deps, formatKnowledgeContext(kc))
				handled = true
			}
			if rk, ok := v.Data["related_knowledge"]; ok {
				deps = append(deps, formatRelatedKnowledge(rk))
				handled = true
			}
			if handled {
				continue
			}
			if hint, ok := v.Data["no_upstream"].(string); ok {
				deps = append(deps, "--- No Upstream Dependencies ---\n"+hint)
				continue
			}
			if _, ok := v.Data["existing_artifacts"]; ok {
				if fileStrs := toStringSlice(v.Data["files"]); len(fileStrs) > 0 {
					var listing strings.Builder
					listing.WriteString("--- Existing Artifacts (READ FIRST before solutioning) ---\n")
					listing.WriteString("These files exist from prior work and may be binding constraints.\n")
					listing.WriteString("Use your shell tool to read the specific upstream files you depend on before implementing changes.\n\n")
					for _, s := range fileStrs {
						listing.WriteString(s)
						listing.WriteByte('\n')
					}
					deps = append(deps, listing.String())
				}
				continue
			}
			if depID, ok := v.Data["dependency_id"].(string); ok {
				agent, _ := v.Data["dependency_agent"].(string)
				label := depID
				if agent != "" {
					label = fmt.Sprintf("%s (%s)", depID, agent)
				}

				contextKind, _ := v.Data["context_kind"].(string)
				isLoopFeedback := contextKind == "loop_feedback" || (currentTaskID != "" && depID == currentTaskID)

				// Constraint anchoring: direct dependencies are binding.
				// Iterative loop feedback is advisory until validated against
				// the binding upstream artifacts.
				anchor := ""
				if isLoopFeedback {
					anchor = " (iterative feedback — validate against binding upstream artifacts)"
				} else if agent != "" {
					anchor = " (binding constraint — implement faithfully)"
				}

				var contentParts []string
				if isLoopFeedback {
					contentParts = append(contentParts, "This context is iterative loop feedback. Validate it against upstream dependency artifacts; if conflicts exist, prioritize upstream artifacts and note the conflict.")
				}
				if uri, ok := v.Data["artifact_uri"].(string); ok && uri != "" {
					contentParts = append(contentParts, "Upstream artifact source (READ FIRST): "+uri)
					if files := toStringSlice(v.Data["artifact_files"]); len(files) > 0 {
						contentParts = append(contentParts, "Upstream artifact files (READ FIRST):\n"+joinFileList(files, 25))
					}
					if !isLoopFeedback {
						contentParts = append(contentParts, "Do not assume file contents from this prompt. Read referenced upstream artifacts via shell before solutioning.")
						contentParts = append(contentParts,
							"After reading upstream artifacts, extract and list binding constraints before implementing:\n"+
								"- Exact color values (hex codes)\n"+
								"- Type/interface definitions (field names, types)\n"+
								"- Required dependencies/libraries\n"+
								"- Layout structure and CSS properties\n"+
								"- UI component types from HTML mockups (e.g., button group vs dropdown, specific input types)\n"+
								"- Specific element labels, placeholder text, and filter/tab names\n"+
								"- Which fields and features are present — do NOT add features absent from specs, do NOT omit specified ones\n"+
								"Your implementation MUST use these exact values and component patterns — do not substitute alternatives.")
					}
				}
				if result, _ := v.Data["result"].(string); strings.TrimSpace(result) != "" {
					maxSummary := 2000
					if isLoopFeedback {
						maxSummary = 1200
					}
					contentParts = append(contentParts, "Upstream summary:\n"+truncateForPrompt(result, maxSummary))
				}
				if len(contentParts) > 0 {
					deps = append(deps, fmt.Sprintf("--- Context from %s%s ---\n%s", label, anchor, strings.Join(contentParts, "\n")))
				}
			} else {
				raw, _ := json.Marshal(v.Data)
				deps = append(deps, fmt.Sprintf("--- Additional data ---\n%s", string(raw)))
			}
		}
	}

	const maxInputChars = 500_000 // ~125k tokens, leaves headroom for system prompt + output
	overhead := len(taskDesc) + len(pipelineCtx) + 4
	if len(deps) > 0 {
		deps = truncateDeps(deps, maxInputChars-overhead)
	}

	var b strings.Builder
	b.WriteString(scopeHint(scope, a.Def.ShellOnly))
	if userRequest != "" {
		b.WriteString("## Original User Request\n")
		b.WriteString(userRequest)
		b.WriteString("\n\n")
	}
	b.WriteString(taskDesc)
	if chatContext != "" {
		b.WriteString("\n\n## User Feedback (from chat amendment)\n")
		b.WriteString("The user chatted with you about this task and provided the following feedback.\n")
		b.WriteString("Incorporate these refinements into your output:\n\n")
		b.WriteString(chatContext)
	}
	if pipelineCtx != "" {
		b.WriteString("\n\n")
		b.WriteString(pipelineCtx)
	}
	if len(deps) > 0 {
		b.WriteString("\n\n## Upstream Dependency Context\n")
		b.WriteString("The following is OUTPUT from completed upstream tasks that YOUR work depends on.\n")
		b.WriteString("These specifications are BINDING REQUIREMENTS, not suggestions.\n")
		b.WriteString("Deviation from upstream specs (e.g., using different colors, types, or libraries) is a defect.\n")
		b.WriteString("Common defects: substituting a dropdown for a button group, changing filter/tab labels,\n")
		b.WriteString("adding features not in the spec, omitting specified fields (e.g., time picker).\n\n")
		b.WriteString("Before writing any code:\n")
		b.WriteString("1. Read the referenced upstream artifact files via shell.\n")
		b.WriteString("2. List the specific values you extracted (colors, types, deps, layout).\n")
		b.WriteString("3. List every UI component pattern from HTML mockups (element type, labels, input types).\n")
		b.WriteString("4. Implement using those exact values and patterns. Do not add or remove features.\n\n")
		b.WriteString(strings.Join(deps, "\n\n"))
	}
	return b.String()
}

func truncateForPrompt(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n[... truncated]"
}

func joinFileList(files []string, max int) string {
	if len(files) == 0 {
		return ""
	}
	if max <= 0 || len(files) <= max {
		return strings.Join(files, "\n")
	}
	return strings.Join(files[:max], "\n") + fmt.Sprintf("\n... +%d more", len(files)-max)
}

// truncateDeps shrinks dependency context strings to fit within budget.
// Each dep gets an equal share; any dep already under its share donates
// the surplus to others.
func truncateDeps(deps []string, budget int) []string {
	total := 0
	for _, d := range deps {
		total += len(d)
	}
	if total <= budget {
		return deps
	}

	slog.Info("truncating dependency contexts", "total_chars", total, "budget", budget, "deps", len(deps))

	// Two-pass allocation: short deps keep their full size, long deps
	// split the remainder equally.
	perDep := budget / len(deps)
	surplus := 0
	longCount := 0
	for _, d := range deps {
		if len(d) <= perDep {
			surplus += perDep - len(d)
		} else {
			longCount++
		}
	}

	longBudget := perDep
	if longCount > 0 {
		longBudget = perDep + surplus/longCount
	}

	result := make([]string, len(deps))
	for i, d := range deps {
		limit := longBudget
		if len(d) <= perDep {
			limit = len(d) // short dep, keep as-is
		}
		if len(d) > limit {
			omitted := len(d) - limit
			// Truncate at a line boundary if possible.
			cutPoint := limit
			if nl := strings.LastIndex(d[:cutPoint], "\n"); nl > cutPoint/2 {
				cutPoint = nl
			}
			result[i] = d[:cutPoint] + fmt.Sprintf("\n\n[... truncated, %d chars omitted]", omitted)
		} else {
			result[i] = d
		}
	}
	return result
}

// readArtifact reads an artifact from storage. If the URI ends with "/" it's
// treated as a directory and all files within are read and concatenated
// (recursively, so nested subdirectories are included).
// Otherwise it reads a single file.
func (a *Agent) readArtifact(ctx context.Context, uri, agent string) string {
	if strings.HasSuffix(uri, "/") {
		return a.readArtifactDir(ctx, uri, agent)
	}

	data, err := a.Storage.Read(ctx, runtime.TierShared, uri)
	if err != nil {
		slog.Warn("failed to read artifact", "uri", uri, "error", err)
		return ""
	}
	return string(data)
}

// readArtifactDir reads all files under a directory URI recursively.
// It tries progressively deeper globs to find nested files.
func (a *Agent) readArtifactDir(ctx context.Context, uri, agent string) string {
	var allFiles []string

	// List at multiple depths to catch nested files (e.g., decisions/auth.md).
	// filepath.Glob doesn't support ** so we expand manually.
	for depth := 1; depth <= 5; depth++ {
		pattern := uri + strings.Repeat("*/", depth-1) + "*"
		files, err := a.Storage.List(ctx, runtime.TierShared, pattern)
		if err != nil {
			break
		}
		allFiles = append(allFiles, files...)
	}

	seen := make(map[string]bool, len(allFiles))
	var unique []string
	for _, f := range allFiles {
		if !seen[f] {
			seen[f] = true
			unique = append(unique, f)
		}
	}

	if len(unique) == 0 {
		slog.Warn("no files found in artifact directory", "uri", uri)
		return ""
	}

	var parts []string
	for _, f := range unique {
		name := strings.TrimPrefix(f, uri)
		if isBinaryPath(f) {
			parts = append(parts, fmt.Sprintf("--- File: %s from %s (binary, skipped) ---", name, agent))
			continue
		}
		data, err := a.Storage.Read(ctx, runtime.TierShared, f)
		if err != nil {
			slog.Warn("failed to read artifact file", "path", f, "error", err)
			continue
		}
		parts = append(parts, fmt.Sprintf("--- File: %s from %s ---\n%s", name, agent, string(data)))
	}
	return strings.Join(parts, "\n\n")
}

// readArtifactFiles reads specific files from a directory URI with per-file
// and total character caps. This is more efficient than readArtifactDir when
// the caller knows exactly which files to read.
func (a *Agent) readArtifactFiles(ctx context.Context, dir string, files []string,
	maxFiles, maxCharsPerFile, maxTotal int) string {
	var parts []string
	totalChars := 0
	for i, f := range files {
		if i >= maxFiles {
			break
		}
		if isBinaryPath(f) {
			parts = append(parts, fmt.Sprintf("--- %s (binary, skipped) ---", f))
			continue
		}
		path := dir + f
		data, err := a.Storage.Read(ctx, runtime.TierShared, path)
		if err != nil {
			continue
		}
		s := string(data)
		if len(s) > maxCharsPerFile {
			s = s[:maxCharsPerFile] + "\n[... truncated]"
		}
		if totalChars+len(s) > maxTotal {
			parts = append(parts, fmt.Sprintf("--- %s (skipped, context budget reached) ---", f))
			break
		}
		parts = append(parts, fmt.Sprintf("--- File: %s ---\n%s", f, s))
		totalChars += len(s)
	}
	return strings.Join(parts, "\n\n")
}

func formatKnowledgeContext(kc any) string {
	entries := toMapSlice(kc)
	if len(entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("--- Prior Knowledge ---\n")
	for _, m := range entries {
		title, _ := m["title"].(string)
		summary, _ := m["summary"].(string)
		file, _ := m["file"].(string)
		fromReq, _ := m["from_req"].(string)

		b.WriteString(fmt.Sprintf("**%s**", title))
		if fromReq != "" {
			b.WriteString(fmt.Sprintf(" (from: %s)", fromReq))
		}
		b.WriteByte('\n')
		if summary != "" {
			b.WriteString(summary)
			b.WriteByte('\n')
		}
		if file != "" {
			b.WriteString(fmt.Sprintf("Full doc: %s\n", file))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func formatRelatedKnowledge(rk any) string {
	entries := toMapSlice(rk)
	if len(entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("--- Related Knowledge (from other agents) ---\n")
	for _, m := range entries {
		title, _ := m["title"].(string)
		summary, _ := m["summary"].(string)
		file, _ := m["file"].(string)
		fromReq, _ := m["from_req"].(string)
		sourceAgent, _ := m["source_agent"].(string)

		b.WriteString(fmt.Sprintf("**%s**", title))
		if sourceAgent != "" {
			b.WriteString(fmt.Sprintf(" (from %s)", sourceAgent))
		}
		if fromReq != "" {
			b.WriteString(fmt.Sprintf(" [req: %s]", fromReq))
		}
		b.WriteByte('\n')
		if summary != "" {
			b.WriteString(summary)
			b.WriteByte('\n')
		}
		if file != "" {
			b.WriteString(fmt.Sprintf("Full doc: %s\n", file))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

func toMapSlice(v any) []map[string]any {
	switch s := v.(type) {
	case []map[string]any:
		return s
	case []any:
		out := make([]map[string]any, 0, len(s))
		for _, item := range s {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func isBinaryMime(mime string) bool {
	return strings.HasPrefix(mime, "image/") ||
		strings.HasPrefix(mime, "audio/") ||
		strings.HasPrefix(mime, "video/") ||
		mime == "application/octet-stream"
}

func isBinaryPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg",
		".ico", ".bmp", ".tiff",
		".mp3", ".wav", ".ogg",
		".mp4", ".webm", ".avi",
		".zip", ".tar", ".gz",
		".pdf", ".woff", ".woff2", ".ttf", ".eot":
		return true
	}
	return false
}

func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "text/x-go"
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "text/yaml"
	case ".js":
		return "text/javascript"
	case ".ts":
		return "text/typescript"
	case ".py":
		return "text/x-python"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".png":
		return "image/png"
	default:
		return "text/plain"
	}
}
