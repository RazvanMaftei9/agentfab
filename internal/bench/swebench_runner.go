package bench

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/razvanmaftei/agentfab/internal/conductor"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/metrics"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// SWEBenchRunner orchestrates SWE-bench instance runs.
type SWEBenchRunner struct {
	BaseFabricDef *config.FabricDef
	Factory       conductor.ModelFactory
	OutputDir     string
	CacheDir      string // directory for cached repo clones
	ModelName     string // model identifier for predictions (e.g., "agentfab-v1")
	Debug         bool
	Hints         bool // include hints_text in the request
}

// RunInstance executes agentfab on a single SWE-bench instance.
func (r *SWEBenchRunner) RunInstance(ctx context.Context, inst SWEBenchInstance) (SWEBenchResult, SWEBenchPrediction, error) {
	result := SWEBenchResult{
		InstanceID: inst.InstanceID,
		Repo:       inst.Repo,
	}
	pred := SWEBenchPrediction{
		InstanceID:      inst.InstanceID,
		ModelNameOrPath: r.ModelName,
	}

	slog.Info("swebench: starting instance",
		"instance", inst.InstanceID, "repo", inst.Repo)

	// Create run directory.
	runDir := filepath.Join(r.OutputDir, sanitizeInstanceID(inst.InstanceID))
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return result, pred, fmt.Errorf("create run dir: %w", err)
	}

	// Clone the repo into the developer's scratch directory so it's
	// accessible within the sandbox. The scratch dir for agent "developer"
	// is os.TempDir()/agentfab-developer — the same path the sandbox uses
	// as its read-write working directory.
	scratchDir := filepath.Join(os.TempDir(), "agentfab-developer")
	if err := os.MkdirAll(scratchDir, 0755); err != nil {
		return result, pred, fmt.Errorf("create scratch dir: %w", err)
	}
	repoDir := filepath.Join(scratchDir, "repo")
	// Clean up any leftover repo from a previous run.
	os.RemoveAll(repoDir)
	if err := CloneRepo(inst.Repo, inst.BaseCommit, r.CacheDir, repoDir); err != nil {
		result.Error = err.Error()
		return result, pred, err
	}

	// Build the request using $SCRATCH_DIR/repo so the developer agent
	// can access it naturally within its sandbox.
	request := r.buildRequest(inst, "$SCRATCH_DIR/repo")

	// Write request to file for debugging.
	_ = os.WriteFile(filepath.Join(runDir, "request.txt"), []byte(request), 0644)

	// Prepare a minimal agent definition (conductor + developer).
	def, err := r.buildDefinition()
	if err != nil {
		result.Error = err.Error()
		return result, pred, err
	}

	// Set up event bus.
	bus := event.NewBus()
	var taskEvents []event.Event
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for evt := range bus {
			switch evt.Type {
			case event.TaskComplete, event.TaskFailed:
				taskEvents = append(taskEvents, evt)
			}
		}
	}()

	// Create and run conductor.
	c, err := conductor.New(def, runDir, r.Factory, bus)
	if err != nil {
		result.Error = err.Error()
		return result, pred, fmt.Errorf("create conductor: %w", err)
	}
	c.SkipDisambiguation = true // No user to answer clarifying questions.
	c.SkipScratchCleanup = true // We need the repo intact to capture git diff.

	start := time.Now()
	if err := c.Start(ctx); err != nil {
		result.Error = err.Error()
		bus.Close()
		<-doneCh
		return result, pred, fmt.Errorf("start conductor: %w", err)
	}

	_, reqErr := c.HandleRequest(ctx, request)
	result.Duration = time.Since(start)

	// Capture the git diff BEFORE shutdown/cleanup removes the scratch dir.
	diff, diffErr := CaptureGitDiff(repoDir)
	if diffErr != nil {
		slog.Warn("swebench: failed to capture diff",
			"instance", inst.InstanceID, "error", diffErr)
		result.Error = diffErr.Error()
	}
	_ = copyGitDiffToRunDir(repoDir, runDir)

	// Clean up scratch dir now that we have the diff.
	os.RemoveAll(repoDir)

	// Shutdown.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	c.Shutdown(shutCtx)

	bus.Close()
	<-doneCh

	// Collect metrics.
	allUsage := c.Meter.AllAgentUsage(ctx)
	allRecords := c.Meter.AllRecords(ctx)

	var totalIn, totalOut, totalCache, totalCalls int64
	for agent, usage := range allUsage {
		totalIn += usage.InputTokens
		totalOut += usage.OutputTokens
		totalCache += usage.CacheReadTokens
		totalCalls += usage.TotalCalls

		agentModel := ""
		for _, a := range def.Agents {
			if a.Name == agent {
				agentModel = a.Model
				break
			}
		}

		result.AgentMetrics = append(result.AgentMetrics, AgentMetric{
			Agent:        agent,
			Model:        agentModel,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			Calls:        usage.TotalCalls,
			EstCostUSD:   metrics.EstimateCost(agentModel, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens),
		})
	}

	result.InputTokens = totalIn
	result.OutputTokens = totalOut
	result.CacheTokens = totalCache
	result.TotalCalls = totalCalls

	for _, rec := range allRecords {
		result.EstCostUSD += metrics.EstimateCost(rec.Model, rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens)
	}

	for _, evt := range taskEvents {
		switch evt.Type {
		case event.TaskComplete:
			result.TasksCompleted++
		case event.TaskFailed:
			result.TasksFailed++
		}
	}

	result.ModelPatch = diff
	result.PatchEmpty = strings.TrimSpace(diff) == ""
	pred.ModelPatch = diff

	// Write patch for debugging.
	_ = os.WriteFile(filepath.Join(runDir, "model_patch.diff"), []byte(diff), 0644)

	if reqErr != nil {
		if result.Error == "" {
			result.Error = reqErr.Error()
		}
		return result, pred, reqErr
	}

	slog.Info("swebench: completed instance",
		"instance", inst.InstanceID,
		"duration", result.Duration.Round(time.Second),
		"patch_empty", result.PatchEmpty,
		"cost", fmt.Sprintf("$%.4f", result.EstCostUSD))

	return result, pred, nil
}

