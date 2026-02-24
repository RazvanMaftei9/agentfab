package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SWEBenchInstance represents a single SWE-bench task instance.
type SWEBenchInstance struct {
	InstanceID string   `json:"instance_id"`
	Repo       string   `json:"repo"`
	BaseCommit string   `json:"base_commit"`
	Problem    string   `json:"problem_statement"`
	Hints      string   `json:"hints_text"`
	Patch      string   `json:"patch"`      // gold standard (not shown to agent)
	TestPatch  string   `json:"test_patch"` // held-out tests (not shown to agent)
	FailToPass []string `json:"FAIL_TO_PASS"`
	PassToPass []string `json:"PASS_TO_PASS"`
	Version    string   `json:"version"`
	CreatedAt  string   `json:"created_at"`
}

// SWEBenchPrediction is the output format for SWE-bench evaluation.
type SWEBenchPrediction struct {
	InstanceID      string `json:"instance_id"`
	ModelNameOrPath string `json:"model_name_or_path"`
	ModelPatch      string `json:"model_patch"`
}

// SWEBenchResult captures metrics for a single SWE-bench instance run.
type SWEBenchResult struct {
	InstanceID string `json:"instance_id"`
	Repo       string `json:"repo"`

	// Patch
	ModelPatch string `json:"model_patch,omitempty"`
	PatchEmpty bool   `json:"patch_empty"`

	// Cost
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheTokens  int64   `json:"cache_tokens"`
	TotalCalls   int64   `json:"total_calls"`
	EstCostUSD   float64 `json:"est_cost_usd"`

	// Time
	Duration time.Duration `json:"duration"`

	// Quality (from agent perspective)
	TasksCompleted int `json:"tasks_completed"`
	TasksFailed    int `json:"tasks_failed"`
	TasksEscalated int `json:"tasks_escalated"`

	// Per-agent breakdown
	AgentMetrics []AgentMetric `json:"agent_metrics,omitempty"`

	Error string `json:"error,omitempty"`
}

// LoadSWEBenchInstances reads instances from a JSONL file.
func LoadSWEBenchInstances(path string) ([]SWEBenchInstance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SWE-bench dataset: %w", err)
	}
	defer f.Close()

	var instances []SWEBenchInstance
	scanner := bufio.NewScanner(f)
	// SWE-bench problem_statements can be large.
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var inst SWEBenchInstance
		if err := json.Unmarshal([]byte(line), &inst); err != nil {
			return nil, fmt.Errorf("parse line %d: %w", lineNo, err)
		}
		instances = append(instances, inst)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan SWE-bench dataset: %w", err)
	}
	return instances, nil
}

// FilterSWEBenchInstances filters instances by ID or repo name.
func FilterSWEBenchInstances(instances []SWEBenchInstance, ids []string, repos []string) []SWEBenchInstance {
	if len(ids) == 0 && len(repos) == 0 {
		return instances
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	repoSet := make(map[string]bool, len(repos))
	for _, r := range repos {
		repoSet[r] = true
	}

	var filtered []SWEBenchInstance
	for _, inst := range instances {
		if len(ids) > 0 && idSet[inst.InstanceID] {
			filtered = append(filtered, inst)
			continue
		}
		if len(repos) > 0 && repoSet[inst.Repo] {
			filtered = append(filtered, inst)
		}
	}
	return filtered
}

// WritePredictions writes predictions in SWE-bench JSONL format.
func WritePredictions(predictions []SWEBenchPrediction, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create predictions file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, p := range predictions {
		if err := enc.Encode(p); err != nil {
			return fmt.Errorf("encode prediction %s: %w", p.InstanceID, err)
		}
	}
	return nil
}

// WriteSWEBenchResultsJSON writes SWE-bench results to a JSON file.
func WriteSWEBenchResultsJSON(results []SWEBenchResult, path string) error {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// CloneRepo clones a repository to cacheDir (if not cached) and creates a
// working copy at workDir checked out to the specified commit.
func CloneRepo(repo, commit, cacheDir, workDir string) error {
	repoURL := fmt.Sprintf("https://github.com/%s.git", repo)
	cachedPath := filepath.Join(cacheDir, strings.ReplaceAll(repo, "/", "__"))

	// Clone to cache if not present.
	if _, err := os.Stat(cachedPath); os.IsNotExist(err) {
		cmd := exec.Command("git", "clone", "--quiet", repoURL, cachedPath)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("clone %s: %w", repo, err)
		}
	}

	// Create working copy by cloning from local cache.
	cmd := exec.Command("git", "clone", "--quiet", "--local", cachedPath, workDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("local clone %s: %w", repo, err)
	}

	// Checkout the specific commit.
	cmd = exec.Command("git", "checkout", "--quiet", "--force", commit)
	cmd.Dir = workDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("checkout %s at %s: %w", repo, commit[:min(8, len(commit))], err)
	}

	// Clean any untracked files.
	cmd = exec.Command("git", "clean", "-fdx", "--quiet")
	cmd.Dir = workDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clean %s: %w", repo, err)
	}

	return nil
}