// buildRequest constructs the agent request from a SWE-bench instance.
// repoRef is the path reference for the request (e.g., "$SCRATCH_DIR/repo").
func (r *SWEBenchRunner) buildRequest(inst SWEBenchInstance, repoRef string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("Fix the following GitHub issue. The repository is cloned at %s.\n\n", repoRef))
	b.WriteString("## Repository\n")
	b.WriteString(fmt.Sprintf("- Repository: %s\n", inst.Repo))
	b.WriteString(fmt.Sprintf("- Path: %s\n\n", repoRef))
	b.WriteString("## Issue\n")
	b.WriteString(stripHTMLComments(inst.Problem))
	b.WriteString("\n")

	if r.Hints && inst.Hints != "" {
		b.WriteString("\n## Discussion Context\n")
		b.WriteString("The following is discussion from maintainers about this issue. ")
		b.WriteString("This may override or refine the approach described above — ")
		b.WriteString("pay close attention to any consensus reached:\n\n")
		b.WriteString(stripHTMLComments(inst.Hints))
		b.WriteString("\n")
	}

	b.WriteString("\n## Instructions\n")
	b.WriteString("- The repository is already cloned and checked out at the correct commit\n")
	b.WriteString(fmt.Sprintf("- The repo is in your working directory at %s\n", repoRef))
	b.WriteString(fmt.Sprintf("- Use `cd %s` first, then explore with find, grep, cat, etc.\n", repoRef))
	b.WriteString("- Make the minimal code changes needed to fix the issue\n")
	b.WriteString("- Edit source files directly using shell commands (sed, patch, or cat with heredoc)\n")
	b.WriteString("- IMPORTANT: After making your fix, run the relevant tests to verify correctness\n")
	b.WriteString("  - Look for test files related to the changed module (e.g., tests/test_<module>.py)\n")
	b.WriteString("  - Run them with: python -m pytest <test_file> -x -q 2>&1 | head -50\n")
	b.WriteString("  - If tests fail, read the failure output and iterate on your fix\n")
	b.WriteString("- Do NOT create new test files or modify existing tests\n")
	b.WriteString("- Do NOT make unrelated changes or refactor code beyond what is needed for the fix\n")
	b.WriteString("- Focus on producing a correct, minimal patch\n")

	return b.String()
}

var htmlCommentRe = regexp.MustCompile(`<!--[\s\S]*?-->`)

// stripHTMLComments removes HTML comments from text and collapses
// resulting blank lines.
func stripHTMLComments(s string) string {
	s = htmlCommentRe.ReplaceAllString(s, "")
	// Collapse runs of 3+ newlines into 2.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// buildDefinition creates a minimal system definition for SWE-bench.
// Uses conductor + developer only (single-agent mode).
func (r *SWEBenchRunner) buildDefinition() (*config.FabricDef, error) {
	def, err := deepCopyDef(r.BaseFabricDef)
	if err != nil {
		return nil, err
	}

	// Keep only conductor + developer.
	var kept []runtime.AgentDefinition
	for _, a := range def.Agents {
		if a.Name == "conductor" || a.Name == "developer" {
			// Disable verify for SWE-bench (repos have their own test suites).
			a.Verify = nil
			if a.Name == "developer" {
				// Shell-only mode: edit files in the cloned repo via shell, no file artifacts.
				a.ShellOnly = true
			}
			kept = append(kept, a)
		}
	}
	def.Agents = kept

	// Point AgentsDir to a temp directory with SWE-bench-specific developer knowledge.
	// This overrides the default developer.md which instructs producing file artifacts.
	tmpAgentsDir, err := os.MkdirTemp("", "agentfab-swebench-agents-")
	if err != nil {
		return nil, fmt.Errorf("create temp agents dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpAgentsDir, "developer.md"), []byte(swebenchDeveloperKnowledge), 0644); err != nil {
		return nil, fmt.Errorf("write swebench developer knowledge: %w", err)
	}
	// Keep conductor.md from defaults by not writing it — setup falls back to embedded.
	def.AgentsDir = tmpAgentsDir

	return &def, nil
}

// swebenchDeveloperKnowledge overrides the default developer special knowledge
// for SWE-bench tasks. The key difference: use shell commands to edit files
// in-place in the cloned repository instead of producing file artifacts.
const swebenchDeveloperKnowledge = `# Developer (SWE-bench Mode)

You are the Developer agent. Your task is to fix bugs in existing repositories by making minimal code changes.

## Critical Rules

- Work EXCLUSIVELY within $SCRATCH_DIR/repo using the shell tool
- Do NOT produce file artifacts — all changes MUST be made directly in the repository files
- Every change must be made via the shell tool (sed, patch, python scripts, or cat with heredoc)
- Make the MINIMAL code changes needed to fix the issue
- Do NOT create new test files or modify existing tests
- Do NOT refactor code beyond what is needed for the fix

## Workflow

1. Navigate: cd $SCRATCH_DIR/repo
2. Explore: Use find, grep, cat via shell to locate relevant code
3. Understand: Read the source files to understand the bug's root cause
4. Fix: Edit the source file(s) using sed, patch, or python -c via shell
5. Verify: Run git diff to confirm your changes are correct
6. Test: If possible, run the project's test suite or a minimal reproduction script

## Shell Editing Patterns

PREFERRED — use Python for ALL edits (cross-platform, handles multi-line):
  shell: python3 -c "
  with open('path/to/file.py') as f: content = f.read()
  content = content.replace('old_code', 'new_code')
  with open('path/to/file.py', 'w') as f: f.write(content)
  "

For simple single-line fixes (note: macOS sed requires -i ''):
  shell: sed -i '' 's/old_pattern/new_pattern/' path/to/file.py

Always verify with:
  shell: git diff

## Non-Negotiables

- The shell tool is your ONLY way to interact with the repository
- Never skip verification — always run git diff before finishing
- If a fix attempt breaks something, revert and try a different approach
- Report exactly what you changed and why in your result
`

// copyGitDiffToRunDir saves the git diff from the scratch-dir repo
// into the persistent run directory for post-mortem inspection.
func copyGitDiffToRunDir(repoDir, runDir string) error {
	diff, err := CaptureGitDiff(repoDir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runDir, "model_patch.diff"), []byte(diff), 0644)
}

// sanitizeInstanceID converts an instance_id to a safe directory name.
func sanitizeInstanceID(id string) string {
	return strings.ReplaceAll(id, "/", "__")
}