// CaptureGitDiff stages all changes (including new files) and returns the
// unified diff. Plain `git diff` misses untracked files — we need
// `git add -A && git diff --cached` to capture new file creations.
func CaptureGitDiff(dir string) (string, error) {
	// Stage everything (new, modified, deleted).
	add := exec.Command("git", "add", "-A")
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add -A: %s: %w", out, err)
	}

	// Diff staged changes against HEAD.
	diff := exec.Command("git", "diff", "--cached")
	diff.Dir = dir
	out, err := diff.Output()
	if err != nil {
		return "", fmt.Errorf("git diff --cached: %w", err)
	}
	return string(out), nil
}

// GenerateSWEBenchReport produces a markdown report for SWE-bench results.
func GenerateSWEBenchReport(results []SWEBenchResult) string {
	var b strings.Builder

	b.WriteString("# SWE-Bench Results\n\n")

	total := len(results)
	var patchCount, errorCount int
	var totalCost float64
	var totalDuration time.Duration
	var totalTokensIn, totalTokensOut int64

	for _, r := range results {
		if !r.PatchEmpty {
			patchCount++
		}
		if r.Error != "" {
			errorCount++
		}
		totalCost += r.EstCostUSD
		totalDuration += r.Duration
		totalTokensIn += r.InputTokens
		totalTokensOut += r.OutputTokens
	}

	b.WriteString("## Summary\n\n")
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	b.WriteString(fmt.Sprintf("| Instances attempted | %d |\n", total))
	b.WriteString(fmt.Sprintf("| Patches produced | %d (%.1f%%) |\n", patchCount, safePct(patchCount, total)))
	b.WriteString(fmt.Sprintf("| Errors | %d |\n", errorCount))
	b.WriteString(fmt.Sprintf("| Total cost | $%.2f |\n", totalCost))
	b.WriteString(fmt.Sprintf("| Avg cost/instance | $%.2f |\n", totalCost/float64(max(total, 1))))
	b.WriteString(fmt.Sprintf("| Total duration | %s |\n", totalDuration.Round(time.Second)))
	b.WriteString(fmt.Sprintf("| Avg duration | %s |\n", (totalDuration / time.Duration(max(total, 1))).Round(time.Second)))
	b.WriteString(fmt.Sprintf("| Total tokens (in/out) | %dk / %dk |\n", totalTokensIn/1000, totalTokensOut/1000))

	// Per-instance table.
	b.WriteString("\n## Per-Instance Results\n\n")
	b.WriteString("| Instance | Repo | Duration | Cost | Patch | Error |\n")
	b.WriteString("|----------|------|----------|------|-------|-------|\n")

	for _, r := range results {
		patchStatus := "empty"
		if !r.PatchEmpty {
			patchStatus = "yes"
		}
		errStr := ""
		if r.Error != "" {
			errStr = r.Error
			if len(errStr) > 40 {
				errStr = errStr[:37] + "..."
			}
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | $%.2f | %s | %s |\n",
			r.InstanceID,
			r.Repo,
			r.Duration.Round(time.Second),
			r.EstCostUSD,
			patchStatus,
			errStr,
		))
	}

	// Per-repo breakdown.
	type repoStat struct {
		count, patches int
		cost           float64
	}
	repoStats := make(map[string]*repoStat)
	for _, r := range results {
		s, ok := repoStats[r.Repo]
		if !ok {
			s = &repoStat{}
			repoStats[r.Repo] = s
		}
		s.count++
		if !r.PatchEmpty {
			s.patches++
		}
		s.cost += r.EstCostUSD
	}

	b.WriteString("\n## Per-Repository Breakdown\n\n")
	b.WriteString("| Repository | Instances | Patches | Avg Cost |\n")
	b.WriteString("|------------|-----------|---------|----------|\n")
	for _, repo := range sortedKeys(repoStats) {
		s := repoStats[repo]
		b.WriteString(fmt.Sprintf("| %s | %d | %d (%.0f%%) | $%.2f |\n",
			repo, s.count, s.patches, safePct(s.patches, s.count), s.cost/float64(s.count)))
	}

	b.WriteString("\n---\n")
	b.WriteString("*Run `python -m swebench.harness.run_evaluation` on predictions.jsonl to get resolution rates.*\n")

	return b.String()
}

func safePct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

